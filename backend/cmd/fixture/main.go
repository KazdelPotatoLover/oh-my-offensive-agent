package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"agent-workbench/backend/internal/fixture"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("FIXTURE_PORT")
	}
	if port == "" {
		port = "29090"
	}

	srv, err := fixture.NewServer(port)
	if err != nil {
		slog.Error("failed to initialize fixture server", "error", err)
		os.Exit(1)
	}

	srv.Start()
	slog.Info("fixture server listening", "addr", srv.Addr(), "port", srv.Port(), "version", srv.GetVersion())

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down fixture server...")
	if err := srv.Close(); err != nil {
		slog.Error("error during fixture server shutdown", "error", err)
	}
	_ = context.Background()
	slog.Info("fixture server stopped")
}
