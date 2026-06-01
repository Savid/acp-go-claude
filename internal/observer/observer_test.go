package observer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testTraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestObserverPromptTraceMetricsAndPrivacy(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	observe := New(Config{
		MeterProvider:  meterProvider,
		TracerProvider: tracerProvider,
		Version:        "test-version",
	})

	ctx, finish := observe.StartPrompt(context.Background(), map[string]any{
		metaTraceParent: testTraceParent,
		metaTraceState:  "vendor=value",
		metaBaggage:     "tenant=test",
		"prompt":        "secret prompt text",
		"tool_input":    "secret tool input",
		"tool_output":   "secret tool output",
		"authorization": "secret auth token",
		"mcp_token":     "secret mcp token",
		"rawClaudeJSON": `{"secret":"raw claude json"}`,
	}, "claude-opus-4-7")

	env := observe.InjectTraceEnv(ctx, nil)
	require.Contains(t, env[envTraceParent], "4bf92f3577b34da6a3ce929d0e0e4736")
	require.Equal(t, "vendor=value", env[envTraceState])
	require.Equal(t, "tenant=test", env[envBaggage])

	observe.ObserveFirstPromptUpdate(ctx)
	observe.ObserveFirstPromptUpdate(ctx)
	finish(PromptResult{
		CachedReadTokens:  5,
		CachedWriteTokens: 2,
		InputTokens:       7,
		Model:             "claude-opus-4-7",
		OutputTokens:      11,
		StopReason:        "end_turn",
		ThoughtTokens:     3,
		TotalTokens:       28,
	})

	spans := tracetest.SpanStubsFromReadOnlySpans(recorder.Ended())
	require.Len(t, spans, 1)
	require.Equal(t, "acp.session.prompt", spans[0].Name)
	require.Equal(t, "test-version", spans[0].InstrumentationScope.Version)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spans[0].Parent.TraceID().String())
	require.Equal(t, "00f067aa0ba902b7", spans[0].Parent.SpanID().String())
	requireSpanAttributeInt(t, spans[0], attrGenAIUsageInputTokens, 14)
	requireSpanAttributeInt(t, spans[0], attrGenAIUsageOutputTokens, 11)
	requireSpanAttributeInt(t, spans[0], attrGenAIUsageCacheCreationInputTokens, 2)
	requireSpanAttributeInt(t, spans[0], attrGenAIUsageCacheReadInputTokens, 5)
	requireSpanAttributeInt(t, spans[0], attrGenAIUsageReasoningOutputTokens, 3)

	metrics := collectMetrics(t, reader)
	requireMetric(t, metrics, "acp_go_claude.acp.request.count")
	requireMetric(t, metrics, "acp_go_claude.acp.request.duration")
	requireMetric(t, metrics, "acp_go_claude.session.prompt.count")
	requireMetric(t, metrics, "acp_go_claude.session.prompt.duration")
	requireMetric(t, metrics, "gen_ai.client.operation.duration")
	requireMetric(t, metrics, "gen_ai.client.operation.time_to_first_chunk")
	requireMetric(t, metrics, "gen_ai.client.token.usage")
	requireHistogramCount(t, metrics, "gen_ai.client.operation.time_to_first_chunk", 1)
	requireIntHistogramSum(t, metrics, "gen_ai.client.token.usage", attrGenAITokenType, "input", 14)
	requireIntHistogramSum(t, metrics, "gen_ai.client.token.usage", attrGenAITokenType, "output", 11)

	for _, forbidden := range []string{
		"secret prompt text",
		"secret tool input",
		"secret tool output",
		"secret auth token",
		"secret mcp token",
		"raw claude json",
	} {
		requireAttributesExclude(t, spans[0].Attributes, forbidden)
		requireMetricsExclude(t, metrics, forbidden)
	}
}

