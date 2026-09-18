// Package graph builds and queries the service dependency graph.
//
// A "service" is anything that can be registered (e.g. "payment-service",
// "paystack", "postgres"). Services declare what they depend on. The graph
// is stored as an adjacency list in both directions so we can answer two
// different questions cheaply:
//
//   1. "What does X depend on?"      -> forward edges (dependsOn)
//   2. "What depends on X?"          -> reverse edges (dependents), used
//                                       for cascading-impact calculation.
package graph

import (
	"fmt"
	"sync"
)

// HealthState is the current observed health of a dependency.
type HealthState string

const (
	Healthy     HealthState = "HEALTHY"
	Degraded    HealthState = "DEGRADED"
	Down        HealthState = "DOWN"
	Unknown     HealthState = "UNKNOWN"
	Compromised HealthState = "COMPROMISED"
)

// Severity is the business-impact level propagated during cascade analysis.
type Severity string

const (
	SeverityNone     Severity = "OK"
	SeverityWarning  Severity = "WARNING"  // ⚠ degraded upstream dependency
	SeverityCritical Severity = "CRITICAL" // 🔴 upstream dependency is down
)

// Service is a node in the dependency graph.
type Service struct {
	Name        string   `json:"service"`
	DependsOn   []string `json:"dependsOn"`
	Criticality int      `json:"criticality"` // 1 (low) - 5 (business-critical), used by risk scoring
}

// Graph is a thread-safe dependency graph.
type Graph struct {
	mu sync.RWMutex

	// forward[a] = list of services a depends on
	forward map[string][]string
	// reverse[a] = list of services that depend on a
	reverse map[string][]string
	// health[a] = current health state of a
	health map[string]HealthState
	// criticality[a] = business criticality weight
	criticality map[string]int
}

func New() *Graph {
	return &Graph{
		forward:     make(map[string][]string),
		reverse:     make(map[string][]string),
		health:      make(map[string]HealthState),
		criticality: make(map[string]int),
	}
}

// Register adds or updates a service and its dependency edges.
func (g *Graph) Register(svc Service) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if svc.Criticality == 0 {
		svc.Criticality = 1
	}

	// Remove old reverse edges for this service before re-adding, so
	// Register can be called again to update a service's dependency list.
	for _, old := range g.forward[svc.Name] {
		g.reverse[old] = removeString(g.reverse[old], svc.Name)
	}

	g.forward[svc.Name] = svc.DependsOn
	g.criticality[svc.Name] = svc.Criticality

	if _, ok := g.health[svc.Name]; !ok {
		g.health[svc.Name] = Unknown
	}

	for _, dep := range svc.DependsOn {
		g.reverse[dep] = appendUnique(g.reverse[dep], svc.Name)
		if _, ok := g.health[dep]; !ok {
			g.health[dep] = Unknown
		}
		if _, ok := g.forward[dep]; !ok {
			g.forward[dep] = []string{}
		}
	}
}

// SetHealth updates the observed health state of a dependency.
func (g *Graph) SetHealth(name string, state HealthState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.health[name] = state
}

// Health returns the current health state of a dependency.
func (g *Graph) Health(name string) (HealthState, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	h, ok := g.health[name]
	return h, ok
}

// Criticality returns the registered business-criticality weight (1-5).
func (g *Graph) Criticality(name string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if c, ok := g.criticality[name]; ok {
		return c
	}
	return 1
}

// DependsOn returns the direct dependencies of a service.
func (g *Graph) DependsOn(name string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, len(g.forward[name]))
	copy(out, g.forward[name])
	return out
}

// Dependents returns everything that directly depends on a service.
func (g *Graph) Dependents(name string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, len(g.reverse[name]))
	copy(out, g.reverse[name])
	return out
}

// Services returns every registered node in the graph.
func (g *Graph) Services() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	seen := make(map[string]bool)
	var out []string
	for k := range g.forward {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range g.reverse {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// ImpactNode describes the propagated severity for one node in a cascade.
type ImpactNode struct {
	Service  string   `json:"service"`
	Severity Severity `json:"severity"`
	Health   HealthState `json:"health"`
	Path     []string `json:"path"` // path from the failing dependency to this node
}

// CascadeImpact walks the reverse graph from a failing dependency (BFS) and
// returns every service transitively affected, along with a severity level.
//
// Severity rule (deliberately simple and explainable, per the "no ML" brief):
//   - A service directly depending on a DOWN/COMPROMISED node is CRITICAL.
//   - A service depending on a DEGRADED node, or transitively behind a
//     CRITICAL node, is WARNING.
//   - Anything not reachable is OK.
func (g *Graph) CascadeImpact(failingService string) []ImpactNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rootHealth := g.health[failingService]
	var rootSeverity Severity
	switch rootHealth {
	case Down, Compromised:
		rootSeverity = SeverityCritical
	case Degraded:
		rootSeverity = SeverityWarning
	default:
		rootSeverity = SeverityNone
	}

	results := []ImpactNode{}
	if rootSeverity == SeverityNone {
		return results
	}

	type queueItem struct {
		name     string
		severity Severity
		path     []string
	}

	visited := map[string]bool{failingService: true}
	queue := []queueItem{{name: failingService, severity: rootSeverity, path: []string{failingService}}}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		for _, dependent := range g.reverse[item.name] {
			if visited[dependent] {
				continue
			}
			visited[dependent] = true

			// Severity degrades by one step as it propagates upward, but
			// never drops below WARNING once something is in the blast radius.
			sev := item.severity
			if sev == SeverityCritical && item.name != failingService {
				sev = SeverityWarning
			}

			path := append(append([]string{}, item.path...), dependent)
			results = append(results, ImpactNode{
				Service:  dependent,
				Severity: sev,
				Health:   g.health[dependent],
				Path:     path,
			})
			queue = append(queue, queueItem{name: dependent, severity: sev, path: path})
		}
	}

	return results
}

// Snapshot is a serializable view of the whole graph, used for the
// /graph API endpoint and for rendering the dependency diagram.
type Snapshot struct {
	Nodes []NodeView `json:"nodes"`
	Edges []EdgeView `json:"edges"`
}

type NodeView struct {
	Name        string      `json:"name"`
	Health      HealthState `json:"health"`
	Criticality int         `json:"criticality"`
}

type EdgeView struct {
	From string `json:"from"` // dependent
	To   string `json:"to"`   // dependency
}

func (g *Graph) Snapshot() Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()

	snap := Snapshot{}
	seen := make(map[string]bool)
	addNode := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		snap.Nodes = append(snap.Nodes, NodeView{
			Name:        name,
			Health:      g.health[name],
			Criticality: g.criticality[name],
		})
	}

	for from, deps := range g.forward {
		addNode(from)
		for _, to := range deps {
			addNode(to)
			snap.Edges = append(snap.Edges, EdgeView{From: from, To: to})
		}
	}
	return snap
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func removeString(slice []string, s string) []string {
	out := slice[:0]
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// ErrNotFound is returned when a lookup targets an unregistered service.
type ErrNotFound struct{ Name string }

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("service %q not found in dependency graph", e.Name)
}
