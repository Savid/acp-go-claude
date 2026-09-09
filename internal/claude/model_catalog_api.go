package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	modelCatalogMaxPages  = 4
	modelCatalogMaxModels = 4096
	modelCatalogPageBytes = 1 << 20
	modelCatalogMaxBytes  = 4 << 20
	modelCatalogMaxText   = 256
	modelCatalogLow       = "low"
	modelCatalogMedium    = "medium"
	modelCatalogHigh      = "high"
	modelCatalogXHigh     = "xhigh"
	modelCatalogMax       = "max"
)

var (
	// ErrModelCatalogNotAuthenticated means the Models API rejected the captured credential.
	ErrModelCatalogNotAuthenticated = errors.New("model catalog is not authenticated")
	errModelCatalogUnavailable      = errors.New("model catalog unavailable")
	errModelCatalogTransient        = errors.New("model catalog unavailable")
)

// ModelCatalogAccess is one captured native authentication and routing identity.
// The caller must establish first-party eligibility before supplying the endpoint.
type ModelCatalogAccess struct {
	Endpoint   string
	Credential string
	OAuth      bool
}

// APIModel contains the Models API facts used to enrich a native model catalog.
type APIModel struct {
	ID                    string
	DisplayName           string
	ContextWindow         int64
	MaxOutputTokens       int64
	SupportedEffortLevels []string
}

//nolint:tagliatelle // Models API response fields use snake_case.
type modelCatalogPage struct {
	Data    []modelCatalogRecord `json:"data"`
	HasMore *bool                `json:"has_more"`
	LastID  string               `json:"last_id"`
}

//nolint:tagliatelle // Models API response fields use snake_case.
type modelCatalogRecord struct {
	ID             string                    `json:"id"`
	DisplayName    string                    `json:"display_name"`
	Type           string                    `json:"type"`
	MaxInputTokens int64                     `json:"max_input_tokens"`
	MaxTokens      int64                     `json:"max_tokens"`
	Capabilities   *modelCatalogCapabilities `json:"capabilities"`
}

type modelCatalogCapabilities struct {
	Effort *modelCatalogEffort `json:"effort"`
}

type modelCatalogSupport struct {
	Supported bool `json:"supported"`
}

type modelCatalogEffort struct {
	Supported bool                 `json:"supported"`
	Low       *modelCatalogSupport `json:"low"`
	Medium    *modelCatalogSupport `json:"medium"`
	High      *modelCatalogSupport `json:"high"`
	XHigh     *modelCatalogSupport `json:"xhigh"`
	Max       *modelCatalogSupport `json:"max"`
}

func modelCatalogURL(access ModelCatalogAccess) (*url.URL, error) {
	if access.Credential == "" {
		return nil, ErrModelCatalogNotAuthenticated
	}

	if len(access.Endpoint) > 2048 || len(access.Credential) > 16<<10 ||
		strings.ContainsFunc(access.Credential, func(r rune) bool { return r < '!' || r > '~' }) {
		return nil, errModelCatalogUnavailable
	}

	endpoint, err := url.Parse(access.Endpoint)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != authLoginURLScheme && endpoint.Scheme != "http") ||
		endpoint.Opaque != "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || strings.Contains(access.Endpoint, "#") ||
		endpoint.Path != "/v1/models" || endpoint.RawPath != "" {
		return nil, errModelCatalogUnavailable
	}

	return endpoint, nil
}

func readModelCatalogAPI(ctx context.Context, transport http.RoundTripper, access ModelCatalogAccess) ([]APIModel, error) {
	endpoint, err := modelCatalogURL(access)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	models := make([]APIModel, 0)
	seen := make(map[string]struct{})
	cursors := make(map[string]struct{})
	cursor, totalBytes := "", 0

	for range modelCatalogMaxPages {
		page, size, pageErr := readModelCatalogPage(ctx, client, endpoint, access, cursor)
		if pageErr != nil {
			return nil, pageErr
		}

		totalBytes += size
		if totalBytes > modelCatalogMaxBytes || len(models)+len(page.Data) > modelCatalogMaxModels {
			return nil, errModelCatalogUnavailable
		}

		for _, record := range page.Data {
			if _, exists := seen[record.ID]; exists {
				return nil, errModelCatalogUnavailable
			}

			seen[record.ID] = struct{}{}
			models = append(models, record.model())
		}

		if !*page.HasMore {
			return models, nil
		}

		if !validModelCatalogCursor(page, cursors) {
			return nil, errModelCatalogUnavailable
		}

		cursor = page.LastID
		cursors[cursor] = struct{}{}
	}

	return nil, errModelCatalogUnavailable
}

