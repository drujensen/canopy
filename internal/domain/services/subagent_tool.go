package services

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/tool"
)

// subagentToolDefaultDescription mirrors tool/agenttool's own
// defaultDescription constant (unexported there) — the .NET AIAgent
// function-tool default description, used when a dispatched agent exposes
// no description of its own.
const subagentToolDefaultDescription = "Invoke an agent to retrieve some information."

// subagentToolNameSanitizer mirrors tool/agenttool's own invalidNameChars
// regex (unexported there): every run of characters invalid in a provider
// function name becomes a single underscore, so an agent display name like
// "Weather Agent" becomes a valid function name.
var subagentToolNameSanitizer = regexp.MustCompile(`[^0-9A-Za-z]+`)

// newUsageTrackingSubagentTool wraps a as a dynamically-dispatchable
// subagent tool (FR9, Design §3.4) — the same Name/Description/Schema/Call
// shape tool/agenttool.New already provides, reimplemented here (rather
// than wrapping agenttool.New's own functool) because that package's Call
// has no hook for anything beyond returning the subagent's text: it
// discards resp.Usage() entirely (see agenttool.go's functool.Call), so a
// dispatched subagent's token usage was otherwise invisible to
// AgentService no matter what — the usage-tracking feature (session
// cost in the TUI sidebar, Chat.Usage) would simply never see subagent
// cost. modelName is the subagent's own resolved entities.ModelConfig.Name
// (buildSubagentAgent) — tagging the reported usage with it is what lets
// mergeUsage bill it at that model's rate, which can differ from the
// parent's.
func newUsageTrackingSubagentTool(a *agent.Agent, modelName string) tool.FuncTool {
	return usageTrackingSubagentTool{agent: a, modelName: modelName}
}

type usageTrackingSubagentTool struct {
	agent     *agent.Agent
	modelName string
}

func (t usageTrackingSubagentTool) Name() string {
	return subagentToolNameSanitizer.ReplaceAllString(t.agent.Name(), "_")
}

func (t usageTrackingSubagentTool) Description() string {
	if d := t.agent.Description(); d != "" {
		return d
	}
	return subagentToolDefaultDescription
}

func (t usageTrackingSubagentTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "input query to invoke the agent",
			},
		},
		"required": []string{"query"},
	}
}

func (t usageTrackingSubagentTool) ReturnSchema() any {
	return map[string]any{
		"type": "string",
	}
}

// Call invokes the subagent and reports its token usage — tagged with
// t.modelName — to whatever usageCollector is attached to ctx (a no-op,
// via usageCollector.record's nil-receiver guard, if none is: a caller
// that never called withUsageCollector, or a direct unit test of this
// tool). ctx here is whatever a.Run(ctx, ...)'s own call chain threads
// down through toolautocall's tool-call loop — the same ctx
// RunMessages/RunMessagesStream attached a collector to, not anything this
// tool constructs or holds a reference to itself.
func (t usageTrackingSubagentTool) Call(ctx context.Context, args string) (any, error) {
	var in struct {
		Query string `json:"query"`
	}
	if args == "" {
		args = "{}"
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return nil, err
	}
	resp, err := t.agent.RunText(ctx, in.Query).Collect()
	if err != nil {
		return "", err
	}
	usageCollectorFromContext(ctx).record(t.modelName, resp.Usage())
	return resp.String(), nil
}
