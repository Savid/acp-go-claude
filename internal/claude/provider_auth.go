package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AuthLoginPrompt is the exact line the login child writes, without a trailing
// newline, when it is ready to accept the pasted value.
const AuthLoginPrompt = "Paste code here if prompted > "

// AuthLoginRedirectURI is the hosted callback the authorization URL must carry
// byte for byte. Anything else is a different flow than the one this adapter
// drives.
const AuthLoginRedirectURI = "https://platform.claude.com/oauth/code/callback"

// authLoginHost is the whole host allowlist for the authorization URL.
const authLoginHost = "claude.com"

// authLoginMaxURLBytes bounds the authorization URL before it is relayed.
const authLoginMaxURLBytes = 2048

var authLoginPresentationReader = ReadAuthLoginPresentation

// authRedirectQueryKey names the query parameter carrying the hosted callback.
const authRedirectQueryKey = "redirect_uri"

// authLoginURLScheme is the only scheme an authorization URL may carry.
const authLoginURLScheme = "https"
const authCommand = "auth"
const authStatus = "status"

const (
	envForceHyperlink = "FORCE_HYPERLINK"
	envCustomOAuthURL = "CLAUDE_CODE_CUSTOM_OAUTH_URL"
	envGoTraceback    = "GOTRACEBACK"
)

// authLoginNonURLLines is the exhaustive set of whole-line patterns that
// classify a login line carrying no authorization URL: the blank padding, and
// the two banners the harness writes ahead of the URL. Every pattern is
// anchored end to end, because a substring match would classify a line that
// merely contains one of them. The ellipsis is written as its code point
// because the harness emits U+2026 rather than three periods, and the optional
// carriage return covers a stream whose newlines crossed as CRLF, which leaves
// the CR on the line the splitter hands over.
var authLoginNonURLLines = []*regexp.Regexp{
	regexp.MustCompile(`\A\s*\z`),
	regexp.MustCompile(`\AOpening browser to sign in\x{2026}\r?\z`),
	regexp.MustCompile(`\AOpening browser to sign in with your Claude account\x{2026}\r?\z`),
}

// authLoginURLMarker starts a candidate authorization URL inside a line. The
// harness wraps the URL in OSC-8 escapes and emits it twice, so a line carries
// the same bytes in the escape parameter and again as the visible text.
const authLoginURLMarker = authLoginURLScheme + "://"

// ErrAuthLoginGrammar reports a login line the pinned whole-line allowlist does
// not classify. The child is terminated and the leg fails closed rather than
// guessing: a patch release that turns a warning into the URL must not be
// absorbed silently.
var ErrAuthLoginGrammar = errors.New("claude auth login emitted an unclassifiable line")

// AuthLoginGrammarError names the exact line the allowlist refused. Line is the
// raw byte sequence and is code-bearing whenever it carries a URL candidate, so
// it stays out of the error text and reaches a sink only after every candidate
// in it has been scrubbed.
type AuthLoginGrammarError struct {
	Line string
}

func (e *AuthLoginGrammarError) Error() string {
	return ErrAuthLoginGrammar.Error()
}

func (e *AuthLoginGrammarError) Unwrap() error {
	return ErrAuthLoginGrammar
}

// ErrAuthLoginNoURL reports a login child that exited before it presented an
// authorization URL and its prompt.
var ErrAuthLoginNoURL = errors.New("claude auth login presented no authorization url")

// authScrubbedEnvNames names the variables removed from every child's
// environment. The login prompt line is byte-identical only under a scrubbed
// environment, and the OSC-8 wrapper is not gated on isatty, so redirecting to
// a pipe or a file is no protection on its own; a custom OAuth URL repoints the
// credential store, which every child that reads or writes it must agree on.
var authScrubbedEnvNames = []string{
	"TERM_PROGRAM",
	envForceHyperlink,
	envCustomOAuthURL,
	envGoTraceback,
}

// authLoginReadLimit bounds the bytes read from one login child before the
// grammar gives up, so a harness that streams forever cannot pin memory.
const authLoginReadLimit = 1 << 20

// authLoginChunkBytes sizes one read from the login child's output.
const authLoginChunkBytes = 4096

