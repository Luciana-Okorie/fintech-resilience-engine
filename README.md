# Fintech Dependency Risk & Resilience Engine

**Day 12 of 100** — Can a fintech system detect, contain, and survive
the failure of the external services it depends on?

## Why this project

On September 10, 2026, the CBN warned Nigerian banks and fintechs that
cybersecurity and third-party technology risk had become a
financial-stability risk — a weakness or outage at one fintech, PSP,
cloud provider, or vendor can cascade through interconnected
institutions. That's a different (and bigger) question than "build
another payment API": *if my fintech depends on 20 external services,
do I actually know what happens when one of them fails?*

This project is a small, honest attempt to answer that for a
simplified checkout path: `checkout → order-service → payment-service
→ {paystack, postgres}` plus a KYC path, with two monitored mock
providers standing in for real externals.

See `docs/ARCHITECTURE.md` for the full component diagram.

## Stack

Go · PostgreSQL · Redis · Prometheus · Grafana · Docker Compose — a
deliberate departure from the usual React + Node + MongoDB stack, to
force real practice with a compiled backend language, a cache/state
store, and a full observability stack.

## Quickstart

```bash
git clone <this repo>
cd fintech-resilience-engine
docker compose up -d --build
```

This starts:

| Service | URL |
|---|---|
| API | http://localhost:8090 |
| Mock provider A | http://localhost:9001 |
| Mock provider B | http://localhost:9002 |
| Postgres | localhost:5435 |
| Redis | localhost:6401 |
| Prometheus | http://localhost:9091 |
| Grafana | http://localhost:3004 (admin/admin, anonymous viewer enabled) |

Then configure the fallback route (already set by default in
`cmd/api/main.go`, but shown here for clarity):

```bash
curl -X POST http://localhost:8090/route \
  -H "Content-Type: application/json" \
  -d '{"primary":"provider-a","fallbacks":["provider-b"]}'
```

Run the failure demo:

```bash
chmod +x scripts/failure-demo.sh
./scripts/failure-demo.sh
```

This drives `provider-a` through DOWN → recovery and prints the
health, breaker, cascade-impact, and incident state at each stage —
the exact story arc for the Day 12 devlog post.

## API surface

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/dependencies` | Register/update a service + its dependencies |
| `GET`  | `/dependencies` | Same as `/graph` (alias) |
| `GET`  | `/graph` | Full dependency graph snapshot |
| `GET`  | `/graph/impact?service=X` | Cascading impact if `X` fails right now |
| `GET`  | `/health` | Rolling health stats per monitored dependency |
| `GET`  | `/breakers` | Current circuit breaker state per dependency |
| `GET`  | `/risk` | Dynamic risk score per dependency, with factor breakdown |
| `GET`  | `/incidents` | All tracked incidents |
| `POST` | `/route` | Configure fallback ordering for a dependency |
| `POST` | `/simulate/call` | Route one simulated request through breaker + fallback logic |
| `GET`  | `/livez` | Liveness (never depends on Postgres/Redis) |
| `GET`  | `/readyz` | Reports Postgres/Redis reachability, but always `200` |
| `GET`  | `/metrics` | Prometheus scrape endpoint |

Toggle a mock provider's behavior:

```bash
curl -X POST http://localhost:9001/mode -d '{"mode":"DOWN"}'
# modes: NORMAL, DEGRADED, DOWN, TIMEOUT, SLOW
```

## Running tests

```bash
go test ./tests/...
```

Covers the breaker state machine (open/half-open/recovery/error-rate
trip), cascade-impact severity propagation, and multi-signal health
decisions (timeout → DEGRADED, slow-but-200 → DEGRADED). See
`docs/FAILURE_MODES.md` for the full scenario-to-test mapping,
including the two scenarios (Redis/Postgres failure) that are verified
manually via Docker rather than in Go tests, since they're
infrastructure-level, not unit-level.

## Known simplifications (documented, not hidden)

- `HALF_OPEN` allows one trial request without a true semaphore — a
  second concurrent caller during the trial window would also be let
  through in this MVP. A production version would gate on a token.
- `COMPROMISED` health state exists in the model but isn't derived
  automatically — it's a hook for a future security-signal integration.
- The risk score's `recent_incidents` factor reads a flat 24h count;
  decaying by recency is listed as a stretch goal.
- Fallback routing is single-hop (`provider-a → provider-b`), not a
  general shortest-safe-path search across the graph.
- `/simulate/call` approximates "would this real payment succeed?"
  using the dependency's current rolling health state rather than
  actually calling through to the mock provider's `/health` endpoint
  per request — kept simple so the demo is deterministic and fast.

## Devlog story arc

> On September 10, 2026, I came across
> something bigger. The CBN recently warned Nigerian banks and
> fintechs that a failure or cyber incident at one technology provider
> could cascade through the interconnected financial system. That made
> me ask: if my fintech depends on 20 external services, do I actually
> know what happens when one of them fails? So I built a
> dependency-risk engine to find out.
>
> Provider fails → dependency detected → risk calculated → circuit
> opens → traffic rerouted → incident created → provider recovers →
> system restores itself.

Fill in `docs/INCIDENT_SIMULATION_REPORT.md` with the real timestamps
from your own run of `scripts/failure-demo.sh` before posting — a
report with actual numbers is a stronger artifact than a described one.
