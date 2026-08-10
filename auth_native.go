package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
	Wait(context.Context) (claude.AuthLoginExit, error)
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

// authNativeUser reports the target account name the platform keystore items
// carry. It comes only from the base environment the launch actually uses: the
// complete policy environment under explicit isolation, and the sanitized
// ambient capture under ordinary same-identity execution.
var authNativeUser = func(options claude.Options) string {
	if value := options.Env["USER"]; value != "" {
		return value
	}

	if options.ProcessIsolation != nil {
		return options.ProcessIsolation.BaseEnvironment["USER"]
	}

	return options.OrdinaryEnvironment["USER"]
}

// nativeOptions builds the launch options every provider-auth subcommand runs
// under. Flows run in the normal session home: a completed login must land in
// the config dir the harness already reads.
func (p *providerAuth) nativeOptions() (claude.Options, error) {
	if p.home.err != nil {
		return claude.Options{}, p.home.err
	}

	// The login leg materialises its browser shim under this parent, so a
	// scratch root that cannot be created fails the leg rather than letting a
	// child open the operator's browser.
	scratch, err := ensureScratchParent(p.agent.options.ScratchDir)
	if err != nil {
		return claude.Options{}, err
	}

	return claude.Options{
		CLIPath:             p.agent.options.ExecutablePath,
		ClaudeHome:          p.home.path,
		Env:                 p.agent.options.Env,
		ProcessIsolation:    p.agent.claudeIsolation(),
		OrdinaryEnvironment: p.agent.ordinaryEnvironment(),
		ScratchParent:       scratch,
		DarwinBestEffort:    p.agent.containmentMode == RuntimeContainmentBestEffort,
		AcquireKeychainDiscovery: func(discoveryCtx context.Context) (func(), error) {
			return acquireNativeRoot(discoveryCtx, p.agent.options.RuntimeResourceHooks, RuntimeResourceDiscovery)
		},
		PrepareKeychainGeneration: func(generationCtx context.Context) (*claude.DarwinGeneration, error) {
			return p.agent.prepareDarwinGeneration(generationCtx, RuntimeResourceDiscovery)
		},
	}, nil
}

// errAuthHomeReplaced ends a removal whose home no longer names the directory
// the consent gate measured.
var errAuthHomeReplaced = errors.New("claude home no longer names the consented directory")

// nativeRemovalOptions builds the launch options an account-level removal runs
// under. It answers only while the resolved home still names the directory
// consent was granted over: a logout plus a keystore wipe cannot be undone, and
// a directory swapped in under the running agent was never consented to.
func (p *providerAuth) nativeRemovalOptions() (claude.Options, error) {
	options, err := p.nativeOptions()
	if err != nil {
		return claude.Options{}, err
	}

	if !p.home.unchanged() {
		return claude.Options{}, errAuthHomeReplaced
	}

	return options, nil
}

