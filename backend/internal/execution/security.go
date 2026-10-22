package execution

import "strings"

// allowedArgv0 is the fixed command protocol. The model cannot name an image,
// a host path, or a shell that the broker evaluates on the host.
var allowedArgv0 = map[string]string{
	"python3": "/usr/local/bin/python3",
	"python":  "/usr/local/bin/python3",
	"curl":    "/usr/bin/curl",
}

// NormalizeArgv rewrites argv[0] to the container absolute path. It rejects
// host paths, shell wrappers, and docker arguments.
func NormalizeArgv(argv []string) ([]string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, invalidInput("command requires argv")
	}
	if len(argv) > 64 {
		return nil, invalidInput("command argv is too long")
	}
	resolved, ok := allowedArgv0[argv[0]]
	if !ok {
		return nil, scopeDenied("command is not in the server allowlist")
	}
	out := make([]string, len(argv))
	out[0] = resolved
	for i := 1; i < len(argv); i++ {
		if strings.ContainsRune(argv[i], 0) || len(argv[i]) > 256*1024 {
			return nil, invalidInput("command argument is empty or too large")
		}
		out[i] = argv[i]
	}
	return out, nil
}

// containerCreateBody is the only HostConfig the broker sends to Docker.
// Privileged, host network, host PID, and arbitrary binds cannot be represented.
func containerCreateBody(spec containerCreate) (map[string]any, error) {
	argv, err := NormalizeArgv(spec.Argv)
	if err != nil {
		return nil, err
	}
	if spec.Image == "" || strings.ContainsAny(spec.Image, " \t\r\n") || strings.HasPrefix(spec.Image, "-") {
		return nil, invalidInput("execution image is not allowlisted")
	}
	if spec.Network == "" || spec.Network == "host" || strings.Contains(spec.Network, "/") {
		return nil, scopeDenied("execution network must be the job network")
	}
	if spec.Network != "none" && spec.ProxyURL == "" {
		return nil, scopeDenied("networked execution requires the egress proxy")
	}
	if spec.WorkspaceHost == "" || !strings.HasPrefix(spec.WorkspaceHost, "/") || strings.Contains(spec.WorkspaceHost, ":") {
		return nil, unavailable("execution workspace is not ready")
	}
	binds := []string{spec.WorkspaceHost + ":/workspace:rw"}
	if spec.CACertHost != "" {
		if !strings.HasPrefix(spec.CACertHost, "/") || strings.Contains(spec.CACertHost, ":") {
			return nil, unavailable("execution CA mount is not ready")
		}
		binds = append(binds, spec.CACertHost+":/etc/oma/ca.pem:ro")
	}
	for _, bind := range binds {
		if strings.Contains(bind, "docker.sock") || strings.Contains(bind, ".env") {
			return nil, scopeDenied("execution mount is not allowed")
		}
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONNOUSERSITE=1",
		"LANG=C.UTF-8",
		"NO_PROXY=",
		"no_proxy=",
	}
	if spec.CACertHost != "" {
		env = append(env,
			"SSL_CERT_FILE=/etc/oma/ca.pem",
			"SSL_CERT_DIR=",
			"CURL_CA_BUNDLE=/etc/oma/ca.pem",
			"REQUESTS_CA_BUNDLE=/etc/oma/ca.pem",
		)
	}
	if spec.ProxyURL != "" {
		env = append(env,
			"HTTP_PROXY="+spec.ProxyURL,
			"HTTPS_PROXY="+spec.ProxyURL,
			"http_proxy="+spec.ProxyURL,
			"https_proxy="+spec.ProxyURL,
		)
	}
	return map[string]any{
		"Image":      spec.Image,
		"User":       execUser,
		"Cmd":        argv,
		"WorkingDir": "/workspace",
		"Env":        env,
		"Tty":        false,
		"OpenStdin":  false,
		"Labels":     spec.Labels,
		"HostConfig": map[string]any{
			"Binds":           binds,
			"ReadonlyRootfs":  true,
			"CapDrop":         []string{"ALL"},
			"CapAdd":          []string{},
			"SecurityOpt":     []string{"no-new-privileges:true"},
			"Privileged":      false,
			"NetworkMode":     spec.Network,
			"PidMode":         "",
			"IpcMode":         "private",
			"Init":            true,
			"Memory":          spec.Memory,
			"MemorySwap":      spec.Memory,
			"NanoCpus":        spec.NanoCPUs,
			"PidsLimit":       spec.Pids,
			"AutoRemove":      false,
			"PublishAllPorts": false,
			"ExtraHosts":      []string{},
			"Tmpfs": map[string]string{
				"/tmp": "rw,noexec,nosuid,nodev,size=32m,uid=10001,gid=10001",
			},
		},
	}, nil
}

