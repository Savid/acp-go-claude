package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/savid/acp-go-core/process"
)

var ErrQuotaProcessActive = errors.New("quota probe process did not stop")

// ProbeQuota forwards at most one native model request and discards generated content.
func ProbeQuota(ctx context.Context, access QuotaAccess, executable string, env []string, scratch string, fable bool) (result QuotaResult, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	model := access.haiku
	if fable {
		model = access.fable
	}

	relay, err := newQuotaRelay(ctx, access, model, fable)
	if err != nil {
		return result, err
	}
	defer relay.close()

	owned := map[string]string{
		"HOME": scratch, "USERPROFILE": scratch, "CLAUDE_CONFIG_DIR": scratch,
		"XDG_CONFIG_HOME": scratch, "XDG_CACHE_HOME": scratch, "XDG_DATA_HOME": scratch, "XDG_STATE_HOME": scratch,
		"TMPDIR": scratch, "TMP": scratch, "TEMP": scratch,
		"ANTHROPIC_BASE_URL":            relay.baseURL,
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "1", "CLAUDE_CODE_MAX_RETRIES": "0",
		"CLAUDE_CODE_NO_MODEL_FALLBACK": "1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
		"CLAUDE_CODE_DISABLE_CRON": "1", "CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "1",
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "http_proxy": "", "https_proxy": "", "all_proxy": "",
		"NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost",
	}

	childEnv, err := (process.Environment{Process: env, Owned: owned}).Build()
	if err != nil {
		return result, err
	}

	args := []string{flagPrint, "quota", flagModel, model, flagOutputFormat, streamJSON, "--verbose",
		"--max-turns", "1", "--no-session-persistence", "--safe-mode", "--tools", "", "--thinking", "disabled",
		"--disable-slash-commands", "--setting-sources=", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		flagSystemPrompt, "Reply with one character."}

	proc, err := process.Start(ctx, process.Request{Executable: executable, Args: args, Env: childEnv, Dir: scratch})
	if err != nil {
		return result, errors.New("start quota probe")
	}

	_ = proc.Stdin().Close()
	drained := make(chan struct{})

	go func() { _, _ = io.Copy(io.Discard, proc.Stdout()); close(drained) }()

	defer func() {
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()

		if stopErr := proc.Shutdown(stopCtx, 100*time.Millisecond); stopErr != nil {
			returnErr = errors.Join(returnErr, ErrQuotaProcessActive, stopErr)
		}

		_ = proc.Close()

		<-drained
	}()

	select {
	case outcome := <-relay.result:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return result, ctx.Err()
	case <-proc.Done():
		select {
		case outcome := <-relay.result:
			return outcome.result, outcome.err
		default:
			return result, errQuotaProbe
		}
	}
}

type quotaProbeOutcome struct {
	result QuotaResult
	err    error
}

type quotaRelay struct {
	baseURL   string
	server    *http.Server
	transport *http.Transport
	done      chan struct{}
	result    chan quotaProbeOutcome
	cancel    context.CancelFunc
}

func newQuotaRelay(ctx context.Context, access QuotaAccess, model string, fable bool) (*quotaRelay, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	path := "/" + rand.Text()
	transport := &http.Transport{}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	relay := &quotaRelay{cancel: cancel, baseURL: "http://" + listener.Addr().String() + path, transport: transport, done: make(chan struct{}), result: make(chan quotaProbeOutcome, 1)}

	var admitted atomic.Bool

	relay.server = &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path+"/v1/messages" || (r.URL.RawQuery != "" && r.URL.RawQuery != "beta=true") ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+access.token)) != 1 {
			http.Error(w, "invalid quota request", http.StatusForbidden)

			return
		}

		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))

		var request struct {
			Model     string            `json:"model"`
			MaxTokens int               `json:"max_tokens"` //nolint:tagliatelle // Native Messages API member.
			Tools     []json.RawMessage `json:"tools"`
		}
		if readErr != nil || json.Unmarshal(body, &request) != nil || request.Model != model || request.MaxTokens != 1 || len(request.Tools) != 0 || !admitted.CompareAndSwap(false, true) {
			http.Error(w, "quota request refused", http.StatusForbidden)

			return
		}

		outcome := forwardQuota(ctx, client, access.endpoint, r.Header, body, fable)
		relay.result <- outcome

		http.Error(w, "quota probe complete", http.StatusServiceUnavailable)
	})}
	go func() { defer close(relay.done); _ = relay.server.Serve(listener) }()

	return relay, nil
}

func (r *quotaRelay) close() {
	r.cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if r.server.Shutdown(ctx) != nil {
		_ = r.server.Close()
	}

	<-r.done
	r.transport.CloseIdleConnections()
}

func forwardQuota(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte, fable bool) quotaProbeOutcome {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return quotaProbeOutcome{err: errQuotaProbe}
	}

	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "content-type" || lower == "user-agent" || lower == "x-app" || strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-anthropic-") || strings.HasPrefix(lower, "x-stainless-") {
			request.Header[key] = values
		}
	}

	response, err := client.Do(request)
	if err != nil {
		return quotaProbeOutcome{err: errQuotaProbe}
	}
	defer response.Body.Close()

	now := time.Now().UTC()

	result := QuotaResult{}
	if seconds, parseErr := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 32); parseErr == nil && seconds > 0 {
		result.RetryAfter = now.Add(time.Duration(seconds) * time.Second)
	} else if at, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil {
		result.RetryAfter = at
	}

	switch response.StatusCode {
	case http.StatusUnauthorized:
		result.NotAuthenticated = true
	case http.StatusForbidden, http.StatusNotFound:
		result.Unavailable = true
	case http.StatusOK, http.StatusTooManyRequests:
		result.Windows = quotaHeaders(response.Header, fable, now)
		if response.StatusCode == http.StatusTooManyRequests || response.Header.Get("anthropic-ratelimit-unified-status") == quotaRejected {
			result.Exhausted = quotaEventID(response.Header.Get("anthropic-ratelimit-unified-representative-claim"), fable)
		}

		if len(result.Windows) == 0 {
			return quotaProbeOutcome{result: result, err: errQuotaProbe}
		}
	default:
		return quotaProbeOutcome{result: result, err: errQuotaProbe}
	}

	return quotaProbeOutcome{result: result}
}