func TestObserverRecordsAdapterSignals(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	recorder := tracetest.NewSpanRecorder()
	observe := New(Config{
		MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		TracerProvider: sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithSpanProcessor(recorder),
		),
	})

	errBoom := errors.New("boom")
	ctx, finishACP := observe.StartACP(context.Background(), nil, "session/load")
	finishACP(ACPResult{
		Err: errBoom,
		Extra: []attribute.KeyValue{
			attribute.String("custom", "structural"),
		},
	})

	ctx, finishPromptErr := observe.StartPrompt(ctx, nil, "")
	finishPromptErr(PromptResult{Err: errBoom})

	ctx, finishPromptCanceled := observe.StartPrompt(ctx, nil, "")
	finishPromptCanceled(PromptResult{StopReason: "cancelled"})

	ctx, finishProcess := observe.StartClaudeProcess(ctx, "start")
	finishProcess(nil)
	observe.RecordClaudeProcessExit(ctx, "closed", nil)
	observe.RecordClaudeProcessExit(ctx, "", errBoom)
	observe.AddActiveSession(ctx, 1)
	observe.AddActiveSession(ctx, -1)
	observe.AddActiveSession(ctx, 0)

	ctx, finishPermission := observe.StartPermission(ctx, "Write", "default")
	finishPermission(PermissionResult{Behavior: "allow", ToolName: "Write", Mode: "default"})

	ctx, finishPermissionErr := observe.StartPermission(ctx, "", "")
	finishPermissionErr(PermissionResult{Err: errBoom, ToolName: "Read", Mode: "plan"})

	ctx, finishElicitation := observe.StartElicitation(ctx)
	finishElicitation(ElicitationResult{Accepted: false, Err: errBoom})

	ctx, finishElicitationOK := observe.StartElicitation(ctx)
	finishElicitationOK(ElicitationResult{Accepted: true})

	ctx, finishCustomSpan := observe.StartSpan(ctx, "custom.span")
	finishCustomSpan(errBoom, attribute.String("custom", "value"))

	observe.RecordMCPPending(ctx, 1, "acp")
	observe.RecordMCPPending(ctx, -1, "acp")
	ctx, finishMCPBridge := observe.StartMCPBridge(ctx, "connect", MCPMessageResult{
		Direction:       "proxy_to_acp",
		Kind:            "hello",
		Method:          "initialize",
		ProtocolVersion: "2025-06-18",
		Transport:       "acp",
	})
	finishMCPBridge(nil)
	observe.RecordMCPBridgeMessage(ctx, time.Now().Add(-time.Millisecond), MCPMessageResult{
		Direction:       "proxy_to_acp",
		Err:             errBoom,
		Kind:            "request",
		Method:          "tools/call",
		ProtocolVersion: "2025-06-18",
		Transport:       "acp",
	})
	observe.RecordMCPSession(ctx, time.Now().Add(-time.Millisecond), MCPMessageResult{
		ProtocolVersion: "2025-06-18",
		Transport:       "acp",
	})
	observe.RecordMCPSession(ctx, time.Now().Add(-time.Millisecond), MCPMessageResult{
		Err:       errBoom,
		Transport: "acp",
	})
	observe.RecordMCPSession(ctx, time.Time{}, MCPMessageResult{Transport: "acp"})
	observe.RecordSessionStore(ctx, time.Now().Add(-time.Millisecond), "load", errBoom)
	ctx, finishStore := observe.StartSessionStore(ctx, "materialize")
	finishStore(nil)
	observe.RecordRawMessageEmitFailure(ctx, errBoom)
	observe.RecordWorkflowFrameError(ctx, "dropped", "bad_workflow_progress", "task_progress")

	metrics := collectMetrics(t, reader)
	for _, name := range []string{
		"acp_go_claude.acp.request.count",
		"acp_go_claude.claude.process.exit.count",
		"acp_go_claude.session.active",
		"acp_go_claude.permission.request.count",
		"acp_go_claude.elicitation.request.count",
		"acp_go_claude.mcp.bridge.pending",
		"acp_go_claude.mcp.bridge.message.count",
		"acp_go_claude.mcp.bridge.error.count",
		"mcp.client.operation.duration",
		"mcp.client.session.duration",
		"acp_go_claude.session_store.operation.duration",
		"acp_go_claude.session_store.error.count",
		"acp_go_claude.raw_message.emit.error.count",
		"acp_go_claude.workflow.frame.error.count",
	} {
		requireMetric(t, metrics, name)
	}

	spans := tracetest.SpanStubsFromReadOnlySpans(recorder.Ended())
	requireSpan(t, spans, "acp.session.load")
	requireSpan(t, spans, "claude.process.start")
	requireSpan(t, spans, "acp.permission.request")
	requireSpan(t, spans, "acp.elicitation.request")
	mcpSpan := requireSpan(t, spans, "acp.mcp.bridge.connect")
	requireSpanAttributeString(t, mcpSpan, attrNetworkTransport, networkTransportPipe)
	requireSpanAttributeString(t, mcpSpan, attrMCPMethodName, "initialize")
	requireSpanAttributeString(t, mcpSpan, attrMCPProtocolVersion, "2025-06-18")
	requireSpan(t, spans, "acp.session_store.materialize")
	requireSpan(t, spans, "custom.span")
}

