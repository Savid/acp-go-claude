package observer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	InstrumentationName = "github.com/savid/acp-go-claude"

	attrACPMethod                          = "acp.method"
	attrClaudeClient                       = "claude.client"
	attrClaudePermission                   = "claude.permission.mode"
	attrErrorType                          = "error.type"
	attrGenAIOperation                     = "gen_ai.operation.name"
	attrGenAIProvider                      = "gen_ai.provider.name"
	attrGenAIRequestModel                  = "gen_ai.request.model"
	attrGenAIResponseModel                 = "gen_ai.response.model"
	attrGenAIStopReason                    = "gen_ai.response.finish_reasons"
	attrGenAITokenType                     = "gen_ai.token.type"                        // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrGenAIUsageCacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens" // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrGenAIUsageCacheReadInputTokens     = "gen_ai.usage.cache_read.input_tokens"     // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrGenAIUsageInputTokens              = "gen_ai.usage.input_tokens"                // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrGenAIUsageOutputTokens             = "gen_ai.usage.output_tokens"               // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrGenAIUsageReasoningOutputTokens    = "gen_ai.usage.reasoning.output_tokens"     // #nosec G101 -- OTel semantic-convention attribute, not a secret.
	attrMCPDirection                       = "mcp.bridge.direction"
	attrMCPMethodName                      = "mcp.method.name"
	attrMCPMessageKind                     = "mcp.message.kind"
	attrMCPProtocolVersion                 = "mcp.protocol.version"
	attrMCPTransport                       = "mcp.server.transport"
	attrNetworkTransport                   = "network.transport"
	attrOperation                          = "operation"
	attrOutcome                            = "outcome"
	attrSessionStoreOp                     = "session.store.operation"
	attrStopReason                         = "stop_reason"
	attrToolName                           = "claude.tool.name"
	attrWorkflowFrameSubtype               = "frame.subtype"

	claudeClientValue    = "claude-code"
	genAIOperationChat   = "chat"
	genAIProviderValue   = "anthropic"
	networkTransportPipe = "pipe"

	metaBaggage     = "baggage"
	metaTraceParent = "traceparent"
	metaTraceState  = "tracestate"

	envBaggage     = "BAGGAGE"
	envTraceParent = "TRACEPARENT"
	envTraceState  = "TRACESTATE"

	outcomeCanceled = "canceled"
	outcomeError    = "error"
	outcomeOK       = "ok"
)

type Config struct {
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
	TracerProvider trace.TracerProvider
	Version        string
}

type Observer struct {
	propagator propagation.TextMapPropagator
	tracer     trace.Tracer
	runtime    *runtimeObserver

	acpRequestCount    metric.Int64Counter
	acpRequestDuration metric.Float64Histogram

	genAIOperationDuration        metric.Float64Histogram
	genAITimeToFirstChunk         metric.Float64Histogram
	genAITokenUsage               metric.Int64Histogram
	mcpClientOperationDuration    metric.Float64Histogram
	mcpClientSessionDuration      metric.Float64Histogram
	promptCount                   metric.Int64Counter
	promptDuration                metric.Float64Histogram
	promptCancelCount             metric.Int64Counter
	sessionActive                 metric.Int64UpDownCounter
	permissionCount               metric.Int64Counter
	permissionDuration            metric.Float64Histogram
	elicitationCount              metric.Int64Counter
	elicitationDuration           metric.Float64Histogram
	mcpBridgeMessageCount         metric.Int64Counter
	mcpBridgeMessageDuration      metric.Float64Histogram
	mcpBridgeErrorCount           metric.Int64Counter
	mcpBridgePending              metric.Int64UpDownCounter
	sessionStoreOperationDuration metric.Float64Histogram
	sessionStoreErrorCount        metric.Int64Counter
	rawMessageEmitErrorCount      metric.Int64Counter
	workflowFrameErrorCount       metric.Int64Counter
	claudeProcessExitCount        metric.Int64Counter
}

type ACPResult struct {
	Err   error
	Extra []attribute.KeyValue
}

