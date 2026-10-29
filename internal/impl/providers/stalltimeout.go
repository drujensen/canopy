package providers

// This file adds an opt-in watchdog over a provider's streaming response
// body, distinct from entities.ProviderConfig.TimeoutSeconds: TimeoutSeconds
// bounds how long Canopy waits for a response's *headers* (see that field's
// doc comment) and has no effect once the body is already streaming — a
// request that gets headers back promptly (the common case for a streaming
// Chat Completions call: the server returns "200 text/event-stream"
// essentially immediately, then trickles "data:" events as the model
// produces them) has no timeout at all protecting the rest of its lifetime
// today.
//
// Real incident this closes: a self-hosted Ollama request (qwen3.8 on a
// Framework Desktop, 128GB shared RAM) went completely silent after headers
// arrived — GPU idle, zero SSE events, "Thinking..." stuck — while the box
// was memory-thrashing processing an oversized, uncompacted prompt (the
// other half of that incident is detect.go's Ollama context_window_tokens
// fix, which stops Canopy from letting that prompt grow that large in the
// first place). "esc" (cancelTurn, chat.go) exists to cancel a stuck turn,
// but it depends on Canopy's own process staying responsive enough to read
// a keypress and run its event loop — exactly what a thrashing machine
// can't guarantee. A stall timeout fires on its own independent timer
// instead, so a silent stream gets caught and reported even if the rest of
// the process is barely scheduling.
//
// Opt-in via entities.ProviderConfig.StallTimeoutSeconds (zero disables it,
// same convention TimeoutSeconds already established) rather than a default
// that's on for every existing provider config: a sensible stall window
// depends on how slow a provider's *first* token can legitimately be (a
// cold-loading local model vs. an always-warm hosted API), and guessing
// wrong in either direction is worse than asking the provider's owner to
// set it once for the provider where it actually matters (self-hosted,
// locally-run models).

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// stallTimeoutDuration converts entities.ProviderConfig.StallTimeoutSeconds
// into a time.Duration, 0 meaning disabled — the single place every
// provider-construction site (factory.go, openaicompat.go, ollama.go)
// converts the raw config value, so they can't drift on what "disabled"
// means.
func stallTimeoutDuration(cfg entities.ProviderConfig) time.Duration {
	if cfg.StallTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(cfg.StallTimeoutSeconds) * time.Second
}

// openaiStallTimeoutOption returns an option.WithHTTPClient installing
// newStallTimeoutTransport, or no options at all when cfg.StallTimeoutSeconds
// is unset — shared between the native OpenAI path (factory.go) and the
// OpenAI-compatible path (openaicompat.go), the same way openaiTimeoutOptions
// already is for TimeoutSeconds. Ollama does not use this: it already builds
// its own custom *http.Client (newOllamaHTTPClient) to install the
// tool-call-content-stripping transport, so it composes
// newStallTimeoutTransport directly as that transport's base instead.
func openaiStallTimeoutOption(cfg entities.ProviderConfig) []option.RequestOption {
	d := stallTimeoutDuration(cfg)
	if d <= 0 {
		return nil
	}
	return []option.RequestOption{option.WithHTTPClient(&http.Client{Transport: newStallTimeoutTransport(nil, d)})}
}

// errStalled is wrapped into the error a stalled read returns, so a caller
// can recognize this specific failure mode (e.g. to show a more specific
// message than a generic network error) if it ever wants to.
var errStalled = errors.New("provider stopped sending data")

// newStallTimeoutTransport wraps base with a watchdog over every response
// body it returns: stallTimeout resets on every byte actually read, and
// fires only on a genuine gap with zero progress — a response that's merely
// slow but still producing bytes (one chunk every few seconds, say) is never
// aborted, no matter how long the whole response takes in total. base
// follows every other transport wrapper in this package's precedent (nil
// clones http.DefaultTransport rather than starting from a bare
// &http.Transport{}).
//
// stallTimeout <= 0 disables the watchdog entirely, returning base
// unwrapped (aside from the nil-clone above) — see this file's package
// comment for why this is opt-in, not a new default for every provider.
func newStallTimeoutTransport(base http.RoundTripper, stallTimeout time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	if stallTimeout <= 0 {
		return base
	}
	return &stallTimeoutTransport{base: base, stallTimeout: stallTimeout}
}

type stallTimeoutTransport struct {
	base         http.RoundTripper
	stallTimeout time.Duration
}

// RoundTrip only wraps resp.Body — it deliberately does not touch how long
// base.RoundTrip itself takes to return a response (that's
// entities.ProviderConfig.TimeoutSeconds' job, via option.WithRequestTimeout
// upstream of this transport; see openaiTimeoutOptions). A provider that's
// still legitimately working on a cold model load before sending any
// response at all is unaffected by the stall watchdog — it only starts
// counting once there's a body to read from.
func (t *stallTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = newStallWatchdogBody(resp.Body, t.stallTimeout)
	return resp, nil
}

// stallWatchdogBody races every underlying Read against stallTimeout, on a
// background goroutine, so a Read blocked forever on a truly silent
// connection can still be bounded.
//
// Safety note on the goroutine-race-to-cancel pattern this relies on (a
// known Go gotcha when misused): once Read below gives up and returns an
// error to its caller, it returns (0, err) — never a partial n — so a
// caller that (correctly, per the io.Reader contract) stops touching its
// buffer after receiving an error can never observe a half-written slice
// from the orphaned goroutine finishing late. That orphaned goroutine itself
// is bounded by Close: calling Close on a live HTTP response body unblocks
// a concurrently-blocked Read with an error (the same mechanism
// context-cancellation of a request relies on internally), so the
// background goroutine always eventually exits and the buffered result
// channel it holds the only reference to is simply garbage collected,
// never blocking anything else.
type stallWatchdogBody struct {
	orig         io.ReadCloser
	stallTimeout time.Duration

	closeOnce sync.Once
	closeErr  error

	trippedMu sync.Mutex
	tripped   bool
}

func newStallWatchdogBody(orig io.ReadCloser, stallTimeout time.Duration) io.ReadCloser {
	return &stallWatchdogBody{orig: orig, stallTimeout: stallTimeout}
}

type stallReadResult struct {
	n   int
	err error
}

func (b *stallWatchdogBody) Read(p []byte) (int, error) {
	b.trippedMu.Lock()
	tripped := b.tripped
	b.trippedMu.Unlock()
	if tripped {
		// Already gave up once; the underlying connection is closed and any
		// further Read on it is meaningless — fail the same way instead of
		// racing a fresh goroutine against a connection we already tore
		// down.
		return 0, fmt.Errorf("providers: %w", errStalled)
	}

	resultCh := make(chan stallReadResult, 1)
	go func() {
		n, err := b.orig.Read(p)
		resultCh <- stallReadResult{n: n, err: err}
	}()

	timer := time.NewTimer(b.stallTimeout)
	defer timer.Stop()

	select {
	case res := <-resultCh:
		return res.n, res.err
	case <-timer.C:
		b.trippedMu.Lock()
		b.tripped = true
		b.trippedMu.Unlock()
		_ = b.Close()
		return 0, fmt.Errorf("providers: %w (no data for %s)", errStalled, b.stallTimeout)
	}
}

func (b *stallWatchdogBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.orig.Close() })
	return b.closeErr
}