// proxyCreateBody is the broker-owned egress proxy. The command is fixed.
// The model cannot select this image's entrypoint, mounts, or network mode.
func proxyCreateBody(spec proxyCreate) (map[string]any, error) {
	if spec.Image == "" || spec.Network == "" || spec.Network == "host" || spec.Network == "none" {
		return nil, unavailable("egress proxy network is not ready")
	}
	if spec.CACertHost == "" || spec.CAKeyHost == "" || strings.Contains(spec.CAKeyHost, "docker.sock") {
		return nil, unavailable("egress proxy CA is not ready")
	}
	binds := []string{
		spec.CACertHost + ":/etc/oma/ca.pem:ro",
		spec.CAKeyHost + ":/etc/oma/ca.key:ro",
	}
	for _, bind := range binds {
		if strings.Contains(bind, "docker.sock") || strings.Contains(bind, ".env") {
			return nil, scopeDenied("egress proxy mount is not allowed")
		}
	}
	env := []string{
		"OMA_LISTEN=" + proxyListen,
		"OMA_CAPTURE_URL=" + spec.CaptureURL,
		"OMA_CAPTURE_TOKEN=" + spec.CaptureToken,
		"OMA_JOB_ID=" + spec.JobID,
		"OMA_ALLOWED_ORIGINS=" + strings.Join(spec.Origins, ","),
		"OMA_CA_CERT=/etc/oma/ca.pem",
		"OMA_CA_KEY=/etc/oma/ca.key",
	}
	return map[string]any{
		"Image":      spec.Image,
		"User":       execUser,
		"Cmd":        []string{"/usr/local/bin/oma-proxy"},
		"WorkingDir": "/",
		"Env":        env,
		"Tty":        false,
		"OpenStdin":  false,
		"Labels":     spec.Labels,
		"HostConfig": map[string]any{
			"Binds":           binds,
			"ReadonlyRootfs":  true,
			"CapDrop":         []string{"ALL"},
			"CapAdd":          []string{},
			"SecurityOpt":     []string{"no-new-privileges:true"},
			"Privileged":      false,
			"NetworkMode":     spec.Network,
			"PidMode":         "",
			"IpcMode":         "private",
			"Init":            true,
			"Memory":          spec.Memory,
			"MemorySwap":      spec.Memory,
			"NanoCpus":        spec.NanoCPUs,
			"PidsLimit":       spec.Pids,
			"AutoRemove":      false,
			"PublishAllPorts": false,
			"ExtraHosts":      []string{},
			"Tmpfs": map[string]string{
				"/tmp": "rw,noexec,nosuid,nodev,size=16m,uid=10001,gid=10001",
			},
		},
	}, nil
}

type proxyCreate struct {
	Image        string
	Network      string
	CACertHost   string
	CAKeyHost    string
	CaptureURL   string
	CaptureToken string
	JobID        string
	Origins      []string
	Memory       int64
	NanoCPUs     int64
	Pids         int64
	Labels       map[string]string
}

type containerCreate struct {
	Image         string
	Argv          []string
	Network       string
	WorkspaceHost string
	CACertHost    string
	ProxyURL      string
	Memory        int64
	NanoCPUs      int64
	Pids          int64
	Labels        map[string]string
}

// SecurityReport is the Docker inspect subset tests use to prove isolation.
type SecurityReport struct {
	Privileged     bool     `json:"Privileged"`
	NetworkMode    string   `json:"NetworkMode"`
	PidMode        string   `json:"PidMode"`
	ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
	CapDrop        []string `json:"CapDrop"`
	CapAdd         []string `json:"CapAdd"`
	User           string   `json:"User"`
	Binds          []string `json:"Binds"`
	ExtraHosts     []string `json:"ExtraHosts"`
	Init           bool     `json:"Init"`
}
