package claudeacp

import "log/slog"

func ExampleNewAgent_options() {
	agent := NewAgent(
		WithClaudeHome("/home/alice/.claude"),
		WithDefaultModel("sonnet"),
		WithEnv(map[string]string{
			"ANTHROPIC_MODEL": "sonnet",
		}),
		WithLogger(slog.Default()),
	)

	_ = agent
}
