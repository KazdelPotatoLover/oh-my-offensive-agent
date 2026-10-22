package execution

import (
	"time"

	"agent-workbench/backend/internal/contract"
)

const (
	ProtocolVersion = "execution.broker.v1"
	DefaultImage    = "oma-executor:ma4"
	LabelOwner      = "oma.owner"
	OwnerValue      = "ma-executor"
	LabelBroker     = "oma.broker_id"
	LabelComponent  = "oma.component"
	LabelJob        = "oma.job_id"
	LabelRun        = "oma.run_id"
	LabelAttempt    = "oma.attempt_id"
	ComponentJob    = "execution-job"
	ComponentProxy  = "egress-proxy"
	ComponentNet    = "job-network"
	ComponentBroker = "broker-network"

	PhaseExplore    = "explore"
	PhaseSettlement = "settlement"

	execUser    = "10001:10001"
	proxyListen = "0.0.0.0:8080"

	// brokerResultGrace is how long the API waits after the command limit for the
	// broker to return a terminal supervision result. It does not extend the command.
	brokerResultGrace = 2 * time.Second
)

// Config is server-owned. A model payload cannot set image, mounts, or limits.
type Config struct {
	Image            string
	AllowedImages    []string
	WorkspaceRoot    string
	DockerSocket     string
	MemoryBytes      int64
	NanoCPUs         int64
	PidsLimit        int64
	OutputLimitBytes int
	BrokerToken      string
}

func DefaultConfig() Config {
	return Config{
		Image:            DefaultImage,
		AllowedImages:    []string{DefaultImage},
		DockerSocket:     "/var/run/docker.sock",
		MemoryBytes:      256 * 1024 * 1024,
		NanoCPUs:         500_000_000,
		PidsLimit:        64,
		OutputLimitBytes: 1 << 20,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Image == "" {
		c.Image = d.Image
	}
	if len(c.AllowedImages) == 0 {
		c.AllowedImages = []string{c.Image}
	}
	if c.DockerSocket == "" {
		c.DockerSocket = d.DockerSocket
	}
	if c.MemoryBytes <= 0 {
		c.MemoryBytes = d.MemoryBytes
	}
	if c.NanoCPUs <= 0 {
		c.NanoCPUs = d.NanoCPUs
	}
	if c.PidsLimit <= 0 {
		c.PidsLimit = d.PidsLimit
	}
	if c.OutputLimitBytes <= 0 {
		c.OutputLimitBytes = d.OutputLimitBytes
	}
	if c.OutputLimitBytes > 8*1024*1024 {
		c.OutputLimitBytes = 8 * 1024 * 1024
	}
	return c
}

func imageAllowed(image string, allow []string) bool {
	for _, item := range allow {
		if item == image {
			return true
		}
	}
	return false
}

// ClampCommandTimeout applies the command ceiling and the attempt/run remainder.
// The historical 5s tool cap is not a command timeout. A non-positive result
// means the caller deadline has already passed; this function does not extend it.
func ClampCommandTimeout(requested, runRemaining, attemptRemaining time.Duration) (time.Duration, error) {
	if requested <= 0 {
		requested = contract.DefaultCommandTimeout
	}
	if requested > contract.MaxCommandTimeout {
		requested = contract.MaxCommandTimeout
	}
	if runRemaining > 0 && runRemaining < requested {
		requested = runRemaining
	}
	if attemptRemaining > 0 && attemptRemaining < requested {
		requested = attemptRemaining
	}
	if requested <= 0 {
		return 0, invalidInput("command deadline has already passed")
	}
	if requested == contract.LegacyImplicitToolTimeout && requested < contract.DefaultCommandTimeout {
		// Equality with the legacy cap is a real remaining deadline, not a default.
		return requested, nil
	}
	return requested, nil
}
