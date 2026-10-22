package execution

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// Broker is the only type that opens the Docker socket. It speaks the fixed
// JSON protocol and never evaluates a host shell.
type Broker struct {
	cfg    Config
	id     string
	docker *dockerAPI

	mu        sync.Mutex
	live      map[uuid.UUID]*liveJob
	done      map[uuid.UUID]*RunResult
	brokerNet string
}

type liveJob struct {
	cancel      context.CancelFunc
	cancelled   atomic.Bool
	containerID atomic.Value
	done        chan struct{}
}

// NewBroker binds a stable broker id. An empty id is refused so cleanup cannot
// match every container.
func NewBroker(cfg Config, id string) (*Broker, error) {
	if strings.TrimSpace(id) == "" {
		return nil, unavailable("execution broker id is empty")
	}
	cfg = cfg.withDefaults()
	if cfg.BrokerToken == "" {
		return nil, unavailable("execution broker token is empty")
	}
	return &Broker{
		cfg:  cfg,
		id:   id,
		live: map[uuid.UUID]*liveJob{},
		done: map[uuid.UUID]*RunResult{},
	}, nil
}

func LoadBrokerID(dir string) (string, error) {
	if dir == "" {
		return "", unavailable("execution broker state directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "broker_id")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id := uuid.NewString()
		if err := os.WriteFile(path, []byte(id), 0600); err != nil {
			return "", err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", unavailable("execution broker id is empty")
	}
	return id, nil
}

func (b *Broker) ID() string { return b.id }

func (b *Broker) dockerAPI(ctx context.Context) (*dockerAPI, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.docker != nil {
		return b.docker, nil
	}
	api, err := newDockerAPI(b.cfg.DockerSocket)
	if err != nil {
		return nil, err
	}
	b.docker = api
	return api, nil
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/jobs", b.handleRun)
	mux.HandleFunc("/v1/jobs/", b.handleJob)
	mux.HandleFunc("/v1/reconcile", b.handleReconcile)
	mux.HandleFunc("/v1/cleanup", b.handleCleanup)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !b.authorized(r) {
			writeBrokerError(w, &contract.Error{Code: contract.CodeUnauthorized, Scope: contract.ScopeRequest, Message: "execution broker token rejected"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (b *Broker) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(got) != len(b.cfg.BrokerToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(b.cfg.BrokerToken)) == 1
}

func (b *Broker) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeBrokerError(w, invalidInput("execution protocol method is not supported"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeBrokerError(w, invalidInput("execution protocol body is not valid"))
		return
	}
	var req RunRequest
	if err := decodeStrict(raw, &req); err != nil {
		writeBrokerError(w, err)
		return
	}
	res, err := b.Run(r.Context(), req)
	if err != nil {
		writeBrokerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (b *Broker) handleJob(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	parts := strings.Split(rest, "/")
	id, err := uuid.Parse(parts[0])
	if err != nil {
		writeBrokerError(w, notFound("execution job was not found"))
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		res, err := b.Inspect(r.Context(), id)
		if err != nil {
			writeBrokerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		res, err := b.Cancel(r.Context(), id)
		if err != nil {
			writeBrokerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeBrokerError(w, invalidInput("execution protocol method is not supported"))
	}
}

func (b *Broker) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeBrokerError(w, invalidInput("execution protocol method is not supported"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeBrokerError(w, invalidInput("execution protocol body is not valid"))
		return
	}
	var req reconcileRequest
	if len(raw) > 0 {
		if err := decodeStrict(raw, &req); err != nil {
			writeBrokerError(w, err)
			return
		}
	}
	if req.Protocol != "" && req.Protocol != ProtocolVersion {
		writeBrokerError(w, invalidInput("execution protocol version is not supported"))
		return
	}
	n, err := b.Reconcile(r.Context(), req.KillContainers)
	if err != nil {
		writeBrokerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"killed": n})
}

func (b *Broker) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeBrokerError(w, invalidInput("execution protocol method is not supported"))
		return
	}
	n, err := b.Cleanup(r.Context())
	if err != nil {
		writeBrokerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": n})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeBrokerError(w http.ResponseWriter, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) || ce == nil {
		ce = unavailable("execution broker failed")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ce.Status())
	_ = json.NewEncoder(w).Encode(ce.Envelope())
}

// Run starts one isolated container and waits for a terminal supervision result.
func (b *Broker) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if err := req.validate(b.cfg.AllowedImages); err != nil {
		return nil, err
	}
	if err := ApproveMount(b.cfg.WorkspaceRoot, req.WorkspaceHost); err != nil {
		return nil, err
	}
	if err := os.Chmod(req.WorkspaceHost, 0777); err != nil {
		return nil, unavailable("execution workspace is not ready")
	}
	api, err := b.dockerAPI(ctx)
	if err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	live := &liveJob{cancel: cancel, done: make(chan struct{})}
	b.mu.Lock()
	if _, exists := b.live[req.JobID]; exists {
		b.mu.Unlock()
		cancel()
		return nil, invalidState("execution job is already running")
	}
	b.live[req.JobID] = live
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.live, req.JobID)
		b.mu.Unlock()
		close(live.done)
	}()

	clean := &jobCleanup{workspace: req.WorkspaceHost}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), contract.CommandCleanupWindow)
		defer ccancel()
		b.cleanupJob(cctx, api, clean)
	}()

	proxyURL := ""
	caHost := ""
	network := "none"
	if !req.Offline {
		proxyURL, caHost, err = b.startProxy(jobCtx, api, req, clean)
		if err != nil {
			return nil, err
		}
		network = clean.networkName
	}
	body, err := containerCreateBody(containerCreate{
		Image:         req.Image,
		Argv:          req.Argv,
		Network:       network,
		WorkspaceHost: req.WorkspaceHost,
		CACertHost:    caHost,
		ProxyURL:      proxyURL,
		Memory:        b.cfg.MemoryBytes,
		NanoCPUs:      b.cfg.NanoCPUs,
		Pids:          b.cfg.PidsLimit,
		Labels:        b.labels(req.JobID, req.RunID, req.AttemptID, ComponentJob),
	})
	if err != nil {
		return nil, err
	}
	execID, err := api.createContainer(jobCtx, "oma-exec-"+req.JobID.String(), body)
	if err != nil {
		return nil, unavailableCause("execution container was not created", err)
	}
	clean.execID = execID
	live.containerID.Store(execID)
	if err := api.startContainer(jobCtx, execID); err != nil {
		return nil, unavailableCause("execution container was not started", err)
	}
	res := b.supervise(jobCtx, api, live, req, execID)
	b.mu.Lock()
	b.done[req.JobID] = res
	b.mu.Unlock()
	return res, nil
}

