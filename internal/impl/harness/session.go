package harness

import (
	"encoding/json"
	"fmt"

	"github.com/microsoft/agent-framework-go/agent"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// LoadSession deserializes chat.SessionState into a *agent.Session (Design
// §3.9): standing tool-approval rules, todo items, and the current
// plan/execute mode all ride in this one blob. An empty/nil SessionState
// (a brand-new chat) yields a fresh &agent.Session{} — its zero value is
// documented by agent.Session as ready to use, so a first turn on a new chat
// needs no special-casing.
func LoadSession(chat *entities.Chat) (*agent.Session, error) {
	session := &agent.Session{}
	if len(chat.SessionState) == 0 {
		return session, nil
	}
	if err := json.Unmarshal(chat.SessionState, session); err != nil {
		return nil, fmt.Errorf("harness: unmarshaling session state for chat %q: %w", chat.ID, err)
	}
	return session, nil
}

// SerializeSession marshals session back into the byte slice
// entities.Chat.SessionState stores, per agent.Session's own doc comment
// ("a Session can be serialized and deserialized directly with
// encoding/json"). A nil session marshals as a fresh, empty session rather
// than panicking or producing "null", so a chat that was never run still
// round-trips to a valid (empty) session.
func SerializeSession(session *agent.Session) ([]byte, error) {
	if session == nil {
		session = &agent.Session{}
	}
	data, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("harness: marshaling session state: %w", err)
	}
	return data, nil
}