func TestObserverNoopAndHelpers(t *testing.T) {
	var observe *Observer

	ctx := context.Background()
	_, finishACP := observe.StartACP(ctx, nil, "initialize")
	finishACP(ACPResult{})

	ctx, finishPrompt := observe.StartPrompt(ctx, nil, "")
	finishPrompt(PromptResult{Err: context.Canceled, StopReason: "cancelled"})
	observe.ObserveFirstPromptUpdate(ctx)

	ctx, finishSpan := observe.StartSpan(ctx, "custom")
	finishSpan(nil)

	ctx, finishProcess := observe.StartClaudeProcess(ctx, "interrupt")
	finishProcess(context.DeadlineExceeded)

	ctx, finishPermission := observe.StartPermission(ctx, "", "")
	finishPermission(PermissionResult{})

	ctx, finishElicitation := observe.StartElicitation(ctx)
	finishElicitation(ElicitationResult{Accepted: true})

	observe.RecordClaudeProcessExit(ctx, "", nil)
	observe.RecordMCPPending(ctx, 0, "")
	observe.RecordMCPBridgeMessage(ctx, time.Now(), MCPMessageResult{})
	observe.RecordMCPSession(ctx, time.Time{}, MCPMessageResult{})
	observe.RecordSessionStore(ctx, time.Now(), "", nil)
	observe.RecordRawMessageEmitFailure(ctx, nil)
	observe.RecordWorkflowFrameError(ctx, "", "", "")
	require.Nil(t, observe.InjectTraceEnv(ctx, nil))
	require.Equal(t, ctx, observe.Extract(ctx, nil))

	require.Equal(t, "", ErrorType(nil))
	require.Equal(t, "context.Canceled", ErrorType(context.Canceled))
	require.Equal(t, "context.DeadlineExceeded", ErrorType(context.DeadlineExceeded))
	require.Equal(t, "*errors.errorString", ErrorType(errors.New("x")))
	require.Equal(t, "fallback", firstNonEmpty("", "fallback"))
	require.Equal(t, "", firstNonEmpty("", " "))
	require.Equal(t, "acp.session.prompt", spanNameForACPMethod("session/prompt"))
	require.Equal(t, []attribute.KeyValue(nil), modelAttrs(""))
	require.Equal(t, []attribute.KeyValue{attribute.String(attrNetworkTransport, networkTransportPipe)}, mcpBridgeAttrs(MCPMessageResult{}))
	require.Empty(t, promptUsageAttrs(PromptResult{}))
	require.Len(t, removeAttribute([]attribute.KeyValue{
		attribute.String(attrToolName, "Read"),
		attribute.String(attrOutcome, outcomeOK),
	}, attrToolName), 1)
	require.Equal(t, outcomeCanceled, outcomeFromError(context.Canceled))
	require.Equal(t, outcomeError, outcomeFromError(errors.New("x")))
	require.Equal(t, outcomeOK, outcomeFromPrompt(PromptResult{}))
	require.Equal(t, outcomeCanceled, outcomeFromPrompt(PromptResult{StopReason: "cancelled"}))
	require.Equal(t, outcomeError, outcomeFromPrompt(PromptResult{Err: errors.New("x")}))
	require.InDelta(t, 0.001, durationSeconds(time.Now().Add(-time.Millisecond)), 0.05)
	require.Equal(t, []int{1, 2}, slicesClone([]int{1, 2}))
}

