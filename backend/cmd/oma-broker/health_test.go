package main

import (
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/execution"
)

func TestHealthUsesBrokerAuthentication(t *testing.T) {
	cfg := execution.DefaultConfig()
	cfg.BrokerToken = "local-health-test-token"
	broker, err := execution.NewBroker(cfg, "health-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker.Handler())
	defer server.Close()
	if err := checkHealth(server.URL, cfg.BrokerToken); err != nil {
		t.Fatal(err)
	}
	if err := checkHealth(server.URL, "wrong-token"); err == nil {
		t.Fatal("health probe ignored rejected credentials")
	}
}
