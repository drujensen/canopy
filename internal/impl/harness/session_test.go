package harness_test

import (
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/agentmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/harness"
)

// TestLoadSession_EmptyState asserts a chat with no stored SessionState (a
// brand-new chat) yields a fresh, usable *agent.Session rather than an error
// or nil.
func TestLoadSession_EmptyState(t *testing.T) {
	chat := &entities.Chat{ID: "chat-1"}
	session, err := harness.LoadSession(chat)
	require.NoError(t, err)
	require.NotNil(t, session)
}

// TestLoadSession_RoundTrip asserts a session serialized via
// SerializeSession and stored on chat.SessionState deserializes back via
// LoadSession, the real round trip domain/services.AgentService relies on
// across turns/process restarts.
func TestLoadSession_RoundTrip(t *testing.T) {
	original := &agent.Session{}
	data, err := harness.SerializeSession(original)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	chat := &entities.Chat{ID: "chat-1", SessionState: data}
	session, err := harness.LoadSession(chat)
	require.NoError(t, err)
	require.NotNil(t, session)
}

// TestLoadSession_InvalidJSON asserts corrupted/foreign SessionState bytes
// (e.g. from a hand-edited or truncated store file) produce a wrapped error
// identifying the offending chat, not a panic.
func TestLoadSession_InvalidJSON(t *testing.T) {
	chat := &entities.Chat{ID: "chat-1", SessionState: []byte("{not valid json")}
	_, err := harness.LoadSession(chat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat-1")
}

// TestLoadSession_TolerantOfOrphanedAgentModeState is a backward-compat
// regression test: a chat persisted before mode/agentmode.Provider was
// removed from this package's wiring may still carry a stale
// "agentModeState" key in its SessionState blob (see agentmode.go's own
// stateKey constant). agent.Session's state is a generic, unvalidated
// map[string]*stateValue with no required-key schema, and nothing in this
// package's current wiring reads or writes that key anymore, so it must
// ride along as harmless, orphaned data — LoadSession must still succeed,
// and a round trip back through SerializeSession must not choke on or drop
// it either.
func TestLoadSession_TolerantOfOrphanedAgentModeState(t *testing.T) {
	session := &agent.Session{}
	// Simulate a session that still has agentmode's own key set, the way a
	// pre-removal chat's persisted SessionState would — using the real
	// agentmode.Provider (still vendored, just no longer wired by this
	// package) rather than hand-rolling the key's internal JSON shape.
	modeProvider := agentmode.New(agentmode.Config{DefaultMode: "execute"})
	require.NoError(t, modeProvider.SetMode("plan", agent.WithSession(session)))

	data, err := harness.SerializeSession(session)
	require.NoError(t, err)
	require.Contains(t, string(data), "agentModeState", "the orphaned key must actually be present in the fixture for this test to mean anything")

	chat := &entities.Chat{ID: "chat-1", SessionState: data}
	loaded, err := harness.LoadSession(chat)
	require.NoError(t, err, "an orphaned agentModeState key must not break LoadSession")
	require.NotNil(t, loaded)

	// A round trip back through SerializeSession must also succeed and keep
	// carrying the orphaned key along untouched, not drop or corrupt it.
	roundTripped, err := harness.SerializeSession(loaded)
	require.NoError(t, err)
	assert.Contains(t, string(roundTripped), "agentModeState")
}

// TestSerializeSession_NilSession asserts a nil session marshals as a fresh
// empty session rather than panicking or producing the JSON literal "null" —
// SerializeSession's own doc comment calls this out explicitly.
func TestSerializeSession_NilSession(t *testing.T) {
	data, err := harness.SerializeSession(nil)
	require.NoError(t, err)
	assert.NotEqual(t, "null", string(data))

	// The result must itself load back via LoadSession.
	chat := &entities.Chat{ID: "chat-1", SessionState: data}
	session, err := harness.LoadSession(chat)
	require.NoError(t, err)
	require.NotNil(t, session)
}
