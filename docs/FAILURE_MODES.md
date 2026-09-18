# Failure-Mode Table

## Health state decision (internal/health, `computeStats`)

Multi-signal, not "HTTP 500 = DOWN":

| Condition (over rolling window of last 50 samples) | State |
|---|---|
| No samples yet | `UNKNOWN` |
| Error rate ≥ 50% | `DOWN` |
| Error rate ≥ 5% (but < 50%) | `DEGRADED` |
| Error rate < 5% but avg latency ≥ 3s | `DEGRADED` ("technically available ≠ healthy") |
| Error rate < 5% and avg latency < 3s | `HEALTHY` |

`COMPROMISED` is not derived automatically from health checks (a slow
provider isn't necessarily malicious) - it's intended to be set
manually or by a future security-signal integration, e.g. `POST
/dependencies` metadata or a SIEM webhook. Documented as a stretch
goal, not implemented in this MVP.

## Circuit breaker triggers (internal/circuitbreaker)

| Trigger | Threshold (default) | Result |
|---|---|---|
| Consecutive failures | 5 | `CLOSED` → `OPEN` |
| Rolling error rate (min. 20 requests sampled) | ≥ 20% | `CLOSED` → `OPEN` |
| Time since opening | 10s | `OPEN` → `HALF_OPEN` (one trial request allowed) |
| Trial request fails | any | `HALF_OPEN` → `OPEN` |
| Consecutive trial successes | 3 | `HALF_OPEN` → `CLOSED` |

## Required test scenarios (Day 12 spec) → where each is covered

| # | Scenario | Covered by |
|---|---|---|
| 1 | Provider timeout → `DEGRADED` | `tests/health_test.go::TestMonitor_DetectsDegradedOnTimeouts`, `cmd/mockprovider` `ModeTimeout` |
| 2 | Provider fully down (503) → breaker `OPEN` | `tests/circuitbreaker_test.go::TestBreaker_OpensOnConsecutiveFailures`, `mockprovider` `ModeDown` |
| 3 | Recovery: `OPEN` → `HALF_OPEN` → `CLOSED` | `tests/circuitbreaker_test.go::TestBreaker_HalfOpenRecovery` |
| 4 | Cascading failure correctly flags dependents | `tests/graph_test.go::TestCascadeImpact_PropagatesThroughChain` |
| 5 | Redis failure doesn't kill the app | `internal/store/redis.go` design notes; `/readyz` always 200; manually verify with `docker compose stop redis` then hit `/breakers` and `/graph` |
| 6 | Postgres failure fails predictably, no corruption | `internal/store/postgres.go` design notes; manually verify with `docker compose stop postgres` then hit `/dependencies` (POST) and confirm in-memory graph still updates even though persistence logs an error |
| 7 | Slow-but-200 provider recognized as unhealthy | `tests/health_test.go::TestMonitor_SlowButSuccessfulIsDegraded`, `mockprovider` `ModeSlow` |
| 8 | Fallback provider used safely (idempotency-gated) | `internal/api/handlers.go::attemptCall` - fallback only fires when `idempotency_key` is supplied |

## Manual verification for Tests 5 & 6

```bash
docker compose up -d
docker compose stop redis
curl http://localhost:8090/readyz     # redis: "unreachable", api still "ok"
curl http://localhost:8090/graph      # still works - graph is in-memory
docker compose start redis

docker compose stop postgres
curl -X POST http://localhost:8090/dependencies \
  -H "Content-Type: application/json" \
  -d '{"service":"test-svc","dependsOn":[]}'   # still 201 - in-memory graph updates,
                                                 # Postgres write fails and is logged,
                                                 # nothing crashes
docker compose start postgres
```
