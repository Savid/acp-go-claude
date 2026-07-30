package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
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

// authRedirectQueryKey names the query parameter carrying the hosted callback.
const authRedirectQueryKey = "redirect_uri"

// authLoginURLScheme is the only scheme an authorization URL may carry.
const authLoginURLScheme = "https"

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
	"FORCE_HYPERLINK",
	"CLAUDE_CODE_CUSTOM_OAUTH_URL",
	"GOTRACEBACK",
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
func AuthStatus(ctx context.Context, options Options, generation *DarwinGeneration) (AuthAccount, int, error) {
	// --json is passed explicitly even though it is the documented default, so
	// a future default flip changes nothing here.
	output, code, err := authCommandOutput(ctx, []string{"auth", "status", "--json"}, options, generation, "claude auth status")
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
func AuthLogout(ctx context.Context, options Options, generation *DarwinGeneration) (int, error) {
	_, code, err := authCommandOutput(ctx, []string{"auth", "logout"}, options, generation, "claude auth logout")

	return code, err
}

// authCommandOutput runs one bounded `claude auth …` invocation and separates
// the child's exit status from a real launch or containment failure. A non-zero
// exit is an answer on this surface, not an error.
func authCommandOutput(
	ctx context.Context,
	args []string,
	options Options,
	generation *DarwinGeneration,
	operation string,
) ([]byte, int, error) {
	path, err := Discover(ctx, options.CLIPath, nil)
	if err != nil {
		return nil, 0, errors.Join(err, generation.finish(true))
	}

	output, err := containedClaudeOutput(ctx, path, args, options, generation, operation)
	if err == nil {
		return output, 0, nil
	}

	if errors.Is(err, ErrProcessContainmentIncomplete) {
		return nil, 0, err
	}

	// A child the wrapper killed on its own deadline also carries an exit
	// status, so the deadline is read first: reporting it as an ordinary
	// non-zero exit would let a timeout answer "not logged in".
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, errors.Join(ctxErr, err)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}

	return nil, 0, err
}

// authScrubbedEnvKey reports whether a variable is dropped from every child's
// environment. None is disarmable in-process, so the spawner owns all of them.
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
	tree       *processContainment
	generation *DarwinGeneration
	shim       *browserShim
	exit       *commandWait

	once     sync.Once
	closeErr error
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
func StartAuthLogin(ctx context.Context, options Options, generation *DarwinGeneration) (*AuthLogin, string, error) {
	path, err := Discover(ctx, options.CLIPath, nil)
	if err != nil {
		return nil, "", errors.Join(err, generation.finish(true))
	}

	login, err := startAuthLoginChild(path, options, generation)
	if err != nil {
		return nil, "", err
	}

	presented := make(chan authPresentation, 1)

	go func() {
		authorizeURL, readErr := ReadAuthLoginPresentation(login.stdout)
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

func startAuthLoginChild(
	path string,
	options Options,
	generation *DarwinGeneration,
) (login *AuthLogin, returnErr error) {
	started := false

	defer func() {
		if !started {
			returnErr = errors.Join(returnErr, generation.finish(!errors.Is(returnErr, ErrProcessContainmentIncomplete)))
		}
	}()

	command := processCommand(path, "auth", "login")
	configureProcessCommand(command)

	cwd, err := processGetwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory for claude auth login: %w", err)
	}

	command.Dir = cwd

	envOptions := options
	envOptions.Cwd = cwd
	command.Env = BuildEnv(envOptions)

	// The shim is applied after BuildEnv because BuildEnv drops keys wholesale,
	// and a PATH or BROWSER it rewrote afterwards would hand the authorization
	// URL to a real browser. A tab that reaches this leg's loopback listener
	// completes the grant outright, so the launch is neutralised rather than
	// merely discouraged.
	shim, err := newBrowserShim(options.ScratchParent)
	if err != nil {
		return nil, fmt.Errorf("contain claude auth login browser launch: %w", err)
	}

	defer func() {
		if !started {
			returnErr = errors.Join(returnErr, shim.remove())
		}
	}()

	command.Env = shim.environ(command.Env)

	launch, err := processPrepareContained(command, processLaunchOptions{
		DarwinBestEffort: options.DarwinBestEffort,
		Generation:       generation,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare claude auth login containment: %w", err)
	}

	stdin, err := launch.cmd.StdinPipe()
	if err != nil {
		launch.close()

		return nil, fmt.Errorf("open claude auth login input: %w", err)
	}

	stdout, err := launch.cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		launch.close()

		return nil, fmt.Errorf("open claude auth login output: %w", err)
	}

	tree, err := processStartContained(launch)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, fmt.Errorf("start claude auth login: %w", err)
	}

	started = true

	return &AuthLogin{
		stdin:      stdin,
		stdout:     stdout,
		tree:       tree,
		generation: generation,
		shim:       shim,
		exit:       startChildExit(tree, launch.cmd),
	}, nil
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
// the grant with no value ever pasted back. It reads the child's own reap, which
// was started with the child, so it answers before and independently of the
// fence below.
func (l *AuthLogin) Exited() bool {
	select {
	case <-l.exit.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the login child exits on its own and reports its exit
// status class. It never signals the process; Close remains the sole
// containment and shutdown boundary.
func (l *AuthLogin) Wait(ctx context.Context) (AuthLoginExit, error) {
	waitErr, completed := l.exit.await(ctx)
	if !completed {
		return AuthLoginExitUnknown, ctx.Err()
	}

	if waitErr == nil {
		return AuthLoginExitZero, nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() >= 0 {
		return AuthLoginExitNonzero, nil
	}

	return AuthLoginExitUnknown, waitErr
}

// Close terminates the login child and completes its containment boundary. It
// is the flow's fence and is idempotent.
func (l *AuthLogin) Close() error {
	l.once.Do(func() {
		_ = l.stdin.Close()

		containErr := l.tree.quiesce(authShutdownWait)
		waitErr := l.reap()

		_ = l.stdout.Close()

		closeErr := processContainmentClose(l.tree)

		l.closeErr = errors.Join(authCloseError(containErr, waitErr, closeErr), l.shim.remove())
		l.closeErr = errors.Join(l.closeErr, l.generation.finish(!errors.Is(l.closeErr, ErrProcessContainmentIncomplete)))
	})

	return l.closeErr
}

// reap collects the result of the child's own exit. That exit is already being
// observed, and it is the only wait on this child, so the fence takes its result
// rather than starting a second one. The bound matters because the boundary has
// just been asked to stop the child: a boundary that reports quiescence has
// reaped it too, so waiting past the fence's own window would be waiting on a
// boundary that already failed and said so.
func (l *AuthLogin) reap() error {
	ctx, cancel := context.WithTimeout(context.Background(), authShutdownWait)
	defer cancel()

	waitErr, _ := l.exit.await(ctx)

	return waitErr
}

// authCloseError joins the fence's results. A fenced login child either exits
// with a status of its own or is signalled by the fence, so an exit status is
// the expected outcome rather than a wrapper failure. A child that exited while
// something else still held its output pipe is reported by the wait delay the
// spawn arms, and a boundary that completed has already accounted for whatever
// held it. Any other wait failure is real and is reported.
func authCloseError(containErr error, waitErr error, closeErr error) error {
	joined := errors.Join(containErr, closeErr)

	var exitErr *exec.ExitError

	expected := errors.As(waitErr, &exitErr) || (joined == nil && errors.Is(waitErr, exec.ErrWaitDelay))
	if waitErr != nil && !expected {
		joined = errors.Join(joined, waitErr)
	}

	return joined
}
