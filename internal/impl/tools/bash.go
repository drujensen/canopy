package tools

import (
	"time"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/shelltool"
)

// BashConfig configures NewBashTool.
type BashConfig struct {
	// WorkingDirectory is the directory commands run in. Empty means the
	// current process working directory (shelltool's own default).
	WorkingDirectory string

	// Timeout is the per-command deadline. Zero means no timeout.
	Timeout time.Duration

	// MaxOutputBytes caps captured stdout/stderr per command. Zero uses
	// shelltool's default (64 KiB).
	MaxOutputBytes int

	// Policy is an optional allow/deny regex guardrail evaluated before a
	// command reaches the shell. It is a UX guardrail, not a security
	// boundary (see shelltool's package doc).
	Policy *shelltool.Policy
}

// NewBashTool builds Canopy's bash/shell tool (docs/DESIGN.md §3.2 row 1) by
// wrapping the framework-provided local shell tool (tool/shelltool) rather
// than reimplementing process execution. Runs stateless-per-call
// (shelltool.ModeStateless): a fresh shell per command, so concurrent chat
// sessions never share persistent-shell state (working directory, exported
// vars) with each other — the tradeoff is that a `cd` doesn't persist across
// calls, which matches how Canopy invokes tools per-turn from potentially
// concurrent sessions.
//
// Approval-gating was removed (it only ever auto-approved silently, adding
// an extra round trip and the risk of malformed-history bugs with no actual
// safety benefit). AcknowledgeUnsafe: true opts out of shelltool's own
// default approval-required gate (shelltool.Local.ApprovalRequired returns
// !AcknowledgeUnsafe) — without this, shelltool itself still requires
// approval regardless of Canopy's own wrapping, since that default is
// independent of tool.ApprovalRequiredFunc.
func NewBashTool(cfg BashConfig) (tool.FuncTool, error) {
	local, err := shelltool.NewLocal(shelltool.LocalConfig{
		Mode:              shelltool.ModeStateless,
		WorkingDirectory:  cfg.WorkingDirectory,
		Timeout:           cfg.Timeout,
		MaxOutputBytes:    cfg.MaxOutputBytes,
		Policy:            cfg.Policy,
		AcknowledgeUnsafe: true,
	})
	if err != nil {
		return nil, err
	}
	return local, nil
}
