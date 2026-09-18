// Package api exposes the HTTP surface of the resilience engine.
// Deliberately stdlib net/http only (no router dependency) to keep the
// dependency footprint small and the routing easy to follow end to end.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/circuitbreaker"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/health"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/incident"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/risk"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/store"
)

// Server bundles every engine component the HTTP layer needs.
type Server struct {
	Graph    *graph.Graph
	Monitor  *health.Monitor
	Incident *incident.Engine
	Postgres *store.Postgres // may be nil / unreachable - handlers must tolerate that (Test 6)
	Redis    *store.Redis    // may be nil / unreachable - handlers must tolerate that (Test 5)

	breakersMu sync.Mutex
	breakers   map[string]*circuitbreaker.Breaker

	// routes maps a "primary" dependency to an ordered list of fallback
	// dependencies, used by /route (Part 6).
	Routes map[string][]string

	recentIncidents map[string]int // dependency -> count, refreshed periodically from Postgres
}

func NewServer(g *graph.Graph, m *health.Monitor, inc *incident.Engine, pg *store.Postgres, rdb *store.Redis) *Server {
	return &Server{
		Graph:           g,
		Monitor:         m,
		Incident:        inc,
		Postgres:        pg,
		Redis:           rdb,
		breakers:        make(map[string]*circuitbreaker.Breaker),
		Routes:          make(map[string][]string),
		recentIncidents: make(map[string]int),
	}
}

func (s *Server) Breaker(dependency string) *circuitbreaker.Breaker {
	s.breakersMu.Lock()
	defer s.breakersMu.Unlock()
	b, ok := s.breakers[dependency]
	if !ok {
		b = circuitbreaker.New(dependency, circuitbreaker.DefaultConfig(), s.onBreakerStateChange)
		s.breakers[dependency] = b
	}
	return b
}

func (s *Server) onBreakerStateChange(name string, from, to circuitbreaker.State) {
	log.Printf("[circuit-breaker] %s: %s -> %s", name, from, to)

	if s.Redis != nil {
		if err := s.Redis.SetBreakerState(context.Background(), name, string(to)); err != nil {
			log.Printf("[redis] failed to cache breaker state for %s: %v (continuing - Redis is not on the hot path)", name, err)
		}
	}

	switch to {
	case circuitbreaker.Open:
		impact := s.Graph.CascadeImpact(name)
		sev := incident.SevHigh
		fallback := ""
		if len(s.Routes[name]) > 0 {
			fallback = s.Routes[name][0]
		}
		inc := s.Incident.Open(name, "circuit breaker opened: error rate/consecutive failures exceeded threshold", impact, sev, "Circuit breaker OPEN - traffic suspended")
		if fallback != "" {
			s.Incident.SetFallback(name, fallback)
		}
		s.persistIncident(inc)
	case circuitbreaker.Closed:
		s.Incident.Resolve(name)
		if inc, ok := s.findLastIncident(name); ok {
			s.persistIncident(inc)
		}
	}
}

func (s *Server) findLastIncident(dependency string) (*incident.Incident, bool) {
	for _, inc := range s.Incident.All() {
		if inc.Dependency == dependency {
			return inc, true
		}
	}
	return nil, false
}

func (s *Server) persistIncident(inc *incident.Incident) {
	if s.Postgres == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Postgres.SaveIncident(ctx, inc); err != nil {
		log.Printf("[postgres] failed to persist incident %s: %v (continuing - Postgres is not on the hot path)", inc.ID, err)
	}
}

// ---- Routes ----

func (s *Server) Routes_() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/dependencies", s.handleDependencies)
	mux.HandleFunc("/graph", s.handleGraph)
	mux.HandleFunc("/graph/impact", s.handleImpact)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/breakers", s.handleBreakers)
	mux.HandleFunc("/risk", s.handleRisk)
	mux.HandleFunc("/incidents", s.handleIncidents)
	mux.HandleFunc("/route", s.handleRoute)
	mux.HandleFunc("/simulate/call", s.handleSimulateCall)
	mux.HandleFunc("/livez", s.handleLivez)
	mux.HandleFunc("/readyz", s.handleReadyz)
	return mux
}

type registerRequest struct {
	Service     string   `json:"service"`
	DependsOn   []string `json:"dependsOn"`
	Criticality int      `json:"criticality,omitempty"`
}

// POST /dependencies - register/update a service and its dependencies (Part 1).
func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.Graph.Snapshot())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Service == "" {
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}
	s.Graph.Register(graph.Service{Name: req.Service, DependsOn: req.DependsOn, Criticality: req.Criticality})

	if s.Postgres != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Postgres.SaveDependency(ctx, req.Service, req.DependsOn, req.Criticality); err != nil {
			log.Printf("[postgres] failed to persist dependency %s: %v (continuing)", req.Service, err)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

// GET /graph - full graph snapshot for diagram rendering (Part 1).
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Graph.Snapshot())
}

// GET /graph/impact?service=X - cascading impact from a failing dependency (Part 4).
func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "service query param is required", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"failing_service": service,
		"impact":          s.Graph.CascadeImpact(service),
	})
}

// GET /health - rolling health stats per dependency (Part 3).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Monitor.AllStats())
}

