package tui

// Phase 8 (docs/PLAN.md): goroutine-leak coverage for stream.go's startTurn,
// which spawns one goroutine per turn to range AgentService.RunMessagesStream's
// returned agent.ResponseStream and forward each event onto a channel (see
// startTurn's own doc comment). goleak.IgnoreCurrent/VerifyNone is used
// rather than a hand-rolled runtime.NumGoroutine() before/after comparison:
// go.uber.org/goleak is already present in this module's dependency graph as
// an indirect test-only dependency of go.uber.org/zap's own test suite (`go
// mod why go.uber.org/goleak` shows the only path is
// .../zap -> zap.test -> goleak), so promoting it to a direct, test-only
// import adds no new module to go.sum and its own go.mod pulls in nothing
// beyond testify and testify's already-vendored transitive deps (confirmed
// via `go mod tidy` leaving go.sum unchanged apart from goleak's own
// require line moving from indirect to direct) — a materially smaller,
// safer diff than reimplementing goleak's retry-with-backoff stack-diffing
// by hand. goleak.Find's default retry (up to 20 attempts, exponential
// backoff capped at 100ms apart) is itself a bounded poll, not a fixed
// sleep, matching this phase's "no flaky fixed sleeps" constraint.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// newLeakTestService builds a real AgentService (JSON-backed repository in a
// temp dir, one OpenAI-compatible provider pointed at an httptest server
// running handler) — the same construction shape newApprovalTestService
// (model_test.go) and newMCPTestService (mcp_stream_test.go) already use in
// this package, factored out here so each leak test below can supply its
// own handler shape while still explicitly controlling the server's
// shutdown (rather than deferring it to t.Cleanup, which would run after
// this test's own goleak check) — see each caller's use of the returned
// closeServer.
func newLeakTestService(t *testing.T, workDir string, handler http.HandlerFunc) (svc *services.AgentService, closeServer func()) {
	t.Helper()
	server := httptest.NewServer(handler)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc = services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: workDir},
	})
	return svc, server.Close
}

// settleHTTPGoroutines closes srv (Server.Close blocks until outstanding
// requests finish and force-closes idle/new connections — see
// net/http/httptest's own doc comment) and, defensively, drops any
// connections the provider SDK's client may be idling on the process-wide
// http.DefaultTransport (impl/providers constructs its OpenAI client with no
// explicit http.Client override — see factory.go/openaicompat.go — so it
// falls back to the SDK's default, which is ultimately backed by
// net/http's default transport). Called before every goleak check below so
// a connection's read-loop goroutine winding down doesn't need to race
// goleak's own bounded retry budget on top of an unclosed server.
func settleHTTPGoroutines(closeServer func()) {
	closeServer()
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// TestStartTurn_NoLeak_ManyTurnsBackToBack drives many ordinary turns
// through the real chatModel.startTurnCmd -> stream.go's startTurn ->
// AgentService.RunMessagesStream path, one after another, and confirms the
// per-turn forwarding goroutine startTurn spawns does not accumulate: after
// every turn is fully drained (drainCmdWithTimeout, the same helper
// mcp_stream_test.go's tests already use), goroutine count must be back at
// whatever it was before this test started.
func TestStartTurn_NoLeak_ManyTurnsBackToBack(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	}
	svc, closeServer := newLeakTestService(t, t.TempDir(), handler)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	opts := goleak.IgnoreCurrent()

	const turns = 30
	for i := 0; i < turns; i++ {
		drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText(fmt.Sprintf("turn %d", i))}), 5*time.Second)
		require.Nil(t, c.statusErr, "turn %d must not have errored", i)
	}

	settleHTTPGoroutines(closeServer)
	goleak.VerifyNone(t, opts)
}

