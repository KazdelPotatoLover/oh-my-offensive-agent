package orchestration

import (
	"context"
	"sync"

	"agent-workbench/backend/internal/contract"
)

// ModelLimiter admits a bounded number of in-flight model requests. Waiting
// does not consume a model call and is not a worker failure. Cancel wakes the
// waiter without granting the slot to a cancelled request.
type ModelLimiter struct {
	mu       sync.Mutex
	limit    int
	inflight int
	waiters  []*modelWaiter
	onQueue  func(queued int)
}

type modelWaiter struct {
	ready chan struct{}
	gone  bool
}

func newModelLimiter(limit int) *ModelLimiter {
	if limit <= 0 {
		limit = 1
	}
	return &ModelLimiter{limit: limit}
}

func (l *ModelLimiter) SetLimit(limit int) {
	if l == nil || limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
	l.admitLocked()
}

func (l *ModelLimiter) SetQueueHook(hook func(queued int)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.onQueue = hook
	l.mu.Unlock()
}

func (l *ModelLimiter) Capacity() contract.ModelCapacity {
	if l == nil {
		return contract.ModelCapacity{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return contract.ModelCapacity{Limit: l.limit, InFlight: l.inflight, Queued: len(l.waiters)}
}

// Acquire waits until a slot is free or ctx is cancelled. The caller must
// Release after the model call returns, including failures.
func (l *ModelLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if l.inflight < l.limit {
		l.inflight++
		l.mu.Unlock()
		return nil
	}
	w := &modelWaiter{ready: make(chan struct{})}
	l.waiters = append(l.waiters, w)
	l.emitQueueLocked()
	l.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		if !w.gone {
			l.removeWaiterLocked(w)
			l.emitQueueLocked()
			l.mu.Unlock()
			return ctx.Err()
		}
		// Release already transferred this slot. Give it back so a cancelled
		// waiter does not keep an in-flight permit.
		l.mu.Unlock()
		l.Release()
		return ctx.Err()
	}
}

func (l *ModelLimiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.waiters) > 0 {
		w := l.waiters[0]
		l.waiters = l.waiters[1:]
		w.gone = true
		close(w.ready)
		l.emitQueueLocked()
		return
	}
	if l.inflight > 0 {
		l.inflight--
	}
}

func (l *ModelLimiter) admitLocked() {
	for l.inflight < l.limit && len(l.waiters) > 0 {
		w := l.waiters[0]
		l.waiters = l.waiters[1:]
		w.gone = true
		l.inflight++
		close(w.ready)
	}
	l.emitQueueLocked()
}

func (l *ModelLimiter) removeWaiterLocked(target *modelWaiter) {
	kept := l.waiters[:0]
	for _, w := range l.waiters {
		if w != target {
			kept = append(kept, w)
		}
	}
	l.waiters = kept
}

func (l *ModelLimiter) emitQueueLocked() {
	if l.onQueue != nil {
		l.onQueue(len(l.waiters))
	}
}
