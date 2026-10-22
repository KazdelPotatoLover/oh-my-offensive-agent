package main

import (
	"context"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/execution"
	"agent-workbench/backend/internal/netcap"

	"github.com/jackc/pgx/v5/pgxpool"
)

// captureWiring is the production capture proxy and, when broker settings are
// complete, the job revoker. An empty broker config leaves jobs unwired. This
// process does not open the Docker socket.
type captureWiring struct {
	gateway *netcap.Gateway
	jobs    contract.ResourceRevoker
}

func validateBrokerConfig(brokerURL, token, workspace string) error {
	n := 0
	for _, value := range []string{brokerURL, token, workspace} {
		if strings.TrimSpace(value) != "" {
			n++
		}
	}
	if n == 0 || n == 3 {
		return nil
	}
	return fmt.Errorf("execution broker config is partial: set OMA_BROKER_URL, OMA_BROKER_TOKEN, and OMA_WORKSPACE_ROOT together")
}

// composeCaptureAndJobs builds the capture gateway and, only when the broker
// settings are complete, the job executor. A partial broker config is an error.
// An empty broker config does not pretend command execution is ready.
func composeCaptureAndJobs(ctx context.Context, pool *pgxpool.Pool, listenAddr, brokerURL, token, workspace string) (captureWiring, error) {
	if err := validateBrokerConfig(brokerURL, token, workspace); err != nil {
		return captureWiring{}, err
	}
	transport := netcap.NewTransport(netcap.NewStore(pool))
	authority, err := netcap.NewAuthority("oma-capture")
	if err != nil {
		return captureWiring{}, err
	}
	gateway := netcap.NewGateway(transport, authority)
	if err := netcap.RejectNonProduction(transport); err != nil {
		return captureWiring{}, err
	}
	if err := netcap.RejectNonProduction(gateway); err != nil {
		return captureWiring{}, err
	}
	if strings.TrimSpace(listenAddr) == "" {
		listenAddr = "127.0.0.1:28183"
	}
	if err := gateway.Listen(listenAddr); err != nil {
		return captureWiring{}, fmt.Errorf("capture proxy listen: %w", err)
	}
	netcap.Install(transport)
	wired := captureWiring{gateway: gateway}
	if strings.TrimSpace(brokerURL) == "" {
		return wired, nil
	}
	cfg := execution.DefaultConfig()
	cfg.WorkspaceRoot = strings.TrimSpace(workspace)
	cfg.BrokerToken = strings.TrimSpace(token)
	exec, err := execution.NewExecutor(cfg, pool, strings.TrimSpace(brokerURL), gateway)
	if err != nil {
		_ = gateway.Close()
		return captureWiring{}, err
	}
	if err := execution.RegisterAdapters(exec); err != nil {
		_ = gateway.Close()
		return captureWiring{}, err
	}
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), contract.CommandCleanupWindow)
	err = exec.Reconcile(clean)
	cancel()
	if err != nil {
		_ = gateway.Close()
		return captureWiring{}, err
	}
	wired.jobs = exec
	return wired, nil
}
