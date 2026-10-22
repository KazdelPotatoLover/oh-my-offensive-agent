package provider

import (
	"context"
	"errors"
	"time"
)

// RecoveryPolicy is scheduled by SessionRunner. Provider.Call does not apply it.
// MaxAttempts <= 0 means one real call and no hidden retry.
type RecoveryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func (p RecoveryPolicy) Attempts() int {
	if p.MaxAttempts <= 0 {
		return 1
	}
	return p.MaxAttempts
}

// ShouldRetry is true only for classified upstream or incomplete-output failures.
// Refusal, truncation, cancellation, 4xx other than 429, and mock errors stay terminal.
func (p RecoveryPolicy) ShouldRetry(err error) bool {
	if p.Attempts() <= 1 || err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ce *CallError
	if !errors.As(err, &ce) || !ce.Retryable {
		return false
	}
	switch ce.Class {
	case ClassRateLimited, ClassUpstream, ClassTimeout, ClassTransport, ClassIncomplete, ClassDuplicate, ClassMissingCall:
		return true
	default:
		return false
	}
}

// Backoff doubles from 500ms and caps at 4s, then honors a larger Retry-After.
// The caller must wait with the run context so deadline and user stop win.
func (p RecoveryPolicy) Backoff(attempt int, retryAfter time.Duration) time.Duration {
	initial := p.InitialBackoff
	if initial <= 0 {
		initial = 500 * time.Millisecond
	}
	capDelay := p.MaxBackoff
	if capDelay <= 0 {
		capDelay = 4 * time.Second
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := initial
	for i := 1; i < attempt; i++ {
		if delay >= capDelay/2 {
			delay = capDelay
			break
		}
		delay *= 2
	}
	if delay > capDelay {
		delay = capDelay
	}
	if retryAfter > delay {
		return retryAfter
	}
	return delay
}

func RetryAfter(err error) time.Duration {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.RetryAfter
	}
	return 0
}
