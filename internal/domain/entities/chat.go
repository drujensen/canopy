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
	// Requirements FR14): standing approval rules and todo items, all
	// persisted together as one opaque byte slice. Phase 1 only carries the
	// field and stores it as raw
	// bytes; actual (de)serialization against *agent.Session is Phase 5's
	// job.
	SessionState []byte `json:"session_state,omitempty"`

	// ModelOverride is a per-chat model switch (post-v0.1.0 addendum,
	// Requirements FR1, Design §4): empty (the default for every chat
	// created before this field existed, and for any chat that hasn't used
	// it) means "use normal resolution" — the agent definition's own
	// "model" frontmatter, or AgentServiceConfig.DefaultModel if the
	// definition doesn't set one (see AgentService.resolveProviderModel).
	// A non-empty value names a ModelConfig.Name (Design §4's flat JSON
	// model config, keyed by ModelConfig.Name — the same key space
	// AgentDefinition.Model already resolves against) to use instead, for
	// this chat specifically, set via AgentService.SetModel (the TUI's
	// ctrl+o model-picker keybinding). It does not affect any other chat
	// bound to the same agent, and — per Design §3.4's subagent-isolation
	// model — it is never inherited by a dynamically-dispatched subagent:
	// only the top-level, chat-bound agent (buildTopLevelAgent) honors it.
	ModelOverride string `json:"model_override,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Title is a short, human-readable summary of this chat (post-v0.1.0
	// addendum, Design §5's addendum — the ctrl+h history browser), set by
	// AgentService.GenerateChatTitle from the chat's first exchange. Empty
	// means no title has been generated yet (a chat's very first turn
	// hasn't completed) or generation failed — the history browser falls
	// back to a formatted date in either case (services.ChatSummary/
	// tui.historyPickerItem), never a blank/placeholder title.
	Title string `json:"title,omitempty"`
}
