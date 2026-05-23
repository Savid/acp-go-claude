package claudeacp

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// SetSessionMode maps ACP modes to Claude permission modes.
func (a *Agent) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}

	mode, ok := permissionModeForACP(params.ModeId)
	if !ok {
		return acp.SetSessionModeResponse{}, acp.NewInvalidParams(map[string]any{"modeId": params.ModeId})
	}

	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	defer releaseTurn()

	_, model, available := session.modeInfo()
	if !modeAvailableForModel(params.ModeId, model, available) {
		return acp.SetSessionModeResponse{}, acp.NewInvalidParams(map[string]any{"modeId": params.ModeId})
	}

	if err := session.client.SetPermissionMode(ctx, mode); err != nil {
		return acp.SetSessionModeResponse{}, err
	}

	session.setMode(params.ModeId)
	options := sessionConfigOptions(session)

	if err := session.emitOptionalUpdates(ctx, []acp.SessionUpdate{
		{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: params.ModeId}},
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}},
	}); err != nil {
		return acp.SetSessionModeResponse{}, err
	}

	return acp.SetSessionModeResponse{}, nil
}

// SetSessionConfigOption handles supported configuration changes.
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	switch {
	case params.ValueId != nil:
		return a.setSessionConfigValue(ctx, params.ValueId)
	case params.Boolean != nil:
		return a.setSessionConfigBoolean(ctx, params.Boolean)
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

	modeChanged := false
	nextMode := acp.SessionModeId("")

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

			nextMode = mode
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

		modeChanged = true
		nextMode = mode
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

	if modeChanged {
		updates = append(updates, acp.SessionUpdate{
			CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: nextMode},
		})
	}

	if err := session.emitOptionalUpdates(ctx, updates); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func (a *Agent) setSessionConfigBoolean(
	ctx context.Context,
	params *acp.SetSessionConfigOptionBoolean,
) (acp.SetSessionConfigOptionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	if params.ConfigId != configFastMode {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{acpFieldConfig: "unsupported option"})
	}

	releaseTurn, err := session.acquireTurn(ctx)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	defer releaseTurn()

	if err := session.client.SetFastMode(ctx, params.Value); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	session.setFastMode(params.Value)

	options := sessionConfigOptions(session)
	if err := session.emitOptionalUpdates(ctx, []acp.SessionUpdate{
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}},
	}); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func sessionConfigOptions(session *Session) []acp.SessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return configOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown)
}

func sessionUnstableConfigOptions(session *Session) []acp.UnstableSessionConfigOption {
	mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown := session.configInfo()

	return unstableConfigOptions(mode, model, available, outputStyle, outputStyles, effort, fastMode, fastModeKnown)
}

func sessionUnstableModelState(session *Session) *acp.UnstableSessionModelState {
	model, available := session.modelInfo()

	return unstableModelState(model, available)
}

func sessionModelState(session *Session) *acp.SessionModelState {
	model, available := session.modelInfo()

	return modelState(model, available)
}