func readModelCatalogPage(ctx context.Context, client *http.Client, endpoint *url.URL, access ModelCatalogAccess, cursor string) (modelCatalogPage, int, error) {
	requestURL := *endpoint

	query := url.Values{"limit": {"1000"}}
	if cursor != "" {
		query.Set("after_id", cursor)
	}

	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), http.NoBody)
	if err != nil {
		return modelCatalogPage{}, 0, errModelCatalogUnavailable
	}

	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("User-Agent", "acp-go-claude")

	if access.OAuth {
		request.Header.Set("Authorization", "Bearer "+access.Credential)
		request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else {
		request.Header.Set("X-Api-Key", access.Credential)
	}

	response, err := client.Do(request)
	if err != nil {
		return modelCatalogPage{}, 0, errModelCatalogTransient
	}
	defer response.Body.Close()

	if statusErr := modelCatalogStatusError(response.StatusCode); statusErr != nil {
		return modelCatalogPage{}, 0, statusErr
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, modelCatalogPageBytes+1))
	if err != nil {
		return modelCatalogPage{}, 0, errModelCatalogTransient
	}

	page, err := decodeModelCatalogPage(body)

	return page, len(body), err
}

func modelCatalogStatusError(status int) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrModelCatalogNotAuthenticated
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return errModelCatalogTransient
	default:
		return errModelCatalogUnavailable
	}
}

func decodeModelCatalogPage(body []byte) (modelCatalogPage, error) {
	var page modelCatalogPage
	if len(body) > modelCatalogPageBytes || !utf8.Valid(body) || json.Unmarshal(body, &page) != nil ||
		page.Data == nil || page.HasMore == nil || len(page.Data) > modelCatalogMaxModels {
		return modelCatalogPage{}, errModelCatalogUnavailable
	}

	for _, model := range page.Data {
		if model.Type != keyModel || !validModelCatalogText(model.ID, true) ||
			!validModelCatalogText(model.DisplayName, false) || model.MaxInputTokens < 0 || model.MaxTokens < 0 {
			return modelCatalogPage{}, errModelCatalogUnavailable
		}
	}

	return page, nil
}

func validModelCatalogText(value string, identifier bool) bool {
	if value == "" || len(value) > modelCatalogMaxText || !utf8.ValidString(value) {
		return false
	}

	return !strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || (identifier && unicode.IsSpace(r))
	})
}

func validModelCatalogCursor(page modelCatalogPage, seen map[string]struct{}) bool {
	if len(page.Data) == 0 || !validModelCatalogText(page.LastID, true) || page.LastID != page.Data[len(page.Data)-1].ID {
		return false
	}

	_, exists := seen[page.LastID]

	return !exists
}

func (r modelCatalogRecord) model() APIModel {
	result := APIModel{
		ID: r.ID, DisplayName: r.DisplayName,
		ContextWindow: r.MaxInputTokens, MaxOutputTokens: r.MaxTokens,
	}
	if r.Capabilities == nil || r.Capabilities.Effort == nil || !r.Capabilities.Effort.Supported {
		return result
	}

	effort := r.Capabilities.Effort
	for _, level := range []struct {
		name    string
		support *modelCatalogSupport
	}{
		{modelCatalogLow, effort.Low}, {modelCatalogMedium, effort.Medium}, {modelCatalogHigh, effort.High},
		{modelCatalogXHigh, effort.XHigh}, {modelCatalogMax, effort.Max},
	} {
		if level.support != nil && level.support.Supported {
			result.SupportedEffortLevels = append(result.SupportedEffortLevels, level.name)
		}
	}

	return result
}
