// Package chaos reads the lab's fault-injection knobs and applies them.
//
// Every knob is read once, at startup, from the environment. A running process
// never changes behaviour; a new behaviour is a new rollout. That is what lets
// Phase 6 compare v1 and v2 of the same binary and attribute the difference to
// configuration alone.
package chaos

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Environment variable names. The chart sets them; nothing else should.
const (
	EnvFailureRate   = "FAILURE_RATE"
	EnvExtraLatency  = "EXTRA_LATENCY_MS"
	EnvCPUBurnFactor = "CPU_BURN_FACTOR"
	EnvAppVersion    = "APP_VERSION"
)

// DefaultAppVersion is used when APP_VERSION is unset, i.e. under `go run`.
const DefaultAppVersion = "dev"

// Config is the knob set for one process.
type Config struct {
	// FailureRate is the fraction of business requests answered with a 500.
	FailureRate float64
	// ExtraLatency is slept before every business request is handled.
	ExtraLatency time.Duration
	// CPUBurnFactor multiplies pricing's hash iterations. Other services read
	// it and ignore it; it lives here so every knob is parsed in one place.
	CPUBurnFactor int
	// AppVersion becomes the `version` label on every metric. It must be a
	// human version such as v1 or v2.
	AppVersion string

	// rnd is swapped in tests; nil means math/rand/v2.
	rnd func() float64
}

// hexID matches what a commit SHA, image digest or Rollout pod-template hash
// looks like. Any of them as a metric label is unbounded cardinality.
var hexID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{7,64}$`)

// FromEnv reads and validates every knob. An invalid value is an error, never a
// silent default: a canary running with a knob it did not ask for is worse than
// a canary that fails to start.
func FromEnv() (Config, error) {
	c := Config{CPUBurnFactor: 1, AppVersion: DefaultAppVersion}

	if v, ok := lookup(EnvFailureRate); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return Config{}, fmt.Errorf("%s=%q: want a float in [0,1]", EnvFailureRate, v)
		}
		c.FailureRate = f
	}

	if v, ok := lookup(EnvExtraLatency); ok {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			return Config{}, fmt.Errorf("%s=%q: want a non-negative integer", EnvExtraLatency, v)
		}
		c.ExtraLatency = time.Duration(ms) * time.Millisecond
	}

	if v, ok := lookup(EnvCPUBurnFactor); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("%s=%q: want a non-negative integer", EnvCPUBurnFactor, v)
		}
		c.CPUBurnFactor = n
	}

	if v, ok := lookup(EnvAppVersion); ok {
		if err := validateVersion(v); err != nil {
			return Config{}, err
		}
		c.AppVersion = v
	}

	return c, nil
}

// validateVersion enforces the cardinality rule in exactly one place: every
// metric in the lab is labelled from this value.
func validateVersion(v string) error {
	if len(v) > 32 {
		return fmt.Errorf("%s=%q: longer than 32 characters; want a human version like v1", EnvAppVersion, v)
	}
	if hexID.MatchString(strings.ToLower(v)) {
		return fmt.Errorf("%s=%q looks like a SHA or digest; that would be an unbounded metric label. Want a human version like v1", EnvAppVersion, v)
	}
	return nil
}

func lookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// Middleware injects latency, then failures, in front of next. Apply it to
// business routes only: probes and /metrics must never be affected, or a
// FAILURE_RATE experiment would also restart pods and hide the scrape.
func (c Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.ExtraLatency > 0 {
			t := time.NewTimer(c.ExtraLatency)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		if c.FailureRate > 0 && c.roll() < c.FailureRate {
			trace.SpanFromContext(r.Context()).AddEvent("chaos.injected_failure",
				trace.WithAttributes(attribute.Float64("chaos.failure_rate", c.FailureRate)))
			http.Error(w, "injected failure (FAILURE_RATE)", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c Config) roll() float64 {
	if c.rnd != nil {
		return c.rnd()
	}
	return rand.Float64()
}