func sessionModeState(session *Session) *acp.SessionModeState {
	mode, model, available := session.modeInfo()

	return modeStateForModel(mode, model, available)
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
		values := modeSelectOptions(mode, model, available)
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

	if fastModeKnown {
		options = append(options, acp.SessionConfigOption{
			Boolean: &acp.SessionConfigOptionBoolean{
				Id:           configFastMode,
				Name:         "Fast Mode",
				Type:         configTypeBoolean,
				Category:     configCategory(modelConfigCategory),
				CurrentValue: fastMode,
			},
		})
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
		values := modeSelectOptions(mode, model, available)
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

	if fastModeKnown {
		options = append(options, acp.UnstableSessionConfigOption{
			Boolean: &acp.UnstableSessionConfigOptionBoolean{
				Id:           configFastMode,
				Name:         "Fast Mode",
				Type:         configTypeBoolean,
				Category:     configCategory(modelConfigCategory),
				CurrentValue: fastMode,
			},
		})
	}

	return options
}

func modelState(model string, available []claude.AvailableModelInfo) *acp.SessionModelState {
	if model == "" {
		return nil
	}

	models := stableModelInfos(model, available)

	return &acp.SessionModelState{
		CurrentModelId:  acp.ModelId(model),
		AvailableModels: models,
	}
}

func unstableModelState(model string, available []claude.AvailableModelInfo) *acp.UnstableSessionModelState {
	if model == "" {
		return nil
	}

	models := unstableModelInfos(model, available)

	return &acp.UnstableSessionModelState{
		CurrentModelId:  acp.UnstableModelId(model),
		AvailableModels: models,
	}
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
	current acp.SessionModeId,
	model string,
	available []claude.AvailableModelInfo,
) acp.SessionConfigSelectOptionsUngrouped {
	state := modeStateForModel(current, model, available)
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(state.AvailableModes))

	for _, mode := range state.AvailableModes {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  mode.Name,
			Value: acp.SessionConfigValueId(mode.Id),
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

func stableModelInfos(model string, available []claude.AvailableModelInfo) []acp.ModelInfo {
	models := make([]acp.ModelInfo, 0, len(available)+1)
	seen := make(map[string]struct{}, len(available)+1)

	for _, info := range available {
		if info.Value == "" {
			continue
		}

		if _, ok := seen[info.Value]; ok {
			continue
		}

		models = append(models, acp.ModelInfo{
			ModelId:     acp.ModelId(info.Value),
			Name:        modelDisplayName(info),
			Description: stringPtrIfNotEmpty(info.Description),
			Meta:        claudeModelInfoMeta(info),
		})
		seen[info.Value] = struct{}{}
	}

	if _, ok := seen[model]; !ok {
		info := claude.AvailableModelInfo{Value: model, DisplayName: model}
		models = append(models, acp.ModelInfo{
			ModelId: acp.ModelId(model),
			Name:    model,
			Meta:    claudeModelInfoMeta(info),
		})
	}

	return models
}

func unstableModelInfos(model string, available []claude.AvailableModelInfo) []acp.UnstableModelInfo {
	models := make([]acp.UnstableModelInfo, 0, len(available)+1)
	seen := make(map[string]struct{}, len(available)+1)

	for _, info := range available {
		if info.Value == "" {
			continue
		}

		if _, ok := seen[info.Value]; ok {
			continue
		}

		models = append(models, acp.UnstableModelInfo{
			ModelId:     acp.UnstableModelId(info.Value),
			Name:        modelDisplayName(info),
			Description: stringPtrIfNotEmpty(info.Description),
			Meta:        claudeModelInfoMeta(info),
		})
		seen[info.Value] = struct{}{}
	}

	if _, ok := seen[model]; !ok {
		info := claude.AvailableModelInfo{Value: model, DisplayName: model}
		models = append(models, acp.UnstableModelInfo{
			ModelId: acp.UnstableModelId(model),
			Name:    model,
			Meta:    claudeModelInfoMeta(info),
		})
	}

	return models
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

func modeStateForModel(
	current acp.SessionModeId,
	model string,
	available []claude.AvailableModelInfo,
) *acp.SessionModeState {
	modes := []acp.SessionMode{
		{Id: modeDefault, Name: modeNameDefault},
		{Id: modePlan, Name: "Plan"},
		{Id: modeAcceptEdits, Name: "Accept Edits"},
	}
	if bypassPermissionsAvailable() {
		modes = append(modes, acp.SessionMode{Id: modeBypassPermissions, Name: "Bypass Permissions"})
	}

	if modelSupportsAutoMode(model, available) {
		modes = append(modes, acp.SessionMode{Id: modeAuto, Name: modeNameAuto})
	}

	modes = append(modes, acp.SessionMode{Id: modeDontAsk, Name: modeNameDontAsk})

	return &acp.SessionModeState{
		CurrentModeId:  current,
		AvailableModes: modes,
	}
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

func providerInfos() []acp.UnstableProviderInfo {
	return []acp.UnstableProviderInfo{
		{
			Id:       providerClaudeCode,
			Required: true,
			Current: &acp.UnstableProviderCurrentConfig{
				ApiType: acp.UnstableLlmProtocolAnthropic,
				BaseUrl: providerClaudeCodeURL,
			},
			Supported: []acp.UnstableLlmProtocol{acp.UnstableLlmProtocolAnthropic},
			Meta: map[string]any{
				jsonFieldTitle: providerClaudeCodeTitle,
			},
		},
	}
}

func nesCapabilities() *acp.NesCapabilities {
	return &acp.NesCapabilities{
		Events: &acp.NesEventCapabilities{
			Document: &acp.NesDocumentEventCapabilities{
				DidOpen:   &acp.NesDocumentDidOpenCapabilities{},
				DidChange: &acp.NesDocumentDidChangeCapabilities{SyncKind: acp.TextDocumentSyncKindFull},
				DidFocus:  &acp.NesDocumentDidFocusCapabilities{},
				DidSave:   &acp.NesDocumentDidSaveCapabilities{},
				DidClose:  &acp.NesDocumentDidCloseCapabilities{},
			},
		},
		Context: &acp.NesContextCapabilities{
			OpenFiles: &acp.NesOpenFilesCapabilities{},
		},
	}
}

func selectPositionEncoding(encodings []acp.PositionEncodingKind) acp.PositionEncodingKind {
	for _, encoding := range encodings {
		switch encoding {
		case acp.PositionEncodingKindUtf16, acp.PositionEncodingKindUtf32, acp.PositionEncodingKindUtf8:
			return encoding
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
