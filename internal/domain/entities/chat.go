// Package entities holds Canopy's core domain types. Per Design §6, this is
// deliberately small: Agent/Skill/Tool configuration lives as files on disk
// (Design §3.11), not as domain entities, so Chat and the provider/model
// config types are what remain here.
package entities

import (
	"time"

	"github.com/microsoft/agent-framework-go/message"
)

// Chat is a persisted conversation between a user and one configured agent
// (Design §6, Requirements FR13/FR14). It is the one entity in Canopy that
// still needs a real repository — Agent/Skill/MCP configuration are
// file-based (Design §3.11) and have no repository of their own.
type Chat struct {
	// ID uniquely identifies this chat/session.
	ID string `json:"id"`

	// AgentName references an agent definition loaded by impl/agentsource
	// by name. There is no database relation here — agentsource (Phase
	// 3.5) owns resolving this name to an actual agent definition; Chat
	// only needs to remember which one it was talking to.
	AgentName string `json:"agent_name"`

	// Messages is the chat history for this conversation, in MAF-Go's own
	// message type so it can later back an agent.HistoryProvider (Design
	// §3.9) without a translation layer. message.Message and its Contents
	// already round-trip through encoding/json.
	Messages []*message.Message `json:"messages"`

	// SessionState holds a serialized *agent.Session blob (Design §3.9,
	// Requirements FR14): standing approval rules, todo items, and the
	// current plan/execute mode, all persisted together as one opaque
	// byte slice. Phase 1 only carries the field and stores it as raw
	// bytes; actual (de)serialization against *agent.Session is Phase 5's
	// job.
	SessionState []byte `json:"session_state,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