type jobCleanup struct {
	execID      string
	proxyID     string
	networkID   string
	networkName string
	secrets     string
	workspace   string
}

func (b *Broker) startProxy(ctx context.Context, api *dockerAPI, req RunRequest, clean *jobCleanup) (proxyURL, caHost string, err error) {
	netID, netName, err := b.jobNetwork(ctx, api, req)
	if err != nil {
		return "", "", err
	}
	clean.networkID = netID
	clean.networkName = netName
	brokerNet, err := b.ensureBrokerNetwork(ctx, api)
	if err != nil {
		return "", "", err
	}
	ca, err := newInterceptCA()
	if err != nil {
		return "", "", unavailable("execution intercept CA was not created")
	}
	secrets := filepath.Join(b.cfg.WorkspaceRoot, "secrets", req.JobID.String())
	if err := os.MkdirAll(secrets, 0700); err != nil {
		return "", "", unavailable("execution secrets directory was not created")
	}
	clean.secrets = secrets
	certPath := filepath.Join(secrets, "ca.pem")
	keyPath := filepath.Join(secrets, "ca.key")
	if err := os.WriteFile(certPath, ca.certPEM, 0444); err != nil {
		return "", "", unavailable("execution intercept CA was not created")
	}
	if err := os.WriteFile(keyPath, ca.keyPEM, 0444); err != nil {
		return "", "", unavailable("execution intercept CA was not created")
	}
	captureURL, err := b.rewriteCapture(ctx, api, brokerNet, req.CaptureURL)
	if err != nil {
		return "", "", err
	}
	body, err := proxyCreateBody(proxyCreate{
		Image:        req.Image,
		Network:      brokerNet,
		CACertHost:   certPath,
		CAKeyHost:    keyPath,
		CaptureURL:   captureURL,
		CaptureToken: req.CaptureToken,
		JobID:        req.JobID.String(),
		Origins:      req.AllowedOrigins,
		Memory:       b.cfg.MemoryBytes,
		NanoCPUs:     b.cfg.NanoCPUs,
		Pids:         b.cfg.PidsLimit,
		Labels:       b.labels(req.JobID, req.RunID, req.AttemptID, ComponentProxy),
	})
	if err != nil {
		return "", "", err
	}
	proxyID, err := api.createContainer(ctx, "oma-proxy-"+req.JobID.String(), body)
	if err != nil {
		return "", "", unavailable("egress proxy was not created")
	}
	clean.proxyID = proxyID
	if err := api.startContainer(ctx, proxyID); err != nil {
		return "", "", unavailable("egress proxy was not started")
	}
	if err := api.connectNetwork(ctx, netID, proxyID); err != nil {
		return "", "", unavailable("egress proxy was not attached to the job network")
	}
	if err := waitListening(ctx, api, proxyID); err != nil {
		return "", "", err
	}
	ip := ""
	for i := 0; i < 25 && ip == ""; i++ {
		ins, inspectErr := api.inspect(ctx, proxyID)
		if inspectErr == nil {
			if n, ok := ins.NetworkSettings.Networks[netName]; ok {
				ip = n.IPAddress
			}
		}
		if ip == "" {
			select {
			case <-ctx.Done():
				return "", "", unavailable("egress proxy address is unavailable")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if ip == "" {
		return "", "", unavailable("egress proxy address is unavailable")
	}
	return "http://" + ip + ":8080", certPath, nil
}

func (b *Broker) jobNetwork(ctx context.Context, api *dockerAPI, req RunRequest) (string, string, error) {
	name := "oma-net-" + strings.ReplaceAll(req.JobID.String(), "-", "")[:12]
	id, err := api.createNetwork(ctx, name, true, b.labels(req.JobID, req.RunID, req.AttemptID, ComponentNet))
	if err != nil {
		return "", "", unavailable("execution network was not created")
	}
	return id, name, nil
}

func (b *Broker) ensureBrokerNetwork(ctx context.Context, api *dockerAPI) (string, error) {
	b.mu.Lock()
	if b.brokerNet != "" {
		name := b.brokerNet
		b.mu.Unlock()
		return name, nil
	}
	b.mu.Unlock()
	name := "oma-broker-" + strings.ReplaceAll(b.id, "-", "")
	if len(name) > 40 {
		name = name[:40]
	}
	id, err := api.createNetwork(ctx, name, false, map[string]string{
		LabelOwner:     OwnerValue,
		LabelBroker:    b.id,
		LabelComponent: ComponentBroker,
	})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		id = name
		err = nil
	}
	if err != nil {
		return "", unavailable("execution broker network was not created")
	}
	b.mu.Lock()
	b.brokerNet = name
	b.mu.Unlock()
	_ = id
	return name, nil
}

func (b *Broker) rewriteCapture(ctx context.Context, api *dockerAPI, brokerNet, captureURL string) (string, error) {
	parsed, err := url.Parse(captureURL)
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return "", unavailable("capture callback URL is not allowed")
	}
	gw, err := api.networkGateway(ctx, brokerNet)
	if err != nil || gw == "" {
		return "", unavailable("execution broker network gateway is unavailable")
	}
	parsed.Host = net.JoinHostPort(gw, parsed.Port())
	return parsed.String(), nil
}

func waitListening(ctx context.Context, api *dockerAPI, id string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return unavailable("egress proxy did not become ready")
		}
		body, err := api.logs(ctx, id, false)
		if err == nil {
			stdout, stderr, _, _ := demuxDockerLogs(body, 8192)
			body.Close()
			if strings.Contains(stdout, "listening") || strings.Contains(stderr, "listening") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return unavailable("egress proxy did not become ready")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return unavailable("egress proxy did not become ready")
}

func (b *Broker) supervise(ctx context.Context, api *dockerAPI, live *liveJob, req RunRequest, execID string) *RunResult {
	res := &RunResult{JobID: req.JobID, ContainerID: execID, State: string(contract.JobOutcomeUnknown)}
	logCtx, logCancel := context.WithCancel(ctx)
	defer logCancel()
	snap, truncated := streamContainerLogs(logCtx, api, execID, b.cfg.OutputLimitBytes)
	waitCh := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := api.waitContainer(ctx, execID)
		waitCh <- struct {
			code int
			err  error
		}{code, err}
	}()
	started := time.Now()
	limit := time.Duration(req.TimeoutSeconds) * time.Second
	timer := time.NewTimer(limit)
	defer timer.Stop()
	state := ""
	var exit *int
	select {
	case <-ctx.Done():
		b.killNow(api, execID)
		if live.cancelled.Load() {
			state = string(contract.JobKilled)
		} else {
			state = string(contract.JobInterrupted)
		}
	case <-timer.C:
		b.killNow(api, execID)
		state = string(contract.JobInterrupted)
	case <-truncated:
		b.killNow(api, execID)
		state = string(contract.JobKilled)
		res.Truncated = true
		res.ErrorCode = string(contract.CodePayloadTooLarge)
	case wr := <-waitCh:
		if live.cancelled.Load() || ctx.Err() != nil {
			state = string(contract.JobKilled)
			if ctx.Err() != nil && !live.cancelled.Load() {
				state = string(contract.JobInterrupted)
			}
			break
		}
		if wr.err != nil {
			b.killNow(api, execID)
			state = string(contract.JobOutcomeUnknown)
			break
		}
		// A kill from the timeout path can win this receive before the timer case.
		if time.Since(started) >= limit-50*time.Millisecond {
			state = string(contract.JobInterrupted)
			break
		}
		ins, err := api.inspect(context.WithoutCancel(ctx), execID)
		if err == nil {
			report := ins.HostConfig
			res.Security = &report
		}
		code := wr.code
		exit = &code
		state = string(contract.JobFinished)
	}
	logCancel()
	stdout, stderr, logsTrunc := b.finalLogs(ctx, api, execID, snap)
	res.Stdout = stdout
	res.Stderr = stderr
	if logsTrunc {
		res.Truncated = true
	}
	res.State = state
	if state == string(contract.JobFinished) {
		res.ExitCode = exit
	}
	return res
}

