package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"agent-workbench/backend/internal/execution"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:28184", "broker listen address")
	state := flag.String("state-dir", "/var/lib/oma/broker", "directory that stores the stable broker id")
	socket := flag.String("socket", "/var/run/docker.sock", "docker engine socket")
	workspace := flag.String("workspace-root", "/var/lib/oma/workspaces", "host workspace root")
	flag.Parse()
	token := os.Getenv("OMA_BROKER_TOKEN")
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
