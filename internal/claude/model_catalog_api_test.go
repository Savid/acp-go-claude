package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type modelCatalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f modelCatalogRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func modelCatalogTestAccess(endpoint string) ModelCatalogAccess {
	return ModelCatalogAccess{Endpoint: endpoint + "/v1/models", Credential: "test-credential"}
}

func TestModelCatalogAPIAuthenticationAndPagination(t *testing.T) {
	t.Parallel()
	for _, oauth := range []bool{false, true} {
		t.Run(fmt.Sprint(oauth), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/v1/models", r.URL.Path)
				require.Equal(t, "1000", r.URL.Query().Get("limit"))
				require.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))
				require.Equal(t, "acp-go-claude", r.Header.Get("User-Agent"))
				if oauth {
					require.Equal(t, "Bearer test-credential", r.Header.Get("Authorization"))
					require.Equal(t, "oauth-2025-04-20", r.Header.Get("anthropic-beta"))
					require.Empty(t, r.Header.Get("X-Api-Key"))
				} else {
					require.Equal(t, "test-credential", r.Header.Get("X-Api-Key"))
					require.Empty(t, r.Header.Get("Authorization"))
					require.Empty(t, r.Header.Get("anthropic-beta"))
				}
				if call == 1 {
					require.Empty(t, r.URL.Query().Get("after_id"))
					_, _ = io.WriteString(w, `{"data":[{"id":"first","display_name":"First model","type":"model","max_input_tokens":1000000,"max_tokens":64000,"capabilities":{"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":false},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true},"future":{"supported":true}}},"unknown":{"ignored":true}}],"has_more":true,"last_id":"first"}`)

					return
				}
				require.Equal(t, "first", r.URL.Query().Get("after_id"))
				_, _ = io.WriteString(w, `{"data":[{"id":"second","display_name":"Second model","type":"model","max_input_tokens":null,"max_tokens":null,"capabilities":null}],"has_more":false}`)
			}))
			defer server.Close()
			access := modelCatalogTestAccess(server.URL)
			access.OAuth = oauth
			models, err := readModelCatalogAPI(t.Context(), server.Client().Transport, access)
			require.NoError(t, err)
			require.Equal(t, []APIModel{
				{ID: "first", DisplayName: "First model", ContextWindow: 1000000, MaxOutputTokens: 64000, SupportedEffortLevels: []string{"low", "high", "xhigh", "max"}},
				{ID: "second", DisplayName: "Second model"},
			}, models)
			require.Equal(t, int32(2), calls.Load())
		})
	}
}

func TestModelCatalogAPIMalformedResponse(t *testing.T) {
	t.Parallel()
	valid := `{"id":"first","display_name":"First model","type":"model"}`
	page := func(record string) string { return `{"data":[` + record + `],"has_more":false}` }
	for name, body := range map[string]string{
		"missing data":          `{"has_more":false}`,
		"null data":             `{"data":null,"has_more":false}`,
		"wrong data type":       `{"data":{},"has_more":false}`,
		"missing has_more":      `{"data":[]}`,
		"null has_more":         `{"data":[],"has_more":null}`,
		"wrong has_more type":   `{"data":[],"has_more":"false"}`,
		"trailing value":        `{"data":[],"has_more":false}{}`,
		"missing id":            page(`{"display_name":"Model","type":"model"}`),
		"missing name":          page(`{"id":"first","type":"model"}`),
		"wrong type":            page(strings.Replace(valid, `"type":"model"`, `"type":"other"`, 1)),
		"id whitespace":         page(strings.Replace(valid, `"first"`, `"white space"`, 1)),
		"id unicode whitespace": page(strings.Replace(valid, `"first"`, `"white\u2003space"`, 1)),
		"name control":          page(strings.Replace(valid, `"First model"`, `"line\nbreak"`, 1)),
		"invalid utf8":          page(strings.Replace(valid, "First model", "bad\xff", 1)),
		"long id":               page(strings.Replace(valid, "first", strings.Repeat("x", modelCatalogMaxText+1), 1)),
		"long name":             page(strings.Replace(valid, "First model", strings.Repeat("x", modelCatalogMaxText+1), 1)),
		"negative input":        page(strings.TrimSuffix(valid, "}") + `,"max_input_tokens":-1}`),
		"negative output":       page(strings.TrimSuffix(valid, "}") + `,"max_tokens":-1}`),
		"fractional tokens":     page(strings.TrimSuffix(valid, "}") + `,"max_tokens":1.5}`),
		"overflow tokens":       page(strings.TrimSuffix(valid, "}") + `,"max_tokens":9223372036854775808}`),
		"string tokens":         page(strings.TrimSuffix(valid, "}") + `,"max_tokens":"1000"}`),
		"wrong capability type": page(strings.TrimSuffix(valid, "}") + `,"capabilities":{"effort":{"supported":true,"low":true}}}`),
		"wrong support type":    page(strings.TrimSuffix(valid, "}") + `,"capabilities":{"effort":{"supported":"true"}}}`),
		"body limit":            strings.Repeat(" ", modelCatalogPageBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := decodeModelCatalogPage([]byte(body))
			require.ErrorIs(t, err, errModelCatalogUnavailable)
			require.Empty(t, result.Data)
		})
	}
}