// finalLogs reads the bounded log snapshot after the container stops. The follow
// stream can still be empty when a short command exits before Docker delivers it.
func (b *Broker) finalLogs(ctx context.Context, api *dockerAPI, id string, snap func() (string, string, bool)) (string, string, bool) {
	streamOut, streamErr, streamTrunc := snap()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	body, err := api.logs(cctx, id, false)
	if err != nil {
		return streamOut, streamErr, streamTrunc
	}
	defer body.Close()
	stdout, stderr, truncated, err := demuxDockerLogs(body, b.cfg.OutputLimitBytes)
	if err != nil && stdout == "" && stderr == "" {
		return streamOut, streamErr, streamTrunc
	}
	if len(stdout) < len(streamOut) {
		stdout = streamOut
	}
	if len(stderr) < len(streamErr) {
		stderr = streamErr
	}
	return stdout, stderr, truncated || streamTrunc
}

func (b *Broker) killNow(api *dockerAPI, id string) {
	cctx, cancel := context.WithTimeout(context.Background(), contract.CommandCleanupWindow)
	defer cancel()
	_ = api.killContainer(cctx, id, "SIGKILL")
}

func streamContainerLogs(ctx context.Context, api *dockerAPI, id string, limit int) (func() (string, string, bool), <-chan struct{}) {
	if limit <= 0 {
		limit = 1 << 20
	}
	hit := make(chan struct{})
	var once sync.Once
	state := &logBuffers{limit: limit}
	var mu sync.Mutex
	go func() {
		body, err := api.logs(ctx, id, true)
		if err != nil {
			return
		}
		defer body.Close()
		header := make([]byte, 8)
		for {
			if _, err := io.ReadFull(body, header); err != nil {
				return
			}
			size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
			if size < 0 || size > 8<<20 {
				once.Do(func() { close(hit) })
				return
			}
			payload := make([]byte, size)
			if _, err := io.ReadFull(body, payload); err != nil {
				return
			}
			mu.Lock()
			state.add(header[0], payload)
			truncated := state.truncated
			mu.Unlock()
			if truncated {
				once.Do(func() { close(hit) })
				return
			}
		}
	}()
	snap := func() (string, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return state.stdout.String(), state.stderr.String(), state.truncated
	}
	return snap, hit
}

