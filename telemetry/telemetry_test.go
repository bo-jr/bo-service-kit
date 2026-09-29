package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestLoggerStampsTraceID(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, "svc", "v1")

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))

	l.InfoContext(ctx, "hello")
	l.Info("no span")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %s", len(lines), buf.String())
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["trace_id"] != tid.String() || first["span_id"] != sid.String() {
		t.Fatalf("trace ids missing or wrong: %v", first)
	}
	if first["service"] != "svc" || first["version"] != "v1" {
		t.Fatalf("service/version missing: %v", first)
	}
	if _, ok := second["trace_id"]; ok {
		t.Fatalf("line without a span must not carry trace_id: %v", second)
	}
}

func TestErrorHandlerIsRateLimited(t *testing.T) {
	var buf bytes.Buffer
	h := newRateLimitedErrorHandler(NewLogger(&buf, "svc", "v1"), time.Hour)
	for range 50 {
		h.Handle(errors.New("connection refused"))
	}
	if n := strings.Count(buf.String(), "otel error"); n != 1 {
		t.Fatalf("logged %d times, want 1", n)
	}
}

func TestSetupWithoutExporterStillMintsTraceIDs(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	var buf bytes.Buffer
	tel, err := Setup(context.Background(), Options{Service: "svc", Version: "v1", LogWriter: &buf})
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Close()
	if tel.Exporting {
		t.Fatal("no endpoint configured, but exporter attached")
	}
	_, span := tel.TracerProvider.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if !span.SpanContext().TraceID().IsValid() {
		t.Fatal("no exporter must still mean real trace IDs")
	}
}