type PromptResult struct {
	CachedReadTokens  int
	CachedWriteTokens int
	Err               error
	InputTokens       int
	Model             string
	OutputTokens      int
	StopReason        string
	ThoughtTokens     int
	TotalTokens       int
}

type PermissionResult struct {
	Behavior string
	Err      error
	Mode     string
	ToolName string
}

type ElicitationResult struct {
	Accepted bool
	Err      error
}

type MCPMessageResult struct {
	Direction       string
	Err             error
	Kind            string
	Method          string
	ProtocolVersion string
	Transport       string
}

func New(config Config) *Observer {
	tracerProvider := config.TracerProvider
	if tracerProvider == nil {
		tracerProvider = tracenoop.NewTracerProvider()
	}

	meterProvider := config.MeterProvider
	if meterProvider == nil {
		meterProvider = metricnoop.NewMeterProvider()
	}

	propagator := config.Propagator
	if propagator == nil {
		propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)
	}

	tracerOptions := []trace.TracerOption(nil)
	meterOptions := []metric.MeterOption(nil)

	if config.Version != "" {
		tracerOptions = append(tracerOptions, trace.WithInstrumentationVersion(config.Version))
		meterOptions = append(meterOptions, metric.WithInstrumentationVersion(config.Version))
	}

	meter := meterProvider.Meter(InstrumentationName, meterOptions...)
	observer := &Observer{
		propagator: propagator,
		tracer:     tracerProvider.Tracer(InstrumentationName, tracerOptions...),
	}
	observer.runtime = newRuntimeObserver(meter, "acp_go_claude")

	observer.acpRequestCount = mustInt64Counter(meter, "acp_go_claude.acp.request.count", "ACP requests.")
	observer.acpRequestDuration = mustFloat64Histogram(meter, "acp_go_claude.acp.request.duration", "ACP request duration.")
	observer.genAIOperationDuration = mustFloat64Histogram(meter, "gen_ai.client.operation.duration", "Claude prompt operation duration.")
	observer.genAITimeToFirstChunk = mustFloat64Histogram(meter, "gen_ai.client.operation.time_to_first_chunk", "Time to first ACP prompt update.")
	observer.genAITokenUsage = mustInt64Histogram(meter, "gen_ai.client.token.usage", "{token}", "Claude token usage.")
	observer.mcpClientOperationDuration = mustFloat64Histogram(meter, "mcp.client.operation.duration", "MCP client operation duration.")
	observer.mcpClientSessionDuration = mustFloat64Histogram(meter, "mcp.client.session.duration", "MCP client session duration.")
	observer.promptCount = mustInt64Counter(meter, "acp_go_claude.session.prompt.count", "Prompt turns.")
	observer.promptDuration = mustFloat64Histogram(meter, "acp_go_claude.session.prompt.duration", "Prompt turn duration.")
	observer.promptCancelCount = mustInt64Counter(meter, "acp_go_claude.session.cancel.count", "Cancelled prompt turns.")
	observer.sessionActive = mustInt64UpDownCounter(meter, "acp_go_claude.session.active", "Active Claude sessions.")
	observer.permissionCount = mustInt64Counter(meter, "acp_go_claude.permission.request.count", "Permission requests.")
	observer.permissionDuration = mustFloat64Histogram(meter, "acp_go_claude.permission.request.duration", "Permission request duration.")
	observer.elicitationCount = mustInt64Counter(meter, "acp_go_claude.elicitation.request.count", "Elicitation requests.")
	observer.elicitationDuration = mustFloat64Histogram(meter, "acp_go_claude.elicitation.request.duration", "Elicitation request duration.")
	observer.mcpBridgeMessageCount = mustInt64Counter(meter, "acp_go_claude.mcp.bridge.message.count", "MCP bridge messages.")
	observer.mcpBridgeMessageDuration = mustFloat64Histogram(meter, "acp_go_claude.mcp.bridge.message.duration", "MCP bridge message duration.")
	observer.mcpBridgeErrorCount = mustInt64Counter(meter, "acp_go_claude.mcp.bridge.error.count", "MCP bridge errors.")
	observer.mcpBridgePending = mustInt64UpDownCounter(meter, "acp_go_claude.mcp.bridge.pending", "Pending MCP bridge requests.")
	observer.sessionStoreOperationDuration = mustFloat64Histogram(meter, "acp_go_claude.session_store.operation.duration", "Session store operation duration.")
	observer.sessionStoreErrorCount = mustInt64Counter(meter, "acp_go_claude.session_store.error.count", "Session store errors.")
	observer.rawMessageEmitErrorCount = mustInt64Counter(meter, "acp_go_claude.raw_message.emit.error.count", "Raw Claude message emission errors.")
	observer.workflowFrameErrorCount = mustInt64Counter(meter, "acp_go_claude.workflow.frame.error.count", "Malformed Claude workflow frames dropped from mapped updates.")
	observer.claudeProcessExitCount = mustInt64Counter(meter, "acp_go_claude.claude.process.exit.count", "Claude process exits.")

	return observer
}

func mustInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	instrument, _ := meter.Int64Counter(name, metric.WithDescription(description))

	return instrument
}

func mustInt64Gauge(meter metric.Meter, name string, description string) metric.Int64Gauge {
	instrument, _ := meter.Int64Gauge(name, metric.WithDescription(description))

	return instrument
}

func mustInt64Histogram(meter metric.Meter, name string, unit string, description string) metric.Int64Histogram {
	instrument, _ := meter.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))

	return instrument
}

func mustFloat64Histogram(meter metric.Meter, name string, description string) metric.Float64Histogram {
	instrument, _ := meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(description))

	return instrument
}

func mustInt64UpDownCounter(meter metric.Meter, name string, description string) metric.Int64UpDownCounter {
	instrument, _ := meter.Int64UpDownCounter(name, metric.WithDescription(description))

	return instrument
}

func (o *Observer) Extract(ctx context.Context, meta map[string]any) context.Context {
	if o == nil || len(meta) == 0 {
		return ctx
	}

	carrier := propagation.MapCarrier{}

	for _, key := range []string{metaTraceParent, metaTraceState, metaBaggage} {
		value, _ := meta[key].(string)
		if strings.TrimSpace(value) != "" {
			carrier[key] = value
		}
	}

	if len(carrier) == 0 {
		return ctx
	}

	return o.propagator.Extract(ctx, carrier)
}

func (o *Observer) InjectTraceEnv(ctx context.Context, env map[string]string) map[string]string {
	if o == nil {
		return env
	}

	carrier := propagation.MapCarrier{}
	o.propagator.Inject(ctx, carrier)

	if len(carrier) == 0 {
		return env
	}

	if env == nil {
		env = make(map[string]string, len(carrier))
	}

	if value := carrier.Get(metaTraceParent); value != "" {
		env[envTraceParent] = value
	}

	if value := carrier.Get(metaTraceState); value != "" {
		env[envTraceState] = value
	}

	if value := carrier.Get(metaBaggage); value != "" {
		env[envBaggage] = value
	}

	return env
}

