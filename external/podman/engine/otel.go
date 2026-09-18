package engine

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// OTELConfig holds OTEL instrumentation handles for the container component.
type OTELConfig struct {
	Tracer trace.Tracer
	Meter  metric.Meter
}

// otelState holds the registered OTEL instruments.
type otelState struct {
	cfg *OTELConfig

	// Observable gauges (updated via callbacks).
	activeCount   atomic.Int64
	poolAvailable atomic.Int64
	poolTotal     atomic.Int64

	// Counters.
	creates  metric.Int64Counter
	execs    metric.Int64Counter
	failures metric.Int64Counter
}

// SetOTEL configures OTEL instrumentation for the container component.
// Call before Start().
func (c *ContainerComponent) SetOTEL(cfg *OTELConfig) {
	if cfg == nil {
		return
	}

	state := &otelState{cfg: cfg}
	meter := cfg.Meter
	if meter == nil {
		c.otel = state
		return
	}

	// Observable gauges.
	meter.Int64ObservableGauge("ycode.container.active",
		metric.WithDescription("Number of active agent containers"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(state.activeCount.Load())
			return nil
		}),
	)

	meter.Int64ObservableGauge("ycode.container.pool.available",
		metric.WithDescription("Number of available containers in the warm pool"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(state.poolAvailable.Load())
			return nil
		}),
	)

	meter.Int64ObservableGauge("ycode.container.pool.total",
		metric.WithDescription("Total capacity of the container pool"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(state.poolTotal.Load())
			return nil
		}),
	)

	// Counters.
	state.creates, _ = meter.Int64Counter("ycode.container.creates",
		metric.WithDescription("Total containers created"),
	)
	state.execs, _ = meter.Int64Counter("ycode.container.execs",
		metric.WithDescription("Total exec operations in containers"),
	)
	state.failures, _ = meter.Int64Counter("ycode.container.failures",
		metric.WithDescription("Total container operation failures"),
	)

	c.otel = state
}

// updateOTELGauges refreshes the gauge values from current state.
func (c *ContainerComponent) updateOTELGauges() {
	if c.otel == nil {
		return
	}

	// Count active containers.
	var count int64
	c.containers.Range(func(_, _ any) bool {
		count++
		return true
	})
	c.otel.activeCount.Store(count)

	// Pool gauges.
	if c.pool != nil {
		c.otel.poolAvailable.Store(int64(c.pool.Available()))
		c.otel.poolTotal.Store(int64(c.pool.Size()))
	}
}

// Tracing helpers.

func (c *ContainerComponent) traceComponentStart(ctx context.Context) {
	if c.otel == nil || c.otel.cfg.Tracer == nil {
		return
	}
	_, span := c.otel.cfg.Tracer.Start(ctx, "ycode.container.component.start",
		trace.WithAttributes(
			attribute.String("session.id", c.sessionID),
			attribute.String("container.image", c.cfg.Image),
			attribute.String("container.network", c.networkName),
		),
	)
	span.End()
}

func (c *ContainerComponent) traceComponentStop(ctx context.Context) {
	if c.otel == nil || c.otel.cfg.Tracer == nil {
		return
	}
	_, span := c.otel.cfg.Tracer.Start(ctx, "ycode.container.component.stop",
		trace.WithAttributes(
			attribute.String("session.id", c.sessionID),
		),
	)
	span.End()
}

func (c *ContainerComponent) traceContainerCreate(ctx context.Context, agentID, containerName string) {
	if c.otel == nil {
		return
	}
	if c.otel.cfg.Tracer != nil {
		_, span := c.otel.cfg.Tracer.Start(ctx, "ycode.container.create",
			trace.WithAttributes(
				attribute.String("agent.id", agentID),
				attribute.String("container.name", containerName),
				attribute.String("container.image", c.cfg.Image),
			),
		)
		span.End()
	}
	if c.otel.creates != nil {
		c.otel.creates.Add(ctx, 1)
	}
}

// RecordContainerExec emits the package-level execs/failures
// counters and a duration histogram for one Container.Exec round-trip.
// Called from Container.Exec where the per-component otel state is
// not reachable (Container has no backlink to its component). Per-
// call meter lookup mirrors the pattern in internal/telemetry/otel.
func RecordContainerExec(ctx context.Context, success bool, dur float64) {
	m := otel.Meter("ycode.container")
	execs, err := m.Int64Counter("ycode.container.execs",
		metric.WithDescription("Total exec operations in containers"),
	)
	if err == nil {
		execs.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", success)))
	}
	hist, err := m.Float64Histogram("ycode.container.exec.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Container.Exec round-trip duration"),
	)
	if err == nil {
		hist.Record(ctx, dur, metric.WithAttributes(attribute.Bool("success", success)))
	}
	if !success {
		failures, ferr := m.Int64Counter("ycode.container.failures",
			metric.WithDescription("Total container operation failures"),
		)
		if ferr == nil {
			failures.Add(ctx, 1)
		}
	}
}

func (c *ContainerComponent) traceContainerRemove(ctx context.Context, agentID string) {
	if c.otel == nil || c.otel.cfg.Tracer == nil {
		return
	}
	_, span := c.otel.cfg.Tracer.Start(ctx, "ycode.container.remove",
		trace.WithAttributes(
			attribute.String("agent.id", agentID),
		),
	)
	span.End()
}
