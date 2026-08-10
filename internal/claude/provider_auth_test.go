package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testForceHyperlinkEnv = "FORCE_HYPERLINK"
	testGoTracebackEnv    = "GOTRACEBACK"
)

const testAuthorizeURL = "https://claude.com/oauth/authorize?code=1&redirect_uri=" + AuthLoginRedirectURI

// The two banners `claude auth login` writes before the URL, measured on the
// pinned 2.1.220 build. Both end in U+2026 rather than three periods.
const (
	testLoginBanner        = "Opening browser to sign in…"
	testAccountLoginBanner = "Opening browser to sign in with your Claude account…"
)

// testVisitPrefix is the text the harness writes ahead of the URL on the line
// that carries it.
const testVisitPrefix = "If the browser didn't open, visit: "

// osc8 wraps a URL the way the harness does when a hyperlink-capable terminal
// is inherited: the same bytes appear in the escape parameter and again as the
// visible text.
func osc8(url string) string {
	return "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\"
}

func authTestOptions(t *testing.T, options Options) (Options, *DarwinGeneration) {
	t.Helper()
	useAuthDirectContainment(t)
	handoff := authLoginHandoffGeneratedNativeTree
	authLoginHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error { return nil }
	t.Cleanup(func() { authLoginHandoffGeneratedNativeTree = handoff })
	options = withTestProcessIsolation(options)

	root, err := os.MkdirTemp(testTraversableTempDir(t), "acp-go-claude-runtime-")
	require.NoError(t, err)

	generation := &DarwinGeneration{
		RuntimeID:   strings.Repeat("a", 32),
		ScratchRoot: root,
		Release: func(complete bool) error {
			if !complete {
				return nil
			}

			return os.RemoveAll(root)
		},
	}

	if options.ScratchParent == "" {
		options.ScratchParent = testTraversableTempDir(t)
	}

	if runtime.GOOS == "darwin" {
		options.DarwinBestEffort = true
	}

	return options, generation
}

func TestAuthLoginURLValidatesIndependently(t *testing.T) {
	t.Parallel()

	validated, ok := AuthLoginURL(testAuthorizeURL)
	require.True(t, ok)
	require.Equal(t, testAuthorizeURL, validated)

	for _, candidate := range []string{
		"",
		strings.Repeat("h", authLoginMaxURLBytes+1),
		"://nope",
		"http://claude.com/?redirect_uri=" + AuthLoginRedirectURI,
		"https://user@claude.com/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com/?redirect_uri=" + AuthLoginRedirectURI + "#frag",
		"https://evil.example/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com:8443/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com/?%zz=1",
		"https://claude.com/?redirect_uri=https://evil.example/callback",
		"https://claude.com/oauth/authorize",
	} {
		_, accepted := AuthLoginURL(candidate)
		require.False(t, accepted, candidate)
	}

	// Port 443 is the same origin and stays legal.
	_, ok = AuthLoginURL("https://claude.com:443/?redirect_uri=" + AuthLoginRedirectURI)
	require.True(t, ok)
}

func TestAuthLoginURLCandidatesReadsBothHalvesOfAnOSC8Wrapper(t *testing.T) {
	t.Parallel()

	candidates := AuthLoginURLCandidates(osc8(testAuthorizeURL))
	require.Equal(t, []string{testAuthorizeURL, testAuthorizeURL}, candidates)

	require.Empty(t, AuthLoginURLCandidates("no url here"))
	require.Equal(t, []string{"https://claude.com/a"}, AuthLoginURLCandidates(`text "https://claude.com/a" tail`))
}

func TestAuthURLTerminator(t *testing.T) {
	t.Parallel()

	for _, char := range []byte{' ', '"', '\'', '<', '>', '`', '\\', 0x7f, 0x00, 0x1b, '\n'} {
		require.True(t, authURLTerminator(char))
	}

	for _, char := range []byte{'a', '/', ':', '?', '='} {
		require.False(t, authURLTerminator(char))
	}
}

