// Package incident turns health/circuit-breaker state changes into
// structured incidents, matching the INC-001 style record from the Day 12
// spec (severity, dependency, failure reason, affected services, current
// action, fallback, timestamps).
package incident

import (
	"fmt"
	"sync"
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
)

type Severity string

const (
	SevLow      Severity = "LOW"
	SevMedium   Severity = "MEDIUM"
	SevHigh     Severity = "HIGH"
	SevCritical Severity = "CRITICAL"
)

type Status string

const (
	StatusOpen     Status = "OPEN"
	StatusMitigated Status = "MITIGATED" // fallback engaged / breaker open, contained
	StatusResolved Status = "RESOLVED"
)

type Incident struct {
	ID               string    `json:"id"`
	Severity         Severity  `json:"severity"`
	Dependency       string    `json:"dependency"`
	FailureReason    string    `json:"failure_reason"`
	AffectedServices []string  `json:"affected_services"`
	CurrentAction    string    `json:"current_action"`
	Fallback         string    `json:"fallback,omitempty"`
	Status           Status    `json:"status"`
	StartedAt        time.Time `json:"started_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

// Engine tracks open/resolved incidents in memory (persisted to Postgres
// separately via internal/store - see store.SaveIncident).
type Engine struct {
	mu        sync.Mutex
	counter   int
	incidents map[string]*Incident
	// openByDependency lets us find "is there already an open incident for
	// this dependency" so we don't spam duplicate incidents on every sample.
	openByDependency map[string]string
}

func New() *Engine {
	return &Engine{
		incidents:        make(map[string]*Incident),
		openByDependency: make(map[string]string),
	}
}

// Open creates a new incident if one isn't already open for this dependency,
// or updates the existing one's affected-services list if it is.
func (e *Engine) Open(dependency, failureReason string, affected []graph.ImpactNode, severity Severity, action string) *Incident {
	e.mu.Lock()
	defer e.mu.Unlock()

	affectedNames := make([]string, len(affected))
	for i, a := range affected {
		affectedNames[i] = a.Service
	}

	if id, ok := e.openByDependency[dependency]; ok {
		inc := e.incidents[id]
		inc.AffectedServices = affectedNames
		inc.CurrentAction = action
		inc.Severity = severity
		return inc
	}

	e.counter++
	id := fmt.Sprintf("INC-%03d", e.counter)
	inc := &Incident{
		ID:               id,
		Severity:         severity,
		Dependency:       dependency,
		FailureReason:    failureReason,
		AffectedServices: affectedNames,
		CurrentAction:    action,
		Status:           StatusOpen,
		StartedAt:        time.Now(),
	}
	e.incidents[id] = inc
	e.openByDependency[dependency] = id
	return inc
}

// SetFallback records that traffic has been rerouted, and marks the
// incident MITIGATED (still open, but contained).
func (e *Engine) SetFallback(dependency, fallbackTo string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.openByDependency[dependency]
	if !ok {
		return
	}
	inc := e.incidents[id]
	inc.Fallback = fallbackTo
	inc.Status = StatusMitigated
}

// Resolve closes the incident for a dependency once it recovers (breaker
// CLOSED again / health back to HEALTHY).
func (e *Engine) Resolve(dependency string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.openByDependency[dependency]
	if !ok {
		return
	}
	inc := e.incidents[id]
	now := time.Now()
	inc.Status = StatusResolved
	inc.ResolvedAt = &now
	delete(e.openByDependency, dependency)
}

func (e *Engine) All() []*Incident {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Incident, 0, len(e.incidents))
	for _, inc := range e.incidents {
		out = append(out, inc)
	}
	return out
}

func (e *Engine) Get(id string) (*Incident, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	inc, ok := e.incidents[id]
	return inc, ok
}

// SeverityFromRisk maps a risk band (see internal/risk) to incident severity.
// Kept as an explicit mapping rather than reusing the risk.Band type
// directly, so the two concepts (ongoing risk vs. a specific incident) can
// diverge later without a breaking change.
func SeverityFromRisk(band string) Severity {
	switch band {
	case "CRITICAL":
		return SevCritical
	case "HIGH":
		return SevHigh
	case "MEDIUM":
		return SevMedium
	default:
		return SevLow
	}
}
