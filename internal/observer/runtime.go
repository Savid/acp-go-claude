package observer

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type runtimeObserver struct {
	admissions metric.Int64Counter
	resources  metric.Int64UpDownCounter
	stages     metric.Float64Histogram
}

func newRuntimeObserver(meter metric.Meter, prefix string) *runtimeObserver {
	return &runtimeObserver{
		admissions: mustInt64Counter(meter, prefix+".runtime.resource.admission.count", "Runtime resource admission decisions."),
		resources:  mustInt64UpDownCounter(meter, prefix+".runtime.resource.active", "Live adapter scratch-root reservations."),
		stages:     mustFloat64Histogram(meter, prefix+".runtime.startup.stage.duration", "Native startup stage duration."),
	}
}

func (o *Observer) RecordRuntimeResourceAdmission(ctx context.Context, resource, lifecycle, outcome string) {
	if o == nil || o.runtime == nil {
		return
	}

	o.runtime.admissions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("runtime.resource", resource),
		attribute.String("runtime.lifecycle", lifecycle),
		attribute.String("runtime.outcome", outcome),
	))
}

func (o *Observer) AddRuntimeResource(ctx context.Context, resource string, delta int64) {
	if o == nil || o.runtime == nil || delta == 0 {
		return
	}

	o.runtime.resources.Add(ctx, delta, metric.WithAttributes(attribute.String("runtime.resource", resource)))
}

func (o *Observer) ObserveRuntimeStartupStage(ctx context.Context, lifecycle, stage string, elapsed time.Duration, err error) {
	if o == nil || o.runtime == nil {
		return
	}

	status := "ok"
	if err != nil {
		status = "error"
	}

	o.runtime.stages.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
		attribute.String("runtime.lifecycle", lifecycle),
		attribute.String("runtime.stage", stage),
		attribute.String("runtime.status", status),
	))
}
