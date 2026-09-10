package claudeacp

import (
	"context"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// SetSessionMode exists only because github.com/coder/acp-go-sdk's generated
// Agent interface still requires it. Remove this when the upstream SDK drops
// session/set_mode; the local ACP dispatcher intentionally does not route it.
// The reserved lifecycle key is still refused by its own path first: a family
// literal is never foreign, so it is answered as the invalid parameter it is
// rather than swallowed by the method's absence.
func (a *Agent) SetSessionMode(_ context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	if err := rejectLifecycleMeta(params.Meta); err != nil {
		return acp.SetSessionModeResponse{}, err
	}

	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// SetSessionConfigOption handles supported configuration changes.
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	// Neither variant is a wire field. The union keys on "value" — present
	// selects the value-id form, absent selects the boolean form — and a "type"
	// it does not recognise fails to decode before reaching here. So over the
	// wire the boolean form is what a request naming no value becomes, and the
	// discriminator is the only path worth pointing the caller at. No variant
	// at all is unreachable from JSON; an in-process caller can still build it,
	// and there "value" is the member it left out.
	if params.Boolean != nil {
		if err := rejectLifecycleMeta(params.Boolean.Meta); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}

		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldType)
	}

	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
	}

	if err := rejectLifecycleMeta(params.ValueId.Meta); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return a.setSessionConfigValue(ctx, params.ValueId)
}

func (a *Agent) setSessionConfigValue(
	ctx context.Context,
	params *acp.SetSessionConfigOptionValueId,
) (acp.SetSessionConfigOptionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	switch params.ConfigId {
	case configModel, configMode, configOutputStyle, configEffort:
	default:
		return acp.SetSessionConfigOptionResponse{}, unsupportedField("configId")
	}

	releaseTurn, err := session.acquirePromptTurn(ctx, false)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	defer releaseTurn()

	switch params.ConfigId {
	case configModel:
		model, cliModel := session.modelSelection(string(params.Value))

		_, _, available := session.modeInfo()
		if claude.ModelDisabled(model, available) || claude.ModelDisabled(cliModel, available) {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
		}

		if err := session.client.SetModel(ctx, cliModel); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}

		modelModeChanged, mode, effortChanged, effort := session.setModelAndClampMode(model)
		if modelModeChanged {
			if err := session.client.SetPermissionMode(ctx, string(mode)); err != nil {
				return acp.SetSessionConfigOptionResponse{}, err
			}
		}

		if effortChanged {
			if err := session.applyEffort(ctx, effort); err != nil {
				return acp.SetSessionConfigOptionResponse{}, err
			}
		}
	case configMode:
		mode := acp.SessionModeId(params.Value)

		permissionMode, ok := permissionModeForACP(mode)
		if !ok {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
		}

		_, model, available := session.modeInfo()
		if !modeAvailableForModel(mode, model, available, session.bypassPermissionsOffered()) {
			return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
		}

		if err := session.client.SetPermissionMode(ctx, permissionMode); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}

		session.setMode(mode)
	case configOutputStyle:
		if err := session.client.SetOutputStyle(ctx, string(params.Value)); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}

		session.setOutputStyle(string(params.Value))
	case configEffort:
		if err := session.client.SetEffort(ctx, string(params.Value)); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}

		session.setEffort(string(params.Value))
	}

	options := sessionConfigOptions(session)
	updates := []acp.SessionUpdate{{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}}}

	if err := session.emitUpdates(ctx, updates); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func sessionConfigOptions(session *agentSession) []acp.SessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return configOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown, session.bypassPermissionsOffered())
}

func sessionUnstableConfigOptions(session *agentSession) []acp.UnstableSessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return unstableConfigOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown, session.bypassPermissionsOffered())
}

