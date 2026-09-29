// Package telemetry sets up tracing and structured logging for a service.
//
// Two properties matter more than anything else here:
//
//  1. Trace IDs exist without a collector. The TracerProvider is always a real
//     SDK provider, so a request gets a trace ID and propagates W3C traceparent
//     even when nothing is exporting. Propagation is what Phase 2 verifies, by
//     finding one trace_id in three services' logs.
//  2. Export never hurts a request. The OTLP exporter is attached only when
//     OTEL_EXPORTER_OTLP_ENDPOINT is set, behind a bounded async batch
//     processor. A dead or absent collector costs dropped spans, never latency,
//     a crash, or a failed readiness check.
package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Options configures Setup.
type Options struct {
	Service string
	Version string
	// LogWriter receives JSON log lines. Nil means stdout.
	LogWriter io.Writer
}

// Telemetry holds what Setup built. Close it on shutdown.
type Telemetry struct {
	Logger         *slog.Logger
	TracerProvider *sdktrace.TracerProvider
	// Exporting reports whether an OTLP exporter is attached.
	Exporting bool
}

// shutdownTimeout bounds the final flush. With a dead collector the flush can
// only fail, and a pod has a fixed termination grace period to spend.
const shutdownTimeout = 5 * time.Second

// Setup builds the logger and TracerProvider and installs both as the process
// globals, along with the W3C TraceContext + Baggage propagator.
func Setup(ctx context.Context, o Options) (*Telemetry, error) {
	w := o.LogWriter
	if w == nil {
		w = os.Stdout
	}
	logger := NewLogger(w, o.Service, o.Version)
	slog.SetDefault(logger)

	otel.SetErrorHandler(newRateLimitedErrorHandler(logger, 30*time.Second))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", o.Service),
		attribute.String("service.version", o.Version),
	))
	if err != nil {
		return nil, err
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}

	exporting := otlpEndpointConfigured()
	if exporting {
		// otlptracehttp reads the standard OTEL_EXPORTER_OTLP_* variables
		// itself. Creating it does not dial; nothing here can block startup.
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		// Defaults are already bounded (queue 2048, drop when full, never
		// block the caller); the export timeout is tightened so a hung
		// collector frees the exporter goroutine sooner.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp,
			sdktrace.WithExportTimeout(5*time.Second)))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)

	logger.Info("telemetry ready", "otlp_export", exporting)
	return &Telemetry{Logger: logger, TracerProvider: tp, Exporting: exporting}, nil
}

// Close flushes and stops the TracerProvider, bounded by shutdownTimeout.
func (t *Telemetry) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := t.TracerProvider.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Logger.Warn("trace flush timed out; unsent spans dropped")
		return nil
	}
	return err
}

func otlpEndpointConfigured() bool {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// NewLogger returns a JSON logger that stamps service, version, and — when the
// context carries a span — trace_id and span_id on every line.
func NewLogger(w io.Writer, service, version string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: logLevel()})
	return slog.New(traceHandler{h}).With("service", service, "version", version)
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// traceHandler adds trace_id/span_id from the record's context. Log with the
// *Context methods (InfoContext, ...) for this to see the span.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(as)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// newRateLimitedErrorHandler logs OTel's internal errors at most once per
// interval. With no collector listening, the exporter fails on every batch; the
// first failure is worth a line, the thousandth is noise that buries the logs
// Phase 2 reads trace IDs from.
func newRateLimitedErrorHandler(l *slog.Logger, every time.Duration) otel.ErrorHandler {
	var (
		mu         sync.Mutex
		last       time.Time
		suppressed int
	)
	return otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if !last.IsZero() && time.Since(last) < every {
			suppressed++
			return
		}
		l.Warn("otel error", "err", err.Error(), "suppressed_since_last", suppressed)
		last, suppressed = time.Now(), 0
	})
}