// authLoginPresentationWait bounds how long a login child may take to present
// its authorization URL and prompt. A child that neither prompts nor exits is a
// pin that broke the grammar, and it fails the leg closed rather than hanging.
var authLoginPresentationWait = 60 * time.Second

// authShutdownWait bounds the login child's own exit once it is asked to stop.
const authShutdownWait = 5 * time.Second

// AuthAccount is the allowlist-and-ignore projection of `claude auth status
// --json`. The payload carries three fields logged out, four on the api-key
// path, and seven on a logged-in subscription session, all on one build, so the
// decoder ignores unknown and absent members rather than pinning a shape.
type AuthAccount struct {
	LoggedIn bool
	// AuthMethod is the native method label. It is "oauth_token" for two
	// different environment variables, so it never identifies which one
	// supplied a token.
	AuthMethod string
	Email      string
	OrgID      string
	OrgName    string
}

// authStatusPayload decodes the fields this adapter reads. Every other member
// is ignored so an upstream addition never breaks the probe.
type authStatusPayload struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgID      string `json:"orgId"`
	OrgName    string `json:"orgName"`
}

// AuthStatus runs `claude auth status --json` under the selected containment
// boundary and reports the account projection beside the child's exit code.
// Both values are needed to decide whether the configured home is logged in.
func AuthStatus(ctx context.Context, options Options) (AuthAccount, int, error) {
	// --json is passed explicitly even though it is the documented default, so
	// a future default flip changes nothing here.
	output, code, err := authCommandOutput(ctx, []string{authCommand, authStatus, "--json"}, options)
	if err != nil {
		return AuthAccount{}, code, err
	}

	account, decodeErr := decodeAuthStatus(output)
	if decodeErr != nil {
		return AuthAccount{}, code, decodeErr
	}

	return account, code, nil
}

func decodeAuthStatus(output []byte) (AuthAccount, error) {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return AuthAccount{}, nil
	}

	var payload authStatusPayload
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return AuthAccount{}, fmt.Errorf("parse claude auth status output: %w", err)
	}

	return AuthAccount(payload), nil
}

// AuthLogout runs `claude auth logout`, which clears the config dir's account
// and, where the platform has one, its keystore items.
func AuthLogout(ctx context.Context, options Options) (int, error) {
	_, code, err := authCommandOutput(ctx, []string{authCommand, "logout"}, options)

	return code, err
}

// authCommandOutput runs one bounded `claude auth …` invocation and separates
// the child's exit status from a real launch or containment failure. A non-zero
// exit is an answer on this surface, not an error.
func authCommandOutput(
	ctx context.Context,
	args []string,
	options Options,
) ([]byte, int, error) {
	output, result, err := runNativeOutput(ctx, options, options.CLIPath, args)
	if err != nil {
		return nil, 0, err
	}

	return output, result.ExitCode, nil
}

// authScrubbedEnvKey reports whether a variable is dropped from every child's
// environment. None is disarmable in-process, so the spawner owns all of them.
// The test folds case on every platform, unlike EnvironmentKey: these names
// carry a credential-store repoint and an output-grammar break rather than
// identity, and the cost of over-scrubbing a lowercase Unix spelling nothing
// reads is nil beside the cost of forwarding one.
func authScrubbedEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, scrubbed := range authScrubbedEnvNames {
		if upper == scrubbed {
			return true
		}
	}

	return false
}

// AuthLoginURL validates a candidate authorization URL independently of, and
// before, the line grammar. Ordering is load-bearing: the harness emits the URL
// twice inside OSC-8 escapes, so a grammar-first parser extracts bytes that are
// not a URL at all.
func AuthLoginURL(candidate string) (string, bool) {
	if candidate == "" || len(candidate) > authLoginMaxURLBytes {
		return "", false
	}

	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != authLoginURLScheme || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}

	if !strings.EqualFold(parsed.Hostname(), authLoginHost) {
		return "", false
	}

	if port := parsed.Port(); port != "" && port != "443" {
		return "", false
	}

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}

	if query.Get(authRedirectQueryKey) != AuthLoginRedirectURI {
		return "", false
	}

	return candidate, true
}

