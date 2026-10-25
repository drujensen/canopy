package providers

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"
)

// RetryNotice describes one HTTP response an openai-go-based provider
// client (newOpenAINative/newOpenAICompatible/newOllama — all three install
// openaiRetryableStatusOption) is about to retry. It exists so a caller
// like the TUI can tell the user they're being throttled instead of the
// turn just silently stalling for a few seconds before either succeeding or
// failing outright — a real user hit exactly that with Groq's per-minute
// token limit (see openaiRetryableStatusOption's doc comment) and had no
// indication anything was happening until it either recovered or gave up.
type RetryNotice struct {
	// StatusCode is the HTTP status that triggered this retry (429, a 5xx,
	// 408/409, or — the Groq case this was added for — 413).
	StatusCode int
	// Reason is a short, human-readable cause ("rate limited (429)",
	// "server error (503)", ...), suitable for displaying directly.
	Reason string
	// Attempt is the 1-based number of the request that just failed (1 ==
	// the very first attempt, before any retry has happened yet).
	Attempt int
	// MaxAttempts is the total number of attempts the retry budget allows,
	// including the first (maxProviderRetries + 1).
	MaxAttempts int
	// Delay is approximately how long the client will wait before the next
	// attempt (from the response's Retry-After header when present,
	// otherwise the same exponential-backoff estimate retryDelay in the
	// vendored SDK uses, minus jitter — close enough to display, not meant
	// to be exact).
	Delay time.Duration
}

type retryNotifierKey struct{}

// WithRetryNotifier attaches fn to ctx so openaiRetryableStatusOption's
// middleware can report a retry it's about to perform. ctx flows from
// tui.startTurn through AgentService.RunMessagesStream and
// agent-framework-go's own Run call unchanged all the way down to the
// *http.Request openai-go actually sends — context.Context is specifically
// designed to carry request-scoped values through exactly this kind of
// call chain, where threading an explicit parameter through every
// intermediate layer (none of which otherwise cares about retries) would
// mean changing signatures across package boundaries this package doesn't
// own.
func WithRetryNotifier(ctx context.Context, fn func(RetryNotice)) context.Context {
	return context.WithValue(ctx, retryNotifierKey{}, fn)
}

func retryNotifierFromContext(ctx context.Context) func(RetryNotice) {
	fn, _ := ctx.Value(retryNotifierKey{}).(func(RetryNotice))
	return fn
}

// defaultRetryableStatus mirrors the openai-go SDK's own shouldRetry status
// check (internal/requestconfig/requestconfig.go, not exported) — duplicated
// here so notifyRetry can tell whether a status code *other* than the 413
// case openaiRetryableStatusOption specially marks is going to be retried
// anyway by the SDK's own default behavior, and is therefore just as worth
// telling the user about.
func defaultRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

// retryReason turns a response into the short human-readable cause
// RetryNotice.Reason carries. forced413 is true when
// openaiRetryableStatusOption's own body-shape check — not the SDK's
// default status-code predicate — is the reason this 413 counts as
// retryable at all.
func retryReason(statusCode int, forced413 bool) string {
	switch {
	case forced413:
		return "rate limited (413, too many input tokens per minute)"
	case statusCode == http.StatusTooManyRequests:
		return "rate limited (429)"
	case statusCode >= http.StatusInternalServerError:
		return "server error (" + strconv.Itoa(statusCode) + ")"
	case statusCode == http.StatusRequestTimeout:
		return "request timeout (408)"
	case statusCode == http.StatusConflict:
		return "conflict (409)"
	default:
		return "status " + strconv.Itoa(statusCode)
	}
}

// estimateRetryDelay approximates how long the SDK's own retry loop
// (requestconfig.go's retryDelay) will wait before the next attempt: the
// response's Retry-After header (seconds) when the server sent one,
// otherwise the same 0.5s*2^attempt exponential curve capped at 8s —
// without the real implementation's random jitter, since this is only for
// display.
func estimateRetryDelay(res *http.Response, attempt int) time.Duration {
	if v := res.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	delay := time.Duration(0.5 * float64(time.Second) * math.Pow(2, float64(attempt)))
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	return delay
}

// notifyRetry reports res via ctx's retry notifier (if any) when res counts
// as retryable — either by the SDK's own default status-code predicate or
// by openaiRetryableStatusOption's 413 body-shape check (forced413) — and
// the SDK's retry loop actually has an attempt left to spend on it. req is
// the already-sent request (its X-Stainless-Retry-Count header, set by the
// SDK itself before each attempt, carries the 0-based index of the attempt
// that just produced res).
func notifyRetry(req *http.Request, res *http.Response, forced413 bool) {
	if !forced413 && !defaultRetryableStatus(res.StatusCode) {
		return
	}
	notify := retryNotifierFromContext(req.Context())
	if notify == nil {
		return
	}

	attempt, _ := strconv.Atoi(req.Header.Get("X-Stainless-Retry-Count"))
	if attempt >= maxProviderRetries {
		// The SDK's own loop breaks without retrying once retryCount >=
		// cfg.MaxRetries (requestconfig.go) — this was the last allowed
		// attempt, so reporting "retrying" here would be wrong; the
		// resulting error speaks for itself instead.
		return
	}

	notify(RetryNotice{
		StatusCode:  res.StatusCode,
		Reason:      retryReason(res.StatusCode, forced413),
		Attempt:     attempt + 1,
		MaxAttempts: maxProviderRetries + 1,
		Delay:       estimateRetryDelay(res, attempt),
	})
}
