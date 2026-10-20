package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agent-workbench/backend/internal/config"
	"agent-workbench/backend/internal/db"
	"agent-workbench/backend/internal/handler"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/worker"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

func main() {
	// Set up structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := config.Load()

	if cfg.DatabaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	// Run migrations
	slog.Info("running database migrations", "path", cfg.MigrationsPath)
	if err := db.RunMigrations(cfg.DatabaseURL, cfg.MigrationsPath); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations completed")

	// Connect to database
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Mark interrupted runs
	if err := db.MarkInterruptedRuns(ctx, pool); err != nil {
		slog.Error("failed to mark interrupted runs", "error", err)
		os.Exit(1)
	}

	// Initialize services
	taskService := service.NewTaskService(pool)
	runService := service.NewRunService(pool)

	// Initialize worker manager
	manager := worker.NewManager(pool, runService)

	// Initialize handlers
	taskHandler := handler.NewTaskHandler(taskService)
	runHandler := handler.NewRunHandler(runService, taskService, manager)
	assetHandler := handler.NewAssetHandler(runService)

	// Set up router
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   strings.Split(cfg.CORSOrigins, ","),
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Idempotency-Key"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Routes
	r.Route("/api", func(r chi.Router) {
		// Assets
		r.Get("/assets", assetHandler.List)
		r.Get("/assets/{assetID}", assetHandler.Get)
		r.Get("/assets/{assetID}/details", assetHandler.GetDetails)

		// Tasks
		r.Get("/tasks", taskHandler.List)
		r.Post("/tasks", taskHandler.Create)
		r.Get("/tasks/{taskID}", taskHandler.Get)
		r.Put("/tasks/{taskID}", taskHandler.Update)
		r.Delete("/tasks/{taskID}", taskHandler.Delete)

		// Runs (nested under tasks)
		r.Get("/tasks/{taskID}/runs", runHandler.ListByTask)
		r.Post("/tasks/{taskID}/runs", runHandler.Create)

		// Runs (direct)
		r.Get("/runs/{runID}", runHandler.Get)
		r.Post("/runs/{runID}/stop", runHandler.Stop)
		r.Get("/runs/{runID}/events", runHandler.GetEvents)
		r.Get("/runs/{runID}/messages", runHandler.GetMessages)
		r.Get("/runs/{runID}/model_calls", runHandler.GetModelCalls)
		r.Get("/runs/{runID}/tool_calls", runHandler.GetToolCalls)
		r.Get("/runs/{runID}/work_items", runHandler.GetWorkItems)
		r.Get("/runs/{runID}/work_items/details", runHandler.GetWorkItemDetails)
		r.Get("/runs/{runID}/observations", runHandler.GetObservations)
		r.Get("/runs/{runID}/sessions", runHandler.GetSessions)
		r.Get("/runs/{runID}/planner_rounds", runHandler.GetPlannerRounds)
		r.Get("/runs/{runID}/assets", runHandler.GetRunAssets)
		r.Get("/runs/{runID}/discoveries", runHandler.GetDiscoveries)
	})

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Start server
	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: r,
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down server gracefully...")

	// Step 1: Stop receiving work - shutdown HTTP server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("server HTTP shutdown error", "error", err)
	}

	// Step 2: Stop receiving work in manager & cancel active executions
	manager.Close()
	manager.StopAll()

	// Step 3: Wait for all workers to exit (with a wait limit)
	if err := manager.Wait(5 * time.Second); err != nil {
		slog.Error("workers wait timeout", "error", err)
	}

	// Step 4: Database connection pool closes via defer pool.Close()
	slog.Info("server stopped gracefully")
}
