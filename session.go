package claudeacp

import (
	"context"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
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
	jsonFieldIndex                = "index"
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
	jsonFieldSHA256               = "sha256"
	jsonFieldServer               = "server"
	jsonFieldSubpath              = "subpath"
	jsonFieldText                 = "text"
	jsonFieldTitle                = "title"
	jsonFieldType                 = "type"
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

	defaultSessionCloseTurnWait = 5 * time.Second
	maxHandledHooks             = 1024
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

	turn               chan struct{}
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
	mirror           *sessionMirror
	rawMessages      rawMessageConfig
	rawEventSequence int64
	handledHooks     map[string]struct{}
	handledHookOrder []string
	closeTurnWait    time.Duration
	turnAcquiredHook func(int)
	closeOnce        sync.Once
	closeErr         error
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