// AuthLoginURLCandidates extracts every https run from a raw line. A run ends
// at the first byte a URL cannot carry, which is what terminates both halves of
// an OSC-8 wrapper: the escape parameter ends at BEL or ESC, and the visible
// text ends at the closing escape.
func AuthLoginURLCandidates(line string) []string {
	candidates := make([]string, 0, 2)
	rest := line

	for {
		index := strings.Index(rest, authLoginURLMarker)
		if index < 0 {
			return candidates
		}

		rest = rest[index:]

		end := len(rest)

		for offset, char := range []byte(rest) {
			if authURLTerminator(char) {
				end = offset

				break
			}
		}

		candidates = append(candidates, rest[:end])
		rest = rest[end:]
	}
}

func authURLTerminator(char byte) bool {
	switch char {
	case ' ', '"', '\'', '<', '>', '`', '\\', 0x7f:
		return true
	default:
		return char < 0x20
	}
}

// classifyAuthLoginLine applies the pinned whole-line allowlist. The URL check
// runs first and independently; only a line carrying no URL candidate at all
// reaches the allowlist, and a line the allowlist does not cover is fatal.
func classifyAuthLoginLine(line string) (string, error) {
	candidates := AuthLoginURLCandidates(line)
	if len(candidates) == 0 {
		for _, pattern := range authLoginNonURLLines {
			if pattern.MatchString(line) {
				return "", nil
			}
		}

		return "", &AuthLoginGrammarError{Line: line}
	}

	authorizeURL := ""

	for _, candidate := range candidates {
		validated, ok := AuthLoginURL(candidate)
		if !ok {
			return "", &AuthLoginGrammarError{Line: line}
		}

		if authorizeURL != "" && validated != authorizeURL {
			return "", &AuthLoginGrammarError{Line: line}
		}

		authorizeURL = validated
	}

	return authorizeURL, nil
}

// ReadAuthLoginPresentation drives the pinned grammar over the login child's
// stdout. It returns once the validated authorization URL and the pinned prompt
// have both arrived; any unclassifiable line fails closed.
func ReadAuthLoginPresentation(stdout io.Reader) (string, error) {
	reader := io.LimitReader(stdout, authLoginReadLimit)
	chunk := make([]byte, authLoginChunkBytes)
	authorizeURL := ""
	pending := ""

	for {
		read, err := reader.Read(chunk)
		if read > 0 {
			pending += string(chunk[:read])

			classified, remainder, classifyErr := classifyAuthLoginLines(pending, authorizeURL)
			if classifyErr != nil {
				return "", classifyErr
			}

			authorizeURL = classified
			pending = remainder

			// The prompt arrives without a trailing newline, so it is never a
			// complete line: it is the residue that says the child is waiting.
			if pending == AuthLoginPrompt {
				if authorizeURL == "" {
					return "", ErrAuthLoginGrammar
				}

				return authorizeURL, nil
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", ErrAuthLoginNoURL
			}

			return "", fmt.Errorf("read claude auth login output: %w", err)
		}
	}
}

// classifyAuthLoginLines runs the grammar over every complete line in buffered
// and reports the unconsumed residue.
func classifyAuthLoginLines(buffered string, authorizeURL string) (string, string, error) {
	for {
		index := strings.IndexByte(buffered, '\n')
		if index < 0 {
			return authorizeURL, buffered, nil
		}

		found, err := classifyAuthLoginLine(buffered[:index])
		if err != nil {
			return "", "", err
		}

		if found != "" {
			authorizeURL = found
		}

		buffered = buffered[index+1:]
	}
}

// AuthLogin is one running `claude auth login` child. The wrapper owns its
// lifetime end to end: claude exposes no native flow cancel, so terminating
// this process is the fence.
type AuthLogin struct {
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	process    NativeProcess
	shim       *browserShim
	options    Options
	prepared   []string
	exitDone   chan struct{}
	exitResult NativeResult
	exitErr    error

	closeMu        sync.Mutex
	processSettled bool
	processErr     error
	shimRemoved    bool
}

// AuthLoginExit is the direct login child's natural exit outcome. Zero is only
// a post-write barrier; the caller still requires the account to advance past
// this flow's baseline before confirming a login.
type AuthLoginExit uint8

const (
	AuthLoginExitUnknown AuthLoginExit = iota
	AuthLoginExitZero
	AuthLoginExitNonzero
)

