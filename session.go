package claudeacp

import (
	"context"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/permissions"
)

const (
	permissionAllowOnce    acp.PermissionOptionId = "allow_once"
	permissionAllowAlways  acp.PermissionOptionId = "allow_always"
	permissionRejectOnce   acp.PermissionOptionId = "reject_once"
	permissionRejectAlways acp.PermissionOptionId = "reject_always"

	jsonFieldID                   = "id"
	jsonFieldCwd                  = "cwd"
	jsonFieldEntries              = "entries"
	jsonFieldError                = "error"
	jsonFieldField                = "field"
	jsonFieldFormat               = "format"
	jsonFieldImportID             = "importId"
	jsonFieldLine                 = "line"
	jsonFieldLimit                = "limit"
	jsonFieldData                 = "data"
	jsonFieldMediaType            = "media_type"
	jsonFieldMessage              = "message"
	jsonFieldMessageID            = "messageId"
	jsonFieldMethod               = "method"
	jsonFieldCommand              = "command"
	jsonFieldMeta                 = "_meta"
	jsonFieldMode                 = "mode"
	jsonFieldOffset               = "offset"
	jsonFieldRequest              = "request"
	jsonFieldResponse             = "response"
	jsonFieldResult               = "result"
	jsonFieldKey                  = "key"
	jsonFieldSHA256               = "sha256"
	jsonFieldServer               = "server"
	jsonFieldSubpath              = "subpath"
	jsonFieldText                 = "text"
	jsonFieldTitle                = "title"
	jsonFieldType                 = "type"
	jsonFieldValue                = "value"
	jsonFieldParams               = "params"
	jsonFieldPrompt               = "prompt"
	jsonFieldReason               = "reason"
	jsonFieldURI                  = "uri"
	jsonFieldURL                  = "url"
	jsonFieldSubtype              = "subtype"
	imageContentType              = "image"
	uriSchemeHTTP                 = "http"
	uriSchemeHTTPS                = "https"
	permissionUpdateAddRules      = "addRules"
	permissionUpdateBehavior      = "behavior"
	permissionUpdateDestination   = "destination"
	permissionUpdateDirectories   = "directories"
	permissionUpdateSetMode       = "setMode"
	permissionUpdateRules         = "rules"
	permissionUpdateRuleContent   = "ruleContent"
	permissionUpdateSession       = "session"
	permissionUpdateToolName      = "toolName"
	permissionUpdateAddDirs       = "addDirectories"
	permissionUpdateLocalSettings = "localSettings"

	acpFieldConfig         = "config"
	acpFieldRaw            = "raw"
	acpFieldSessionID      = "sessionId"
	claudeMetaToolResponse = "toolResponse"

	askUserQuestionTool        = "AskUserQuestion"
	enterPlanModeTool          = "EnterPlanMode"
	exitPlanModeTool           = "ExitPlanMode"
	workflowTool               = "Workflow"
	elicitationComplete        = "elicitation_complete"
	permissionCancelledMessage = "Permission request cancelled"
	permissionRejectedMessage  = "Rejected by ACP client"
	exitPlanModeOutsideMessage = "ExitPlanMode callback is outside the active turn"

	askFieldAnswers     = "answers"
	askFieldDescription = "description"
	askFieldHeader      = "header"
	askFieldID          = jsonFieldID
	askFieldIsOther     = "isOther"
	askFieldIsSecret    = "isSecret"
	askFieldMultiSelect = "multiSelect"
	askFieldOptions     = "options"
	askFieldQuestion    = "question"
	askFieldQuestions   = "questions"

	jsonSchemaTypeString       = "string"
	originKindTaskNotification = "task-notification"
	stopReasonMaxTokens        = "max_tokens"

	systemContent          = "content"
	systemHookEventName    = "hook_event_name"
	systemHookPostToolUse  = "PostToolUse"
	systemHookCallbackID   = "acp_post_tool_use"
	systemState            = "state"
	systemStateIdle        = "idle"
	systemStatus           = "status"
	systemStatusCompacting = "compacting"
	systemToolResponse     = "tool_response"
	systemToolUseID        = "tool_use_id"

	systemSubtypeCompactBoundary     = "compact_boundary"
	systemSubtypeHookResponse        = "hook_response"
	systemSubtypeLocalCommand        = "local_command"
	systemSubtypeLocalCommandOutput  = "local_command_output"
	systemSubtypeSessionStateChanged = "session_state_changed"

	compactingMessageText         = "Compacting..."
	compactingCompleteMessageText = "\n\nCompacting completed."

	defaultContextWindow = 200000
	largeContextWindow   = 1000000
	largeContextToken    = "1m"
	syntheticModelName   = "<synthetic>"

	streamEventMessageDelta = "message_delta"
	streamEventMessageStart = "message_start"

	localCommandContext    = "/context"
	localCommandExtraUsage = "/extra-usage"
	localCommandHeapdump   = "/heapdump"
	commandReloadSkills    = "reload-skills"
	commandReloadPlugins   = "reload-plugins"

	maxHandledHooks = 1024
)

