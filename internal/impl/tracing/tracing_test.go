package tracing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetup_Disabled proves the "off by default, zero overhead" half of
// Design §3.10: with Config.Enabled false (the zero value), Setup must not
// build any middleware and must hand back a shutdown func a caller can defer
// unconditionally.
func TestSetup_Disabled(t *testing.T) {
	mw, shutdown, err := Setup(context.Background(), Config{})
	require.NoError(t, err)
	assert.Nil(t, mw)
	require.NotNil(t, shutdown)
	assert.NoError(t, shutdown(context.Background()))
}

// TestSetup_EnabledDoesNotBlockOnUnreachableCollector proves Plan Phase 7's
// "don't let a missing/unreachable OTel collector cause Canopy to fail to
// start or hang": with tracing enabled and no collector listening on the
// default OTLP/HTTP endpoint (nothing in this test environment is), Setup
// must still return promptly with a usable middleware, and the shutdown func
// it returns must also return promptly under a short timeout rather than
// blocking forever trying to flush to a dead collector.
func TestSetup_EnabledDoesNotBlockOnUnreachableCollector(t *testing.T) {
	setupDone := make(chan struct{})
	var mw any
	var shutdown func(context.Context) error
	var setupErr error

	go func() {
		defer close(setupDone)
		mw, shutdown, setupErr = Setup(context.Background(), Config{Enabled: true, ServiceName: "canopy-test"})
	}()

	select {
	case <-setupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Setup did not return within 5s — it must not block on an unreachable collector")
	}

	require.NoError(t, setupErr)
	assert.NotNil(t, mw)
	require.NotNil(t, shutdown)

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownDone <- shutdown(ctx)
	}()

	select {
	case <-shutdownDone:
		// Whether the flush itself reports an error against a dead
		// collector doesn't matter here — only that it returned instead
		// of hanging.
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return within 5s — it must not hang on an unreachable collector")
	}
}