func (b *Broker) Cancel(ctx context.Context, id uuid.UUID) (*RunResult, error) {
	b.mu.Lock()
	live := b.live[id]
	done := b.done[id]
	b.mu.Unlock()
	if live == nil {
		if done != nil {
			return done, nil
		}
		return nil, notFound("execution job was not found")
	}
	live.cancelled.Store(true)
	live.cancel()
	if raw := live.containerID.Load(); raw != nil {
		if api, err := b.dockerAPI(ctx); err == nil {
			_ = api.killContainer(ctx, raw.(string), "SIGKILL")
		}
	}
	select {
	case <-live.done:
	case <-ctx.Done():
		return &RunResult{JobID: id, State: string(contract.JobKilled), ErrorCode: string(contract.CodeExecutionUnavailable)}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if res := b.done[id]; res != nil {
		return res, nil
	}
	return &RunResult{JobID: id, State: string(contract.JobKilled)}, nil
}

func (b *Broker) Inspect(ctx context.Context, id uuid.UUID) (*RunResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if res := b.done[id]; res != nil {
		return res, nil
	}
	if _, ok := b.live[id]; ok {
		return &RunResult{JobID: id, State: string(contract.JobRunning)}, nil
	}
	return nil, notFound("execution job was not found")
}

func (b *Broker) labels(jobID, runID, attemptID uuid.UUID, component string) map[string]string {
	return map[string]string{
		LabelOwner:     OwnerValue,
		LabelBroker:    b.id,
		LabelComponent: component,
		LabelJob:       jobID.String(),
		LabelRun:       runID.String(),
		LabelAttempt:   attemptID.String(),
	}
}

func (b *Broker) cleanupJob(ctx context.Context, api *dockerAPI, clean *jobCleanup) {
	if clean.execID != "" {
		_ = api.killContainer(ctx, clean.execID, "SIGKILL")
		_ = api.removeContainer(ctx, clean.execID)
	}
	if clean.proxyID != "" {
		_ = api.killContainer(ctx, clean.proxyID, "SIGKILL")
		_ = api.removeContainer(ctx, clean.proxyID)
	}
	if clean.networkID != "" {
		_ = api.removeNetwork(ctx, clean.networkID)
	}
	if clean.secrets != "" {
		_ = os.RemoveAll(clean.secrets)
	}
	if clean.workspace != "" {
		relaxWorkspace(clean.workspace)
	}
}

// relaxWorkspace lets the API user read files created by uid 10001. It does not
// follow symlinks and does not change anything outside the job workspace.
func relaxWorkspace(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			if info != nil && info.Mode()&os.ModeSymlink != 0 && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0755)
			return nil
		}
		_ = os.Chmod(path, 0644)
		return nil
	})
}