// StartAuthLogin spawns the login child under the selected containment boundary
// with a scrubbed environment and returns once the grammar has yielded the
// validated authorization URL.
func StartAuthLogin(ctx context.Context, options Options) (*AuthLogin, string, error) {
	login, err := startAuthLoginChild(options)
	if err != nil {
		return nil, "", err
	}

	presented := make(chan authPresentation, 1)

	go func() {
		authorizeURL, readErr := authLoginPresentationReader(login.stdout)
		presented <- authPresentation{url: authorizeURL, err: readErr}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, authLoginPresentationWait)
	defer cancel()

	select {
	case result := <-presented:
		if result.err != nil {
			return nil, "", errors.Join(result.err, login.Close())
		}

		return login, result.url, nil
	case <-waitCtx.Done():
		return nil, "", errors.Join(waitCtx.Err(), login.Close())
	}
}

// authPresentation carries the grammar's verdict off the reader goroutine.
type authPresentation struct {
	url string
	err error
}

func startAuthLoginChild(options Options) (login *AuthLogin, returnErr error) {
	// The shim is applied after BuildEnv because BuildEnv drops keys wholesale,
	// and a PATH or BROWSER it rewrote afterwards would hand the authorization
	// URL to a real browser. A tab that reaches this leg's loopback listener
	// completes the grant outright, so the launch is neutralised rather than
	// merely discouraged.
	shim, err := newBrowserShim(options.ScratchParent)
	if err != nil {
		return nil, fmt.Errorf("contain claude auth login browser launch: %w", err)
	}

	environment := BuildEnv(options)
	if environment == nil {
		return nil, errors.Join(authorityUnavailable(options.Authority), shim.remove())
	}

	options.PreparedEnvironment = shim.environ(environment)
	prepared := make([]string, 0, 2)
	retainPrepared := false
	failedRoot := ""

	defer func() {
		if login == nil {
			if retainPrepared {
				return
			}

			if failedRoot != "" {
				if failedRoot != shim.dir {
					returnErr = errors.Join(returnErr, shim.remove())
				}

				return
			}

			reclaimErr := reclaimPreparedRoots(options.Authority, prepared)

			returnErr = errors.Join(returnErr, reclaimErr)
			if reclaimErr == nil {
				returnErr = errors.Join(returnErr, shim.remove())
			}
		}
	}()

	if options.Authority != nil {
		if options.Authority.PrepareNativeTree == nil {
			return nil, errors.Join(authorityUnavailable(options.Authority), shim.remove())
		}

		roots := []string{shim.dir}
		if !options.TreePrepared {
			roots = append([]string{options.ClaudeHome}, roots...)
		}

		for _, root := range roots {
			if root == "" {
				continue
			}

			if prepareErr := options.Authority.PrepareNativeTree(context.Background(), root); prepareErr != nil {
				failedRoot = root

				if errors.Is(prepareErr, options.Authority.ContainmentIncomplete) ||
					errors.Is(prepareErr, options.Authority.Unavailable) {
					return nil, prepareErr
				}

				return nil, containmentIncomplete(options, "prepare claude auth login native tree", prepareErr)
			}

			prepared = append(prepared, root)
		}

		options.TreePrepared = true
	}

	process, err := startNative(context.Background(), options, options.CLIPath, []string{authCommand, "login"})
	if err != nil {
		if options.Authority != nil &&
			(errors.Is(err, options.Authority.Unavailable) || errors.Is(err, options.Authority.ContainmentIncomplete)) {
			retainPrepared = true
		}

		return nil, fmt.Errorf("start claude auth login: %w", err)
	}

	login = &AuthLogin{stdin: process.Stdin(), stdout: process.Stdout(), process: process, shim: shim, options: options, prepared: prepared, exitDone: make(chan struct{})}
	go func() { login.exitResult, login.exitErr = process.Wait(context.Background()); close(login.exitDone) }()

	return login, nil
}

// Submit writes the pasted value and closes the child's input. The value is
// `<code>#<state>`; relaying only the code half cannot complete this flow.
func (l *AuthLogin) Submit(value string) error {
	if _, err := io.WriteString(l.stdin, value+"\n"); err != nil {
		return errors.Join(fmt.Errorf("submit claude auth login value: %w", err), l.stdin.Close())
	}

	return l.stdin.Close()
}