func TestClassifyAuthLoginLineKillsOnAnyUnclassifiableLine(t *testing.T) {
	t.Parallel()

	found, err := classifyAuthLoginLine(osc8(testAuthorizeURL))
	require.NoError(t, err)
	require.Equal(t, testAuthorizeURL, found)

	found, err = classifyAuthLoginLine("   \t ")
	require.NoError(t, err)
	require.Empty(t, found)

	// Both pinned banners classify, with and without the carriage return a CRLF
	// stream leaves behind.
	for _, banner := range []string{
		testLoginBanner,
		testLoginBanner + "\r",
		testAccountLoginBanner,
		testAccountLoginBanner + "\r",
	} {
		found, err = classifyAuthLoginLine(banner)
		require.NoError(t, err, banner)
		require.Empty(t, found)
	}

	// A banner that is only contained in the line stays unclassifiable: the
	// patterns are anchored, never substring matches.
	for _, line := range []string{
		"Warning: something new",
		"note: " + testLoginBanner,
		testLoginBanner + " and more",
		"https://evil.example/authorize",
		// Two different valid-looking URLs on one line are not the pinned shape.
		testAuthorizeURL + " " + testAuthorizeURL + "&extra=1",
	} {
		_, err = classifyAuthLoginLine(line)
		require.ErrorIs(t, err, ErrAuthLoginGrammar, line)
	}
}

// TestClassifyAuthLoginLineNamesTheLineThatKilledIt pins the diagnosability of
// a broken pin: the refused line travels with the error, and never inside its
// text, where a URL-bearing line would put the authorization URL into every
// sink that renders an error.
func TestClassifyAuthLoginLineNamesTheLineThatKilledIt(t *testing.T) {
	t.Parallel()

	_, err := classifyAuthLoginLine("Warning: something new")

	var grammar *AuthLoginGrammarError

	require.ErrorAs(t, err, &grammar)
	require.Equal(t, "Warning: something new", grammar.Line)
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
	require.Equal(t, ErrAuthLoginGrammar.Error(), err.Error())

	_, err = classifyAuthLoginLine("https://evil.example/authorize?code=secret")

	require.ErrorAs(t, err, &grammar)
	require.Equal(t, "https://evil.example/authorize?code=secret", grammar.Line)
	require.NotContains(t, err.Error(), "secret")
}

// TestReadAuthLoginPresentationAcceptsTheMeasuredStream drives the exact three
// writes `claude auth login` makes on the pinned build, in order: the banner,
// the visit line carrying the OSC-8 wrapped URL, and the prompt that arrives
// without a newline.
func TestReadAuthLoginPresentationAcceptsTheMeasuredStream(t *testing.T) {
	t.Parallel()

	for _, banner := range []string{testLoginBanner, testAccountLoginBanner} {
		stream := banner + "\n" + testVisitPrefix + osc8(testAuthorizeURL) + "\n" + AuthLoginPrompt

		found, err := ReadAuthLoginPresentation(strings.NewReader(stream))
		require.NoError(t, err, banner)
		require.Equal(t, testAuthorizeURL, found)
	}

	crlf := testLoginBanner + "\r\n" + testVisitPrefix + osc8(testAuthorizeURL) + "\r\n" + AuthLoginPrompt

	found, err := ReadAuthLoginPresentation(strings.NewReader(crlf))
	require.NoError(t, err)
	require.Equal(t, testAuthorizeURL, found)
}

func TestReadAuthLoginPresentationDrivesThePinnedGrammar(t *testing.T) {
	t.Parallel()

	stream := "\n" + osc8(testAuthorizeURL) + "\n\n" + AuthLoginPrompt

	found, err := ReadAuthLoginPresentation(strings.NewReader(stream))
	require.NoError(t, err)
	require.Equal(t, testAuthorizeURL, found)

	// A prompt with no URL before it is not a presentation.
	_, err = ReadAuthLoginPresentation(strings.NewReader(AuthLoginPrompt))
	require.ErrorIs(t, err, ErrAuthLoginGrammar)

	// An unclassifiable line terminates the read before the prompt arrives.
	_, err = ReadAuthLoginPresentation(strings.NewReader("Warning: something new\n" + AuthLoginPrompt))
	require.ErrorIs(t, err, ErrAuthLoginGrammar)

	// A child that exits without prompting presented nothing.
	_, err = ReadAuthLoginPresentation(strings.NewReader(testAuthorizeURL + "\n"))
	require.ErrorIs(t, err, ErrAuthLoginNoURL)

	_, err = ReadAuthLoginPresentation(&failingReader{})
	require.ErrorIs(t, err, errAuthTest)
}

// failingReader reports a transport failure part-way through the stream.
type failingReader struct{ read bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errAuthTest
	}

	r.read = true
	copy(p, "partial")

	return len("partial"), nil
}

