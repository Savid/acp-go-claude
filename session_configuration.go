package claudeacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/coder/acp-go-sdk"
)

const (
	sessionConfigurationEntryType = "acp_session_configuration"
	sessionConfigurationVersion   = 1
)

type sessionConfiguration struct {
	Env           map[string]string
	ExtraPathDirs []string
}

type sessionConfigurationPresence struct {
	env           bool
	extraPathDirs bool
}

func configurationFromOptions(options ClaudeOptions) sessionConfiguration {
	configuration := sessionConfiguration{}
	if len(options.Env) > 0 {
		configuration.Env = cloneStringMap(options.Env)
	}

	if len(options.ExtraPathDirs) > 0 {
		configuration.ExtraPathDirs = slices.Clone(options.ExtraPathDirs)
	}

	return configuration
}

func configurationPresence(meta map[string]any) sessionConfigurationPresence {
	namespace, _ := meta[claudeMetaKey].(map[string]any)
	options, _ := namespace[metaOptionsKey].(map[string]any)
	_, env := options[settingsFieldEnv]
	_, extraPathDirs := options[metaExtraPathDirsKey]

	return sessionConfigurationPresence{env: env, extraPathDirs: extraPathDirs}
}

func inheritSessionConfiguration(
	options ClaudeOptions,
	presence sessionConfigurationPresence,
	stored sessionConfiguration,
) ClaudeOptions {
	if !presence.env {
		options.Env = nil
		if len(stored.Env) > 0 {
			options.Env = cloneStringMap(stored.Env)
		}
	}

	if !presence.extraPathDirs {
		options.ExtraPathDirs = nil
		if len(stored.ExtraPathDirs) > 0 {
			options.ExtraPathDirs = slices.Clone(stored.ExtraPathDirs)
		}
	}

	return options
}

func resumeSessionConfiguration(
	options ClaudeOptions,
	presence sessionConfigurationPresence,
	stored sessionConfiguration,
) (ClaudeOptions, error) {
	if presence.env && !maps.Equal(options.Env, stored.Env) {
		return ClaudeOptions{}, sessionResumeIncompatibleError(metaOptionPath(settingsFieldEnv))
	}

	if presence.extraPathDirs && !slices.Equal(options.ExtraPathDirs, stored.ExtraPathDirs) {
		return ClaudeOptions{}, sessionResumeIncompatibleError(metaOptionPath(metaExtraPathDirsKey))
	}

	return inheritSessionConfiguration(options, presence, stored), nil
}

func explicitCarrierChange(
	options ClaudeOptions,
	presence sessionConfigurationPresence,
	accepted sessionConfiguration,
) bool {
	return presence.env && !maps.Equal(options.Env, accepted.Env) ||
		presence.extraPathDirs && !slices.Equal(options.ExtraPathDirs, accepted.ExtraPathDirs)
}

func sessionResumeIncompatibleError(field string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: "session_resume_incompatible",
		jsonFieldField: field,
	})
}

func marshalSessionConfiguration(configuration sessionConfiguration) (SessionStoreEntry, error) {
	env := configuration.Env
	if env == nil {
		env = map[string]string{}
	}

	extraPathDirs := configuration.ExtraPathDirs
	if extraPathDirs == nil {
		extraPathDirs = []string{}
	}

	entry, err := json.Marshal(struct {
		Type          string            `json:"type"`
		Version       int               `json:"version"`
		Env           map[string]string `json:"env"`
		ExtraPathDirs []string          `json:"extraPathDirs"`
	}{
		Type:          sessionConfigurationEntryType,
		Version:       sessionConfigurationVersion,
		Env:           env,
		ExtraPathDirs: extraPathDirs,
	})
	if err != nil {
		return nil, fmt.Errorf("encode session configuration: %w", err)
	}

	return entry, nil
}

func unmarshalSessionConfiguration(entry SessionStoreEntry) (sessionConfiguration, error) {
	decoder := json.NewDecoder(bytes.NewReader(entry))

	opening, openErr := decoder.Token()
	if openErr != nil || opening != json.Delim('{') {
		return sessionConfiguration{}, errors.New("session configuration must be an object")
	}

	fields := make(map[string]json.RawMessage, 4)

	for decoder.More() {
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return sessionConfiguration{}, fmt.Errorf("decode session configuration field: %w", tokenErr)
		}

		name, ok := token.(string)
		if !ok {
			return sessionConfiguration{}, errors.New("session configuration field name must be a string")
		}

		switch name {
		case jsonFieldType, metaVersionKey, settingsFieldEnv, metaExtraPathDirsKey:
		default:
			return sessionConfiguration{}, fmt.Errorf("unknown session configuration field %q", name)
		}

		if _, duplicate := fields[name]; duplicate {
			return sessionConfiguration{}, fmt.Errorf("duplicate session configuration field %q", name)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return sessionConfiguration{}, fmt.Errorf("decode session configuration field %q: %w", name, err)
		}

		fields[name] = value
	}

	if _, closeErr := decoder.Token(); closeErr != nil {
		return sessionConfiguration{}, fmt.Errorf("close session configuration: %w", closeErr)
	}

	if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) {
		return sessionConfiguration{}, errors.New("session configuration carries trailing input")
	}

	var entryType string
	if err := json.Unmarshal(fields[jsonFieldType], &entryType); err != nil || entryType != sessionConfigurationEntryType {
		return sessionConfiguration{}, errors.New("session configuration type is invalid")
	}

	if !bytes.Equal(bytes.TrimSpace(fields[metaVersionKey]), []byte("1")) {
		return sessionConfiguration{}, errors.New("session configuration version is invalid")
	}

	var configuration sessionConfiguration

	decodedEnv, err := decodeSessionConfigurationEnv(fields[settingsFieldEnv])
	if err != nil {
		return sessionConfiguration{}, errors.New("session configuration env is invalid")
	}

	configuration.Env = decodedEnv

	if pathErr := json.Unmarshal(fields[metaExtraPathDirsKey], &configuration.ExtraPathDirs); pathErr != nil || configuration.ExtraPathDirs == nil {
		return sessionConfiguration{}, errors.New("session configuration extraPathDirs is invalid")
	}

	validated, err := validateClaudeOptions(ClaudeOptions{
		Env:           configuration.Env,
		ExtraPathDirs: configuration.ExtraPathDirs,
	})
	if err != nil {
		return sessionConfiguration{}, errors.New("session configuration values are invalid")
	}

	return configurationFromOptions(validated), nil
}

func decodeSessionConfigurationEnv(raw json.RawMessage) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	opening, openErr := decoder.Token()
	if openErr != nil || opening != json.Delim('{') {
		return nil, errors.New("environment must be an object")
	}

	environment := make(map[string]string)

	for decoder.More() {
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, tokenErr
		}

		name, ok := token.(string)
		if !ok {
			return nil, errors.New("environment key must be a string")
		}

		if _, duplicate := environment[name]; duplicate {
			return nil, fmt.Errorf("duplicate environment key %q", name)
		}

		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}

		environment[name] = value
	}

	if _, closeErr := decoder.Token(); closeErr != nil {
		return nil, closeErr
	}

	if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) {
		return nil, errors.New("environment carries trailing input")
	}

	return environment, nil
}
