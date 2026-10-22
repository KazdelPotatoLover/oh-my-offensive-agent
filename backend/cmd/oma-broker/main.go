package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"agent-workbench/backend/internal/execution"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:28184", "broker listen address")
	state := flag.String("state-dir", "/var/lib/oma/broker", "directory that stores the stable broker id")
	socket := flag.String("socket", "/var/run/docker.sock", "docker engine socket")
	workspace := flag.String("workspace-root", "/var/lib/oma/workspaces", "host workspace root")
	healthcheck := flag.Bool("healthcheck", false, "check the local broker without starting a server")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional arguments; pass flags directly to the broker entrypoint")
	}
	token := os.Getenv("OMA_BROKER_TOKEN")
	if *healthcheck {
		_, port, err := net.SplitHostPort(*listen)
		if err != nil {
			log.Fatal("invalid broker listen address")
		}
		if err := checkHealth("http://127.0.0.1:"+port, token); err != nil {
			log.Fatal(err)
		}
		return
	}
	id, err := execution.LoadBrokerID(*state)
	if err != nil {
		log.Fatal(err)
	}
	cfg := execution.DefaultConfig()
	cfg.BrokerToken = token
	cfg.DockerSocket = *socket
	cfg.WorkspaceRoot = *workspace
	broker, err := execution.NewBroker(cfg, id)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("execution broker listening on %s", *listen)
	if err := http.ListenAndServe(*listen, broker.Handler()); err != nil {
		log.Fatal(err)
	}
}

func checkHealth(base, token string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/health", nil)
	if err != nil {
		return fmt.Errorf("invalid broker health URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("broker health unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker health status %d", resp.StatusCode)
	}
	return nil
}