// TestStartTurn_NoLeak_ErrorMidStream reproduces a turn whose provider call
// fails partway through — not on the very first round trip, but after the
// model has already made one non-approval-gated tool call (file_read, which
// toolautocall auto-executes and immediately continues the same turn with a
// second HTTP round trip — Bash/FileWrite are the only approval-gated core
// tools, per AgentService's own buildTools doc comment, so file_read is the
// simplest way to force a genuine second in-turn HTTP call without a user
// decision in between). The second call returns 500, which must surface as
// a streamErrMsg (not hang, not be swallowed), and startTurn's forwarding
// goroutine for that turn must still exit cleanly.
func TestStartTurn_NoLeak_ErrorMidStream(t *testing.T) {
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hi"), 0o644))

	var callCount int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			args, err := json.Marshal(map[string]string{"path": "hello.txt"})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "file_read", string(args)))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}
	svc, closeServer := newLeakTestService(t, workDir, handler)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	opts := goleak.IgnoreCurrent()

	drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please read hello.txt")}), 10*time.Second)
	require.GreaterOrEqual(t, atomic.LoadInt32(&callCount), int32(2), "the tool call must have triggered a second, failing HTTP round trip within the same turn")
	require.Error(t, c.statusErr, "the second round trip's failure must surface as a streamErrMsg")

	settleHTTPGoroutines(closeServer)
	goleak.VerifyNone(t, opts)
}

// TestStartTurn_NoLeak_ContextCancelledMidTurn cancels the turn's context
// while the provider's HTTP call is genuinely in flight (the fake handler
// signals requestReceived once it sees the request, and the test only
// calls cancel() after that handshake — not a fixed sleep).
// AgentService.RunMessagesStream's own doc comment already documents the
// resulting contract: the turn's range loop ends via a non-nil error,
// finalize (had it been reached) would report that error, and
// persistSession never runs ("no persistence happens" on an early stop).
// What this test adds is confirmation that this exact path — cancellation
// causing an early, error-terminated stream — still lets stream.go's
// forwarding goroutine observe the terminal error, forward it, and exit,
// rather than leaving it blocked forever on a channel send nobody reads
// (which would be a genuine leak the doc comment's contract doesn't
// cover).
//
// The handler waits on r.Context().Done() OR a bounded fallback timer,
// deliberately not an unbounded wait: an earlier version of this test
// blocked on r.Context().Done() alone and discovered, empirically, that
// canceling the *client*-side context here makes AgentService's call
// return an error quickly (proving the goroutine-forwarding path itself
// doesn't leak — this test's actual subject) without the provider SDK's
// HTTP client necessarily tearing down the underlying TCP connection on
// the same timescale, which left the *fake test server's* handler
// goroutine blocked and made httptest.Server.Close() hang for the rest of
// the test binary's run — a leak in the test's own server fixture, not in
// Canopy. The bounded fallback lets that abandoned request complete on its
// own shortly after, so server cleanup (settleHTTPGoroutines) can't hang
// regardless of the vendored SDK client's own connection-teardown timing.
func TestStartTurn_NoLeak_ContextCancelledMidTurn(t *testing.T) {
	requestReceived := make(chan struct{}, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestReceived <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		// Either the client tore down the connection (Done fired) or the
		// bounded fallback elapsed; either way, return now so the fake
		// server never has an outstanding request lingering forever.
	}
	svc, closeServer := newLeakTestService(t, t.TempDir(), handler)

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, err := svc.StartChat(runCtx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	opts := goleak.IgnoreCurrent()

	cmd := c.startTurnCmd(svc, runCtx, []*message.Message{message.NewText("hi")})

	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		select {
		case <-requestReceived:
		case <-time.After(5 * time.Second):
			t.Error("provider handler never observed the request within 5s")
		}
		cancel()
	}()

	drainCmdWithTimeout(t, c, cmd, 10*time.Second)
	<-cancelled
	require.Error(t, c.statusErr, "a context cancellation mid-turn must surface as a streamErrMsg, not hang")

	settleHTTPGoroutines(closeServer)
	goleak.VerifyNone(t, opts)
}
