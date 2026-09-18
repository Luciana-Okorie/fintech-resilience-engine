# Incident Simulation Report (template)

Run `scripts/failure-demo.sh` and fill this in from the actual output -
this is a template, not a fabricated result.

## Scenario
Simulated outage of `provider-a` (standing in for a Paystack-style
delayed/failed processor), while `provider-b` remains healthy.

## Timeline

| Time | Event |
|---|---|
| T+0s | `provider-a` mode set to `DOWN` |
| T+? | Health monitor first records a failed sample |
| T+? | Rolling error rate crosses 50% → health state `DOWN` |
| T+? | Circuit breaker for `provider-a` trips `OPEN` |
| T+? | `graph.CascadeImpact("provider-a")` computed |
| T+? | Incident `INC-00X` opened, severity `HIGH` |
| T+? | `/simulate/call` with an idempotency key reroutes to `provider-b` |
| T+? | Incident marked `MITIGATED` (fallback engaged) |
| T+? | `provider-a` mode set back to `NORMAL` |
| T+? | Breaker transitions `OPEN` → `HALF_OPEN` after `OpenTimeout` |
| T+? | 3 consecutive successful trial requests → `CLOSED` |
| T+? | Incident marked `RESOLVED` |

## Cascading impact observed

_Paste the `/graph/impact?service=provider-a` response here._

## Risk score before / during / after

_Paste `/risk` output for `provider-a` at each stage - this should show
the score rising as error rate and recent-incident count increase, and
falling back down after recovery._

## Lessons

- What would have happened without the circuit breaker? (Hint: every
  request to `checkout` would have kept hitting a dead provider at
  full latency until manual intervention.)
- Why did the fallback require an idempotency key? What could go wrong
  without one, specifically for a payment call?
- Was `HALF_OPEN`'s "one trial request" enough evidence to fully
  reopen the gate, or would you want more trial volume in production?
