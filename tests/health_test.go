package tests

import (
	"context"
	"testing"
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/health"
)

// fakeChecker lets tests script a fixed sequence of samples, simulating
// Test 1 (timeout), Test 2 (down), Test 3 (recovery), and Test 7 (slow but
// technically successful) from the Day 12 spec without needing real HTTP servers.
type fakeChecker struct {
	samples []health.Sample
	i       int
}

func (f *fakeChecker) Check(ctx context.Context) health.Sample {
	if f.i >= len(f.samples) {
		f.i = len(f.samples) - 1
	}
	s := f.samples[f.i]
	f.i++
	return s
}

func TestMonitor_DetectsDegradedOnTimeouts(t *testing.T) {
	g := graph.New()
	g.Register(graph.Service{Name: "provider-a"})

	m := health.NewMonitor(g, health.DefaultThresholds(), 10, nil)
	// 10 samples, 2 timeouts -> 20% error rate -> DOWN under default thresholds
	// (DownErrorRate=0.5 means this should actually land as DEGRADED; kept
	// explicit here rather than hand-waved, per the "don't just say HTTP
	// 500 = DOWN" instruction).
	fc := &fakeChecker{}
	for i := 0; i < 8; i++ {
		fc.samples = append(fc.samples, health.Sample{Success: true, Latency: 100 * time.Millisecond})
	}
	fc.samples = append(fc.samples,
		health.Sample{Success: false, TimedOut: true, Latency: 5 * time.Second},
		health.Sample{Success: false, TimedOut: true, Latency: 5 * time.Second},
	)
	m.Register("provider-a", fc)

	ctx := context.Background()
	for i := 0; i < len(fc.samples); i++ {
		sample := fc.samples[i]
		mSimulateRecord(m, "provider-a", sample)
	}
	_ = ctx

	stats := m.Stats("provider-a")
	if stats.State != graph.Degraded {
		t.Fatalf("expected DEGRADED at 20%% error rate (>=5%% DegradedErrorRate, <50%% DownErrorRate), got %s (error rate %.2f)", stats.State, stats.ErrorRate)
	}
}

func TestMonitor_SlowButSuccessfulIsDegraded(t *testing.T) {
	g := graph.New()
	g.Register(graph.Service{Name: "provider-a"})
	m := health.NewMonitor(g, health.DefaultThresholds(), 10, nil)

	// All calls succeed (Test 7: "technically available") but take 8s,
	// well past the 3s SlowLatency threshold -> must still be DEGRADED.
	for i := 0; i < 5; i++ {
		mSimulateRecord(m, "provider-a", health.Sample{Success: true, Latency: 8 * time.Second})
	}

	stats := m.Stats("provider-a")
	if stats.ErrorRate != 0 {
		t.Fatalf("expected 0%% error rate (all calls succeeded), got %.2f", stats.ErrorRate)
	}
	if stats.State != graph.Degraded {
		t.Fatalf("expected DEGRADED due to high latency despite 0%% errors (technically available != healthy), got %s", stats.State)
	}
}

// mSimulateRecord reaches into the monitor's exported Stats/Register-based
// flow by registering a one-shot fake checker and running a manual check;
// this keeps the test black-box (no unexported access) while still letting
// us feed exact samples deterministically.
func mSimulateRecord(m *health.Monitor, dependency string, sample health.Sample) {
	m.Register(dependency, &fakeChecker{samples: []health.Sample{sample}})
	m.RunOnce(context.Background(), dependency)
}
