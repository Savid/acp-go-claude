package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const rateLimitsProbeMaxRequest = 64 << 10

type rateLimitsProbeResult struct {
	value RateLimits
	err   error
}

// rateLimitsBridge admits one native request to one captured upstream endpoint.
// The native process supplies its own authentication/request format; generated
// content never leaves the bridge or becomes an ACP prompt response.
type rateLimitsBridge struct {
	access    rateLimitsAPIAccess
	baseURL   string
	route     string
	server    *http.Server
	client    *http.Client
	transport *http.Transport
	result    chan rateLimitsProbeResult
	served    chan struct{}
	finished  chan struct{}
	admitMu   sync.Mutex
	admitted  bool
	closed    bool
	closeOnce sync.Once
}

func newRateLimitsBridge(ctx context.Context, access rateLimitsAPIAccess) (*rateLimitsBridge, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("create quota probe route")
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("listen for quota probe")
	}

	transport := &http.Transport{}
	bridge := &rateLimitsBridge{
		access: access, route: "/" + hex.EncodeToString(nonce[:]),
		transport: transport, result: make(chan rateLimitsProbeResult, 1),
		served: make(chan struct{}), finished: make(chan struct{}),
		client: &http.Client{Transport: transport, Timeout: 12 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	bridge.baseURL = "http://" + listener.Addr().String() + bridge.route
	bridge.server = &http.Server{
		Handler: bridge, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 12 * time.Second,
		WriteTimeout: 12 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	go func() {
		defer close(bridge.served)

		_ = bridge.server.Serve(listener)
	}()

	return bridge, nil
}

func (b *rateLimitsBridge) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != b.route+"/v1/messages" ||
		(request.URL.RawQuery != "" && request.URL.RawQuery != "beta=true") ||
		subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte("Bearer "+b.access.token)) != 1 {
		http.Error(w, "quota probe refused", http.StatusForbidden)

		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, rateLimitsProbeMaxRequest+1))
	if err != nil || len(body) > rateLimitsProbeMaxRequest || !validRateLimitsProbeBody(body, b.access.fableModel) {
		http.Error(w, "quota probe refused", http.StatusBadRequest)

		return
	}

	b.admitMu.Lock()
	if b.closed || b.admitted {
		b.admitMu.Unlock()
		http.Error(w, "quota probe already complete", http.StatusConflict)

		return
	}

	b.admitted = true
	b.admitMu.Unlock()

	defer close(b.finished)

	value, err := b.forward(request, body)
	b.result <- rateLimitsProbeResult{value: value, err: err}
	// No response content is needed for a quota read. End the native turn with
	// a fixed local error; any native recovery request is refused above.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"quota probe complete"}}`)
}

func validRateLimitsProbeBody(body []byte, model string) bool {
	var request struct {
		Model     string            `json:"model"`
		MaxTokens int               `json:"max_tokens"` //nolint:tagliatelle // Native Messages API field.
		Tools     []json.RawMessage `json:"tools"`
	}

	return json.Unmarshal(body, &request) == nil && request.Model == model &&
		request.MaxTokens == 1 && len(request.Tools) == 0
}

func (b *rateLimitsBridge) forward(native *http.Request, body []byte) (RateLimits, error) {
	endpoint := b.access.endpoint
	if native.URL.RawQuery == "beta=true" {
		endpoint += "?beta=true"
	}

	request, err := http.NewRequestWithContext(native.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RateLimits{}, errors.New("construct native quota probe")
	}

	for key, values := range native.Header {
		if rateLimitsForwardHeader(key) {
			request.Header[key] = append([]string(nil), values...)
		}
	}

	response, err := b.client.Do(request)
	if err != nil {
		return RateLimits{}, errors.New("native quota probe failed")
	}
	defer response.Body.Close()

	observedAt := time.Now().UTC()

	if response.StatusCode == http.StatusUnauthorized {
		return RateLimits{}, ErrRateLimitsNotAuthenticated
	}

	read, err := io.Copy(io.Discard, io.LimitReader(response.Body, rateLimitsMaxBody+1))
	if err != nil || read > rateLimitsMaxBody {
		return RateLimits{}, errors.New("native quota probe response unreadable")
	}

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusTooManyRequests {
		return RateLimits{}, errors.New("native quota probe rejected")
	}

	return parseRateLimitsHeaders(response.Header, observedAt), nil
}

func rateLimitsForwardHeader(key string) bool {
	key = strings.ToLower(key)

	return key == "authorization" || key == "content-type" || key == "user-agent" || key == "x-app" ||
		strings.HasPrefix(key, "anthropic-") || strings.HasPrefix(key, "x-anthropic-") || strings.HasPrefix(key, "x-stainless-")
}

func (b *rateLimitsBridge) close() {
	b.closeOnce.Do(func() {
		b.admitMu.Lock()
		b.closed = true
		admitted := b.admitted
		b.admitMu.Unlock()
		_ = b.server.Close()
		<-b.served

		if admitted {
			<-b.finished
		}

		b.transport.CloseIdleConnections()
	})
}