func TestObserverDefaultsAndEmptyPropagation(t *testing.T) {
	observe := New(Config{})
	ctx := context.Background()
	require.Equal(t, ctx, observe.Extract(ctx, map[string]any{"traceparent": 42}))
	require.Equal(t, map[string]string{"keep": "value"}, observe.InjectTraceEnv(ctx, map[string]string{"keep": "value"}))

	ctx, finishPrompt := observe.StartPrompt(ctx, nil, "")
	finishPrompt(PromptResult{Err: context.Canceled, StopReason: "cancelled"})
	observe.ObserveFirstPromptUpdate(context.Background())

	_, finishACP := observe.StartACP(ctx, nil, "initialize")
	finishACP(ACPResult{})
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))

	var metrics []metricdata.Metrics
	for _, scope := range data.ScopeMetrics {
		require.Equal(t, InstrumentationName, scope.Scope.Name)
		metrics = append(metrics, scope.Metrics...)
	}

	return metrics
}

func requireMetric(t *testing.T, metrics []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()

	for _, metric := range metrics {
		if metric.Name == name {
			return metric
		}
	}

	require.Failf(t, "metric not found", "missing %s in %v", name, metricNames(metrics))

	return metricdata.Metrics{}
}

func requireHistogramCount(t *testing.T, metrics []metricdata.Metrics, name string, count uint64) {
	t.Helper()

	metric := requireMetric(t, metrics, name)
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.Truef(t, ok, "%s has type %T", name, metric.Data)
	require.Len(t, histogram.DataPoints, 1)
	require.Equal(t, count, histogram.DataPoints[0].Count)
}

func requireSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()

	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}

	require.Failf(t, "span not found", "missing %s in %v", name, spanNames(spans))

	return tracetest.SpanStub{}
}

func requireSpanAttributeString(t *testing.T, span tracetest.SpanStub, key string, expected string) {
	t.Helper()

	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			require.Equal(t, expected, attr.Value.AsString())

			return
		}
	}

	require.Failf(t, "attribute not found", "missing %s in %v", key, span.Attributes)
}

func requireSpanAttributeInt(t *testing.T, span tracetest.SpanStub, key string, expected int64) {
	t.Helper()

	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			require.Equal(t, expected, attr.Value.AsInt64())

			return
		}
	}

	require.Failf(t, "attribute not found", "missing %s in %v", key, span.Attributes)
}

func requireIntHistogramSum(
	t *testing.T,
	metrics []metricdata.Metrics,
	name string,
	attrKey string,
	attrValue string,
	expected int64,
) {
	t.Helper()

	metric := requireMetric(t, metrics, name)
	histogram, ok := metric.Data.(metricdata.Histogram[int64])
	require.Truef(t, ok, "%s has type %T", name, metric.Data)

	for _, point := range histogram.DataPoints {
		value, ok := point.Attributes.Value(attribute.Key(attrKey))
		if ok && value.AsString() == attrValue {
			require.Equal(t, uint64(1), point.Count)
			require.Equal(t, expected, point.Sum)

			return
		}
	}

	require.Failf(t, "histogram point not found", "missing %s=%s in %s", attrKey, attrValue, name)
}

func requireAttributesExclude(t *testing.T, attrs []attribute.KeyValue, forbidden string) {
	t.Helper()

	for _, attr := range attrs {
		require.NotContains(t, attr.Value.AsString(), forbidden)
	}
}

func requireMetricsExclude(t *testing.T, metrics []metricdata.Metrics, forbidden string) {
	t.Helper()

	for _, metric := range metrics {
		switch data := metric.Data.(type) {
		case metricdata.Sum[int64]:
			for _, point := range data.DataPoints {
				requireAttributeSetExcludes(t, point.Attributes, forbidden)
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				requireAttributeSetExcludes(t, point.Attributes, forbidden)
			}
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				requireAttributeSetExcludes(t, point.Attributes, forbidden)
			}
		}
	}
}

func requireAttributeSetExcludes(t *testing.T, attrs attribute.Set, forbidden string) {
	t.Helper()

	attrs.Encoded(attribute.DefaultEncoder())
	for _, attr := range attrs.ToSlice() {
		require.False(t, strings.Contains(string(attr.Key), forbidden))
		require.NotContains(t, attr.Value.AsString(), forbidden)
	}
}

func metricNames(metrics []metricdata.Metrics) []string {
	names := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		names = append(names, metric.Name)
	}

	return names
}

func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}

	return names
}
