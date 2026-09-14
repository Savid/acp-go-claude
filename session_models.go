package claudeacp

import (
	"context"
	"errors"
	"slices"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
)

const (
	configModel       acp.SessionConfigId = "model"
	configMode        acp.SessionConfigId = "mode"
	configEffort      acp.SessionConfigId = "effort"
	configOutputStyle acp.SessionConfigId = "output_style"
	configTypeSelect                      = "select"
)

func (s *session) configOptions() []acp.SessionConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()

	options := []acp.SessionConfigOption{selectConfig(configModel, "Model", s.model, modelSelectOptions(s.model, s.models, s.agent.options.ConfiguredModels))}
	modes := []string{nativeDefault, permissionModePlan, "acceptEdits", "bypassPermissions", "dontAsk"}

	for _, model := range s.models {
		if model.SupportsAutoMode {
			modes = append(modes, "auto")

			break
		}
	}

	if s.options.PermissionMode != "" && !slices.Contains(modes, s.options.PermissionMode) {
		modes = append(modes, s.options.PermissionMode)
	}

	options = append(options, selectConfig(configMode, "Permission mode", s.options.PermissionMode, stringChoices(modes)))
	for _, model := range s.models {
		if model.Value != s.model || !model.SupportsEffort || s.effort == "" {
			continue
		}

		options = append(options, selectConfig(configEffort, "Effort", s.effort, stringChoices(model.SupportedEffortLevels)))

		break
	}

	if len(s.outputStyles) > 0 {
		options = append(options, selectConfig(configOutputStyle, "Output style", s.outputStyle, stringChoices(s.outputStyles)))
	}

	return options
}
func selectConfig(id acp.SessionConfigId, name, current string, values acp.SessionConfigSelectOptionsUngrouped) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategory("model_config")

	switch id {
	case configModel:
		category = acp.SessionConfigOptionCategoryModel
	case configMode:
		category = acp.SessionConfigOptionCategoryMode
	case configEffort:
		category = acp.SessionConfigOptionCategoryThoughtLevel
	}

	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{Id: id, Category: &category, Name: name, Type: configTypeSelect, CurrentValue: acp.SessionConfigValueId(current), Options: acp.SessionConfigSelectOptions{Ungrouped: &values}}}
}
func stringChoices(names []string) acp.SessionConfigSelectOptionsUngrouped {
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(names))
	for _, name := range names {
		values = append(values, acp.SessionConfigSelectOption{Name: name, Value: acp.SessionConfigValueId(name)})
	}

	return values
}
func modelSelectOptions(current string, models []claude.Model, configured []string) acp.SessionConfigSelectOptionsUngrouped {
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(models)+len(configured)+1)

	seen := make(map[string]bool)
	for _, model := range models {
		if model.Value == "" || seen[model.Value] {
			continue
		}

		seen[model.Value] = true

		name := model.DisplayName
		if name == "" {
			name = model.Value
		}

		meta := map[string]any{"modelId": model.Value}
		if len(model.SupportedEffortLevels) > 0 {
			meta["supportedEffortLevels"] = slices.Clone(model.SupportedEffortLevels)
		}

		values = append(values, acp.SessionConfigSelectOption{Name: name, Value: acp.SessionConfigValueId(model.Value), Meta: map[string]any{vendor: meta}})
	}

	for _, id := range append(slices.Clone(configured), current) {
		if id == "" || seen[id] {
			continue
		}

		seen[id] = true
		values = append(values, acp.SessionConfigSelectOption{Name: id, Value: acp.SessionConfigValueId(id)})
	}

	return values
}
func (s *session) setConfigOption(ctx context.Context, id acp.SessionConfigId, value string) ([]acp.SessionConfigOption, error) {
	if err := s.admissionError(); err != nil {
		return nil, err
	}

	if value == "" {
		return nil, wire.Unsupported("value")
	}

	switch id {
	case configModel, configMode, configEffort, configOutputStyle:
	default:
		return nil, wire.Unsupported("configId")
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return nil, err
	}
	defer release()

	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return nil, err
	}

	switch id {
	case configModel:
		err = rt.client.SetModel(ctx, value)
	case configMode:
		err = rt.client.SetPermissionMode(ctx, value)
	case configEffort:
		err = rt.client.ApplySettings(ctx, map[string]any{nativeEffortLevel: value})
	case configOutputStyle:
		err = rt.client.ApplySettings(ctx, map[string]any{nativeOutputStyle: value})
	}

	if err != nil {
		var commandErr *claude.CommandError
		if errors.As(err, &commandErr) {
			return nil, wire.Unsupported("value")
		}

		return nil, s.transportFailure(ctx, rt, err)
	}

	settings, settingsErr := rt.client.Settings(ctx)
	if settingsErr != nil {
		return nil, s.transportFailure(ctx, rt, settingsErr)
	}

	s.mu.Lock()
	switch id {
	case configModel:
		s.model = value
		s.options.Model = value
	case configMode:
		s.options.PermissionMode = value
	case configEffort:
		s.effort = settings.Effective.EffortLevel
		s.options.Effort = s.effort
	case configOutputStyle:
		s.outputStyle = settings.Effective.OutputStyle
	}
	s.mu.Unlock()

	if err := s.commitMirror(ctx); err != nil {
		return nil, wire.InternalFailure(vendor, "")
	}

	options := s.configOptions()
	_ = s.emit(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}})

	return options, nil
}
