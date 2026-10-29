package providers

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushWriter writes b then flushes immediately, so the client sees it as
// its own chunk rather than buffered up with whatever write comes next.
func flushWriter(w http.ResponseWriter, b []byte) {
	_, _ = w.Write(b)
	w.(http.Flusher).Flush()
}

// TestStallTimeoutTransport_AbortsOnSilentStream proves the watchdog fires
// close to stallTimeout, not after the handler's full (much longer) sleep —
// the exact shape of the real incident this guards against: headers and a
// first chunk arrive fine, then the connection goes completely silent.
func TestStallTimeoutTransport_AbortsOnSilentStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushWriter(w, []byte("first chunk\n"))
		// Deliberately short (not minutes, like the real incident) so the
		// test suite doesn't pay for it — httptest.Server.Close() waits for
		// in-flight handlers to finish, so this sleep is a floor on this
		// test's own runtime regardless of how fast the watchdog fires (see
		// TestNew_OpenAICompatible_TimeoutSecondsEnforced's comment for the
		// same tradeoff). Still a healthy 10x the 100ms stall timeout below.
		time.Sleep(1 * time.Second)
	}))
	defer server.Close()

	client := &http.Client{Transport: newStallTimeoutTransport(nil, 100*time.Millisecond)}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	buf := make([]byte, 256)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err, "the first chunk must come through untouched")
	assert.Equal(t, "first chunk\n", string(buf[:n]))

	start := time.Now()
	_, err = resp.Body.Read(buf)
	elapsed := time.Since(start)

	require.Error(t, err, "a silent connection must eventually error, not hang for the handler's full sleep")
	assert.True(t, errors.Is(err, errStalled), "error must be recognizable as a stall, got: %v", err)
	assert.Less(t, elapsed, 500*time.Millisecond, "must fire near the 100ms stall timeout, not wait out the handler's sleep")
}

// TestStallTimeoutTransport_SlowButProgressingStreamSucceeds proves a
// response that's merely slow — producing small chunks well inside the
// stall window, but whose *total* duration exceeds it many times over — is
// never aborted. Only a true gap with zero progress should ever trip the
// watchdog.
func TestStallTimeoutTransport_SlowButProgressingStreamSucceeds(t *testing.T) {
	const chunks = 5
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < chunks; i++ {
			flushWriter(w, []byte("chunk\n"))
			time.Sleep(50 * time.Millisecond) // well under the 200ms stall timeout
		}
	}))
	defer server.Close()

	// Total time (5 * 50ms = 250ms) exceeds the 200ms stall window, but no
	// single gap between chunks does.
	client := &http.Client{Transport: newStallTimeoutTransport(nil, 200*time.Millisecond)}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "a slow-but-progressing stream must read to completion without the watchdog firing")
	assert.Equal(t, "chunk\nchunk\nchunk\nchunk\nchunk\n", string(body))
}

// TestStallTimeoutTransport_DisabledWhenZero proves stallTimeout<=0 installs
// no watchdog at all — entities.ProviderConfig.StallTimeoutSeconds is
// opt-in, so a request that would trip a configured watchdog must behave
// exactly as before (bounded only by whatever else governs it, e.g.
// TimeoutSeconds upstream — not exercised here) when it's left unset.
func TestStallTimeoutTransport_DisabledWhenZero(t *testing.T) {
	transport := newStallTimeoutTransport(nil, 0)
	if _, ok := transport.(*stallTimeoutTransport); ok {
		t.Fatal("stallTimeout<=0 must return the base transport unwrapped, not install a watchdog")
	}
}