// authRemovalCause classifies a removal that cannot run. A home that no longer
// names the consented directory is this adapter refusing on its own terms,
// which reconnecting does not clear; anything else failed to launch.
func authRemovalCause(err error) string {
	if errors.Is(err, errAuthHomeReplaced) {
		return authCausePolicy
	}

	return authCauseProcess
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

// authAccountIdentity is the comparable projection of one account the config
// dir names. The native struct it is projected from is converted whole from the
// decoded payload, so a member added upstream joins it silently — and a pointer
// member is comparable, allocated afresh by every decode, and would make two
// readings of one unchanged account differ. The signal compares this instead,
// which is exactly the fields this surface chose.
type authAccountIdentity struct {
	loggedIn   bool
	authMethod string
	email      string
	orgID      string
	orgName    string
}

func authAccountIdentityOf(account claude.AuthAccount) authAccountIdentity {
	return authAccountIdentity{
		loggedIn:   account.LoggedIn,
		authMethod: account.AuthMethod,
		email:      account.Email,
		orgID:      account.OrgID,
		orgName:    account.OrgName,
	}
}

// authAccountReading is one `claude auth status --json` answer: the account the
// config dir names beside whether it holds a credential. The reading describes
// the config dir and never a flow, so a flow that wants to know what its own
// login did compares two of them.
type authAccountReading struct {
	identity authAccountIdentity
	loggedIn bool
}

// advancedPast reports whether this reading can only have been produced by a
// login that landed after the baseline was taken. It is the no-callback
// completion witness, where no submitted child exit status is available.
func (r authAccountReading) advancedPast(baseline authAccountReading) bool {
	return r.loggedIn && r != baseline
}

// readAccount runs `claude auth status --json` and reports the config dir's
// account beside whether it holds a credential. The payload is decoded
// allowlist-and-ignore because its field set varies with credential state
// rather than with version. A non-empty cause is the classified native failure,
// which every caller carries through rather than flattening: a wrapper deadline
// is a timeout and never a transport answer.
func (p *providerAuth) readAccount(ctx context.Context) (authAccountReading, string) {
	options, err := p.nativeOptions()
	if err != nil {
		return authAccountReading{}, authCauseProcess
	}

	generation, release, err := p.authNativeAdmission(ctx)
	if err != nil {
		return authAccountReading{}, p.authNativeCause(err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, authNativeTimeout)
	defer cancel()

	account, code, err := authStatusProbe(probeCtx, options, generation)

	if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		release()
	}

	if err != nil {
		return authAccountReading{}, p.authNativeCause(err)
	}

	return authAccountReading{identity: authAccountIdentityOf(account), loggedIn: code == 0 && account.LoggedIn}, ""
}

// nativeLogout runs the harness's own account-level removal. Its exit status is
// an answer rather than a failure: a home that holds nothing exits non-zero and
// there is nothing left to remove.
func (p *providerAuth) nativeLogout(ctx context.Context) error {
	options, err := p.nativeRemovalOptions()
	if err != nil {
		return authFailed(authRemovalCause(err), authProviderID, "", "")
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
	options, err := p.nativeRemovalOptions()
	if err != nil {
		return authFailed(authRemovalCause(err), authProviderID, "", "")
	}

	if err := authKeychainRemove(ctx, options.ClaudeHome, authNativeUser(options), options); err != nil {
		if errors.Is(err, claude.ErrProcessContainmentIncomplete) {
			return authFailed(p.authNativeCause(err), authProviderID, "", "")
		}

		return authFailed(authCauseTransport, authProviderID, "", "")
	}

	return nil
}

// startLogin spawns the login child and returns the validated authorization URL
// beside the handle that fences it. A non-empty cause is the classified native
// failure; the flow the caller owns performs the transition it pairs with.
func (p *providerAuth) startLogin(ctx context.Context) (*authLoginHandle, string, string) {
	options, err := p.nativeOptions()
	if err != nil {
		return nil, "", authCauseProcess
	}

	generation, release, err := p.authNativeAdmission(ctx)
	if err != nil {
		return nil, "", p.authNativeCause(err)
	}

	login, authorizeURL, err := authLoginBegin(ctx, options, generation)
	if err != nil {
		if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
			release()
		}

		if errors.Is(err, claude.ErrAuthLoginGrammar) || errors.Is(err, claude.ErrAuthLoginNoURL) {
			p.logAuthLoginGrammar(ctx, err)

			return nil, "", authCauseNativeVeto
		}

		return nil, "", p.authNativeCause(err)
	}

	return &authLoginHandle{login: login, release: release, agent: p.agent}, authorizeURL, ""
}

// logAuthLoginGrammar records which login line the pinned whole-line grammar
// refused. Without it a broken pin is indistinguishable from every other native
// veto, and the line is only recoverable by capturing the harness out of band.
// Each URL candidate in the line is reduced by the diagnostic URI rule before
// it reaches the sink, because claude's authorization URL is code-bearing for
// the flow's life and follows userCode into every sink restriction.
func (p *providerAuth) logAuthLoginGrammar(ctx context.Context, err error) {
	var grammar *claude.AuthLoginGrammarError
	if !errors.As(err, &grammar) {
		return
	}

	line := grammar.Line
	for _, candidate := range claude.AuthLoginURLCandidates(line) {
		line = strings.ReplaceAll(line, candidate, redactDiagnosticURI(candidate))
	}

	p.agent.log.DebugContext(ctx, "claude auth login line rejected by the pinned grammar", slog.String(jsonFieldLine, line))
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

func (h *authLoginHandle) wait(ctx context.Context) (claude.AuthLoginExit, error) {
	return h.login.Wait(ctx)
}

func (h *authLoginHandle) fence() error {
	if h == nil {
		return nil
	}

	err := h.login.Close()
	if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		h.release()
	}

	return err
}

// close terminates the login child. It is the flow's fence and runs on every
// terminal transition, so a flow is never abandoned to a live child.
func (h *authLoginHandle) close() {
	if err := h.fence(); errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		h.agent.recordContainmentError(err)
	}
}
