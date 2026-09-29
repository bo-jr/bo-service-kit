package chaos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{name: "defaults", want: Config{CPUBurnFactor: 1, AppVersion: "dev"}},
		{
			name: "all set",
			env:  map[string]string{EnvFailureRate: "0.5", EnvExtraLatency: "300", EnvCPUBurnFactor: "40", EnvAppVersion: "v2"},
			want: Config{FailureRate: 0.5, ExtraLatency: 300 * time.Millisecond, CPUBurnFactor: 40, AppVersion: "v2"},
		},
		{name: "empty values fall back", env: map[string]string{EnvFailureRate: " ", EnvAppVersion: ""}, want: Config{CPUBurnFactor: 1, AppVersion: "dev"}},
		{name: "dotted version", env: map[string]string{EnvAppVersion: "v2.1"}, want: Config{CPUBurnFactor: 1, AppVersion: "v2.1"}},
		{name: "failure rate above 1", env: map[string]string{EnvFailureRate: "1.5"}, wantErr: true},
		{name: "failure rate negative", env: map[string]string{EnvFailureRate: "-0.1"}, wantErr: true},
		{name: "failure rate not a number", env: map[string]string{EnvFailureRate: "half"}, wantErr: true},
		{name: "latency negative", env: map[string]string{EnvExtraLatency: "-1"}, wantErr: true},
		{name: "latency float", env: map[string]string{EnvExtraLatency: "1.5"}, wantErr: true},
		{name: "burn factor negative", env: map[string]string{EnvCPUBurnFactor: "-3"}, wantErr: true},
		{name: "short commit sha", env: map[string]string{EnvAppVersion: "3f2a9c1"}, wantErr: true},
		{name: "full commit sha", env: map[string]string{EnvAppVersion: "0123456789abcdef0123456789abcdef01234567"}, wantErr: true},
		{name: "image digest", env: map[string]string{EnvAppVersion: "sha256:" + "ab12cd34"}, wantErr: true},
		{name: "upper-case sha", env: map[string]string{EnvAppVersion: "3F2A9C1D"}, wantErr: true},
		{name: "too long", env: map[string]string{EnvAppVersion: "v1-this-is-far-too-long-for-a-label"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvFailureRate, EnvExtraLatency, EnvCPUBurnFactor, EnvAppVersion} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.FailureRate != tc.want.FailureRate || got.ExtraLatency != tc.want.ExtraLatency ||
				got.CPUBurnFactor != tc.want.CPUBurnFactor || got.AppVersion != tc.want.AppVersion {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMiddlewareFailureRate(t *testing.T) {
	// A deterministic sequence of rolls: exactly half fall under 0.5.
	rolls := []float64{0.1, 0.9, 0.4, 0.6, 0.0, 0.99, 0.49, 0.5}
	i := 0
	c := Config{FailureRate: 0.5, rnd: func() float64 { r := rolls[i%len(rolls)]; i++; return r }}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	failed := 0
	for range rolls {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code == http.StatusInternalServerError {
			failed++
		}
	}
	if failed != 4 {
		t.Fatalf("failed %d of %d, want 4", failed, len(rolls))
	}
}

func TestMiddlewareFailureRateStatistical(t *testing.T) {
	c := Config{FailureRate: 0.5}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	const n = 4000
	failed := 0
	for range n {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code == http.StatusInternalServerError {
			failed++
		}
	}
	// 0.5 +/- ~5 standard deviations at n=4000.
	if got := float64(failed) / n; got < 0.46 || got > 0.54 {
		t.Fatalf("failure fraction %.3f, want about 0.5", got)
	}
}

func TestMiddlewareZeroRateNeverFails(t *testing.T) {
	c := Config{rnd: func() float64 { return 0 }}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d with FailureRate 0", rec.Code)
	}
}

func TestMiddlewareLatency(t *testing.T) {
	c := Config{ExtraLatency: 80 * time.Millisecond}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("returned after %v, want >= 80ms", d)
	}

	// A cancelled request stops waiting and never reaches the handler.
	called := false
	h = Config{ExtraLatency: time.Hour}.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start = time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	if d := time.Since(start); d > time.Second || called {
		t.Fatalf("cancelled request waited %v (handler called: %v)", d, called)
	}
}
