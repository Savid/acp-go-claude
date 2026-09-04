package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const currentAuthorizeURL = "https://claude.com/oauth/authorize?code=1&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback"

func currentOSC8(url string) string {
	return "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\"
}

func TestAuthLoginURLCurrentAllowlist(t *testing.T) {
	validated, ok := AuthLoginURL(currentAuthorizeURL)
	require.True(t, ok)
	require.Equal(t, currentAuthorizeURL, validated)

	for _, candidate := range []string{
		"",
		strings.Repeat("h", authLoginMaxURLBytes+1),
		"://nope",
		"http://claude.com/?redirect_uri=" + AuthLoginRedirectURI,
		"https://user@claude.com/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com/?redirect_uri=" + AuthLoginRedirectURI + "#fragment",
		"https://evil.example/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com:8443/?redirect_uri=" + AuthLoginRedirectURI,
		"https://claude.com/?%zz=1",
		"https://claude.com/?redirect_uri=https://evil.example/callback",
		"https://claude.com/oauth/authorize",
	} {
		_, accepted := AuthLoginURL(candidate)
		require.False(t, accepted, candidate)
	}

	_, ok = AuthLoginURL("https://claude.com:443/?redirect_uri=" + AuthLoginRedirectURI)
	require.True(t, ok)
}

func TestAuthLoginPresentationCurrentGrammar(t *testing.T) {
	require.Equal(t, []string{currentAuthorizeURL, currentAuthorizeURL}, AuthLoginURLCandidates(currentOSC8(currentAuthorizeURL)))

	for _, banner := range []string{
		"Opening browser to sign in…",
		"Opening browser to sign in with your Claude account…",
	} {
		stream := banner + "\nIf the browser didn't open, visit: " + currentOSC8(currentAuthorizeURL) + "\n" + AuthLoginPrompt
		found, err := ReadAuthLoginPresentation(strings.NewReader(stream))
		require.NoError(t, err)
		require.Equal(t, currentAuthorizeURL, found)
	}

	_, err := ReadAuthLoginPresentation(strings.NewReader(AuthLoginPrompt))
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
	_, err = ReadAuthLoginPresentation(strings.NewReader("warning with code=secret\n" + AuthLoginPrompt))
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
	require.NotContains(t, err.Error(), "secret")
	_, err = ReadAuthLoginPresentation(strings.NewReader(currentAuthorizeURL + "\n"))
	require.ErrorIs(t, err, ErrAuthLoginNoURL)
}

func TestAuthLoginGrammarErrorCarriesRefusedLineOutsideRendering(t *testing.T) {
	_, err := classifyAuthLoginLine("https://evil.example/authorize?code=secret")
	var grammar *AuthLoginGrammarError
	require.ErrorAs(t, err, &grammar)
	require.Equal(t, "https://evil.example/authorize?code=secret", grammar.Line)
	require.Equal(t, ErrAuthLoginGrammar.Error(), err.Error())
	require.NotContains(t, err.Error(), "secret")
}

func TestAuthEnvironmentScrubAppliesToEveryNativeChild(t *testing.T) {
	for _, key := range []string{"term_program", "FORCE_HYPERLINK", "CLAUDE_CODE_CUSTOM_OAUTH_URL", "GOTRACEBACK"} {
		require.True(t, authScrubbedEnvKey(key))
	}
	require.False(t, authScrubbedEnvKey("HOME"))

	environment := BuildEnv(Options{
		OrdinaryEnvironment: map[string]string{
			"PATH": "/usr/bin", "TERM_PROGRAM": "terminal", "FORCE_HYPERLINK": "1",
			"CLAUDE_CODE_CUSTOM_OAUTH_URL": "https://example.invalid", "GOTRACEBACK": "crash",
		},
		Env: map[string]string{"KEEP": "1", "CLAUDE_CODE_CUSTOM_OAUTH_URL": "https://example.invalid"},
	})
	require.Contains(t, environment, "PATH=/usr/bin")
	require.Contains(t, environment, "KEEP=1")
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		require.False(t, authScrubbedEnvKey(key), entry)
	}
}

func TestAuthLoginURLAndPresentationGrammar(t *testing.T) {
	valid := "https://claude.com/oauth/authorize?redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback"
	got, ok := AuthLoginURL(valid)
	require.True(t, ok)
	require.Equal(t, valid, got)
	_, ok = AuthLoginURL("https://evil.example/oauth?redirect_uri=" + AuthLoginRedirectURI)
	require.False(t, ok)

	presentation := "Opening browser to sign in…\n" + valid + "\n" + AuthLoginPrompt
	got, err := ReadAuthLoginPresentation(&shortReader{value: presentation})
	require.NoError(t, err)
	require.Equal(t, valid, got)
}

