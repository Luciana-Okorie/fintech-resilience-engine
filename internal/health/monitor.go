// Package health runs periodic checks against dependencies and turns raw
// signals (latency, status code, timeout, error rate) into one of the five
// HealthState values - deliberately NOT a single "HTTP 500 == DOWN" rule.
package health

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
)

// Sample is one raw observation of a dependency check.
type Sample struct {
	Success bool
	TimedOut bool
	StatusCode int
	Latency  time.Duration
	At       time.Time
}

// Stats is the rolling window summary used to decide health state, and is
// also what gets exposed on the /health endpoint (mirrors the Day 12 spec's
// "Provider A: Requests / Successful / Failed / Error rate / Avg latency" example).
type Stats struct {
	Requests     int
	Successful   int
	Failed       int
	TimeoutCount int
	ErrorRate    float64
	AvgLatency   time.Duration
	State        graph.HealthState
}

// Thresholds controls how raw samples map to a HealthState. Tunable per
// dependency criticality if needed - kept global + simple for Day 12.
type Thresholds struct {
	DegradedErrorRate float64       // e.g. 0.05 (5%) -> DEGRADED
	DownErrorRate     float64       // e.g. 0.5  (50%) -> DOWN
	SlowLatency       time.Duration // "technically available != healthy": above this -> DEGRADED even with 0 errors
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		DegradedErrorRate: 0.05,
		DownErrorRate:     0.5,
		SlowLatency:       3 * time.Second,
	}
}

// Checker knows how to produce one Sample for a named dependency.
// In production this would be an HTTP ping, a DB ping, a Redis PING, etc.
// Day 12 ships an HTTPChecker (below) that hits the mock providers.
type Checker interface {
	Check(ctx context.Context) Sample
}

// HTTPChecker pings a URL and turns the response into a Sample.
type HTTPChecker struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
}

func NewHTTPChecker(url string) *HTTPChecker {
	return &HTTPChecker{
		URL:     url,
		Client:  &http.Client{},
		Timeout: 5 * time.Second,
	}
}

func (c *HTTPChecker) Check(ctx context.Context) Sample {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	start := time.Now()
	if err != nil {
		return Sample{Success: false, At: start}
	}

	resp, err := c.Client.Do(req)
	latency := time.Since(start)

	if err != nil {
		timedOut := ctx.Err() == context.DeadlineExceeded
		return Sample{Success: false, TimedOut: timedOut, Latency: latency, At: start}
	}
	defer resp.Body.Close()

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	return Sample{
		Success:    success,
		StatusCode: resp.StatusCode,
		Latency:    latency,
		At:         start,
	}
}

// Monitor periodically checks a set of dependencies and updates the graph's
// health state, keeping a rolling window of samples for the /health endpoint.
type Monitor struct {
	mu         sync.RWMutex
	graph      *graph.Graph
	checkers   map[string]Checker
	thresholds Thresholds
	window     int // how many recent samples to keep per dependency
	samples    map[string][]Sample

	onStateChange func(dependency string, from, to graph.HealthState)
}

func NewMonitor(g *graph.Graph, thresholds Thresholds, windowSize int, onStateChange func(dependency string, from, to graph.HealthState)) *Monitor {
	return &Monitor{
		graph:         g,
		checkers:      make(map[string]Checker),
		thresholds:    thresholds,
		window:        windowSize,
		samples:       make(map[string][]Sample),
		onStateChange: onStateChange,
	}
}

// Register wires a checker to a dependency name already known to the graph.
func (m *Monitor) Register(dependency string, checker Checker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkers[dependency] = checker
}

// Run starts the periodic check loop; call in a goroutine. Stops on ctx.Done().
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// RunOnce checks a single dependency immediately and records the result.
// Used by tests to feed deterministic samples without waiting on the ticker.
func (m *Monitor) RunOnce(ctx context.Context, dependency string) {
	m.mu.RLock()
	checker, ok := m.checkers[dependency]
	m.mu.RUnlock()
	if !ok {
		return
	}
	sample := checker.Check(ctx)
	m.record(dependency, sample)
}

func (m *Monitor) checkAll(ctx context.Context) {
	m.mu.RLock()
	checkers := make(map[string]Checker, len(m.checkers))
	for k, v := range m.checkers {
		checkers[k] = v
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for name, checker := range checkers {
		wg.Add(1)
		go func(name string, checker Checker) {
			defer wg.Done()
			sample := checker.Check(ctx)
			m.record(name, sample)
		}(name, checker)
	}
	wg.Wait()
}

func (m *Monitor) record(dependency string, sample Sample) {
	m.mu.Lock()
	m.samples[dependency] = append(m.samples[dependency], sample)
	if len(m.samples[dependency]) > m.window {
		m.samples[dependency] = m.samples[dependency][len(m.samples[dependency])-m.window:]
	}
	stats := computeStats(m.samples[dependency], m.thresholds)
	m.mu.Unlock()

	prev, _ := m.graph.Health(dependency)
	if prev != stats.State {
		m.graph.SetHealth(dependency, stats.State)
		if m.onStateChange != nil {
			m.onStateChange(dependency, prev, stats.State)
		}
	}
}

// Stats returns the current rolling-window stats for a dependency.
func (m *Monitor) Stats(dependency string) Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return computeStats(m.samples[dependency], m.thresholds)
}

// AllStats returns stats for every monitored dependency (used by /health).
func (m *Monitor) AllStats() map[string]Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Stats, len(m.samples))
	for name, samples := range m.samples {
		out[name] = computeStats(samples, m.thresholds)
	}
	return out
}

func computeStats(samples []Sample, t Thresholds) Stats {
	s := Stats{}
	if len(samples) == 0 {
		s.State = graph.Unknown
		return s
	}

	var totalLatency time.Duration
	for _, sample := range samples {
		s.Requests++
		if sample.Success {
			s.Successful++
		} else {
			s.Failed++
		}
		if sample.TimedOut {
			s.TimeoutCount++
		}
		totalLatency += sample.Latency
	}
	s.ErrorRate = float64(s.Failed) / float64(s.Requests)
	s.AvgLatency = totalLatency / time.Duration(s.Requests)

	switch {
	case s.ErrorRate >= t.DownErrorRate:
		s.State = graph.Down
	case s.ErrorRate >= t.DegradedErrorRate:
		s.State = graph.Degraded
	case s.AvgLatency >= t.SlowLatency:
		// "Technically available != healthy" - Test 7 from the Day 12 spec.
		s.State = graph.Degraded
	default:
		s.State = graph.Healthy
	}
	return s
}
