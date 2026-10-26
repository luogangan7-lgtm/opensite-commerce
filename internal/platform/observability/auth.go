// Responsibility: finite, process-local authentication availability telemetry.
// Dependencies: Metrics request context; callers: staff admission and private pool adapter.
// Invariants: no tokens, IPs, tenant IDs, SQL or caller-controlled labels; no global collector.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type metricsKey struct{}
type authLaneKey struct{}
type authKey struct{ stage, outcome string }

// WithAuthenticationLane marks the private session pool without exposing it.
func WithAuthenticationLane(ctx context.Context, lane string) context.Context {
	if lane != "staff_session" && lane != "staff_credential" {
		return ctx
	}
	return context.WithValue(ctx, authLaneKey{}, lane)
}

// AuthenticationLane returns a bounded label even for callers outside HTTP.
func AuthenticationLane(ctx context.Context, fallback string) string {
	if lane, ok := ctx.Value(authLaneKey{}).(string); ok {
		return lane
	}
	if fallback == "authorization" {
		return fallback
	}
	return "staff"
}

// ObserveAuthentication records monotonic elapsed time, never durable session time.
func ObserveAuthentication(ctx context.Context, stage, outcome string, started time.Time) {
	m, ok := ctx.Value(metricsKey{}).(*Metrics)
	if !ok {
		return
	}
	switch stage {
	case "session_admission", "staff_queue", "staff_pool", "staff_session_queue", "staff_session_pool", "staff_credential_queue", "staff_credential_pool", "authorization_queue", "authorization_pool":
	default:
		return
	}
	switch outcome {
	case "admitted", "denied", "ok", "error", "timeout":
	default:
		return
	}
	seconds := time.Since(started).Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth == nil {
		m.auth = make(map[authKey]uint64)
		m.authDurations = make(map[string]latency)
	}
	m.auth[authKey{stage, outcome}]++
	d := m.authDurations[stage]
	d.count++
	d.seconds += seconds
	for i, bound := range bounds {
		if seconds <= bound {
			d.buckets[i]++
		}
	}
	m.authDurations[stage] = d
}

func (m *Metrics) writeAuthentication(w http.ResponseWriter) {
	m.mu.Lock()
	counts := make(map[authKey]uint64, len(m.auth))
	for key, count := range m.auth {
		counts[key] = count
	}
	durations := make(map[string]latency, len(m.authDurations))
	for stage, d := range m.authDurations {
		durations[stage] = d
	}
	m.mu.Unlock()
	keys := make([]authKey, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].stage != keys[j].stage {
			return keys[i].stage < keys[j].stage
		}
		return keys[i].outcome < keys[j].outcome
	})
	fmt.Fprint(w, "# HELP opensite_auth_operations_total Authentication admission and pool outcomes.\n# TYPE opensite_auth_operations_total counter\n")
	for _, key := range keys {
		fmt.Fprintf(w, "opensite_auth_operations_total{stage=%q,outcome=%q} %d\n", key.stage, key.outcome, counts[key])
	}
	stages := make([]string, 0, len(durations))
	for stage := range durations {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	fmt.Fprint(w, "# HELP opensite_auth_duration_seconds Authentication admission, acquisition or full pool operation duration.\n# TYPE opensite_auth_duration_seconds histogram\n")
	for _, stage := range stages {
		d := durations[stage]
		for i, bound := range bounds {
			fmt.Fprintf(w, "opensite_auth_duration_seconds_bucket{stage=%q,le=%q} %d\n", stage, fmt.Sprint(bound), d.buckets[i])
		}
		fmt.Fprintf(w, "opensite_auth_duration_seconds_bucket{stage=%q,le=\"+Inf\"} %d\n", stage, d.count)
		fmt.Fprintf(w, "opensite_auth_duration_seconds_sum{stage=%q} %g\n", stage, d.seconds)
		fmt.Fprintf(w, "opensite_auth_duration_seconds_count{stage=%q} %d\n", stage, d.count)
	}
}