func (o *Observer) StartACP(ctx context.Context, meta map[string]any, method string, attrs ...attribute.KeyValue) (context.Context, func(ACPResult)) {
	if o == nil {
		return ctx, func(ACPResult) {}
	}

	ctx = o.Extract(ctx, meta)
	start := time.Now()

	spanAttrs := make([]attribute.KeyValue, 0, 1+len(attrs))
	spanAttrs = append(spanAttrs, attribute.String(attrACPMethod, method))
	spanAttrs = append(spanAttrs, attrs...)

	ctx, span := o.tracer.Start(ctx, spanNameForACPMethod(method), trace.WithAttributes(spanAttrs...))

	return ctx, func(result ACPResult) {
		allAttrs := append(slicesClone(spanAttrs), result.Extra...)
		outcome := outcomeFromError(result.Err)
		allAttrs = append(allAttrs, attribute.String(attrOutcome, outcome))

		if errType := ErrorType(result.Err); errType != "" {
			allAttrs = append(allAttrs, attribute.String(attrErrorType, errType))

			span.RecordError(result.Err)
			span.SetStatus(codes.Error, errType)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		span.SetAttributes(allAttrs...)
		span.End()

		o.acpRequestCount.Add(ctx, 1, metric.WithAttributes(allAttrs...))
		o.acpRequestDuration.Record(ctx, durationSeconds(start), metric.WithAttributes(allAttrs...))
	}
}

func (o *Observer) StartPrompt(ctx context.Context, meta map[string]any, model string) (context.Context, func(PromptResult)) {
	ctx, finishACP := o.StartACP(ctx, meta, "session/prompt", modelAttrs(model)...)
	if o == nil {
		return ctx, func(PromptResult) {}
	}

	state := &promptState{start: time.Now(), model: model}
	ctx = context.WithValue(ctx, promptStateKey{}, state)

	return ctx, func(result PromptResult) {
		promptAttrs := []attribute.KeyValue{
			attribute.String(attrGenAIProvider, genAIProviderValue),
			attribute.String(attrClaudeClient, claudeClientValue),
			attribute.String(attrGenAIOperation, genAIOperationChat),
		}

		promptAttrs = append(promptAttrs, modelAttrs(firstNonEmpty(result.Model, model))...)

		promptAttrs = append(promptAttrs, promptUsageAttrs(result)...)

		if result.StopReason != "" {
			promptAttrs = append(promptAttrs,
				attribute.String(attrStopReason, result.StopReason),
				attribute.StringSlice(attrGenAIStopReason, []string{result.StopReason}),
			)
		}

		outcome := outcomeFromPrompt(result)
		metricAttrs := append(slicesClone(promptAttrs), attribute.String(attrOutcome, outcome))

		if errType := ErrorType(result.Err); errType != "" {
			metricAttrs = append(metricAttrs, attribute.String(attrErrorType, errType))
		}

		duration := durationSeconds(state.start)

		o.promptCount.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
		o.promptDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))
		o.genAIOperationDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))

		if outcome == outcomeCanceled {
			o.promptCancelCount.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
		}

		o.recordTokenUsage(ctx, result, promptAttrs)
		finishACP(ACPResult{Err: result.Err, Extra: promptAttrs})
	}
}

func (o *Observer) ObserveFirstPromptUpdate(ctx context.Context) {
	if o == nil {
		return
	}

	state, _ := ctx.Value(promptStateKey{}).(*promptState)
	if state == nil {
		return
	}

	state.mu.Lock()
	if state.observed {
		state.mu.Unlock()

		return
	}

	state.observed = true
	start := state.start
	model := state.model
	state.mu.Unlock()

	attrs := make([]attribute.KeyValue, 0, 4)
	attrs = append(attrs,
		attribute.String(attrGenAIProvider, genAIProviderValue),
		attribute.String(attrClaudeClient, claudeClientValue),
		attribute.String(attrGenAIOperation, genAIOperationChat),
	)
	attrs = append(attrs, modelAttrs(model)...)
	o.genAITimeToFirstChunk.Record(ctx, durationSeconds(start), metric.WithAttributes(attrs...))
}

func (o *Observer) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error, ...attribute.KeyValue)) {
	if o == nil {
		return ctx, func(error, ...attribute.KeyValue) {}
	}

	ctx, span := o.tracer.Start(ctx, name, trace.WithAttributes(attrs...))

	return ctx, func(err error, extra ...attribute.KeyValue) {
		if errType := ErrorType(err); errType != "" {
			extra = append(extra, attribute.String(attrErrorType, errType))

			span.RecordError(err)
			span.SetStatus(codes.Error, errType)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		if len(extra) > 0 {
			span.SetAttributes(extra...)
		}

		span.End()
	}
}

func (o *Observer) StartClaudeProcess(ctx context.Context, operation string) (context.Context, func(error)) {
	ctx, finish := o.StartSpan(ctx, "claude.process."+operation,
		attribute.String(attrOperation, operation),
		attribute.String(attrClaudeClient, claudeClientValue),
		attribute.String(attrGenAIProvider, genAIProviderValue),
	)

	return ctx, func(err error) { finish(err) }
}

func (o *Observer) RecordClaudeProcessExit(ctx context.Context, outcome string, err error) {
	if o == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String(attrOutcome, firstNonEmpty(outcome, outcomeFromError(err))),
		attribute.String(attrClaudeClient, claudeClientValue),
	}
	if errType := ErrorType(err); errType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errType))
	}

	o.claudeProcessExitCount.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (o *Observer) AddActiveSession(ctx context.Context, delta int64) {
	if o == nil || delta == 0 {
		return
	}

	o.sessionActive.Add(ctx, delta)
}

