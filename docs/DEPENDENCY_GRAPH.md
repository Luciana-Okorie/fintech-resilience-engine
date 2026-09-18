# Dependency Graph

The API seeds this graph on startup (`cmd/api/main.go`), modeling a
simplified fintech checkout path plus the two monitored mock providers:

```
                 ┌────────────┐
                 │ PostgreSQL │
                 └─────┬──────┘
                       │
              ┌────────▼────────┐
              │ Payment Service │
              └───────┬─────────┘
                       │
             ┌─────────┴─────────┐
             ↓                   ↓
        Paystack            (postgres, above)

   checkout ← order-service ← payment-service ← { paystack, postgres }
                            ← kyc-service ← { identity-provider, nibss }

   provider-a  (monitored mock, fallback target: none)
   provider-b  (monitored mock, fallback target for provider-a)
```

## Registering a new dependency

```bash
curl -X POST http://localhost:8090/dependencies \
  -H "Content-Type: application/json" \
  -d '{
    "service": "payment-service",
    "dependsOn": ["paystack", "postgres"],
    "criticality": 5
  }'
```

`criticality` is 1 (low) to 5 (business-critical) and feeds directly
into the risk score's `business_criticality` factor.

## Inspecting the graph

- `GET /graph` - full node/edge snapshot (for rendering).
- `GET /graph/impact?service=paystack` - cascading impact if `paystack`
  fails right now, using the current health state.

## Cascade severity rule (Part 4)

Deliberately simple and explainable, per the "no ML model" instruction:

- A service **directly** depending on a `DOWN`/`COMPROMISED` node →
  `CRITICAL`.
- A service depending on a `DEGRADED` node, or **transitively** behind
  a `CRITICAL` node → `WARNING`.
- Anything not reachable from the failing node → not returned (`OK`,
  implicitly).

Applied to the spec's example (Payment Provider goes DOWN):

```
Payment Provider      DOWN
      ↓
Payment Service       🔴 CRITICAL   (direct dependent of the DOWN node)
      ↓
Order Service         ⚠  WARNING    (transitive - one hop further removed)
      ↓
Checkout              ⚠  WARNING    (transitive)
```

(See `tests/graph_test.go::TestCascadeImpact_PropagatesThroughChain` for
the exact, tested severity assignment.)
