package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Ledger record states. The proof a residence answer can carry is a total
// function of these and a native probe, so the set is closed.
const (
	authLedgerIntent    = "intent"
	authLedgerConfirmed = "confirmed"
	authLedgerRemoved   = "removed"
)

// Closed proofSource enum. Presence alone is never enough: without durable
// provenance binding the resident credential to this connection generation the
// honest answer is not_confirmed however plainly the slot is occupied.
const (
	authProofConfirmedPresent = "confirmed_present"
	authProofConfirmedAbsent  = "confirmed_absent"
	authProofNotConfirmed     = "not_confirmed"
)

const (
	authLedgerVendorDir = "claude"
	authLedgerLeafDir   = "ledger"
	authLedgerFileMode  = 0o600
	authLedgerDirMode   = 0o700
)

// authLedgerRecord is the whole content a ledger entry may carry. It never
// holds credential material, authorization URLs, user codes, prompt answers, or
// native text.
type authLedgerRecord struct {
	ProviderID         string `json:"providerId"`
	ConnectionID       string `json:"connectionId"`
	Revision           int64  `json:"revision"`
	BindingGeneration  int64  `json:"bindingGeneration"`
	FlowID             string `json:"flowId"`
	AuthorizeRequestID string `json:"authorizeRequestId"`
	State              string `json:"state"`
	CreatedAt          int64  `json:"createdAt"`
	UpdatedAt          int64  `json:"updatedAt"`
}

var (
	ledgerMkdirAll   = os.MkdirAll
	ledgerChmod      = os.Chmod
	ledgerStat       = os.Stat
	ledgerRename     = os.Rename
	ledgerOpen       = os.Open
	ledgerReadFile   = os.ReadFile
	ledgerReadDir    = os.ReadDir
	ledgerRemove     = os.Remove
	ledgerMarshal    = json.Marshal
	ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) {
		return os.CreateTemp(dir, pattern)
	}
)

// ledgerFile is the file surface an atomic ledger write drives.
type ledgerFile interface {
	Name() string
	Write([]byte) (int, error)
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

// authLedger is the durable values-free record of which native slot each
// connection generation owns. It outlives every session and every native
// generation, so its path is deterministic by design: a bookkeeping record that
// could not be found again after the crash that makes it matter answers
// nothing.
type authLedger struct {
	dir string
}

// authLedgerRootConfigured reports whether the host supplied a durable ledger
// root at all, which is what separates a surface nobody asked for from one that
// was asked for and could not be prepared.
func authLedgerRootConfigured(options Options) bool {
	return options.ProviderAuthRoot != ""
}

// validateProviderAuthRoot fails an agent whose ledger root is relative,
// joining the same construction verdict a relative handoff root reaches.
func validateProviderAuthRoot(options Options) error {
	if options.ProviderAuthRoot == "" || filepath.IsAbs(options.ProviderAuthRoot) {
		return nil
	}

	return errors.New("ProviderAuthRoot must be an absolute path")
}

// newAuthLedger resolves and validates the configured durable root. A root that
// does not exist and cannot be created, is not a directory, or is not writable
// leaves the provider-auth surface unadvertised, exactly as an unset one does.
func newAuthLedger(options Options) (*authLedger, error) {
	root := options.ProviderAuthRoot
	if !filepath.IsAbs(root) {
		return nil, errors.New("provider auth root must be an absolute path")
	}

	// The configured root is restricted in its own right, not merely the leaf
	// under it: an operator-supplied directory that already exists keeps
	// whatever mode it was created with, and the ledger under it is only as
	// private as the directory holding it.
	if err := ledgerMkdirAll(root, authLedgerDirMode); err != nil {
		return nil, fmt.Errorf("create provider auth root: %w", err)
	}

	if err := ledgerChmod(root, authLedgerDirMode); err != nil {
		return nil, fmt.Errorf("restrict provider auth root: %w", err)
	}

	dir := filepath.Join(root, authLedgerVendorDir, authLedgerHomeKey(options.Home), authLedgerLeafDir)
	if err := ledgerMkdirAll(dir, authLedgerDirMode); err != nil {
		return nil, fmt.Errorf("create provider auth ledger root: %w", err)
	}

	if err := ledgerChmod(dir, authLedgerDirMode); err != nil {
		return nil, fmt.Errorf("restrict provider auth ledger root: %w", err)
	}

	info, err := ledgerStat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect provider auth ledger root: %w", err)
	}

	if !info.IsDir() {
		return nil, errors.New("provider auth ledger root is not a directory")
	}

	probe, err := ledgerCreateTemp(dir, "writable-")
	if err != nil {
		return nil, fmt.Errorf("verify provider auth ledger root is writable: %w", err)
	}

	name := probe.Name()

	return &authLedger{dir: dir}, errors.Join(probe.Close(), ledgerRemove(name))
}

