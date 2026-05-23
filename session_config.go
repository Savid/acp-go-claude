package claudeacp

import (
	"context"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

func (s *Session) modelSelection(preference string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	model := preference
	if resolved := resolveModelPreference(s.availableModels, preference); resolved != nil {
		model = resolved.Value
	}

	return model, claudeModelID(model, s.modelOverrides)
}

func (s *Session) setModelAndClampMode(model string) (bool, acp.SessionModeId, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.model = model
	s.contextWindowSize = contextWindowForAvailableModel(model, s.availableModels)

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

func (s *Session) setMode(mode acp.SessionModeId) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = mode
}

func (s *Session) modelInfo() (string, []claude.AvailableModelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.model, append([]claude.AvailableModelInfo(nil), s.availableModels...)
}

func (s *Session) modeInfo() (acp.SessionModeId, string, []claude.AvailableModelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mode, s.model, append([]claude.AvailableModelInfo(nil), s.availableModels...)
}

func (s *Session) configInfo() (acp.SessionModeId, string, []claude.AvailableModelInfo, string, []string, string, bool, bool) {
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

func (s *Session) setOutputStyle(style string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.outputStyle = style
}

func (s *Session) setEffort(effort string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.effort = effort
}

func (s *Session) applyEffort(ctx context.Context, effort string) error {
	if effort == "" {
		return s.client.ApplyFlagSettings(ctx, map[string]any{string(configEffort): nil})
	}

	return s.client.SetEffort(ctx, effort)
}

func (s *Session) setFastMode(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fastMode = enabled
	s.fastModeKnown = true
}

func (s *Session) commands() []claude.SlashCommand {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]claude.SlashCommand(nil), s.availableCommands...)
}
