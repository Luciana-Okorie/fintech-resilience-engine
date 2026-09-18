# Architecture

```
                         ┌─────────────────┐
                         │   API Gateway   │   (net/http mux, cmd/api)
                         └────────┬────────┘
                                  │
                        ┌──────────────────┐
                        │ Dependency       │
                        │ Risk Engine      │   (internal/api.Server)
                        └────────┬─────────┘
                                 │
                ┌────────────────┼────────────────┐
                ↓                ↓                 ↓
          Health Monitor    Risk Engine       Graph Engine
        (internal/health)  (internal/risk)  (internal/graph)
                ↓                ↓                 ↓
                └────────────────┼────────────────┘
                                 ↓
                        ┌──────────────────┐
                        │ Circuit Breaker  │   (internal/circuitbreaker)
                        └────────┬─────────┘
                                 │
                        Routing / Fallback
                          ↙            ↘
                   provider-a      provider-b
                   (mock, :9001)   (mock, :9002)
```

## Components

| Component | Package | Responsibility |
|---|---|---|
| API Gateway | `cmd/api`, `internal/api` | HTTP surface, wires everything together |
| Graph Engine | `internal/graph` | Dependency registration, cascade-impact BFS |
| Health Monitor | `internal/health` | Periodic checks, multi-signal state decisions |
| Risk Engine | `internal/risk` | Transparent additive risk scoring |
| Circuit Breaker | `internal/circuitbreaker` | CLOSED/OPEN/HALF_OPEN state machine per dependency |
| Incident Engine | `internal/incident` | Opens/mitigates/resolves incidents from state changes |
| Store | `internal/store` | PostgreSQL (durability) + Redis (fast shared state) |
| Mock Providers | `cmd/mockprovider` | Toggleable fake externals for the failure demo |

## Data flow for one failure

1. `health.Monitor` polls `provider-a` every 5s via `HTTPChecker`.
2. Samples accumulate in a rolling window; `computeStats` turns error
   rate + latency into a `HealthState` (see `docs/FAILURE_MODES.md`).
3. On a state change, `graph.Graph.SetHealth` updates the node, and
   `api.Server.onBreakerStateChange` (triggered separately by the
   breaker, see below) computes `graph.CascadeImpact` to find every
   business capability behind the failing dependency.
4. Every call routed through `/simulate/call` first checks
   `circuitbreaker.Breaker.Allow()`. Enough failures trip it to `OPEN`.
5. `OPEN` triggers `incident.Engine.Open(...)` with the cascade impact
   attached, and - only if the caller supplied an idempotency key -
   attempts the configured fallback (`provider-b`).
6. After `OpenTimeout`, the breaker allows one trial request
   (`HALF_OPEN`). Enough consecutive successes closes it again, which
   resolves the incident.

## Why Postgres and Redis are NOT on the hot path

Tests 5 and 6 in the Day 12 spec require the system to survive losing
Redis or Postgres. Concretely:

- Circuit breaker state lives in an in-process `sync.Mutex`-guarded
  struct (`internal/circuitbreaker`), not in Redis. Redis only receives
  a best-effort cache write for cross-instance visibility.
- The dependency graph lives in an in-process `Graph` struct
  (`internal/graph`), not in Postgres. Postgres only receives a
  best-effort write for durability/restart-recovery.
- Every store call site logs-and-continues on error rather than
  propagating a fatal error (see `internal/api/handlers.go`,
  `onBreakerStateChange`, `persistIncident`).
- `/readyz` reports Postgres/Redis reachability but always returns
  `200` - losing them degrades observability, not availability.