type shortReader struct{ value string }

func (r *shortReader) Read(p []byte) (int, error) {
	if r.value == "" {
		return 0, io.EOF
	}
	n := copy(p, r.value)
	r.value = r.value[n:]

	return n, nil
}

func TestDecodeAuthStatusProjectsKnownFields(t *testing.T) {
	account, err := decodeAuthStatus([]byte(`{"loggedIn":true,"authMethod":"oauth_token","email":"a@example.com","unknown":1}`))
	require.NoError(t, err)
	require.True(t, account.LoggedIn)
	require.Equal(t, "oauth_token", account.AuthMethod)
	require.Equal(t, "a@example.com", account.Email)
}

func TestDecodeAuthStatusCurrentShapes(t *testing.T) {
	account, err := decodeAuthStatus(nil)
	require.NoError(t, err)
	require.Equal(t, AuthAccount{}, account)

	account, err = decodeAuthStatus([]byte(`{"loggedIn":false,"authMethod":"none","apiProvider":null}`))
	require.NoError(t, err)
	require.False(t, account.LoggedIn)
	require.Equal(t, "none", account.AuthMethod)

	account, err = decodeAuthStatus([]byte(`{"loggedIn":true,"authMethod":"oauth_token","email":"a@b.c","orgId":"org","orgName":"Org","brandNew":1}`))
	require.NoError(t, err)
	require.Equal(t, AuthAccount{LoggedIn: true, AuthMethod: "oauth_token", Email: "a@b.c", OrgID: "org", OrgName: "Org"}, account)

	_, err = decodeAuthStatus([]byte("not json"))
	require.Error(t, err)
}

func TestAuthCommandsOrdinaryBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	options := Options{Cwd: dir, OrdinaryEnvironment: OrdinaryEnvironment()}
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "status"), "#!/bin/sh\nprintf '{\"loggedIn\":true}'\n")
	account, code, err := AuthStatus(context.Background(), options)
	require.NoError(t, err)
	require.Zero(t, code)
	require.True(t, account.LoggedIn)

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "logged-out"), "#!/bin/sh\nprintf '{\"loggedIn\":false}'\nexit 1\n")
	account, code, err = AuthStatus(context.Background(), options)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	require.False(t, account.LoggedIn)

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "garbage"), "#!/bin/sh\nprintf 'not json'\n")
	_, _, err = AuthStatus(context.Background(), options)
	require.Error(t, err)

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "logout"), "#!/bin/sh\nexit 0\n")
	code, err = AuthLogout(context.Background(), options)
	require.NoError(t, err)
	require.Zero(t, code)
}

func TestAuthLoginOrdinaryBoundaryEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	recorded := filepath.Join(dir, "pasted")
	cli := writeShellScript(t, filepath.Join(dir, "login"), "#!/bin/sh\n"+
		"printf '%s\\n' '"+currentOSC8(currentAuthorizeURL)+"'\n"+
		"printf '"+AuthLoginPrompt+"'\n"+
		"read value\n"+
		"printf '%s' \"$value\" > \"$PASTED\"\n")
	login, authorizeURL, err := StartAuthLogin(context.Background(), Options{
		CLIPath: cli, Cwd: dir, ScratchParent: dir, OrdinaryEnvironment: OrdinaryEnvironment(), Env: map[string]string{"PASTED": recorded},
	})
	require.NoError(t, err)
	require.Equal(t, currentAuthorizeURL, authorizeURL)
	require.False(t, login.Exited())
	require.NoError(t, login.Submit("code-half#state-half"))
	require.Eventually(t, func() bool {
		contents, readErr := os.ReadFile(recorded)

		return readErr == nil && string(contents) == "code-half#state-half"
	}, 5*time.Second, 10*time.Millisecond)

	exit, err := login.Wait(context.Background())
	require.NoError(t, err)
	require.Equal(t, AuthLoginExitZero, exit)
	require.NoError(t, login.Close())
	require.NoError(t, login.Close())
}

type currentAuthFailingReader struct{ read bool }

func (r *currentAuthFailingReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("read failed")
	}
	r.read = true

	return copy(p, "partial"), nil
}

func TestAuthLoginPresentationPropagatesReaderFailure(t *testing.T) {
	_, err := ReadAuthLoginPresentation(&currentAuthFailingReader{})
	require.ErrorContains(t, err, "read claude auth login output")
}
