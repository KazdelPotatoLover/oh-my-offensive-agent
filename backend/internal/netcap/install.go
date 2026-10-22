package netcap

import "sync"

var (
	sharedMu sync.RWMutex
	shared   *Transport
)

// Install publishes the process-wide production transport. There is no mock
// fallback: a nil Shared means the caller must pass a transport explicitly.
func Install(t *Transport) {
	sharedMu.Lock()
	shared = t
	sharedMu.Unlock()
}

// Shared returns the installed transport, or nil when production has not
// injected one. Callers must not substitute a fake success client.
func Shared() *Transport {
	sharedMu.RLock()
	defer sharedMu.RUnlock()
	return shared
}
