package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/shelltool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBashTool_ContractAndApproval(t *testing.T) {
	bt, err := NewBashTool(BashConfig{})
	require.NoError(t, err)

	var _ tool.FuncTool = bt
	assert.NotEmpty(t, bt.Name())
	assert.NotEmpty(t, bt.Description())
	assert.NotNil(t, bt.Schema())

	approval, ok := bt.(tool.ApprovalRequiredTool)
	require.True(t, ok, "bash tool must implement tool.ApprovalRequiredTool")
	assert.True(t, approval.ApprovalRequired(), "bash tool must require approval per FR5")
}

func TestNewBashTool_RunsEcho(t *testing.T) {
	bt, err := NewBashTool(BashConfig{})
	require.NoError(t, err)

	args, err := json.Marshal(map[string]string{"command": "echo hello"})
	require.NoError(t, err)

	out, err := bt.Call(context.Background(), string(args))
	require.NoError(t, err)

	text, ok := out.(string)
	require.True(t, ok, "expected string output from shelltool")
	assert.Contains(t, text, "hello")
	assert.Contains(t, text, "exit_code: 0")
}

func TestNewBashTool_NonZeroExitReported(t *testing.T) {
	bt, err := NewBashTool(BashConfig{})
	require.NoError(t, err)

	args, err := json.Marshal(map[string]string{"command": "exit 3"})
	require.NoError(t, err)

	out, err := bt.Call(context.Background(), string(args))
	require.NoError(t, err)

	text, ok := out.(string)
	require.True(t, ok)
	assert.Contains(t, text, "exit_code: 3")
}

func TestNewBashTool_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	bt, err := NewBashTool(BashConfig{WorkingDirectory: dir})
	require.NoError(t, err)

	args, err := json.Marshal(map[string]string{"command": "pwd"})
	require.NoError(t, err)

	out, err := bt.Call(context.Background(), string(args))
	require.NoError(t, err)

	text, ok := out.(string)
	require.True(t, ok)
	assert.Contains(t, text, dir)
}

func TestNewBashTool_InvalidConfigPropagatesError(t *testing.T) {
	// Shell and ShellArgv are mutually exclusive per shelltool; forcing that
	// conflict indirectly isn't possible through BashConfig today, so this
	// test instead verifies a negative MaxOutputBytes value (which shelltool
	// itself rejects) surfaces as a constructor error rather than a panic.
	_, err := NewBashTool(BashConfig{MaxOutputBytes: -1})
	require.Error(t, err)
}

// Sanity check that BashConfig.Policy actually reaches shelltool (a deny
// pattern rejects a matching command before it ever runs).
func TestNewBashTool_PolicyDenyRejectsCommand(t *testing.T) {
	policy, err := shelltool.NewPolicy(shelltool.PolicyConfig{DenyList: []string{"^rm "}})
	require.NoError(t, err)

	bt, err := NewBashTool(BashConfig{Policy: policy})
	require.NoError(t, err)

	args, err := json.Marshal(map[string]string{"command": "rm -rf /nonexistent"})
	require.NoError(t, err)

	_, err = bt.Call(context.Background(), string(args))
	require.Error(t, err)
}
