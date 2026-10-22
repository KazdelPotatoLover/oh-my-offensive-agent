package provider

import (
	"context"
	"sync"

	"agent-workbench/backend/internal/contract"
)

// RequestLimiter is the shared upstream request limit. It is not the worker pool.
// A non-positive limit means unlimited and still reports in_flight.
type RequestLimiter struct {
	limit    int
	mu       sync.Mutex
	inflight int
	queued   int
	cond     *sync.Cond
}

func NewRequestLimiter(limit int) *RequestLimiter {
	limiter := &RequestLimiter{limit: limit}
	limiter.cond = sync.NewCond(&limiter.mu)
	return limiter
}

func (l *RequestLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit <= 0 || l.inflight < l.limit {
		l.inflight++
		return nil
	}
	l.queued++
	defer func() { l.queued-- }()
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()
	for l.inflight >= l.limit {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	l.inflight++
	return nil
}

func (l *RequestLimiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *RequestLimiter) Snapshot() contract.ModelCapacity {
	if l == nil {
		return contract.ModelCapacity{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limit
	if limit < 0 {
		limit = 0
	}
	return contract.ModelCapacity{Limit: limit, InFlight: l.inflight, Queued: l.queued}
}
