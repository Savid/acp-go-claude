package claude

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A probe remains attached to its client until native settlement, reclaim, and
// removal all succeed. Close retries retained owners after a failed read.
type rateLimitsNativeProbe struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	root      string
	options   Options
	bridge    *rateLimitsBridge
	transport *ProcessTransport
	prepared  bool
	ambiguous error
}

func (c *Client) readRateLimitsNativeProbe(ctx context.Context, access rateLimitsAPIAccess) (_ RateLimits, returnErr error) {
	select {
	case c.rateLimitsProbeGate <- struct{}{}:
		defer func() { <-c.rateLimitsProbeGate }()
	case <-ctx.Done():
		return RateLimits{}, ctx.Err()
	}

	if err := ctx.Err(); err != nil {
		return RateLimits{}, err
	}

	parent := c.options.ScratchParent
	if parent == "" {
		parent = c.options.Cwd
	}

	if !filepath.IsAbs(parent) || (c.options.Authority != nil && rateLimitsPreparedParent(parent, c.options.ClaudeHome)) {
		return RateLimits{}, nil
	}

	source, ok := c.transport.(interface{ rateLimitsEnvironment() map[string]string })
	if !ok {
		return RateLimits{}, nil
	}

	environment := source.rateLimitsEnvironment()

	current, eligible := resolveRateLimitsAPIAccess(environment)
	if !eligible || current != access {
		return RateLimits{}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	root, err := os.MkdirTemp(parent, ".acp-quota-")
	if err != nil {
		return RateLimits{}, errors.New("create native quota probe directory")
	}

	probe := &rateLimitsNativeProbe{root: root, cancel: cancel, options: Options{
		CLIPath: c.options.CLIPath, Authority: c.options.Authority,
		ContainmentIncomplete: c.options.ContainmentIncomplete, TreePrepared: true,
		Cwd: filepath.Join(root, "work"), ClaudeHome: filepath.Join(root, "config"),
	}}
	if !c.addRateLimitsProbe(probe) {
		_ = os.RemoveAll(root)

		return RateLimits{}, ErrClientClosed
	}
	defer func() {
		closeErr := probe.close()
		if closeErr == nil {
			c.forgetRateLimitsProbe(probe)
		}

		returnErr = errors.Join(returnErr, closeErr)
	}()

	events, err := probe.start(probeCtx, c, access, environment)
	if err != nil {
		return RateLimits{}, err
	}

	for {
		select {
		case observed := <-probe.bridge.result:
			return observed.value, observed.err
		case event, open := <-events:
			if !open || event.Err != nil {
				select {
				case observed := <-probe.bridge.result:
					return observed.value, observed.err
				default:
					if event.Err != nil {
						return RateLimits{}, event.Err
					}

					return RateLimits{}, errors.New("native quota probe ended without headers")
				}
			}
		case <-probeCtx.Done():
			return RateLimits{}, probeCtx.Err()
		}
	}
}

func rateLimitsPreparedParent(parent, nativeHome string) bool {
	if nativeHome == "" {
		return false
	}

	relative, err := filepath.Rel(nativeHome, parent)

	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (p *rateLimitsNativeProbe) start(ctx context.Context, owner *Client, access rateLimitsAPIAccess, environment map[string]string) (<-chan TransportEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, name := range []string{"work", "config", "home", "cache", "data", "state", "tmp"} {
		if err := os.Mkdir(filepath.Join(p.root, name), 0o700); err != nil {
			return nil, errors.New("initialize native quota probe directory")
		}
	}

	bridge, err := newRateLimitsBridge(ctx, access)
	if err != nil {
		return nil, err
	}

	p.bridge = bridge

	p.options.PreparedEnvironment = rateLimitsProbeEnvironment(environment, p.root, bridge.baseURL)
	if authority := p.options.Authority; authority != nil {
		if authority.PrepareNativeTree == nil {
			return nil, authorityUnavailable(authority)
		}

		if err := authority.PrepareNativeTree(ctx, p.root); err != nil {
			p.ambiguous = containmentIncomplete(p.options, "prepare native quota probe tree", err)

			return nil, p.ambiguous
		}

		p.prepared = true
	}

	p.transport = NewProcessTransport(owner.log, p.options)
	if err := p.transport.startWithArgs(ctx, rateLimitsNativeProbeArgs(access.fableModel)); err != nil {
		if rateLimitsContainmentError(p.options, err) {
			p.ambiguous = err
		}

		return nil, err
	}

	events := p.transport.Events(context.WithoutCancel(ctx))
	if err := p.transport.closeStdin(); err != nil {
		return events, err
	}

	return events, nil
}

func rateLimitsNativeProbeArgs(model string) []string {
	return []string{
		"--print", "quota", "--model", model, cliArgOutputFormat, streamJSON, cliArgVerbose,
		"--max-turns", "1", "--max-budget-usd", "0.000000001", "--no-session-persistence",
		"--safe-mode", "--tools", "", "--thinking", "disabled", "--disable-slash-commands",
		"--setting-sources=", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--system-prompt", "Reply with one character.",
	}
}

func rateLimitsProbeEnvironment(environment map[string]string, root, baseURL string) []string {
	values := maps.Clone(environment)
	for key, value := range map[string]string{
		envClaudeConfigDir: filepath.Join(root, "config"), "PWD": filepath.Join(root, "work"),
		envHome: filepath.Join(root, "home"), "USERPROFILE": filepath.Join(root, "home"),
		envXDGConfigHome: filepath.Join(root, "config"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state"),
		"TMPDIR": filepath.Join(root, "tmp"), "TEMP": filepath.Join(root, "tmp"), "TMP": filepath.Join(root, "tmp"),
		rateLimitsBaseURL: baseURL, "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "1", "CLAUDE_CODE_MAX_RETRIES": "0",
		"CLAUDE_CODE_NO_MODEL_FALLBACK": "1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
		"CLAUDE_CODE_DISABLE_CRON": "1", "CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "1",
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "NO_PROXY": "127.0.0.1,localhost",
		"http_proxy": "", "https_proxy": "", "all_proxy": "", "no_proxy": "127.0.0.1,localhost",
	} {
		values[EnvironmentKey(key)] = value
	}

	for _, key := range []string{"CLAUDE_CODE_EXTRA_BODY", "CLAUDE_CODE_EXTRA_METADATA", "ANTHROPIC_UNIX_SOCKET", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR"} {
		delete(values, EnvironmentKey(key))
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	result := make([]string, 0, len(values))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}

	return result
}

func (p *rateLimitsNativeProbe) close() error {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.bridge != nil {
		p.bridge.close()
	}

	if p.ambiguous != nil {
		return p.ambiguous
	}

	if p.transport != nil {
		if err := p.transport.Close(); err != nil {
			return containmentIncomplete(p.options, "close native quota probe", err)
		}
	}

	if p.prepared {
		if err := reclaimNativeTree(p.options.Authority, p.root); err != nil {
			return containmentIncomplete(p.options, "reclaim native quota probe tree", err)
		}

		p.prepared = false
	}

	if err := os.RemoveAll(p.root); err != nil {
		return containmentIncomplete(p.options, "remove native quota probe tree", err)
	}

	return nil
}

func rateLimitsContainmentError(options Options, err error) bool {
	if err == nil {
		return false
	}

	if options.ContainmentIncomplete != nil && errors.Is(err, options.ContainmentIncomplete) {
		return true
	}

	return options.Authority != nil &&
		((options.Authority.Unavailable != nil && errors.Is(err, options.Authority.Unavailable)) ||
			(options.Authority.ContainmentIncomplete != nil && errors.Is(err, options.Authority.ContainmentIncomplete)))
}

func (c *Client) addRateLimitsProbe(probe *rateLimitsNativeProbe) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.closed {
		return false
	}

	if c.rateLimitsProbes == nil {
		c.rateLimitsProbes = make(map[*rateLimitsNativeProbe]struct{})
	}

	c.rateLimitsProbes[probe] = struct{}{}

	return true
}

func (c *Client) forgetRateLimitsProbe(probe *rateLimitsNativeProbe) {
	c.stateMu.Lock()
	delete(c.rateLimitsProbes, probe)
	c.stateMu.Unlock()
}

func (c *Client) closeRateLimitsProbes() error {
	c.stateMu.RLock()

	probes := make([]*rateLimitsNativeProbe, 0, len(c.rateLimitsProbes))
	for probe := range c.rateLimitsProbes {
		probes = append(probes, probe)
	}

	c.stateMu.RUnlock()

	var closeErr error

	for _, probe := range probes {
		err := probe.close()
		if err == nil {
			c.forgetRateLimitsProbe(probe)
		}

		closeErr = errors.Join(closeErr, err)
	}

	return closeErr
}
