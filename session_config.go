package claudeacp

import (
	"context"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

func (s *agentSession) modelSelection(preference string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	model := preference
	if resolved := resolveModelPreference(s.availableModels, preference); resolved != nil {
		model = resolved.Value
	}

	return model, claudeModelID(model, s.modelOverrides)
}

func (s *agentSession) setModelAndClampMode(model string) (bool, acp.SessionModeId, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.model = model
	// A model change leaves the context window unknown until the harness reports
	// one again; never fabricate it from a static catalog.
	s.contextWindowSize = 0

	nextEffort, effortChanged := reconcileEffortForModel(model, s.availableModels, s.effort)
	if effortChanged {
		s.effort = nextEffort
	}

	if !modeAvailableForModel(s.mode, s.model, s.availableModels) {
		s.mode = modeDefault

		return true, modeDefault, effortChanged, nextEffort
	}

	return false, s.mode, effortChanged, nextEffort
}

func (s *agentSession) setMode(mode acp.SessionModeId) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = mode
}

func (s *agentSession) modeInfo() (acp.SessionModeId, string, []claude.AvailableModelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mode, s.model, append([]claude.AvailableModelInfo(nil), s.availableModels...)
}

func (s *agentSession) configInfo() (acp.SessionModeId, string, []claude.AvailableModelInfo, string, []string, string, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mode,
		s.model,
		append([]claude.AvailableModelInfo(nil), s.availableModels...),
		s.outputStyle,
		append([]string(nil), s.availableOutputStyles...),
		s.effort,
		s.fastMode,
		s.fastModeKnown
}

func (s *agentSession) setOutputStyle(style string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.outputStyle = style
}

func (s *agentSession) setEffort(effort string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.effort = effort
}

func (s *agentSession) applyEffort(ctx context.Context, effort string) error {
	if effort == "" {
		return s.client.ApplyFlagSettings(ctx, map[string]any{string(configEffort): nil})
	}

	return s.client.SetEffort(ctx, effort)
}

func (s *agentSession) commands() []claude.SlashCommand {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneSlashCommands(s.availableCommands)
}

func cloneSlashCommands(commands []claude.SlashCommand) []claude.SlashCommand {
	cloned := append([]claude.SlashCommand(nil), commands...)
	for i, command := range commands {
		cloned[i].Aliases = append([]string(nil), command.Aliases...)
	}

	return cloned
}