var errAuthTest = errors.New("auth test failure")

// envCustomOAuthURL is the scrubbed variable whose value repoints the store.
const envCustomOAuthURL = "CLAUDE_CODE_CUSTOM_OAUTH_URL"

func TestAuthScrubbedEnvKeyCoversEveryVariableThatMovesTheBytesOrTheStore(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"term_program", testForceHyperlinkEnv, envCustomOAuthURL, testGoTracebackEnv} {
		require.True(t, authScrubbedEnvKey(key))
	}

	require.False(t, authScrubbedEnvKey("HOME"))
}

// TestBuildEnvScrubsEveryChildNotJustTheLoginOne pins the scrub at the one seam
// every spawn crosses: the status and logout children read the store a custom
// OAuth URL repoints, so a login scrubbed alone reports success about a store
// the other two never described.
func TestBuildEnvScrubsEveryChildNotJustTheLoginOne(t *testing.T) {
	env := BuildEnv(Options{ProcessIsolation: &ProcessIsolation{
		UID: 1, GID: 2, BaseEnvironment: map[string]string{
			"PATH": "/usr/bin", "TERM_PROGRAM": "iTerm.app", testForceHyperlinkEnv: "1",
			envCustomOAuthURL: "https://claude.example", testGoTracebackEnv: "crash",
		},
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}, Env: map[string]string{
		"KEEP":            "1",
		envCustomOAuthURL: "https://claude.example",
	}})

	require.Contains(t, env, "PATH=/usr/bin")
	require.Contains(t, env, "KEEP=1")

	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		require.False(t, authScrubbedEnvKey(key), entry)
	}
}

func TestDecodeAuthStatusIgnoresUnknownAndAbsentMembers(t *testing.T) {
	t.Parallel()

	account, err := decodeAuthStatus(nil)
	require.NoError(t, err)
	require.Equal(t, AuthAccount{}, account)

	// The logged-out shape.
	account, err = decodeAuthStatus([]byte(`{"loggedIn":false,"authMethod":"none","apiProvider":null}`))
	require.NoError(t, err)
	require.False(t, account.LoggedIn)
	require.Equal(t, "none", account.AuthMethod)

	// The api-key shape.
	account, err = decodeAuthStatus([]byte(`{"loggedIn":true,"authMethod":"api_key","apiProvider":"anthropic","apiKeySource":"env"}`))
	require.NoError(t, err)
	require.True(t, account.LoggedIn)

	// The seven-field subscription shape, plus a member added upstream.
	account, err = decodeAuthStatus([]byte(`{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"anthropic",` +
		`"apiKeySource":"none","email":"a@b.c","orgId":"org","orgName":"Org","subscriptionType":"max","brandNew":1}`))
	require.NoError(t, err)
	require.Equal(t, "a@b.c", account.Email)
	require.Equal(t, "org", account.OrgID)
	require.Equal(t, "Org", account.OrgName)
	require.Equal(t, "oauth_token", account.AuthMethod)

	_, err = decodeAuthStatus([]byte("not json"))
	require.Error(t, err)
}

func TestAuthCommandOutputSeparatesExitStatusFromFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "ok"), "#!/bin/sh\nprintf '{\"loggedIn\":true}'\n")

	account, code, err := AuthStatus(t.Context(), options, generation)
	require.NoError(t, err)
	require.Zero(t, code)
	require.True(t, account.LoggedIn)

	options, generation = authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "loggedout"), "#!/bin/sh\nprintf '{\"loggedIn\":false}'\nexit 1\n")

	account, code, err = AuthStatus(t.Context(), options, generation)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	require.False(t, account.LoggedIn)

	options, generation = authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "garbage"), "#!/bin/sh\nprintf 'not json'\n")

	_, _, err = AuthStatus(t.Context(), options, generation)
	require.Error(t, err)

	options, generation = authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "logout"), "#!/bin/sh\nexit 0\n")

	code, err = AuthLogout(t.Context(), options, generation)
	require.NoError(t, err)
	require.Zero(t, code)
}

func TestAuthCommandOutputFailsWhenTheBinaryCannotBeFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	options, generation := authTestOptions(t, Options{})

	_, _, err := AuthStatus(t.Context(), options, generation)
	require.Error(t, err)

	options, generation = authTestOptions(t, Options{})

	_, _, err = StartAuthLogin(t.Context(), options, generation)
	require.Error(t, err)
}