// Reconcile kills containers for the supplied job ids after checking owner and
// job labels, then kills leftovers that still carry this broker id.
func (b *Broker) Reconcile(ctx context.Context, extra []reconcileKill) (int, error) {
	if b.id == "" {
		return 0, unavailable("execution broker id is empty")
	}
	api, err := b.dockerAPI(ctx)
	if err != nil {
		return 0, err
	}
	killed := 0
	for _, item := range extra {
		if item.ID == "" || item.JobID == uuid.Nil {
			continue
		}
		ins, err := api.inspect(ctx, item.ID)
		if err != nil {
			if dockerGone(err) {
				continue
			}
			return killed, unavailable("execution reconcile could not inspect a container")
		}
		owned, err := b.containerLabels(ctx, api, item.ID)
		if err != nil {
			return killed, err
		}
		if owned[LabelOwner] != OwnerValue || owned[LabelJob] != item.JobID.String() {
			continue
		}
		if ins.State.Running {
			if err := api.killContainer(ctx, item.ID, "SIGKILL"); err != nil {
				return killed, unavailable("execution reconcile could not kill a container")
			}
			killed++
		}
		_ = api.removeContainer(ctx, item.ID)
	}
	listed, err := api.listContainers(ctx, map[string]string{LabelOwner: OwnerValue, LabelBroker: b.id})
	if err != nil {
		return killed, unavailable("execution reconcile could not list containers")
	}
	for _, item := range listed {
		if item.State != "running" {
			_ = api.removeContainer(ctx, item.ID)
			continue
		}
		if err := api.killContainer(ctx, item.ID, "SIGKILL"); err != nil {
			return killed, unavailable("execution reconcile could not kill a container")
		}
		_ = api.removeContainer(ctx, item.ID)
		killed++
	}
	return killed, nil
}