func TestModelCatalogAPIEffortRequiresExplicitSupport(t *testing.T) {
	t.Parallel()
	for _, capabilities := range []string{
		`null`, `{}`, `{"effort":null}`, `{"effort":{}}`,
		`{"effort":{"supported":false,"low":{"supported":true}}}`,
		`{"effort":{"low":{"supported":true}}}`,
		`{"effort":{"supported":true,"low":{},"medium":null,"high":{"supported":false},"future":{"supported":true}}}`,
	} {
		page, err := decodeModelCatalogPage([]byte(`{"data":[{"id":"first","display_name":"First","type":"model","capabilities":` + capabilities + `}],"has_more":false}`))
		require.NoError(t, err)
		require.Empty(t, page.Data[0].model().SupportedEffortLevels)
	}
}

func TestModelCatalogAPIPaginationRejectsPartialResults(t *testing.T) {
	t.Parallel()
	for name, pages := range map[string][]string{
		"missing cursor":                 {`{"data":[{"id":"a","display_name":"A","type":"model"}],"has_more":true}`},
		"empty continued page":           {`{"data":[],"has_more":true,"last_id":"a"}`},
		"cursor differs from last model": {`{"data":[{"id":"a","display_name":"A","type":"model"}],"has_more":true,"last_id":"b"}`},
		"malformed second page":          {modelCatalogTestPage("a", true), `{"data":[],"has_more":null}`},
		"duplicate model":                {modelCatalogTestPage("a", true), modelCatalogTestPage("a", false)},
		"cursor loop":                    {modelCatalogTestPage("a", true), modelCatalogTestPage("b", true), modelCatalogTestPage("a", true)},
		"too many pages":                 {modelCatalogTestPage("a", true), modelCatalogTestPage("b", true), modelCatalogTestPage("c", true), modelCatalogTestPage("d", true)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				index := int(calls.Add(1)) - 1
				require.Less(t, index, len(pages))
				_, _ = io.WriteString(w, pages[index])
			}))
			defer server.Close()
			models, err := readModelCatalogAPI(t.Context(), server.Client().Transport, modelCatalogTestAccess(server.URL))
			require.ErrorIs(t, err, errModelCatalogUnavailable)
			require.Nil(t, models)
			require.Equal(t, int32(len(pages)), calls.Load())
		})
	}
}