func TestAuthCommandOutputSurfacesContainmentAndLaunchFailures(t *testing.T) {
	useAuthDirectContainment(t)
	originalPrepare := processPrepareContained

	t.Cleanup(func() { processPrepareContained = originalPrepare })

	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, ErrProcessContainmentIncomplete
	}

	options, generation := authTestOptions(t, Options{Cwd: t.TempDir(), CLIPath: "/bin/sh"})

	_, _, err := AuthStatus(t.Context(), options, generation)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, errAuthTest
	}

	options, generation = authTestOptions(t, Options{Cwd: t.TempDir(), CLIPath: "/bin/sh"})

	_, _, err = AuthStatus(t.Context(), options, generation)
	require.ErrorIs(t, err, errAuthTest)
}

func TestStartAuthLoginDrivesTheChildEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	recorded := filepath.Join(dir, "pasted")
	script := "#!/bin/sh\n" +
		"printf '\\n'\n" +
		"printf '%s\\n' '" + osc8(testAuthorizeURL) + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"read value\n" +
		"printf '%s' \"$value\" > " + recorded + "\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "login"), script)

	login, authorizeURL, err := StartAuthLogin(t.Context(), options, generation)
	require.NoError(t, err)
	require.Equal(t, testAuthorizeURL, authorizeURL)
	require.False(t, login.Exited())

	require.NoError(t, login.Submit("code-half#state-half"))
	require.Eventually(t, func() bool {
		contents, readErr := os.ReadFile(recorded)

		return readErr == nil && string(contents) == "code-half#state-half"
	}, 10*time.Second, 10*time.Millisecond)

	require.NoError(t, login.Close())
	require.True(t, login.Exited())

	// Close is the fence and is idempotent.
	require.NoError(t, login.Close())

	pasted, err := os.ReadFile(recorded)
	require.NoError(t, err)
	require.Equal(t, "code-half#state-half", string(pasted))
}

// A login the operator completes in the browser is answered by the harness's
// own loopback listener: the child installs the credential and exits without
// ever being handed a pasted value, so nothing on this surface calls Close. The
// child's own exit is the only signal the status poll has, and reporting the
// wrapper's teardown instead leaves that poll permanently unable to run.
func TestAuthLoginReportsTheChildExitBeforeTheFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"while [ ! -f " + release + " ]; do sleep 0.05; done\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "selfcompleting"), script)

	login, _, err := StartAuthLogin(t.Context(), options, generation)
	require.NoError(t, err)
	require.False(t, login.Exited())

	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	exit, err := login.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, AuthLoginExitUnknown, exit)
	require.False(t, login.Exited())

	require.NoError(t, os.WriteFile(release, nil, 0o600))
	exitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	exit, err = login.Wait(exitCtx)
	require.NoError(t, err)
	require.Equal(t, AuthLoginExitZero, exit)
	require.True(t, login.Exited())

	require.NoError(t, login.Close())
}

func TestAuthLoginWaitReportsANaturalNonzeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"read value\n" +
		"exit 7\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "refused"), script)

	login, _, err := StartAuthLogin(t.Context(), options, generation)
	require.NoError(t, err)
	require.NoError(t, login.Submit("code-half#state-half"))

	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	exit, err := login.Wait(waitCtx)
	require.NoError(t, err)
	require.Equal(t, AuthLoginExitNonzero, exit)
	require.NoError(t, login.Close())
}

func TestAuthLoginWaitDoesNotClassifyAWaitFailureAsAnExit(t *testing.T) {
	done := make(chan struct{})
	close(done)

	login := &AuthLogin{exit: &commandWait{done: done, err: exec.ErrWaitDelay}}
	exit, err := login.Wait(t.Context())
	require.ErrorIs(t, err, exec.ErrWaitDelay)
	require.Equal(t, AuthLoginExitUnknown, exit)
}