// Exited reports whether the child has already exited, which is the only signal
// that lets a status poll run at all: the login process's stdout never signals
// success, and a browser that reaches the harness's loopback listener completes
// the grant with no value ever pasted back. Presentation ownership ends and the
// child's reap is armed before the login is exposed, so this answers before and
// independently of the fence below.
func (l *AuthLogin) Exited() bool {
	select {
	case <-l.exitDone:
		return true
	default:
		return false
	}
}

// Wait blocks until the login child exits on its own and reports its exit
// status class. It never signals the process; Close remains the sole
// containment and shutdown boundary.
func (l *AuthLogin) Wait(ctx context.Context) (AuthLoginExit, error) {
	select {
	case <-ctx.Done():
		return AuthLoginExitUnknown, ctx.Err()
	case <-l.exitDone:
	}

	if l.exitErr != nil {
		return AuthLoginExitUnknown, l.exitErr
	}

	if l.exitResult.ExitCode == 0 {
		return AuthLoginExitZero, nil
	}

	return AuthLoginExitNonzero, nil
}

// Close terminates the login child and completes its containment boundary. It
// is the flow's fence and is idempotent.
func (l *AuthLogin) Close() error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()

	if !l.processSettled {
		_ = l.stdin.Close()

		stdoutErr := l.stdout.Close()
		if errors.Is(stdoutErr, os.ErrClosed) {
			stdoutErr = nil
		}

		revokeCtx, cancel := context.WithTimeout(context.Background(), authShutdownWait)
		revokeErr := l.process.Revoke(revokeCtx)

		cancel()

		waitCtx, cancelWait := context.WithTimeout(context.Background(), authShutdownWait)
		waitErr := l.reap(waitCtx)

		cancelWait()

		if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
			waitErr = containmentIncomplete(l.options, "wait for claude auth login", waitErr)
		} else if waitErr != nil && l.options.Authority != nil &&
			!errors.Is(waitErr, l.options.Authority.ContainmentIncomplete) {
			waitErr = containmentIncomplete(l.options, "wait for claude auth login", waitErr)
		}

		if waitErr == nil && (errors.Is(revokeErr, context.DeadlineExceeded) || errors.Is(revokeErr, context.Canceled)) {
			revokeErr = nil
		}

		l.processErr = errors.Join(revokeErr, waitErr, stdoutErr)
		if waitErr != nil {
			return l.processErr
		}

		l.processSettled = true
	}

	for len(l.prepared) > 0 {
		index := len(l.prepared) - 1
		if err := reclaimNativeTree(l.options.Authority, l.prepared[index]); err != nil {
			return errors.Join(l.processErr, err)
		}

		l.prepared = l.prepared[:index]
	}

	if !l.shimRemoved {
		if err := l.shim.remove(); err != nil {
			return errors.Join(l.processErr, err)
		}

		l.shimRemoved = true
	}

	return l.processErr
}

// CleanupPending reports whether another Close call still owns a process or
// native-tree cleanup rung.
func (l *AuthLogin) CleanupPending() bool {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()

	return !l.processSettled || len(l.prepared) > 0 || !l.shimRemoved
}

func reclaimPreparedRoots(authority *NativeAuthority, roots []string) error {
	var errs []error
	for index := len(roots) - 1; index >= 0; index-- {
		errs = append(errs, reclaimNativeTree(authority, roots[index]))
	}

	return errors.Join(errs...)
}

func reclaimNativeTree(authority *NativeAuthority, root string) error {
	if authority == nil || root == "" {
		return nil
	}

	if authority.ReclaimNativeTree == nil {
		return authorityUnavailable(authority)
	}

	if err := authority.ReclaimNativeTree(context.Background(), root); err != nil {
		if errors.Is(err, authority.TreeBusy) || errors.Is(err, authority.ContainmentIncomplete) {
			return err
		}

		if authority.ContainmentIncomplete != nil {
			return fmt.Errorf("%w: reclaim native tree %q: %w", authority.ContainmentIncomplete, root, err)
		}

		return err
	}

	return nil
}

func (l *AuthLogin) reap(ctx context.Context) error {
	select {
	case <-l.exitDone:
		return l.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
