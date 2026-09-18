// Package risk computes a transparent, explainable risk score per
// dependency. Per the Day 12 brief: "You don't need a sophisticated ML
// model. A transparent scoring algorithm is better." Every point on the
// score is traceable to a named factor, which matters for an incident
// review ("why did the system think this was HIGH risk?").
package risk

import (
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/health"
)

type Band string

const (
	Low      Band = "LOW"
	Medium   Band = "MEDIUM"
	High     Band = "HIGH"
	Critical Band = "CRITICAL"
)

// Score is the computed risk for one dependency, with a factor breakdown
// so the number is never a black box.
type Score struct {
	Dependency string         `json:"dependency"`
	Total      int            `json:"total"`
	Band       Band           `json:"band"`
	Factors    map[string]int `json:"factors"`
}

// Inputs bundles everything the score needs about one dependency.
type Inputs struct {
	Dependency         string
	Stats              health.Stats
	Criticality        int // 1-5, from the graph
	DependentCount     int // how many services rely on this one (blast radius)
	RecentIncidents24h int
}

// Compute produces a Score using simple, additive, capped sub-scores.
// Total is capped at 100 to keep the LOW/MEDIUM/HIGH/CRITICAL bands stable
// regardless of how many factors are added later.
func Compute(in Inputs) Score {
	factors := make(map[string]int)

	latencyMs := int(in.Stats.AvgLatency.Milliseconds())
	latencyPoints := min(20, (latencyMs*20)/10000)
	factors["latency"] = latencyPoints

	// Error rate: 0-30 points, linear on the 0-100% error rate.
	errorPoints := min(30, int(in.Stats.ErrorRate*30))
	factors["error_rate"] = errorPoints

	// Recent incidents: 10 points each, capped at 20.
	incidentPoints := min(20, in.RecentIncidents24h*10)
	factors["recent_incidents"] = incidentPoints

	// Business criticality: registered 1-5 -> 0-20 points.
	criticalityPoints := min(20, in.Criticality*4)
	factors["business_criticality"] = criticalityPoints

	// Dependency count (blast radius): how many services would be hit if
	// this one fails. 2 points per dependent, capped at 10.
	dependentPoints := min(10, in.DependentCount*2)
	factors["dependency_count"] = dependentPoints

	total := latencyPoints + errorPoints + incidentPoints + criticalityPoints + dependentPoints
	if total > 100 {
		total = 100
	}

	return Score{
		Dependency: in.Dependency,
		Total:      total,
		Band:       bandFor(total),
		Factors:    factors,
	}
}

func bandFor(total int) Band {
	switch {
	case total >= 80:
		return Critical
	case total >= 45:
		return High
	case total >= 20:
		return Medium
	default:
		return Low
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ComputeAll scores every dependency currently known to the graph, using
// the monitor's live stats and the graph's registered criticality/edges.
func ComputeAll(g *graph.Graph, m *health.Monitor, recentIncidents map[string]int) []Score {
	all := m.AllStats()
	scores := make([]Score, 0, len(all))
	for name, stats := range all {
		scores = append(scores, Compute(Inputs{
			Dependency:         name,
			Stats:              stats,
			Criticality:        g.Criticality(name),
			DependentCount:     len(g.Dependents(name)),
			RecentIncidents24h: recentIncidents[name],
		}))
	}
	return scores
}

// Age is a small helper kept for future use (e.g. decaying incident counts
// by recency instead of a flat 24h bucket) - not wired in yet, documented
// as a stretch goal in the README.
func Age(t time.Time) time.Duration { return time.Since(t) }