// A login child answers a human who has not opened the authorization URL yet,
// so it must outlive the call that started it. The ACP SDK dispatches every
// request on its own goroutine and cancels that request's context the moment
// the handler returns, so a child bound to the starting context dies before the
// URL is ever visited.
func TestStartAuthLoginChildOutlivesTheStartingContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	recorded := filepath.Join(dir, "pasted")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"read value\n" +
		"printf '%s' \"$value\" > " + recorded + "\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "login"), script)

	startCtx, endStart := context.WithCancel(t.Context())

	login, _, err := StartAuthLogin(startCtx, options, generation)
	require.NoError(t, err)

	endStart()

	require.NoError(t, login.Submit("code-half#state-half"))
	require.Eventually(t, func() bool {
		contents, readErr := os.ReadFile(recorded)

		return readErr == nil && string(contents) == "code-half#state-half"
	}, 10*time.Second, 10*time.Millisecond)

	require.NoError(t, login.Close())
}

func TestStartAuthLoginFailsClosedOnAnUnclassifiableLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'A new warning line\\n'\nsleep 30\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "drifted"), script)

	_, _, err := StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
}

func TestStartAuthLoginKillIsTheFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"sleep 30\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "hanging"), script)

	login, _, err := StartAuthLogin(t.Context(), options, generation)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, login.Close())
	require.Less(t, time.Since(start), authShutdownWait+2*time.Second)
}

func TestStartAuthLoginChildLaunchFailures(t *testing.T) {
	useAuthDirectContainment(t)
	originalGetwd := processGetwd
	originalPrepare := processPrepareContained
	originalStart := processStartContained

	t.Cleanup(func() {
		processGetwd = originalGetwd
		processPrepareContained = originalPrepare
		processStartContained = originalStart
	})

	processGetwd = func() (string, error) { return "", errAuthTest }

	options, generation := authTestOptions(t, Options{CLIPath: "/bin/sh"})

	_, _, err := StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, errAuthTest)

	processGetwd = originalGetwd

	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, errAuthTest
	}

	options, generation = authTestOptions(t, Options{CLIPath: "/bin/sh"})

	_, _, err = StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, errAuthTest)

	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		command.Stdin = strings.NewReader("")

		return &processTreeCommand{cmd: command}, nil
	}

	options, generation = authTestOptions(t, Options{CLIPath: "/bin/sh"})

	_, _, err = StartAuthLogin(t.Context(), options, generation)
	require.ErrorContains(t, err, "open claude auth login input")

	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		command.Stdout = io.Discard

		return &processTreeCommand{cmd: command}, nil
	}

	options, generation = authTestOptions(t, Options{CLIPath: "/bin/sh"})

	_, _, err = StartAuthLogin(t.Context(), options, generation)
	require.ErrorContains(t, err, "open claude auth login output")

	processPrepareContained = originalPrepare
	processStartContained = func(*processTreeCommand) (*processContainment, error) { return nil, errAuthTest }

	options, generation = authTestOptions(t, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})

	_, _, err = StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, errAuthTest)
}

func TestStartAuthLoginChildRefusesBeforeItSpawns(t *testing.T) {
	options, generation := authTestOptions(t, Options{Cwd: t.TempDir()})
	options.ProcessIsolation = &ProcessIsolation{}

	_, err := startAuthLoginChild("/bin/sh", options, generation)
	require.ErrorContains(t, err, "invalid process isolation")

	options, generation = authTestOptions(t, Options{Cwd: t.TempDir()})
	authLoginHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error { return errAuthTest }

	_, err = startAuthLoginChild("/bin/sh", options, generation)
	require.ErrorIs(t, err, errAuthTest)
	require.ErrorContains(t, err, "handoff claude auth login browser shim")
}

func TestAuthLoginSubmitFailsWhenTheChildIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"exit 0\n"

	options := Options{
		CLIPath:             writeShellScript(t, filepath.Join(dir, "quick"), script),
		Cwd:                 dir,
		ScratchParent:       dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
	}

	login, _, err := StartAuthLogin(t.Context(), options, nil)
	require.NoError(t, err)
	require.NoError(t, login.Close())

	require.Error(t, login.Submit("code#state"))
}