// GET /breakers - current circuit breaker state per dependency (Part 5).
func (s *Server) handleBreakers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.BreakerStates())
}

// BreakerStates returns a snapshot of every breaker's current state,
// exported so cmd/api can publish it as a Prometheus gauge.
func (s *Server) BreakerStates() map[string]string {
	s.breakersMu.Lock()
	defer s.breakersMu.Unlock()
	out := make(map[string]string, len(s.breakers))
	for name, b := range s.breakers {
		out[name] = string(b.State())
	}
	return out
}

// GET /risk - dynamic risk score per dependency (Part 8).
func (s *Server) handleRisk(w http.ResponseWriter, r *http.Request) {
	scores := risk.ComputeAll(s.Graph, s.Monitor, s.recentIncidents)
	writeJSON(w, http.StatusOK, scores)
}

// GET /incidents - all tracked incidents (Part 7).
func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Incident.All())
}

type routeRequest struct {
	Primary   string   `json:"primary"`
	Fallbacks []string `json:"fallbacks"`
}

// POST /route - declare fallback ordering for a dependency (Part 6 config).
func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.Routes[req.Primary] = req.Fallbacks
	writeJSON(w, http.StatusOK, map[string]string{"status": "route configured"})
}

type simulateCallRequest struct {
	Dependency string `json:"dependency"`
	// IdempotencyKey MUST be supplied for any call that could be retried on
	// a fallback provider - see Part 6's "safe fallback" requirement.
	// Without it, /simulate/call refuses to fail over, matching the spec's
	// warning that automatic failover without idempotency risks double
	// processing of a payment.
	IdempotencyKey string `json:"idempotency_key"`
}

// POST /simulate/call - routes one simulated request through the breaker +
// fallback logic, demonstrating Part 5 and Part 6 together. This is what
// the "failure demo" in the devlog post should hit repeatedly while a mock
// provider is toggled between healthy/failing.
func (s *Server) handleSimulateCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req simulateCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Dependency == "" {
		http.Error(w, "dependency is required", http.StatusBadRequest)
		return
	}

	result := s.attemptCall(r.Context(), req.Dependency, req.IdempotencyKey, true)
	writeJSON(w, http.StatusOK, result)
}

type callResult struct {
	Dependency string `json:"dependency"`
	UsedFallback bool `json:"used_fallback"`
	Success    bool   `json:"success"`
	Reason     string `json:"reason,omitempty"`
}

func (s *Server) attemptCall(ctx context.Context, dependency, idempotencyKey string, allowFallback bool) callResult {
	b := s.Breaker(dependency)
	if err := b.Allow(); err != nil {
		// Breaker is OPEN - try a fallback ONLY if the caller gave us an
		// idempotency key. This is the "don't automatically fail over every
		// request" safeguard from Part 6.
		if allowFallback && idempotencyKey != "" && len(s.Routes[dependency]) > 0 {
			for _, fb := range s.Routes[dependency] {
				res := s.attemptCall(ctx, fb, idempotencyKey, false) // no chained fallback
				if res.Success {
					s.Incident.SetFallback(dependency, fb)
					return callResult{Dependency: dependency, UsedFallback: true, Success: true, Reason: "rerouted to " + fb}
				}
			}
		}
		return callResult{Dependency: dependency, Success: false, Reason: "circuit open, no safe fallback available"}
	}

	stats := s.Monitor.Stats(dependency)
	// Use the live rolling error rate as the simulated "did this call
	// succeed" probability so /simulate/call reflects whatever the mock
	// provider is currently doing.
	success := stats.State == graph.Healthy || stats.State == graph.Unknown
	if success {
		b.RecordSuccess()
		return callResult{Dependency: dependency, Success: true}
	}
	b.RecordFailure()
	if allowFallback && idempotencyKey != "" && len(s.Routes[dependency]) > 0 {
		for _, fb := range s.Routes[dependency] {
			res := s.attemptCall(ctx, fb, idempotencyKey, false)
			if res.Success {
				s.Incident.SetFallback(dependency, fb)
				return callResult{Dependency: dependency, UsedFallback: true, Success: true, Reason: "rerouted to " + fb}
			}
		}
	}
	return callResult{Dependency: dependency, Success: false, Reason: "call failed, no safe fallback available"}
}

// GET /livez - process is up. Must NOT depend on Postgres/Redis (Tests 5 & 6).
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// GET /readyz - reports dependency reachability but still returns 200 for
// the parts that ARE up, so a Postgres or Redis outage degrades rather than
// takes the whole API down (Tests 5 & 6).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	status := map[string]string{"api": "ok"}

	if s.Postgres != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()
		if err := s.Postgres.Ping(ctx); err != nil {
			status["postgres"] = "unreachable"
		} else {
			status["postgres"] = "ok"
		}
	}
	if s.Redis != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()
		if err := s.Redis.Ping(ctx); err != nil {
			status["redis"] = "unreachable"
		} else {
			status["redis"] = "ok"
		}
	}
	// Always 200: the point of Tests 5 & 6 is that losing Postgres/Redis
	// degrades observability, not availability.
	writeJSON(w, http.StatusOK, status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] failed to encode response: %v", err)
	}
}
