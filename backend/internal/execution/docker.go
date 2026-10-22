package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type dockerAPI struct {
	http    *http.Client
	version string
}

func newDockerAPI(socket string) (*dockerAPI, error) {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	api := &dockerAPI{http: client, version: "v1.41"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ver struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := api.do(ctx, http.MethodGet, "/version", nil, &ver); err != nil {
		return nil, unavailable("docker broker socket is unavailable")
	}
	if ver.APIVersion != "" {
		api.version = "v" + ver.APIVersion
	}
	return api, nil
}

func (d *dockerAPI) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	if !strings.HasPrefix(path, "/v") && path != "/version" {
		path = "/" + d.version + path
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.http.Do(req)
	if err != nil {
		return unavailable("docker broker call failed")
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("docker %s %s: %s", method, path, strings.TrimSpace(string(payload)))
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, out)
}

func (d *dockerAPI) createNetwork(ctx context.Context, name string, internal bool, labels map[string]string) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodPost, "/networks/create", map[string]any{
		"Name":           name,
		"CheckDuplicate": true,
		"Internal":       internal,
		"EnableIPv6":     false,
		"Labels":         labels,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *dockerAPI) removeNetwork(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/networks/"+id, nil, nil)
	if err == nil || dockerGone(err) {
		return nil
	}
	return err
}

func (d *dockerAPI) networkGateway(ctx context.Context, id string) (string, error) {
	var out struct {
		IPAM struct {
			Config []struct {
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := d.do(ctx, http.MethodGet, "/networks/"+id, nil, &out); err != nil {
		return "", err
	}
	for _, cfg := range out.IPAM.Config {
		if cfg.Gateway != "" {
			return cfg.Gateway, nil
		}
	}
	return "", fmt.Errorf("docker network %s has no gateway", id)
}

func (d *dockerAPI) connectNetwork(ctx context.Context, networkID, containerID string) error {
	return d.do(ctx, http.MethodPost, "/networks/"+networkID+"/connect", map[string]any{
		"Container": containerID,
	}, nil)
}

func (d *dockerAPI) createContainer(ctx context.Context, name string, body map[string]any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &out)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *dockerAPI) startContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

func (d *dockerAPI) waitContainer(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, &out); err != nil {
		return 0, err
	}
	return out.StatusCode, nil
}

func (d *dockerAPI) killContainer(ctx context.Context, id, signal string) error {
	if signal == "" {
		signal = "SIGKILL"
	}
	err := d.do(ctx, http.MethodPost, "/containers/"+id+"/kill?signal="+url.QueryEscape(signal), nil, nil)
	if err == nil || dockerGone(err) {
		return nil
	}
	return err
}

func dockerGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not running") || strings.Contains(msg, "no such container") || strings.Contains(msg, "409") || strings.Contains(msg, "404")
}

func (d *dockerAPI) removeContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/containers/"+id+"?force=1", nil, nil)
}

type dockerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
	HostConfig      SecurityReport `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (d *dockerAPI) inspect(ctx context.Context, id string) (*dockerInspect, error) {
	var out dockerInspect
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &out); err != nil {
		return nil, err
	}
	out.HostConfig.User = out.Config.User
	return &out, nil
}

func (d *dockerAPI) logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	path := "/containers/" + id + "/logs?stdout=1&stderr=1"
	if follow {
		path += "&follow=1"
	}
	if !strings.HasPrefix(path, "/v") {
		path = "/" + d.version + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("docker logs: %s", strings.TrimSpace(string(payload)))
	}
	return res.Body, nil
}

func (d *dockerAPI) listContainers(ctx context.Context, labels map[string]string) ([]dockerListed, error) {
	filters := map[string][]string{"label": {}}
	for k, v := range labels {
		filters["label"] = append(filters["label"], k+"="+v)
	}
	raw, _ := json.Marshal(filters)
	var out []dockerListed
	err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(raw)), nil, &out)
	return out, err
}

func (d *dockerAPI) listNetworks(ctx context.Context, labels map[string]string) ([]dockerListed, error) {
	filters := map[string][]string{"label": {}}
	for k, v := range labels {
		filters["label"] = append(filters["label"], k+"="+v)
	}
	raw, _ := json.Marshal(filters)
	var out []dockerListed
	err := d.do(ctx, http.MethodGet, "/networks?filters="+url.QueryEscape(string(raw)), nil, &out)
	return out, err
}

type dockerListed struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// BuildImage builds an image from a Dockerfile context tar. It does not run a host shell.
func BuildImage(ctx context.Context, socket, tag string, contextTar io.Reader) error {
	api, err := newDockerAPI(socket)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("t", tag)
	q.Set("dockerfile", "Dockerfile")
	q.Set("rm", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/"+api.version+"/build?"+q.Encode(), contextTar)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := api.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	var buildErr string
	for sc.Scan() {
		var line struct {
			Error  string `json:"error"`
			Stream string `json:"stream"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Error != "" {
			buildErr = line.Error
		}
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("docker build failed: %s", buildErr)
	}
	if buildErr != "" {
		return fmt.Errorf("docker build failed: %s", buildErr)
	}
	return sc.Err()
}

type logBuffers struct {
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	truncated bool
	limit     int
}

func (b *logBuffers) add(stream byte, payload []byte) {
	dst := &b.stdout
	if stream == 2 {
		dst = &b.stderr
	}
	remain := b.limit - dst.Len()
	if remain <= 0 {
		b.truncated = true
		return
	}
	if len(payload) > remain {
		_, _ = dst.Write(payload[:remain])
		b.truncated = true
		return
	}
	_, _ = dst.Write(payload)
}

func demuxDockerLogs(r io.Reader, limit int) (stdout, stderr string, truncated bool, err error) {
	buf := &logBuffers{limit: limit}
	header := make([]byte, 8)
	for {
		_, err = io.ReadFull(r, header)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return buf.stdout.String(), buf.stderr.String(), buf.truncated, nil
		}
		if err != nil {
			return buf.stdout.String(), buf.stderr.String(), buf.truncated, err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size > 8<<20 {
			return buf.stdout.String(), buf.stderr.String(), true, nil
		}
		payload := make([]byte, size)
		if _, err = io.ReadFull(r, payload); err != nil {
			return buf.stdout.String(), buf.stderr.String(), buf.truncated, err
		}
		buf.add(header[0], payload)
		if buf.truncated && buf.stdout.Len() >= limit && buf.stderr.Len() >= limit {
			return buf.stdout.String(), buf.stderr.String(), true, nil
		}
	}
}
