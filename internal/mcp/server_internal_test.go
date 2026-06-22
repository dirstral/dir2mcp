package mcp

import (
	"net/http"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestNewMCPHTTPServer_NoWriteTimeout pins that the MCP HTTP server has no write
// deadline: net/http's WriteTimeout spans the entire handler execution, so any
// nonzero value would tear down legitimately long-running tool calls (annotate,
// ask, OCR) mid-flight and surface as "Failed to call tool". Regression for
// issue #362. ReadHeaderTimeout must remain set (slowloris protection).
//
// Lives in this existing in-package test file (not a new tests/ file) because it
// asserts the unexported newMCPHTTPServer constructor, which the external tests/
// package cannot reach.
func TestNewMCPHTTPServer_NoWriteTimeout(t *testing.T) {
	srv := newMCPHTTPServer(http.NewServeMux())

	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 (disabled) so long tool calls aren't killed mid-flight", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want > 0 (slowloris protection must stay)", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v, want > 0 (idle keep-alives must still be reaped)", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout > time.Minute {
		t.Fatalf("ReadHeaderTimeout = %v, unexpectedly large for slowloris protection", srv.ReadHeaderTimeout)
	}
}

// The six legacy tests below described various combinations of the
// configured session timeouts.  They're all very similar; exercising the
// same s.sessionSweepInterval logic with slightly different config values.
// Combining them into a single table-driven test reduces boilerplate while
// keeping the individual expectations named and diagnostic-friendly.
func TestSessionSweepInterval(t *testing.T) {
	cases := []struct {
		name           string
		setInactivity  bool
		inactivity     time.Duration
		setMaxLifetime bool
		maxLifetime    time.Duration
		want           time.Duration
	}{
		{
			name: "defaults",
			// leave both values at whatever config.Default() gives us
			// config.Default() sets SessionInactivityTimeout=1h and
			// SessionMaxLifetime=24h, so the sweep interval is
			// min(24h,1h)/2 == 30m.
			want: 30 * time.Minute, // min(24h, 1h)/2
		},
		{
			name:           "smaller inactivity",
			setInactivity:  true,
			inactivity:     10 * time.Minute,
			setMaxLifetime: true,
			maxLifetime:    0,
			want:           5 * time.Minute, // inactivity/2
		},
		{
			name:           "max lifetime smaller",
			setInactivity:  true,
			inactivity:     1 * time.Hour,
			setMaxLifetime: true,
			maxLifetime:    10 * time.Minute,
			want:           5 * time.Minute, // maxLifetime/2
		},
		{
			name:           "floor applied",
			setInactivity:  true,
			inactivity:     1500 * time.Millisecond,
			setMaxLifetime: true,
			maxLifetime:    2 * time.Second,
			want:           time.Second, // floor at 1s
		},
		{
			name:           "explicit zeroes",
			setInactivity:  true,
			inactivity:     0,
			setMaxLifetime: true,
			maxLifetime:    0,
			want:           30 * time.Minute, // fallback to defaults
		},
		{
			name:           "zero inactivity uses max",
			setInactivity:  true,
			inactivity:     0,
			setMaxLifetime: true,
			maxLifetime:    1 * time.Second,
			// inactivity is 0 so we fall back to maxLifetime (1s). half of
			// that is 500ms, which gets floored/upgraded to the minimum floor
			// (1s) in sessionSweepInterval, hence want=1s.
			want: time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			if tc.setInactivity {
				cfg.SessionInactivityTimeout = tc.inactivity
			}
			if tc.setMaxLifetime {
				cfg.SessionMaxLifetime = tc.maxLifetime
			}
			s := NewServer(cfg, nil)

			got := s.sessionSweepInterval()
			if got != tc.want {
				t.Fatalf("%s: sessionSweepInterval()=%v want=%v", tc.name, got, tc.want)
			}
		})
	}
}
