package main

import (
	"context"
	"log/slog"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/savid/acp-go-core/observer/exporters"
)

// configureTelemetry builds the exporters the OTEL_* environment enables and
// maps the configured providers onto the agent's options.
func configureTelemetry(ctx context.Context, baseLogger *slog.Logger, version string) (exporters.Bundle, []claudeacp.Option, error) {
	bundle, err := exporters.Configure(ctx, exporters.Config{Vendor: "claude", Version: version, Logger: baseLogger})
	if err != nil {
		return exporters.Bundle{}, nil, err
	}

	options := []claudeacp.Option{claudeacp.WithTextMapPropagator(bundle.Propagator)}
	if bundle.TracerProvider != nil {
		options = append(options, claudeacp.WithTracerProvider(bundle.TracerProvider))
	}

	if bundle.MeterProvider != nil {
		options = append(options, claudeacp.WithMeterProvider(bundle.MeterProvider))
	}

	return bundle, options, nil
}
