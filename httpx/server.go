// Package httpx is the server scaffolding every service runs on: business
// routes wrapped in tracing, metrics and chaos; /healthz, /readyz and /metrics;
// dependency readiness; and graceful shutdown on SIGTERM.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/bo-jr/bo-service-kit/chaos"
	"github.com/bo-jr/bo-service-kit/telemetry"
)

// Environment variables read by New. The chart renders all of them.
const (
	EnvPort         = "PORT"
	EnvDependencies = "DEPENDENCIES"
)

const (
	defaultPort = "8080"
	// defaultDrainDelay is how long /readyz reports 503 before the listener
	// stops accepting: long enough for the endpoint removal to propagate so
	// no new request is routed to a pod that is going away.
	defaultDrainDelay = 5 * time.Second
	// shutdownGrace bounds how long in-flight requests may take to finish.
	// drain delay + grace + telemetry flush must stay under the chart's
	// terminationGracePeriodSeconds.
	shutdownGrace = 20 * time.Second
	readyzTimeout = time.Second
	depTimeout    = 500 * time.Millisecond
)

// Check is a named readiness check. It must honour ctx.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Server is one service's HTTP server.
type Server struct {
	service string
	chaos   chaos.Config
	tel     *telemetry.Telemetry
	log     *slog.Logger
	metrics *metrics

	mux        *http.ServeMux
	drainDelay time.Duration

	mu       sync.Mutex
	checks   []Check
	draining atomic.Bool
}

// Option adjusts New. Services pass none; tests use them.
type Option func(*options)

type options struct {
	logWriter  io.Writer
	drainDelay time.Duration
}

// WithLogWriter sends JSON logs somewhere other than stdout.
func WithLogWriter(w io.Writer) Option { return func(o *options) { o.logWriter = w } }

// WithDrainDelay overrides the pre-shutdown readiness drain.
func WithDrainDelay(d time.Duration) Option { return func(o *options) { o.drainDelay = d } }

// New reads the chaos knobs, sets up telemetry and returns a Server with the
// operational endpoints mounted. service is the in-cluster name, without the
// bo- prefix.
func New(ctx context.Context, service string, opts ...Option) (*Server, error) {
	o := options{drainDelay: defaultDrainDelay}
	for _, opt := range opts {
		opt(&o)
	}

	cfg, err := chaos.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("chaos config: %w", err)
	}
	tel, err := telemetry.Setup(ctx, telemetry.Options{
		Service: service, Version: cfg.AppVersion, LogWriter: o.logWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	s := &Server{
		service:    service,
		chaos:      cfg,
		tel:        tel,
		log:        tel.Logger,
		metrics:    newMetrics(),
		mux:        http.NewServeMux(),
		drainDelay: o.drainDelay,
	}

	// Operational endpoints: not traced, not counted, never chaos'd. Kubelet
	// probes would otherwise dilute the SLO ratio and FAILURE_RATE would
	// restart the pod it is meant to be measuring.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))

	for _, dep := range dependencies() {
		s.AddCheck(dependencyCheck(dep))
	}

	s.log.Info("configured",
		"failure_rate", cfg.FailureRate,
		"extra_latency_ms", cfg.ExtraLatency.Milliseconds(),
		"cpu_burn_factor", cfg.CPUBurnFactor,
		"dependencies", dependencies())
	return s, nil
}

// Logger returns the service logger. Log with the *Context methods so lines
// carry the request's trace_id.
func (s *Server) Logger() *slog.Logger { return s.log }

// Chaos returns the knobs read at startup.
func (s *Server) Chaos() chaos.Config { return s.chaos }

// Handler returns the root handler, for tests.
func (s *Server) Handler() http.Handler { return s.mux }

// Handle registers a business route. pattern is a Go ServeMux pattern such as
// "GET /items/{sku}" and becomes the metric `route` label verbatim, so it must
// be a pattern and never contain request data.
//
// Wrapping order, outermost first: tracing (so everything below, including an
// injected failure, happens inside the span and logs carry its trace_id), then
// metrics and the access log (so injected latency and failures are measured
// like real ones), then chaos, then the handler.
func (s *Server) Handle(pattern string, h http.Handler) {
	chain := otelhttp.NewHandler(
		s.observe(pattern, s.chaos.Middleware(h)),
		pattern,
	)
	s.mux.Handle(pattern, chain)
}

// AddCheck adds a readiness check.
func (s *Server) AddCheck(c Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, c)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "draining"})
		return
	}

	s.mu.Lock()
	checks := append([]Check(nil), s.checks...)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	results := make(map[string]string, len(checks))
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed bool
	)
	for _, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := "ok"
			if err := c.Fn(ctx); err != nil {
				res = err.Error()
			}
			mu.Lock()
			defer mu.Unlock()
			results[c.Name] = res
			if res != "ok" {
				failed = true
			}
		}()
	}
	wg.Wait()

	status := "ok"
	if failed {
		status = "unready"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": results})
}

// Run listens on $PORT (default 8080) and serves until SIGTERM or SIGINT.
func (s *Server) Run(ctx context.Context) error {
	port := os.Getenv(EnvPort)
	if port == "" {
		port = defaultPort
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is done, then shuts down gracefully:
//
//  1. /readyz starts answering 503, so the endpoint is withdrawn;
//  2. after the drain delay the listener closes and in-flight requests get
//     up to shutdownGrace to finish;
//  3. buffered spans are flushed, bounded by the telemetry shutdown timeout.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		_ = s.tel.Close()
		return err
	case <-ctx.Done():
	}

	s.draining.Store(true)
	s.log.Info("shutdown: draining", "drain_delay", s.drainDelay.String())
	time.Sleep(s.drainDelay)

	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
		err = serr
	}
	if terr := s.tel.Close(); terr != nil {
		s.log.Warn("telemetry close", "err", terr.Error())
	}
	if err != nil {
		s.log.Error("shutdown incomplete", "err", err.Error())
		return err
	}
	s.log.Info("shutdown: complete")
	return nil
}

// Client returns an HTTP client that propagates the W3C traceparent of the
// request context it is called with. Use it for every call to a dependency.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport.(*http.Transport).Clone()),
	}
}

// DependencyURL is the base URL of a dependency: http://<name>, the Service
// the chart creates for it in the same namespace. <NAME>_URL overrides it for
// the local `go run` loop, e.g. CATALOG_URL=http://localhost:8081.
func DependencyURL(name string) string {
	if v := os.Getenv(strings.ToUpper(name) + "_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://" + name
}

func dependencies() []string {
	deps := []string{}
	for _, d := range strings.Split(os.Getenv(EnvDependencies), ",") {
		if d = strings.TrimSpace(d); d != "" {
			deps = append(deps, d)
		}
	}
	return deps
}

// dependencyCheck probes a dependency's /healthz — liveness, not readiness.
// Chaining readiness would let one unready leaf mark the whole call graph
// unready; this only asks "is something answering at that address".
func dependencyCheck(name string) Check {
	client := &http.Client{Timeout: depTimeout}
	url := DependencyURL(name) + "/healthz"
	return Check{Name: name, Fn: func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s/healthz: %s", name, resp.Status)
		}
		return nil
	}}
}