func (o *Observer) StartPermission(ctx context.Context, toolName string, mode string) (context.Context, func(PermissionResult)) {
	if o == nil {
		return ctx, func(PermissionResult) {}
	}

	start := time.Now()

	attrs := []attribute.KeyValue{
		attribute.String(attrClaudeClient, claudeClientValue),
	}
	if toolName != "" {
		attrs = append(attrs, attribute.String(attrToolName, toolName))
	}

	if mode != "" {
		attrs = append(attrs, attribute.String(attrClaudePermission, mode))
	}

	ctx, span := o.tracer.Start(ctx, "acp.permission.request", trace.WithAttributes(attrs...))

	return ctx, func(result PermissionResult) {
		finalAttrs := slicesClone(attrs)
		if result.Behavior != "" {
			finalAttrs = append(finalAttrs, attribute.String(attrOutcome, result.Behavior))
		} else {
			finalAttrs = append(finalAttrs, attribute.String(attrOutcome, outcomeFromError(result.Err)))
		}

		if errType := ErrorType(result.Err); errType != "" {
			finalAttrs = append(finalAttrs, attribute.String(attrErrorType, errType))

			span.RecordError(result.Err)
			span.SetStatus(codes.Error, errType)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		if result.Mode != "" {
			finalAttrs = append(finalAttrs, attribute.String(attrClaudePermission, result.Mode))
		}

		if result.ToolName != "" && result.ToolName != toolName {
			finalAttrs = append(finalAttrs, attribute.String(attrToolName, result.ToolName))
		}

		span.SetAttributes(finalAttrs...)
		span.End()

		metricAttrs := removeAttribute(finalAttrs, attrToolName)
		o.permissionCount.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
		o.permissionDuration.Record(ctx, durationSeconds(start), metric.WithAttributes(metricAttrs...))
	}
}

func (o *Observer) StartElicitation(ctx context.Context) (context.Context, func(ElicitationResult)) {
	if o == nil {
		return ctx, func(ElicitationResult) {}
	}

	start := time.Now()
	attrs := []attribute.KeyValue{attribute.String(attrClaudeClient, claudeClientValue)}
	ctx, span := o.tracer.Start(ctx, "acp.elicitation.request", trace.WithAttributes(attrs...))

	return ctx, func(result ElicitationResult) {
		outcome := outcomeOK
		if !result.Accepted {
			outcome = "declined"
		}

		if result.Err != nil {
			outcome = outcomeError
		}

		finalAttrs := append(slicesClone(attrs), attribute.String(attrOutcome, outcome))
		if errType := ErrorType(result.Err); errType != "" {
			finalAttrs = append(finalAttrs, attribute.String(attrErrorType, errType))

			span.RecordError(result.Err)
			span.SetStatus(codes.Error, errType)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		span.SetAttributes(finalAttrs...)
		span.End()
		o.elicitationCount.Add(ctx, 1, metric.WithAttributes(finalAttrs...))
		o.elicitationDuration.Record(ctx, durationSeconds(start), metric.WithAttributes(finalAttrs...))
	}
}

func (o *Observer) RecordMCPPending(ctx context.Context, delta int64, transport string) {
	if o == nil || delta == 0 {
		return
	}

	attrs := []attribute.KeyValue(nil)
	if transport != "" {
		attrs = append(attrs, attribute.String(attrMCPTransport, transport))
	}

	o.mcpBridgePending.Add(ctx, delta, metric.WithAttributes(attrs...))
}

func (o *Observer) StartMCPBridge(ctx context.Context, operation string, result MCPMessageResult) (context.Context, func(error)) {
	attrs := mcpBridgeAttrs(result)
	if operation != "" {
		attrs = append(attrs, attribute.String(attrOperation, operation))
	}

	ctx, finish := o.StartSpan(ctx, "acp.mcp.bridge."+operation, attrs...)

	return ctx, func(err error) { finish(err) }
}

func (o *Observer) RecordMCPBridgeMessage(ctx context.Context, start time.Time, result MCPMessageResult) {
	if o == nil {
		return
	}

	duration := durationSeconds(start)

	attrs := append(
		[]attribute.KeyValue{attribute.String(attrOutcome, outcomeFromError(result.Err))},
		mcpBridgeAttrs(result)...,
	)
	standardAttrs := append(
		[]attribute.KeyValue{attribute.String(attrOutcome, outcomeFromError(result.Err))},
		mcpStandardAttrs(result)...,
	)

	if errType := ErrorType(result.Err); errType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errType))
		standardAttrs = append(standardAttrs, attribute.String(attrErrorType, errType))

		o.mcpBridgeErrorCount.Add(ctx, 1, metric.WithAttributes(attrs...))
	}

	o.mcpBridgeMessageCount.Add(ctx, 1, metric.WithAttributes(attrs...))
	o.mcpBridgeMessageDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
	o.mcpClientOperationDuration.Record(ctx, duration, metric.WithAttributes(standardAttrs...))
}

