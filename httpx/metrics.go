package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// The lab's two HTTP metrics, defined here and nowhere else. Pyrra SLOs
// (Phase 4) and the canary AnalysisTemplate (Phase 6) query these names and
// labels; a service registering its own copy is a bug.
//
// Label values are bounded by construction:
//   - service: a constant compiled into each binary
//   - route:   the ServeMux pattern registered with Handle, never the raw path
//   - status:  the numeric HTTP status code
//   - version: APP_VERSION, validated by chaos.FromEnv to not be a SHA or digest
var metricLabels = []string{"service", "route", "status", "version"}

type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests handled, by route pattern and status code.",
		}, metricLabels),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency, by route pattern and status code.",
			Buckets: prometheus.DefBuckets,
		}, metricLabels),
	}
	m.registry.MustRegister(
		m.requests, m.duration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// statusRecorder captures the status code a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe wraps a business route: it counts and times the request, and writes
// one access-log line carrying the trace ID.
func (s *Server) observe(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		elapsed := time.Since(start)
		code := strconv.Itoa(rec.status)
		s.metrics.requests.WithLabelValues(s.service, route, code, s.chaos.AppVersion).Inc()
		s.metrics.duration.WithLabelValues(s.service, route, code, s.chaos.AppVersion).Observe(elapsed.Seconds())
		s.log.InfoContext(r.Context(), "request",
			"method", r.Method, "route", route, "path", r.URL.Path,
			"status", rec.status, "duration_ms", float64(elapsed.Microseconds())/1000)
	})
}
