package claudeacp

import (
	"fmt"
	"slices"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

// validateConfiguredModels refuses an id that could not name a model: empty,
// carrying surrounding space, or listed twice.
func validateConfiguredModels(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			return fmt.Errorf("configured model %d %q is not a model id", index, id)
		}

		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("configured model %q is listed twice", id)
		}

		seen[id] = struct{}{}
	}

	return nil
}

// appendHostListedModels publishes the ids the host listed explicitly after
// the native rows. A native row of the same identity stands and the host entry
// adds nothing, so a row Claude refused stays refused; a host row carries its
// id alone. Dispatch identity still applies: where Anthropic's own names are
// withheld, a host-listed Anthropic identity is withheld with them.
func appendHostListedModels(
	native []claude.AvailableModelInfo,
	hostListed []string,
	anthropicWithheld bool,
) []claude.AvailableModelInfo {
	if len(hostListed) == 0 {
		return native
	}

	seen := make(map[string]struct{}, len(native)+len(hostListed))
	for _, model := range native {
		seen[claude.NormalizeModelIdentity(model.Value)] = struct{}{}
	}

	models := slices.Clone(native)

	for _, id := range hostListed {
		row := claude.AvailableModelInfo{Value: id, DisplayName: id}

		identity := claude.NormalizeModelIdentity(id)
		if _, present := seen[identity]; present {
			continue
		}

		if anthropicWithheld && claude.AnthropicModelIdentity(row) {
			continue
		}

		seen[identity] = struct{}{}

		models = append(models, row)
	}

	return models
}
