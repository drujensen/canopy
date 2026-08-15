// Command hello is a Phase 0 spike: construct one agent via openaiprovider and
// stream a real response, to validate the agent-framework-go dependency
// actually resolves and works end to end before anything else is built on it.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	client := openai.NewClient(option.WithAPIKey(apiKey))

	a := openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agent.Config{
			Name: "hello",
		},
		Instructions: "You are a terse test agent. Reply in under 10 words.",
		Model:        "gpt-4o-mini",
	})

	ctx := context.Background()
	for update, err := range a.RunText(ctx, "Say hello and name the model you are.") {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		for _, c := range update.Contents {
			if text, ok := c.(*message.TextContent); ok {
				fmt.Print(text.Text)
			}
		}
	}
	fmt.Println()
}