func (b *Broker) containerLabels(ctx context.Context, api *dockerAPI, id string) (map[string]string, error) {
	var out struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := api.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &out); err != nil {
		return nil, err
	}
	return out.Config.Labels, nil
}

// Cleanup removes containers and networks labeled for this broker only.
func (b *Broker) Cleanup(ctx context.Context) (int, error) {
	if strings.TrimSpace(b.id) == "" {
		return 0, unavailable("execution broker id is empty")
	}
	api, err := b.dockerAPI(ctx)
	if err != nil {
		return 0, err
	}
	labels := map[string]string{LabelOwner: OwnerValue, LabelBroker: b.id}
	removed := 0
	containers, err := api.listContainers(ctx, labels)
	if err != nil {
		return 0, unavailable("execution cleanup could not list containers")
	}
	for _, item := range containers {
		if !ownedLabels(item.Labels, b.id) {
			continue
		}
		_ = api.killContainer(ctx, item.ID, "SIGKILL")
		if err := api.removeContainer(ctx, item.ID); err == nil {
			removed++
		}
	}
	networks, err := api.listNetworks(ctx, labels)
	if err != nil {
		return removed, unavailable("execution cleanup could not list networks")
	}
	for _, item := range networks {
		if !ownedLabels(item.Labels, b.id) {
			continue
		}
		if err := api.removeNetwork(ctx, item.ID); err == nil {
			removed++
		}
	}
	return removed, nil
}

func ownedLabels(labels map[string]string, brokerID string) bool {
	if strings.TrimSpace(brokerID) == "" || labels == nil {
		return false
	}
	return labels[LabelOwner] == OwnerValue && labels[LabelBroker] == brokerID
}
