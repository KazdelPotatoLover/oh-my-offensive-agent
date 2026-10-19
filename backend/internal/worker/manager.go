package worker

import (
	"context"
	"log/slog"
	"sync"

	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Manager struct {
	pool       *pgxpool.Pool
	runService *service.RunService
	cancels    sync.Map // map[uuid.UUID]context.CancelFunc
}

func NewManager(pool *pgxpool.Pool, runService *service.RunService) *Manager {
	return &Manager{
		pool:       pool,
		runService: runService,
	}
}

func (m *Manager) Start(runID uuid.UUID, failAtStep *int) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels.Store(runID, cancel)

	worker := &FakeWorker{
		runID:      runID,
		failAtStep: failAtStep,
		runService: m.runService,
	}

	go func() {
		defer func() {
			m.cancels.Delete(runID)
			slog.Info("worker finished", "run_id", runID)
		}()
		worker.Run(ctx)
	}()

	slog.Info("worker started", "run_id", runID)
}

func (m *Manager) Stop(runID uuid.UUID) {
	if cancelFn, ok := m.cancels.Load(runID); ok {
		cancelFn.(context.CancelFunc)()
		slog.Info("worker stop requested", "run_id", runID)
	}
}

func (m *Manager) StopAll() {
	m.cancels.Range(func(key, value interface{}) bool {
		value.(context.CancelFunc)()
		return true
	})
	slog.Info("all workers stop requested")
}