func TestAuthLoginOrdinaryWaitStartsAfterThePresentationVerdict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"sleep 30\n"
	options := Options{
		CLIPath:             writeShellScript(t, filepath.Join(dir, "gated"), script),
		Cwd:                 dir,
		ScratchParent:       dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
	}

	readerEntered := make(chan struct{})
	releaseReader := make(chan struct{})
	originalReader := authLoginPresentationReader
	authLoginPresentationReader = func(reader io.Reader) (string, error) {
		close(readerEntered)
		<-releaseReader

		return ReadAuthLoginPresentation(reader)
	}
	t.Cleanup(func() { authLoginPresentationReader = originalReader })

	waiterReady, starts := observeAuthLoginExitStart(t)
	type startResult struct {
		login *AuthLogin
		url   string
		err   error
	}
	started := make(chan startResult, 1)
	go func() {
		login, url, err := StartAuthLogin(t.Context(), options, nil)
		started <- startResult{login: login, url: url, err: err}
	}()

	waiter := <-waiterReady
	<-readerEntered
	require.Zero(t, starts.Load())
	select {
	case <-waiter.done:
		t.Fatal("ordinary login child was reaped before presentation ownership ended")
	default:
	}

	close(releaseReader)
	result := <-started
	require.NoError(t, result.err)
	require.Equal(t, testAuthorizeURL, result.url)
	require.Equal(t, int64(1), starts.Load())
	require.NoError(t, result.login.Close())
	require.Equal(t, int64(1), starts.Load())
	select {
	case <-waiter.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary login child was not reaped")
	}
}

func TestAuthLoginPresentationTimeoutStartsAndReapsTheOrdinaryWaiter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()
	options := Options{
		CLIPath:             writeShellScript(t, filepath.Join(dir, "silent"), "#!/bin/sh\nsleep 30\n"),
		Cwd:                 dir,
		ScratchParent:       dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
	}
	originalWait := authLoginPresentationWait
	authLoginPresentationWait = 20 * time.Millisecond
	t.Cleanup(func() { authLoginPresentationWait = originalWait })

	waiterReady, starts := observeAuthLoginExitStart(t)
	result := make(chan error, 1)
	go func() {
		_, _, err := StartAuthLogin(t.Context(), options, nil)
		result <- err
	}()

	waiter := <-waiterReady
	require.ErrorIs(t, <-result, context.DeadlineExceeded)
	require.Equal(t, int64(1), starts.Load())
	select {
	case <-waiter.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out ordinary login child was not reaped")
	}
}

func observeAuthLoginExitStart(t *testing.T) (<-chan *commandWait, *atomic.Int64) {
	t.Helper()

	original := authLoginPrepareChildExit
	waiterReady := make(chan *commandWait, 1)
	starts := &atomic.Int64{}
	authLoginPrepareChildExit = func(tree *processContainment, command *exec.Cmd) (*commandWait, func()) {
		waiter, begin := prepareChildExit(tree, command)
		waiterReady <- waiter

		var once sync.Once

		return waiter, func() {
			once.Do(func() {
				starts.Add(1)
				begin()
			})
		}
	}
	t.Cleanup(func() { authLoginPrepareChildExit = original })

	return waiterReady, starts
}

func TestAuthCloseErrorTreatsAnExitStatusAsExpected(t *testing.T) {
	t.Parallel()

	require.NoError(t, authCloseError(nil, nil, nil))
	require.NoError(t, authCloseError(nil, &exec.ExitError{}, nil))
	require.NoError(t, authCloseError(nil, exec.ErrWaitDelay, nil))
	require.ErrorIs(t, authCloseError(errAuthTest, exec.ErrWaitDelay, nil), exec.ErrWaitDelay)
	require.ErrorIs(t, authCloseError(nil, errAuthTest, nil), errAuthTest)
	require.ErrorIs(t, authCloseError(errAuthTest, nil, nil), errAuthTest)
	require.ErrorIs(t, authCloseError(nil, nil, errAuthTest), errAuthTest)
}

func TestDarwinGenerationFinishIsExportedForUnwind(t *testing.T) {
	t.Parallel()

	finished := 0
	generation := &DarwinGeneration{Release: func(bool) error {
		finished++

		return nil
	}}

	require.NoError(t, generation.Finish(true))
	require.NoError(t, generation.Finish(true))
	require.Equal(t, 1, finished)
}

func TestStartAuthLoginFailsClosedWhenNoPresentationArrives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	original := authLoginPresentationWait

	authLoginPresentationWait = 20 * time.Millisecond

	t.Cleanup(func() { authLoginPresentationWait = original })

	dir := t.TempDir()

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "silent"), "#!/bin/sh\nsleep 30\n")

	_, _, err := StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAuthCommandOutputContextTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "hang"), "#!/bin/sh\nsleep 30\n")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	_, _, err := AuthStatus(ctx, options, generation)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