func selectInitialModel(
	defaultModel string,
	envModel string,
	settingsModel string,
	available []claude.AvailableModelInfo,
) initialModelSelection {
	for index, preference := range []string{defaultModel, envModel, settingsModel} {
		if strings.TrimSpace(preference) == "" {
			continue
		}

		fromSettings := index == 2
		if fromSettings && claude.ModelDisabled(preference, available) {
			// Native settings report the concrete runtime model even when only
			// the Default picker choice is permitted by availableModels.
			for _, info := range available {
				if info.Value == modelDefault && !info.Disabled && info.ResolvedModel == preference {
					return initialModelSelection{Model: modelDefault}
				}
			}
		}

		if resolved := resolveModelPreference(available, preference); resolved != nil {
			return initialModelSelection{
				Model:       resolved.Value,
				ShouldApply: !fromSettings && resolved.Value != defaultModel,
			}
		}

		return initialModelSelection{
			Model:       preference,
			ShouldApply: !fromSettings && preference != defaultModel,
		}
	}

	for _, model := range available {
		if model.Value != "" && !model.Disabled {
			return initialModelSelection{
				Model:       model.Value,
				ShouldApply: model.Value != defaultModel,
			}
		}
	}

	return initialModelSelection{}
}

func configOptions(
	mode acp.SessionModeId,
	model string,
	available []claude.AvailableModelInfo,
	outputStyle string,
	outputStyles []string,
	effort string,
	fastMode bool,
	fastModeKnown bool,
	bypassAvailable bool,
) []acp.SessionConfigOption {
	var options []acp.SessionConfigOption

	if model != "" {
		values := configSelectOptions(model, available)
		if len(values) > 0 {
			options = append(options, acp.SessionConfigOption{
				Select: &acp.SessionConfigOptionSelect{
					Id:           configModel,
					Name:         "Model",
					Category:     new(acp.SessionConfigOptionCategoryModel),
					CurrentValue: acp.SessionConfigValueId(model),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	if mode != "" {
		values := modeSelectOptions(model, available, bypassAvailable)
		options = append(options, acp.SessionConfigOption{
			Select: &acp.SessionConfigOptionSelect{
				Id:           configMode,
				Name:         "Mode",
				Category:     new(acp.SessionConfigOptionCategoryMode),
				CurrentValue: acp.SessionConfigValueId(mode),
				Options: acp.SessionConfigSelectOptions{
					Ungrouped: &values,
				},
			},
		})
	}

	if outputStyle != "" {
		values := outputStyleSelectOptions(outputStyle, outputStyles)
		options = append(options, acp.SessionConfigOption{
			Select: &acp.SessionConfigOptionSelect{
				Id:           configOutputStyle,
				Name:         "Output Style",
				Category:     new(modelConfigCategory),
				CurrentValue: acp.SessionConfigValueId(outputStyle),
				Options: acp.SessionConfigSelectOptions{
					Ungrouped: &values,
				},
			},
		})
	}

	if effort != "" {
		if values := effortSelectOptions(model, available, effort); len(values) > 0 {
			options = append(options, acp.SessionConfigOption{
				Select: &acp.SessionConfigOptionSelect{
					Id:           configEffort,
					Name:         "Effort",
					Category:     new(acp.SessionConfigOptionCategoryThoughtLevel),
					CurrentValue: acp.SessionConfigValueId(effort),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	return options
}

func unstableConfigOptions(
	mode acp.SessionModeId,
	model string,
	available []claude.AvailableModelInfo,
	outputStyle string,
	outputStyles []string,
	effort string,
	fastMode bool,
	fastModeKnown bool,
	bypassAvailable bool,
) []acp.UnstableSessionConfigOption {
	var options []acp.UnstableSessionConfigOption

	if model != "" {
		values := configSelectOptions(model, available)
		if len(values) > 0 {
			options = append(options, acp.UnstableSessionConfigOption{
				Select: &acp.UnstableSessionConfigOptionSelect{
					Id:           configModel,
					Name:         "Model",
					Type:         configTypeSelect,
					Category:     new(acp.SessionConfigOptionCategoryModel),
					CurrentValue: acp.SessionConfigValueId(model),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	if mode != "" {
		values := modeSelectOptions(model, available, bypassAvailable)
		options = append(options, acp.UnstableSessionConfigOption{
			Select: &acp.UnstableSessionConfigOptionSelect{
				Id:           configMode,
				Name:         "Mode",
				Type:         configTypeSelect,
				Category:     new(acp.SessionConfigOptionCategoryMode),
				CurrentValue: acp.SessionConfigValueId(mode),
				Options: acp.SessionConfigSelectOptions{
					Ungrouped: &values,
				},
			},
		})
	}

	if outputStyle != "" {
		values := outputStyleSelectOptions(outputStyle, outputStyles)
		options = append(options, acp.UnstableSessionConfigOption{
			Select: &acp.UnstableSessionConfigOptionSelect{
				Id:           configOutputStyle,
				Name:         "Output Style",
				Type:         configTypeSelect,
				Category:     new(modelConfigCategory),
				CurrentValue: acp.SessionConfigValueId(outputStyle),
				Options: acp.SessionConfigSelectOptions{
					Ungrouped: &values,
				},
			},
		})
	}

	if effort != "" {
		if values := effortSelectOptions(model, available, effort); len(values) > 0 {
			options = append(options, acp.UnstableSessionConfigOption{
				Select: &acp.UnstableSessionConfigOptionSelect{
					Id:           configEffort,
					Name:         "Effort",
					Type:         configTypeSelect,
					Category:     new(acp.SessionConfigOptionCategoryThoughtLevel),
					CurrentValue: acp.SessionConfigValueId(effort),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	return options
}

func configSelectOptions(model string, available []claude.AvailableModelInfo) acp.SessionConfigSelectOptionsUngrouped {
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(available)+1)
	seen := make(map[string]struct{}, len(available)+1)

	for _, info := range available {
		if info.Value == "" || info.Disabled || claude.ModelDisabled(info.Value, available) {
			continue
		}

		if _, ok := seen[info.Value]; ok {
			continue
		}

		values = append(values, acp.SessionConfigSelectOption{
			Name:        modelDisplayName(info),
			Value:       acp.SessionConfigValueId(info.Value),
			Description: stringPtrIfNotEmpty(info.Description),
			Meta:        claudeModelInfoMeta(info),
		})
		seen[info.Value] = struct{}{}
	}

	if _, ok := seen[model]; !ok && !claude.ModelDisabled(model, available) {
		info := claude.AvailableModelInfo{Value: model, DisplayName: model}
		if resolved, found := availableModelInfo(model, available); found {
			info = resolved
		}

		values = append(values, acp.SessionConfigSelectOption{
			Name:  model,
			Value: acp.SessionConfigValueId(model),
			Meta:  claudeModelInfoMeta(info),
		})
	}

	return values
}

func outputStyleSelectOptions(current string, available []string) acp.SessionConfigSelectOptionsUngrouped {
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(available)+1)
	seen := make(map[string]struct{}, len(available)+1)

	for _, style := range available {
		if style == "" {
			continue
		}

		if _, ok := seen[style]; ok {
			continue
		}

		values = append(values, acp.SessionConfigSelectOption{
			Name:  style,
			Value: acp.SessionConfigValueId(style),
		})
		seen[style] = struct{}{}
	}

	if _, ok := seen[current]; !ok {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  current,
			Value: acp.SessionConfigValueId(current),
		})
	}

	return values
}

func modeSelectOptions(
	model string,
	available []claude.AvailableModelInfo,
	bypassAvailable bool,
) acp.SessionConfigSelectOptionsUngrouped {
	choices := []struct {
		id   acp.SessionModeId
		name string
	}{
		{id: modeDefault, name: modeNameDefault},
		{id: modePlan, name: "Plan"},
		{id: modeAcceptEdits, name: "Accept Edits"},
	}

	if bypassAvailable {
		choices = append(choices, struct {
			id   acp.SessionModeId
			name string
		}{id: modeBypassPermissions, name: "Bypass Permissions"})
	}

	if modelSupportsAutoMode(model, available) {
		choices = append(choices, struct {
			id   acp.SessionModeId
			name string
		}{id: modeAuto, name: modeNameAuto})
	}

	choices = append(choices, struct {
		id   acp.SessionModeId
		name string
	}{id: modeDontAsk, name: modeNameDontAsk})

	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(choices))

	for _, mode := range choices {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  mode.name,
			Value: acp.SessionConfigValueId(mode.id),
		})
	}

	return values
}

func effortSelectOptions(
	model string,
	available []claude.AvailableModelInfo,
	current string,
) acp.SessionConfigSelectOptionsUngrouped {
	levels := effortLevelsForModel(model, available)
	if len(levels) == 0 {
		return nil
	}

	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(levels)+1)
	seen := make(map[string]struct{}, len(levels)+1)

	for _, level := range levels {
		if level == "" {
			continue
		}

		if _, ok := seen[level]; ok {
			continue
		}

		values = append(values, acp.SessionConfigSelectOption{
			Name:  effortDisplayName(level),
			Value: acp.SessionConfigValueId(level),
		})
		seen[level] = struct{}{}
	}

	if _, ok := seen[current]; !ok {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  effortDisplayName(current),
			Value: acp.SessionConfigValueId(current),
		})
	}

	return values
}

func effortLevelsForModel(model string, available []claude.AvailableModelInfo) []string {
	info, found := availableModelInfo(model, available)
	if found && !info.Disabled && !info.EffortUnsupported && !claude.ModelDisabled(model, available) {
		return info.SupportedEffortLevels
	}

	return nil
}

func reconcileEffortForModel(model string, available []claude.AvailableModelInfo, current string) (string, bool) {
	if current == "" {
		return "", false
	}

	levels := effortLevelsForModel(model, available)
	if len(levels) == 0 {
		return "", true
	}

	if slices.Contains(levels, current) {
		return current, false
	}

	for _, preferred := range []string{effortXHigh, effortHigh} {
		if slices.Contains(levels, preferred) {
			return preferred, true
		}
	}

	return levels[0], true
}

func effortDisplayName(effort string) string {
	switch effort {
	case effortLow:
		return "Low"
	case effortMedium:
		return "Medium"
	case effortHigh:
		return "High"
	case effortXHigh:
		return "Extra High"
	case effortMax:
		return "Max"
	default:
		return effort
	}
}

func modelDisplayName(info claude.AvailableModelInfo) string {
	if info.DisplayName != "" {
		return info.DisplayName
	}

	return info.Value
}

func stringPtrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

func modeAvailableForModel(
	mode acp.SessionModeId,
	model string,
	available []claude.AvailableModelInfo,
	bypassAvailable bool,
) bool {
	switch mode {
	case modeDefault, modePlan, modeAcceptEdits, modeDontAsk:
		return true
	case modeBypassPermissions:
		return bypassAvailable
	case modeAuto:
		return modelSupportsAutoMode(model, available)
	default:
		return false
	}
}

// bypassPermissionsAvailable mirrors Claude Code's own rule: a root process
// refuses to skip permissions unless the environment it runs in declares
// IS_SANDBOX. effectiveEnv is the environment the native process inherits.
func bypassPermissionsAvailable(effectiveEnv map[string]string) bool {
	return osGeteuid() != 0 || strings.TrimSpace(effectiveEnv[claude.EnvironmentKey("IS_SANDBOX")]) != ""
}

func modelSupportsAutoMode(model string, available []claude.AvailableModelInfo) bool {
	info, found := availableModelInfo(model, available)

	return found && !info.Disabled && !claude.ModelDisabled(model, available) && info.SupportsAutoMode
}

func acpModeForPermission(mode string) acp.SessionModeId {
	switch mode {
	case string(modePlan):
		return modePlan
	case permissionModeAcceptEdits:
		return modeAcceptEdits
	case permissionModeBypassPermissions:
		return modeBypassPermissions
	case string(modeAuto):
		return modeAuto
	case permissionModeDontAsk:
		return modeDontAsk
	default:
		return modeDefault
	}
}

func selectPositionEncoding(encodings []acp.PositionEncodingKind) acp.PositionEncodingKind {
	if slices.Contains(encodings, acp.PositionEncodingKindUtf8) {
		return acp.PositionEncodingKindUtf8
	}

	if slices.Contains(encodings, acp.PositionEncodingKindUtf16) {
		return acp.PositionEncodingKindUtf16
	}

	return acp.PositionEncodingKindUtf16
}

func permissionModeForACP(mode acp.SessionModeId) (string, bool) {
	switch mode {
	case modeDefault:
		return string(modeDefault), true
	case modePlan:
		return string(modePlan), true
	case modeAcceptEdits:
		return permissionModeAcceptEdits, true
	case modeBypassPermissions:
		return permissionModeBypassPermissions, true
	case modeAuto:
		return "auto", true
	case modeDontAsk:
		return permissionModeDontAsk, true
	default:
		return "", false
	}
}