// authLedgerHomeKey scopes the ledger to the credential root it describes. Two
// agents pointed at different config dirs describe different slots, so their
// records must not alias.
func authLedgerHomeKey(home string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(home)))

	return hex.EncodeToString(sum[:])[:16]
}

func (l *authLedger) path(providerID string) string {
	sum := sha256.Sum256([]byte(providerID))

	return filepath.Join(l.dir, hex.EncodeToString(sum[:])[:32]+".json")
}

func (l *authLedger) read(providerID string) (authLedgerRecord, bool, error) {
	contents, err := ledgerReadFile(l.path(providerID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return authLedgerRecord{}, false, nil
		}

		return authLedgerRecord{}, false, fmt.Errorf("read provider auth ledger entry: %w", err)
	}

	var record authLedgerRecord
	if err := json.Unmarshal(contents, &record); err != nil {
		return authLedgerRecord{}, false, fmt.Errorf("decode provider auth ledger entry: %w", err)
	}

	return record, true, nil
}

// write persists a record atomically and durably: a temporary file in the same
// directory, fsynced, renamed over its target, with the directory fsynced
// after. Persisted here means fsynced, never merely written.
func (l *authLedger) write(record authLedgerRecord) error {
	contents, err := ledgerMarshal(record)
	if err != nil {
		return fmt.Errorf("encode provider auth ledger entry: %w", err)
	}

	file, err := ledgerCreateTemp(l.dir, "entry-")
	if err != nil {
		return fmt.Errorf("create provider auth ledger entry: %w", err)
	}

	temp := file.Name()

	if err := writeLedgerFile(file, contents); err != nil {
		return errors.Join(fmt.Errorf("write provider auth ledger entry: %w", err), ledgerRemove(temp))
	}

	if err := ledgerRename(temp, l.path(record.ProviderID)); err != nil {
		return errors.Join(fmt.Errorf("commit provider auth ledger entry: %w", err), ledgerRemove(temp))
	}

	return l.syncDir()
}

func writeLedgerFile(file ledgerFile, contents []byte) error {
	if _, err := file.Write(contents); err != nil {
		return errors.Join(err, file.Close())
	}

	if err := file.Chmod(authLedgerFileMode); err != nil {
		return errors.Join(err, file.Close())
	}

	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}

	return file.Close()
}

func (l *authLedger) syncDir() error {
	dir, err := ledgerOpen(l.dir)
	if err != nil {
		return fmt.Errorf("open provider auth ledger root: %w", err)
	}

	return errors.Join(dir.Sync(), dir.Close())
}

func (l *authLedger) list() ([]authLedgerRecord, error) {
	entries, err := ledgerReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("list provider auth ledger: %w", err)
	}

	records := make([]authLedgerRecord, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		contents, err := ledgerReadFile(filepath.Join(l.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read provider auth ledger entry: %w", err)
		}

		var record authLedgerRecord
		if err := json.Unmarshal(contents, &record); err != nil {
			return nil, fmt.Errorf("decode provider auth ledger entry: %w", err)
		}

		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].ProviderID < records[j].ProviderID })

	return records, nil
}

