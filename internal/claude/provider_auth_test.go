package claude

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

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
