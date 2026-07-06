package claudeacp

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// SetSessionMode exists only because github.com/coder/acp-go-sdk's generated
// Agent interface still requires it. Remove this when the upstream SDK drops
// session/set_mode; the local ACP dispatcher intentionally does not route it.
func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// SetSessionConfigOption handles supported configuration changes.
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	switch {
	case params.ValueId != nil:
		return a.setSessionConfigValue(ctx, params.ValueId)
	case params.Boolean != nil:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{
			jsonFieldError: validationUnsupported,
			jsonFieldField: "boolean",
		})
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{acpFieldConfig: validationRequired})
	}
}

func (a *Agent) setSessionConfigValue(
	ctx context.Context,
	params *acp.SetSessionConfigOptionValueId,
) (acp.SetSessionConfigOptionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	if poisonErr := session.poisonedError(); poisonErr != nil {
		return acp.SetSessionConfigOptionResponse{}, poisonErr
	}

	switch params.ConfigId {
	case configModel, configMode, configOutputStyle, configEffort:
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{acpFieldConfig: "unsupported option"})
	}

	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	defer releaseTurn()

	if poisonErr := session.poisonedError(); poisonErr != nil {
		return acp.SetSessionConfigOptionResponse{}, poisonErr
	}

	switch params.ConfigId {
	case configModel:
		model, cliModel := session.modelSelection(string(params.Value))
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
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{acpFieldConfig: "unsupported mode"})
		}

		_, model, available := session.modeInfo()
		if !modeAvailableForModel(mode, model, available) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{acpFieldConfig: "unavailable mode"})
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

	if err := session.emitOptionalUpdates(ctx, updates); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func sessionConfigOptions(session *agentSession) []acp.SessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return configOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown)
}

func sessionUnstableConfigOptions(session *agentSession) []acp.UnstableSessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return unstableConfigOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown)
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

	if len(available) > 0 {
		return initialModelSelection{
			Model:       available[0].Value,
			ShouldApply: available[0].Value != defaultModel,
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
) []acp.SessionConfigOption {
	var options []acp.SessionConfigOption

	if model != "" {
		values := configSelectOptions(model, available)
		if len(values) > 0 {
			options = append(options, acp.SessionConfigOption{
				Select: &acp.SessionConfigOptionSelect{
					Id:           configModel,
					Name:         "Model",
					Category:     configCategory(acp.SessionConfigOptionCategoryModel),
					CurrentValue: acp.SessionConfigValueId(model),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	if mode != "" {
		values := modeSelectOptions(model, available)
		options = append(options, acp.SessionConfigOption{
			Select: &acp.SessionConfigOptionSelect{
				Id:           configMode,
				Name:         "Mode",
				Category:     configCategory(acp.SessionConfigOptionCategoryMode),
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
				Category:     configCategory(modelConfigCategory),
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
					Category:     configCategory(acp.SessionConfigOptionCategoryThoughtLevel),
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
					Category:     configCategory(acp.SessionConfigOptionCategoryModel),
					CurrentValue: acp.SessionConfigValueId(model),
					Options: acp.SessionConfigSelectOptions{
						Ungrouped: &values,
					},
				},
			})
		}
	}

	if mode != "" {
		values := modeSelectOptions(model, available)
		options = append(options, acp.UnstableSessionConfigOption{
			Select: &acp.UnstableSessionConfigOptionSelect{
				Id:           configMode,
				Name:         "Mode",
				Type:         configTypeSelect,
				Category:     configCategory(acp.SessionConfigOptionCategoryMode),
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
				Category:     configCategory(modelConfigCategory),
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
					Category:     configCategory(acp.SessionConfigOptionCategoryThoughtLevel),
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
		if info.Value == "" {
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

	if _, ok := seen[model]; !ok {
		info := claude.AvailableModelInfo{Value: model, DisplayName: model}
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
) acp.SessionConfigSelectOptionsUngrouped {
	choices := []struct {
		id   acp.SessionModeId
		name string
	}{
		{id: modeDefault, name: modeNameDefault},
		{id: modePlan, name: "Plan"},
		{id: modeAcceptEdits, name: "Accept Edits"},
	}

	if bypassPermissionsAvailable() {
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
	for _, info := range available {
		if info.Value == model && len(info.SupportedEffortLevels) > 0 {
			return append([]string(nil), info.SupportedEffortLevels...)
		}
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
) bool {
	switch mode {
	case modeDefault, modePlan, modeAcceptEdits, modeDontAsk:
		return true
	case modeBypassPermissions:
		return bypassPermissionsAvailable()
	case modeAuto:
		return modelSupportsAutoMode(model, available)
	default:
		return false
	}
}

func bypassPermissionsAvailable() bool {
	if osGeteuid() != 0 {
		return true
	}

	return strings.TrimSpace(os.Getenv("IS_SANDBOX")) != ""
}

func modelSupportsAutoMode(model string, available []claude.AvailableModelInfo) bool {
	for _, info := range available {
		if info.Value == model {
			return info.SupportsAutoMode
		}
	}

	return false
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
	for _, encoding := range encodings {
		if encoding == acp.PositionEncodingKindUtf8 {
			return acp.PositionEncodingKindUtf8
		}
	}

	for _, encoding := range encodings {
		if encoding == acp.PositionEncodingKindUtf16 {
			return acp.PositionEncodingKindUtf16
		}
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
