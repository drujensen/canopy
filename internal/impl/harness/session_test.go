package harness_test

import (
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
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
