package main

import (
	"context"
	"strings"
	"testing"
)

func TestComposePartialBrokerFailsClosed(t *testing.T) {
	_, err := composeCaptureAndJobs(context.Background(), nil, "127.0.0.1:0", "http://127.0.0.1:9", "", "")
	if err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("got %v", err)
	}
}

func TestComposeEmptyBrokerDoesNotPretendExecutorReady(t *testing.T) {
	wired, err := composeCaptureAndJobs(context.Background(), nil, "127.0.0.1:0", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wired.gateway.Close() })
	if wired.jobs != nil {
		t.Fatal("empty broker config presented a job executor")
	}
	if wired.gateway == nil || wired.gateway.ProxyURL() == "" {
		t.Fatal("capture proxy did not listen")
	}
}