func (o *Observer) RecordMCPSession(ctx context.Context, start time.Time, result MCPMessageResult) {
	if o == nil || start.IsZero() {
		return
	}

	attrs := append(
		[]attribute.KeyValue{attribute.String(attrOutcome, outcomeFromError(result.Err))},
		mcpStandardAttrs(result)...,
	)
	if errType := ErrorType(result.Err); errType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errType))
	}

	o.mcpClientSessionDuration.Record(ctx, durationSeconds(start), metric.WithAttributes(attrs...))
}

func mcpBridgeAttrs(result MCPMessageResult) []attribute.KeyValue {
	attrs := mcpStandardAttrs(result)
	if result.Direction != "" {
		attrs = append(attrs, attribute.String(attrMCPDirection, result.Direction))
	}

	if result.Kind != "" {
		attrs = append(attrs, attribute.String(attrMCPMessageKind, result.Kind))
	}

	if result.Transport != "" {
		attrs = append(attrs, attribute.String(attrMCPTransport, result.Transport))
	}

	return attrs
}

func mcpStandardAttrs(result MCPMessageResult) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(attrNetworkTransport, networkTransportPipe)}
	if result.Method != "" {
		attrs = append(attrs, attribute.String(attrMCPMethodName, result.Method))
	}

	if result.ProtocolVersion != "" {
		attrs = append(attrs, attribute.String(attrMCPProtocolVersion, result.ProtocolVersion))
	}

	return attrs
}

func (o *Observer) RecordSessionStore(ctx context.Context, start time.Time, operation string, err error) {
	if o == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String(attrSessionStoreOp, operation),
		attribute.String(attrOutcome, outcomeFromError(err)),
	}
	if errType := ErrorType(err); errType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errType))
		o.sessionStoreErrorCount.Add(ctx, 1, metric.WithAttributes(attrs...))
	}

	o.sessionStoreOperationDuration.Record(ctx, durationSeconds(start), metric.WithAttributes(attrs...))
}

func (o *Observer) StartSessionStore(ctx context.Context, operation string) (context.Context, func(error)) {
	start := time.Now()
	ctx, finishSpan := o.StartSpan(ctx, "acp.session_store."+operation, attribute.String(attrSessionStoreOp, operation))

	return ctx, func(err error) {
		finishSpan(err)
		o.RecordSessionStore(ctx, start, operation, err)
	}
}

