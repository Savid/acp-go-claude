package claudeacp

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

// authNativeTimeout bounds one native `claude auth …` subcommand. Every native
// call on this surface carries a deadline: an unbounded one hangs the leg
// behind a platform prompt nobody can answer.
const authNativeTimeout = 60 * time.Second

// authLoginSession is the running login child a flow drives. Claude exposes no
// native flow cancel, so terminating this child is the wrapper's fence.
type authLoginSession interface {
	Submit(value string) error
	Exited() bool
	Close() error
}

// authLoginBegin starts the login child and returns it beside the validated
// authorization URL.
var authLoginBegin = func(
	ctx context.Context,
	options claude.Options,
	generation *claude.DarwinGeneration,
) (authLoginSession, string, error) {
	login, authorizeURL, err := authLoginStart(ctx, options, generation)
	if err != nil {
		return nil, "", err
	}

	return login, authorizeURL, nil
}

// authNativeUser reports the account name the platform keystore items carry.
var authNativeUser = func() string { return os.Getenv("USER") }

// nativeOptions builds the launch options every provider-auth subcommand runs
// under. Flows run in the normal session home: a completed login must land in
// the config dir the harness already reads.
func (p *providerAuth) nativeOptions() (claude.Options, error) {
	home, err := canonicalClaudeHome(p.agent.options.Home)
	if err != nil {
		return claude.Options{}, err
	}

	return claude.Options{
		CLIPath:          p.agent.options.ExecutablePath,
		ClaudeHome:       home,
		Env:              p.agent.options.Env,
		DarwinBestEffort: p.agent.containmentMode == RuntimeContainmentBestEffort,
	}, nil
}

// authNativeAdmission admits one native root and its fresh generation. The
// caller owns both until the boundary it started completes.
func (p *providerAuth) authNativeAdmission(ctx context.Context) (*claude.DarwinGeneration, func(), error) {
	generation, err := p.agent.prepareDiscoveryGeneration(ctx)
	if err != nil {
		return nil, nil, err
	}

	release, err := acquireNativeRoot(ctx, p.agent.options.RuntimeResourceHooks, RuntimeResourceDiscovery)
	if err != nil {
		return nil, nil, errors.Join(err, generation.Finish(true))
	}

	return generation, release, nil
}

// authNativeCause classifies a native failure without forwarding any of its
// text. An incomplete containment boundary is never a leg answer: it is the
// agent's own terminal condition and is recorded as one.
func (p *providerAuth) authNativeCause(err error) string {
	if errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		p.agent.recordContainmentError(err)

		return authCauseProcess
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return authCauseTimeout
	}

	return authCauseProcess
}

// probeAccount runs `claude auth status --json` and reports whether this config
// dir holds a credential. The exit code is the completion signal on this
// surface; the payload is decoded allowlist-and-ignore because its field set
// varies with credential state rather than with version.
func (p *providerAuth) probeAccount(ctx context.Context) (claude.AuthAccount, bool, error) {
	options, err := p.nativeOptions()
	if err != nil {
		return claude.AuthAccount{}, false, authFailed(authCauseProcess, "", "", "")
	}

	generation, release, err := p.authNativeAdmission(ctx)
	if err != nil {
		return claude.AuthAccount{}, false, authFailed(p.authNativeCause(err), "", "", "")
	}

	probeCtx, cancel := context.WithTimeout(ctx, authNativeTimeout)
	defer cancel()

	account, code, err := authStatusProbe(probeCtx, options, generation)

	if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		release()
	}

	if err != nil {
		return claude.AuthAccount{}, false, authFailed(p.authNativeCause(err), "", "", "")
	}

	return account, code == 0 && account.LoggedIn, nil
}

// nativeLogout runs the harness's own account-level removal. Its exit status is
// an answer rather than a failure: a home that holds nothing exits non-zero and
// there is nothing left to remove.
func (p *providerAuth) nativeLogout(ctx context.Context) error {
	options, err := p.nativeOptions()
	if err != nil {
		return authFailed(authCauseProcess, authProviderID, "", "")
	}

	generation, release, err := p.authNativeAdmission(ctx)
	if err != nil {
		return authFailed(p.authNativeCause(err), authProviderID, "", "")
	}

	logoutCtx, cancel := context.WithTimeout(ctx, authNativeTimeout)
	defer cancel()

	_, err = authLogoutCommand(logoutCtx, options, generation)

	if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		release()
	}

	if err != nil {
		return authFailed(p.authNativeCause(err), authProviderID, "", "")
	}

	return nil
}

// removeKeystoreItems clears the platform keystore items native logout may
// leave behind. Both items per config dir are removed across both reachable
// name shapes: either may be present, and removing only the credential item
// leaves a usable legacy API key behind.
func (p *providerAuth) removeKeystoreItems(ctx context.Context) error {
	options, err := p.nativeOptions()
	if err != nil {
		return authFailed(authCauseProcess, authProviderID, "", "")
	}

	if err := authKeychainRemove(ctx, options.ClaudeHome, authNativeUser()); err != nil {
		return authFailed(authCauseTransport, authProviderID, "", "")
	}

	return nil
}

// startLogin spawns the login child and returns the validated authorization URL
// beside the handle that fences it.
func (p *providerAuth) startLogin(ctx context.Context) (*authLoginHandle, string, error) {
	options, err := p.nativeOptions()
	if err != nil {
		return nil, "", authFailed(authCauseProcess, authProviderID, authMethodID, "")
	}

	generation, release, err := p.authNativeAdmission(ctx)
	if err != nil {
		return nil, "", authFailed(p.authNativeCause(err), authProviderID, authMethodID, "")
	}

	login, authorizeURL, err := authLoginBegin(ctx, options, generation)
	if err != nil {
		if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
			release()
		}

		if errors.Is(err, claude.ErrAuthLoginGrammar) || errors.Is(err, claude.ErrAuthLoginNoURL) {
			return nil, "", authFailed(authCauseNativeVeto, authProviderID, authMethodID, "")
		}

		return nil, "", authFailed(p.authNativeCause(err), authProviderID, authMethodID, "")
	}

	return &authLoginHandle{login: login, release: release, agent: p.agent}, authorizeURL, nil
}

// authLoginHandle pairs the login child with the native-root permit it holds.
// The permit is released only when the child's containment boundary completes.
type authLoginHandle struct {
	login   authLoginSession
	release func()
	agent   *Agent
}

func (h *authLoginHandle) submit(value string) error {
	return h.login.Submit(value)
}

func (h *authLoginHandle) exited() bool {
	return h.login.Exited()
}

// close terminates the login child. It is the flow's fence and runs on every
// terminal transition, so a flow is never abandoned to a live child.
func (h *authLoginHandle) close() {
	if h == nil {
		return
	}

	err := h.login.Close()
	if errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		h.agent.recordContainmentError(err)

		return
	}

	h.release()
}