func TestModelCatalogAPIModelAndPageBounds(t *testing.T) {
	t.Parallel()
	for _, total := range []int{modelCatalogMaxModels, modelCatalogMaxModels + 1} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				index := int(calls.Add(1)) - 1
				first := index * (modelCatalogMaxModels / modelCatalogMaxPages)
				last := first + modelCatalogMaxModels/modelCatalogMaxPages
				if index == modelCatalogMaxPages-1 {
					last = total
				}
				rows := make([]map[string]any, 0, last-first)
				for i := first; i < last; i++ {
					rows = append(rows, map[string]any{"id": fmt.Sprint(i), "display_name": "Model", "type": "model"})
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": rows, "has_more": index < modelCatalogMaxPages-1, "last_id": fmt.Sprint(last - 1)}))
			}))
			defer server.Close()
			models, err := readModelCatalogAPI(t.Context(), server.Client().Transport, modelCatalogTestAccess(server.URL))
			if total > modelCatalogMaxModels {
				require.ErrorIs(t, err, errModelCatalogUnavailable)
				require.Nil(t, models)
			} else {
				require.NoError(t, err)
				require.Len(t, models, total)
			}
			require.Equal(t, int32(modelCatalogMaxPages), calls.Load())
		})
	}
}

func TestModelCatalogAPIErrorsAreSanitized(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "sensitive-provider-message test-credential")
			}))
			defer server.Close()
			models, err := readModelCatalogAPI(t.Context(), server.Client().Transport, modelCatalogTestAccess(server.URL))
			require.Error(t, err)
			require.Nil(t, models)
			require.NotContains(t, err.Error(), server.URL)
			require.NotContains(t, err.Error(), "test-credential")
			require.NotContains(t, err.Error(), "sensitive-provider-message")
			require.Equal(t, status == 401 || status == 403, errors.Is(err, ErrModelCatalogNotAuthenticated))
		})
	}
	transport := modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failure https://private.invalid/v1/models test-credential")
	})
	models, err := readModelCatalogAPI(t.Context(), transport, modelCatalogTestAccess("https://private.invalid"))
	require.Nil(t, models)
	require.EqualError(t, err, "model catalog unavailable")
}

func TestModelCatalogAPIRefusesRedirectsAndUnsafeAccess(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	models, err := readModelCatalogAPI(t.Context(), server.Client().Transport, modelCatalogTestAccess(server.URL))
	require.ErrorIs(t, err, errModelCatalogUnavailable)
	require.Nil(t, models)
	require.Zero(t, redirected.Load())

	transport := modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unsafe access reached transport")

		return nil, errModelCatalogUnavailable
	})
	for _, endpoint := range []string{
		"", "://bad", "file:///v1/models", "https:///v1/models", "https://user:secret@host/v1/models",
		"https://host/v1/models?key=secret", "https://host/v1/models?", "https://host/v1/models#secret",
		"https://host/v1/models#", "https://host/v1/%6dodels", "https://host/other", "https://host/v1/models\n",
	} {
		models, err = readModelCatalogAPI(t.Context(), transport, ModelCatalogAccess{Endpoint: endpoint, Credential: "test-credential"})
		require.ErrorIs(t, err, errModelCatalogUnavailable)
		require.Nil(t, models)
	}
	for _, credential := range []string{"", "line\nbreak", "white space", strings.Repeat("x", (16<<10)+1)} {
		models, err = readModelCatalogAPI(context.Background(), transport, ModelCatalogAccess{Endpoint: "https://host/v1/models", Credential: credential})
		require.Error(t, err)
		require.Nil(t, models)
	}
}

func TestModelCatalogAPIBodyReadIsBounded(t *testing.T) {
	t.Parallel()
	var consumed atomic.Int64
	transport := modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: &modelCatalogCountingBody{consumed: &consumed}}, nil
	})
	models, err := readModelCatalogAPI(t.Context(), transport, modelCatalogTestAccess("https://host"))
	require.ErrorIs(t, err, errModelCatalogUnavailable)
	require.Nil(t, models)
	require.Equal(t, int64(modelCatalogPageBytes+1), consumed.Load())
}

type modelCatalogCountingBody struct {
	consumed *atomic.Int64
}

func (b *modelCatalogCountingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	b.consumed.Add(int64(len(p)))

	return len(p), nil
}

func (*modelCatalogCountingBody) Close() error { return nil }

func modelCatalogTestPage(id string, more bool) string {
	return fmt.Sprintf(`{"data":[{"id":%q,"display_name":"Model","type":"model","capabilities":{"effort":{"supported":true,"high":{"supported":true}}}}],"has_more":%t,"last_id":%q}`, id, more, id)
}