func (o *Observer) RecordRawMessageEmitFailure(ctx context.Context, err error) {
	if o == nil {
		return
	}

	attrs := []attribute.KeyValue{attribute.String(attrOutcome, outcomeFromError(err))}
	if errType := ErrorType(err); errType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errType))
	}

	o.rawMessageEmitErrorCount.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (o *Observer) RecordWorkflowFrameError(ctx context.Context, outcome string, errorType string, frameSubtype string) {
	if o == nil {
		return
	}

	attrs := []attribute.KeyValue(nil)
	if outcome != "" {
		attrs = append(attrs, attribute.String(attrOutcome, outcome))
	}

	if errorType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, errorType))
	}

	if frameSubtype != "" {
		attrs = append(attrs, attribute.String(attrWorkflowFrameSubtype, frameSubtype))
	}

	o.workflowFrameErrorCount.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (o *Observer) recordTokenUsage(ctx context.Context, result PromptResult, attrs []attribute.KeyValue) {
	inputTokens := result.InputTokens + result.CachedReadTokens + result.CachedWriteTokens
	if inputTokens > 0 {
		o.genAITokenUsage.Record(ctx, int64(inputTokens), metric.WithAttributes(appendTokenType(attrs, "input")...))
	}

	if result.OutputTokens > 0 {
		o.genAITokenUsage.Record(ctx, int64(result.OutputTokens), metric.WithAttributes(appendTokenType(attrs, "output")...))
	}
}

func promptUsageAttrs(result PromptResult) []attribute.KeyValue {
	attrs := []attribute.KeyValue(nil)

	inputTokens := result.InputTokens + result.CachedReadTokens + result.CachedWriteTokens
	if inputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIUsageInputTokens, inputTokens))
	}

	if result.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIUsageOutputTokens, result.OutputTokens))
	}

	if result.CachedWriteTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIUsageCacheCreationInputTokens, result.CachedWriteTokens))
	}

	if result.CachedReadTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIUsageCacheReadInputTokens, result.CachedReadTokens))
	}

	if result.ThoughtTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIUsageReasoningOutputTokens, result.ThoughtTokens))
	}

	return attrs
}

func appendTokenType(attrs []attribute.KeyValue, tokenType string) []attribute.KeyValue {
	cloned := slicesClone(attrs)

	return append(cloned, attribute.String(attrGenAITokenType, tokenType))
}

func removeAttribute(attrs []attribute.KeyValue, key string) []attribute.KeyValue {
	filtered := attrs[:0]
	for _, attr := range attrs {
		if string(attr.Key) == key {
			continue
		}

		filtered = append(filtered, attr)
	}

	return filtered
}

func modelAttrs(model string) []attribute.KeyValue {
	if strings.TrimSpace(model) == "" {
		return nil
	}

	return []attribute.KeyValue{
		attribute.String(attrGenAIRequestModel, model),
		attribute.String(attrGenAIResponseModel, model),
	}
}

func spanNameForACPMethod(method string) string {
	return "acp." + strings.ReplaceAll(method, "/", ".")
}

func outcomeFromPrompt(result PromptResult) string {
	if result.Err != nil {
		return outcomeError
	}

	if strings.EqualFold(result.StopReason, "cancelled") || strings.EqualFold(result.StopReason, "canceled") {
		return outcomeCanceled
	}

	return outcomeOK
}

func outcomeFromError(err error) string {
	if err == nil {
		return outcomeOK
	}

	if errors.Is(err, context.Canceled) {
		return outcomeCanceled
	}

	return outcomeError
}

func ErrorType(err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.Canceled) {
		return "context.Canceled"
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "context.DeadlineExceeded"
	}

	typ := reflect.TypeOf(err)

	return typ.String()
}

func durationSeconds(start time.Time) float64 {
	return time.Since(start).Seconds()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func slicesClone[T any](values []T) []T {
	return append([]T(nil), values...)
}

type promptStateKey struct{}

type promptState struct {
	mu       sync.Mutex
	model    string
	observed bool
	start    time.Time
}
