package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the decoded JSON log lines whose msg equals msg.
func (b *syncBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// clearEnv resets every variable the kit reads, so tests do not inherit the
// developer's shell.
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"FAILURE_RATE", "EXTRA_LATENCY_MS", "CPU_BURN_FACTOR", "APP_VERSION",
		"DEPENDENCIES", "PORT", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		t.Setenv(k, "")
	}
}

func newTestServer(t *testing.T, service string, logs io.Writer) *Server {
	t.Helper()
	s, err := New(context.Background(), service, WithLogWriter(logs), WithDrainDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMetricsUseRoutePatternAndSkipProbes(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_VERSION", "v7")
	s := newTestServer(t, "svc", io.Discard)
	s.Handle("GET /items/{sku}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get(t, ts.URL+"/items/SKU-0001", nil)
	get(t, ts.URL+"/items/SKU-0002", nil)
	get(t, ts.URL+"/healthz", nil)
	get(t, ts.URL+"/readyz", nil)
	_, body := get(t, ts.URL+"/metrics", nil)

	want := `http_requests_total{route="GET /items/{sku}",service="svc",status="200",version="v7"} 2`
	if !strings.Contains(body, want) {
		t.Fatalf("missing %q in:\n%s", want, grep(body, "http_requests_total"))
	}
	if !strings.Contains(body, `http_request_duration_seconds_count{route="GET /items/{sku}",service="svc",status="200",version="v7"} 2`) {
		t.Fatalf("histogram missing:\n%s", grep(body, "http_request_duration_seconds_count"))
	}
	for _, bad := range []string{"SKU-0001", "/healthz", "/readyz", "/metrics"} {
		if strings.Contains(grep(body, "http_request"), bad) {
			t.Fatalf("metrics leak %q:\n%s", bad, grep(body, "http_request"))
		}
	}
}

func TestInjectedFailuresAreCountedAndProbesUnaffected(t *testing.T) {
	clearEnv(t)
	t.Setenv("FAILURE_RATE", "1")
	s := newTestServer(t, "svc", io.Discard)
	s.Handle("GET /work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if code, _ := get(t, ts.URL+"/work", nil); code != http.StatusInternalServerError {
		t.Fatalf("FAILURE_RATE=1 returned %d", code)
	}
	if code, _ := get(t, ts.URL+"/healthz", nil); code != http.StatusOK {
		t.Fatalf("/healthz must ignore chaos, got %d", code)
	}
	if code, _ := get(t, ts.URL+"/readyz", nil); code != http.StatusOK {
		t.Fatalf("/readyz must ignore chaos, got %d", code)
	}
	_, body := get(t, ts.URL+"/metrics", nil)
	if !strings.Contains(body, `http_requests_total{route="GET /work",service="svc",status="500",version="dev"} 1`) {
		t.Fatalf("injected 500 not counted:\n%s", grep(body, "http_requests_total"))
	}
}

func TestTraceIDPropagatesAcrossTwoHops(t *testing.T) {
	clearEnv(t)

	var backLogs, frontLogs syncBuffer
	back := newTestServer(t, "back", &backLogs)
	back.Handle("GET /leaf", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "leaf")
	}))
	backTS := httptest.NewServer(back.Handler())
	defer backTS.Close()

	front := newTestServer(t, "front", &frontLogs)
	client := Client(2 * time.Second)
	front.Handle("GET /root", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, backTS.URL+"/leaf", nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(w, resp.Body)
	}))
	frontTS := httptest.NewServer(front.Handler())
	defer frontTS.Close()

	// No incoming traceparent: front starts the trace, back must join it.
	if code, body := get(t, frontTS.URL+"/root", nil); code != http.StatusOK || body != "leaf" {
		t.Fatalf("got %d %q", code, body)
	}
	f, b := frontLogs.lines(t, "request"), backLogs.lines(t, "request")
	if len(f) != 1 || len(b) != 1 {
		t.Fatalf("want one request line each, got front=%d back=%d", len(f), len(b))
	}
	tid, _ := f[0]["trace_id"].(string)
	if len(tid) != 32 || b[0]["trace_id"] != tid {
		t.Fatalf("trace_id did not propagate: front=%v back=%v", f[0]["trace_id"], b[0]["trace_id"])
	}
	if f[0]["span_id"] == b[0]["span_id"] {
		t.Fatal("front and back must be different spans of one trace")
	}

	// An incoming traceparent is honoured end to end.
	const incoming = "4bf92f3577b34da6a3ce929d0e0e4736"
	get(t, frontTS.URL+"/root", map[string]string{"traceparent": "00-" + incoming + "-00f067aa0ba902b7-01"})
	f, b = frontLogs.lines(t, "request"), backLogs.lines(t, "request")
	if f[1]["trace_id"] != incoming || b[1]["trace_id"] != incoming {
		t.Fatalf("incoming traceparent not honoured: front=%v back=%v", f[1]["trace_id"], b[1]["trace_id"])
	}
}

func TestReadyzChecksDependencies(t *testing.T) {
	clearEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()

	t.Setenv("DEPENDENCIES", "up, down")
	t.Setenv("UP_URL", up.URL)
	t.Setenv("DOWN_URL", "http://127.0.0.1:1") // nothing listens on port 1
	s := newTestServer(t, "svc", io.Discard)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	code, body := get(t, ts.URL+"/readyz", nil)
	var r struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusServiceUnavailable || r.Checks["up"] != "ok" || r.Checks["down"] == "ok" {
		t.Fatalf("got %d %+v", code, r)
	}
}

func TestGracefulShutdownDrainsInFlightRequest(t *testing.T) {
	clearEnv(t)
	var logs syncBuffer
	s, err := New(context.Background(), "svc", WithLogWriter(&logs), WithDrainDelay(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	s.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(600 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()

	type result struct {
		code int
		body string
	}
	slow := make(chan result, 1)
	go func() {
		code, body := get(t, base+"/slow", nil)
		slow <- result{code, body}
	}()
	<-started
	stop() // what SIGTERM does in Run

	// During the drain delay the listener is still open but /readyz says 503,
	// so the endpoint is withdrawn before connections are refused.
	time.Sleep(100 * time.Millisecond)
	if code, _ := get(t, base+"/readyz", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during drain = %d, want 503", code)
	}

	if r := <-slow; r.code != http.StatusOK || r.body != "done" {
		t.Fatalf("in-flight request cut off: %d %q", r.code, r.body)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if len(logs.lines(t, "shutdown: complete")) != 1 {
		t.Fatal("no clean-shutdown log line")
	}
}

func TestDeadCollectorNeverHurtsRequests(t *testing.T) {
	clearEnv(t)
	// Nothing listens on port 1: every export fails with connection refused.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	var logs syncBuffer
	s := newTestServer(t, "svc", &logs)
	if !s.tel.Exporting {
		t.Fatal("endpoint set, but no exporter attached")
	}
	s.Handle("GET /work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()

	start := time.Now()
	for range 200 {
		if code, _ := get(t, base+"/work", nil); code != http.StatusOK {
			t.Fatalf("request failed with dead collector: %d", code)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("200 requests took %v with a dead collector; export is blocking", d)
	}
	if code, _ := get(t, base+"/readyz", nil); code != http.StatusOK {
		t.Fatalf("/readyz = %d with a dead collector, want 200", code)
	}

	stop()
	start = time.Now()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown hung on a dead collector")
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("shutdown took %v; the flush must be bounded", d)
	}
}

func grep(s, substr string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
