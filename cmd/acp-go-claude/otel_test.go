package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestConfigureTelemetry(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("OTEL_METRICS_EXPORTER", "console")
	t.Setenv("OTEL_LOGS_EXPORTER", "console")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")

	base := slog.New(slog.DiscardHandler)
	config, err := configureTelemetry(context.Background(), base, "test-version")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, shutdownTelemetry(context.Background(), config.shutdown))
	})

	require.NotSame(t, base, config.logger)

	var options claudeacp.Options
	for _, opt := range config.options {
		opt(&options)
	}
	require.NotNil(t, options.TextMapPropagator)
	require.NotNil(t, options.TracerProvider)
	require.NotNil(t, options.MeterProvider)
}

func TestConfigureTelemetryNoExporters(t *testing.T) {
	config, err := configureTelemetry(context.Background(), slog.New(slog.DiscardHandler), "test-version")
	require.NoError(t, err)
	require.NoError(t, config.shutdown(context.Background()))

	var options claudeacp.Options
	for _, opt := range config.options {
		opt(&options)
	}
	require.NotNil(t, options.TextMapPropagator)
	require.Nil(t, options.TracerProvider)
	require.Nil(t, options.MeterProvider)
}

func TestConfigureTelemetryErrors(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "bad")
	config, err := configureTelemetry(context.Background(), slog.New(slog.DiscardHandler), "test-version")
	require.Error(t, err)
	require.Nil(t, config.logger)

	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "invalid")

	config, err = configureTelemetry(context.Background(), slog.New(slog.DiscardHandler), "test-version")
	require.Error(t, err)
	require.Nil(t, config.logger)

	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "invalid")
	config, err = configureTelemetry(context.Background(), slog.New(slog.DiscardHandler), "test-version")
	require.Error(t, err)
	require.Nil(t, config.logger)

	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "invalid")
	config, err = configureTelemetry(context.Background(), slog.New(slog.DiscardHandler), "test-version")
	require.Error(t, err)
	require.Nil(t, config.logger)
}

func TestTelemetrySignalEnabled(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	require.False(t, telemetrySignalEnabled("OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", true))

	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	require.True(t, telemetrySignalEnabled("OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", true))

	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	require.True(t, telemetrySignalEnabled("OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", true))

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	require.True(t, telemetrySignalEnabled("OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", true))
	require.False(t, telemetrySignalEnabled("OTEL_LOGS_EXPORTER", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", false))

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	require.True(t, telemetrySignalEnabled("OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", true))

	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=value")
	require.True(t, telemetrySignalEnabled("OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", true))

	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	require.False(t, telemetrySignalEnabled("OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", true))
}

func TestShutdownTelemetry(t *testing.T) {
	require.NoError(t, shutdownTelemetry(context.Background(), nil))

	errBoom := errors.New("boom")
	err := shutdownTelemetry(context.Background(), func(context.Context) error { return errBoom })
	require.ErrorIs(t, err, errBoom)
}

func TestJoinedSlogHandler(t *testing.T) {
	var left bytes.Buffer
	var right bytes.Buffer
	leftHandler := slog.NewTextHandler(&left, &slog.HandlerOptions{Level: slog.LevelInfo})
	rightHandler := slog.NewTextHandler(&right, &slog.HandlerOptions{Level: slog.LevelWarn})
	handler := joinSlogHandlers(nil, leftHandler, rightHandler)

	require.True(t, handler.Enabled(context.Background(), slog.LevelInfo))
	require.NoError(t, handler.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)))
	require.Contains(t, left.String(), "hello")
	require.NotContains(t, right.String(), "hello")

	grouped := handler.WithAttrs([]slog.Attr{slog.String("component", "test")}).WithGroup("group")
	require.NoError(t, grouped.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelWarn, "warn", 0)))
	require.Contains(t, left.String(), "component=test")
	require.Contains(t, right.String(), "component=test")

	empty := joinSlogHandlers(nil)
	require.False(t, empty.Enabled(context.Background(), slog.LevelError))
	require.NoError(t, empty.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "drop", 0)))
}

func TestRunHandlesTelemetryConfigError(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		t.Fatal("serve should not be called")

		return nil
	}

	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "invalid")

	var stderr bytes.Buffer
	code := run(context.Background(), isolatedArgs(), bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)

	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "configure OpenTelemetry")
}

func TestRunHandlesTelemetryShutdownError(t *testing.T) {
	stubProcessIsolationConfig(t)
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return nil
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error {
		return errors.New("flush failed")
	}

	var stderr bytes.Buffer
	code := run(context.Background(), isolatedArgs(), bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)

	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "shutdown OpenTelemetry")
}