// recordAuthorizeIntent persists the write-ahead intent naming the exact native
// slot this flow targets. It has been fsynced before authorize returns.
//
// The read that carries the prior generation forward and the write that
// supersedes it are one sequence under the slot gate: a disconnect landing
// between them would have its own bump read back as this flow's generation and
// silently undone by the write that follows.
func (p *providerAuth) recordAuthorizeIntent(ctx context.Context, request authorizeRequest, flowID string) (authLedgerRecord, error) {
	release, admitted := p.admitSlot(ctx)
	if !admitted {
		return authLedgerRecord{}, authFailed(authCauseTimeout, request.providerID, request.method, "")
	}

	defer release()

	now := authNow().UnixMilli()
	record := authLedgerRecord{
		ProviderID:         request.providerID,
		ConnectionID:       request.connectionID,
		Revision:           1,
		BindingGeneration:  1,
		FlowID:             flowID,
		AuthorizeRequestID: request.authorizeRequestID,
		State:              authLedgerIntent,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if prior, ok, err := p.ledger.read(request.providerID); err == nil && ok {
		record.Revision = prior.Revision + 1
		record.BindingGeneration = prior.BindingGeneration
		record.CreatedAt = prior.CreatedAt
	}

	if err := p.ledger.write(record); err != nil {
		return authLedgerRecord{}, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	return record, nil
}

// confirmAuthorize writes the post-mutation confirmation. Until it lands the
// ledger holds intent only, and a residence answer over intent is
// not_confirmed however plainly the slot is occupied. Its caller holds the slot
// gate, so the binding checked here cannot move before the write below lands.
func (p *providerAuth) confirmAuthorize(flow *authFlow) error {
	if !p.lineageCurrent(flow) {
		return authFailed(authCauseBindingConflict, flow.providerID, flow.method.ID, flow.id)
	}

	record := authLedgerRecord{
		ProviderID:         flow.providerID,
		ConnectionID:       flow.connectionID,
		Revision:           flow.revision,
		BindingGeneration:  flow.bindingGeneration,
		FlowID:             flow.id,
		AuthorizeRequestID: flow.authorizeRequestID,
		State:              authLedgerConfirmed,
		CreatedAt:          flow.createdAt,
		UpdatedAt:          authNow().UnixMilli(),
	}

	if err := p.ledger.write(record); err != nil {
		return p.fail(flow, authCauseProcess, true)
	}

	return nil
}

type authInventoryEntry struct {
	ProviderID        string `json:"providerId"`
	ConnectionID      string `json:"connectionId"`
	Revision          int64  `json:"revision"`
	BindingGeneration int64  `json:"bindingGeneration"`
	ProofSource       string `json:"proofSource"`
}

type authInventoryResult struct {
	Entries []authInventoryEntry `json:"entries"`
}

// inventory reads the ledger and probes the named native slot. The probe is the
// harness's own `auth status --json`, never the plaintext credential file: on
// Darwin that file is a fallback the composite store leaves behind, so its
// presence proves nothing and its expiry is not the live credential's expiry.
//
// The probe answers residence for this config dir and nothing finer. Identity
// and credential are unbound here — the account the harness reports is a cached
// echo of the last successful login rather than something derived from the
// resident token — so no entry claims more than the ledger's own provenance
// plus a present-or-absent slot.
func (p *providerAuth) inventory(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	if _, sessionErr := p.authSession(sessionID); sessionErr != nil {
		return nil, sessionErr
	}

	records, err := p.ledger.list()
	if err != nil {
		return nil, authFailed(authCauseHarvestFailed, "", "", "")
	}

	entries := make([]authInventoryEntry, 0, len(records))
	probed := false
	present := false

	for _, record := range records {
		if record.State == authLedgerRemoved {
			continue
		}

		if !probed {
			reading, cause := p.readAccount(ctx)
			if cause != "" {
				return nil, authFailed(cause, record.ProviderID, "", "")
			}

			present = reading.loggedIn
			probed = true
		}

		entries = append(entries, authInventoryEntry{
			ProviderID:        record.ProviderID,
			ConnectionID:      record.ConnectionID,
			Revision:          record.Revision,
			BindingGeneration: record.BindingGeneration,
			ProofSource:       authProofSource(record.State, present),
		})
	}

	return authInventoryResult{Entries: entries}, nil
}

// authProofSource is the total function of ledger state and native probe. A
// sibling reports exactly the cell the two select and never chooses a value.
func authProofSource(state string, present bool) string {
	if state != authLedgerConfirmed {
		return authProofNotConfirmed
	}

	if present {
		return authProofConfirmedPresent
	}

	return authProofConfirmedAbsent
}

// disconnect bumps the binding generation before it touches anything else, then
// clears the exactly-fenced slot and verifies absence. It removes the account
// this config dir names and nothing else, and it promises no provider-side
// revocation: the harness's connected-apps entry names the harness.
func (p *providerAuth) disconnect(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldConnectionID, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	connectionID, err := authRequiredConnectionID(fields)
	if err != nil {
		return nil, err
	}

	bindingGeneration, err := authRequiredInt64(fields, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	if _, sessionErr := p.authSession(sessionID); sessionErr != nil {
		return nil, sessionErr
	}

	release, admitted := p.admitSlot(ctx)
	if !admitted {
		return nil, authFailed(authCauseTimeout, providerID, "", "")
	}

	defer release()

	record, ok, err := p.ledger.read(providerID)
	if err != nil {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	if !ok || record.ConnectionID != connectionID || record.BindingGeneration != bindingGeneration {
		return nil, authFailed(authCauseBindingConflict, providerID, "", "")
	}

	record.BindingGeneration++
	record.UpdatedAt = authNow().UnixMilli()
	record.State = authLedgerIntent

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	p.fenceLogins()

	if err := p.nativeLogout(ctx); err != nil {
		return nil, err
	}

	// Native logout clears what it knows about; the legacy API-key item may
	// survive it, and leaving that behind leaves a usable credential behind.
	if err := p.removeKeystoreItems(ctx); err != nil {
		return nil, err
	}

	reading, cause := p.readAccount(ctx)
	if cause != "" {
		return nil, authFailed(cause, providerID, "", "")
	}

	if reading.loggedIn {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	record.State = authLedgerRemoved
	record.UpdatedAt = authNow().UnixMilli()

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	return struct{}{}, nil
}