var savePermissionRules = func(ctx context.Context, claudeHome string, sessionID acp.SessionId, rules map[string]string) error {
	store := permissions.Store{ClaudeHome: claudeHome}

	return store.Save(ctx, string(sessionID), rules)
}

// agentSession is the internal handle for one Claude CLI process owned by an
// ACP session.
type agentSession struct {
	agent                 *Agent
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	title                 string
	updatedAt             string
	fingerprint           string
	model                 string
	availableModels       []claude.AvailableModelInfo
	modelOverrides        map[string]string
	outputStyle           string
	availableOutputStyles []string
	mode                  acp.SessionModeId
	effort                string
	fastMode              bool
	fastModeKnown         bool
	availableCommands     []claude.SlashCommand
	advertisedCommands    []acp.AvailableCommand
	contextWindowSize     int
	poisonCause           string
	providerAuthInjection string
	providerAuthResident  map[string]authInjectedLineage

	client *claude.Client
	// clientOptions holds the fully-built options used to launch the Claude
	// client so a crashed native process can be relaunched lazily on the next
	// turn. canRelaunch gates that behavior to real sessions (test sessions that
	// inject a client directly leave it false).
	clientOptions claude.Options
	canRelaunch   bool
	// mcpRefreshPending is true until the first MCP-bearing user turn has
	// relaunched Claude while that turn's host authority is armed. Claude fixes
	// its MCP tool registry at process startup, while a session-bound proxy may
	// deliberately expose only a readiness tool during session establishment.
	mcpRefreshPending bool

	turn chan struct{}
	// foreground is the session's foreground token: exactly one of a prompt turn
	// and an agent-origin excursion holds it at a time. It is a one-slot channel
	// rather than a mutex because the native reader must be able to offer a frame
	// to a prompt that publishes its sink while the reader is still waiting for
	// the foreground, which a blocking lock could not do.
	foreground chan struct{}
	// excursion is the open agent-origin turn, held under the foreground token. It
	// exists only while native work runs with no prompt behind it.
	excursion *agentExcursion
	// autonomousErr latches the first between-prompt failure this session could
	// not report to the host, and autonomousClient names the native client whose
	// incarnation that failure ended. Both are guarded by mu. The pair is what
	// makes the refusal specific: work addressed to the contained incarnation is
	// refused, and a relaunched one clears it.
	autonomousErr    error
	autonomousClient *claude.Client
	// toolUpdates is the incarnation's tool-call and workflow correlation, held
	// under the foreground token. It follows the native process rather than the
	// prompt, so a task started inside a prompt and finished after it stays one
	// task.
	toolUpdates mapper.ToolUpdateOptions
	// autonomousNonce is the callback route this incarnation's native work carries
	// while no prompt owns the foreground. It rotates whenever a prompt dispatches,
	// so a callback captured before that prompt can never become current again when
	// the prompt hands control back. autonomousIncarnation is the exact pump
	// generation that owns every route minted during that process lifetime.
	// autonomousMu guards both and the foreground token, and is a leaf: it is taken
	// under the lifecycle stream's lock and takes nothing itself.
	autonomousMu          sync.Mutex
	autonomousNonce       string
	autonomousIncarnation *nativeIncarnation
	// callbackOwnershipMu is the callback/prompt admission primitive. A native
	// callback registers its exact route and incarnation under it, while prompt
	// dispatch checks the same set before sending. The stream lock is always below
	// this lock.
	callbackOwnershipMu sync.Mutex
	callbackAdmissions  map[*controlCallbackAdmission]struct{}
	// producers gates work that may emit after an establishing response or from
	// detached native-retirement supervision. Close shuts the gate before it
	// tears down the carrier and joins every producer admitted before that point.
	producers sessionProducerGate
	// establishment holds the exact route native callbacks capture while the
	// session response has not reached the host yet.
	establishmentMu    sync.Mutex
	establishment      *sessionEstablishment
	cancelMu           sync.Mutex
	toolCallUpdateMu   sync.Mutex
	imageMu            sync.Mutex
	rawEventMu         sync.Mutex
	mu                 sync.Mutex
	permissionSaveMu   sync.Mutex
	cancel             context.CancelFunc
	turnCancelled      bool
	turnContainmentErr error
	turnNonce          string
	// cancelledNonce names the turn a routed cancel has already applied to. A
	// repeated cancel of that same turn is the same request arriving twice, and
	// applying it twice would re-issue a native interrupt against the process the
	// first one already contained.
	cancelledNonce     string
	publishedToolCalls map[acp.ToolCallId]struct{}
	toolContent        map[acp.ToolCallId][]acp.ToolCallContent
	emittedAgentImages map[string]struct{}
	imageArtifacts     map[string]storedImageArtifact
	permissionCancel   map[string]*permissionRequestCancel
	elicitationCancel  map[int64]*elicitationRequestCancel
	elicitationSeq     int64
	permissionRules    map[string]string
	materialized       *materializedSession
	mcpConfigDir       string
	imageScratchDir    string
	nativeRootRelease  func()
	scratchRootRelease func()
	// Started sessions always mirror transcript rows into the agent's
	// authoritative session store.
	mirror *sessionMirror
	// pump is the session-owned native event loop: one continuous reader for the
	// current native incarnation and one ordered durable outbox.
	pump *nativePump
	// lifecycle is the session's ordered lifecycle stream, present only on a
	// connection whose negotiated answer carries the key.
	lifecycle        *sessionStream
	rawMessages      rawMessageConfig
	rawEventSequence int64
	handledHooks     map[string]struct{}
	handledHookOrder []string
	// closeMu serializes close and guards the memoized terminal result.
	// closeSettled is set only for a close that completed every rung it owed, so
	// neither an abandoned barrier nor a failed boundary latches a terminal result
	// no teardown stands behind: both leave the session closable again.
	closeMu      sync.Mutex
	closeSettled bool
	// pumpServeMu serializes pointing the session's pump at a native incarnation.
	// Retiring the previous identity, minting the new one and publishing the
	// reader that serves it are one step: two callers interleaving them would open
	// two incarnations of one process and leave two readers racing for its frames.
	pumpServeMu sync.Mutex
	// closing is the terminal close state, guarded by mu. It is latched once,
	// before any teardown runs, and never cleared. Every door that would start
	// new native work for this session reads it inside the same critical section
	// that installs that work, so once it is set no such work can begin.
	closing bool
}

type promptLoopState struct {
	promptUsage            *acp.Usage
	lastAssistantMessageID string
	lastAssistantModel     string
	lastStreamUsage        usageSnapshot
	lastStreamUsageKnown   bool
	lastEmittedUsageTotal  int
}

type usageSnapshot struct {
	inputTokens          int
	outputTokens         int
	cacheReadTokens      int
	cacheCreationTokens  int
	reasoningOutputToken int
}

type askUserQuestion struct {
	ID          string
	Question    string
	Header      string
	MultiSelect bool
	IsOther     bool
	IsSecret    bool
	Options     []askUserQuestionOption
}

type askUserQuestionOption struct {
	Label       string
	Description string
}
