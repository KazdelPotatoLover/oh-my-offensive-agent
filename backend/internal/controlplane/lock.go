package controlplane

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	controlLockNS int64 = 0x4D330001
	// mainDrainLockNS is reserved so a drain never takes the control lock in
	// the opposite order. Drain claims a turn, releases, then Apply takes the
	// control lock. Apply never takes the drain lock.
	mainDrainLockNS int64 = 0x4D330002
)

func advisoryKey(namespace int64, id uuid.UUID) int64 {
	var n int64
	for i := 0; i < 8; i++ {
		n = (n << 8) | int64(id[i])
	}
	return n ^ namespace
}

func lockAdvisory(ctx context.Context, pool *pgxpool.Pool, key int64) (func(), error) {
	if pool == nil {
		return nil, persistence("database pool is not configured")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, persistence("failed to acquire control lock connection")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		return nil, persistence("failed to acquire control lock")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
			conn.Release()
		})
	}, nil
}
