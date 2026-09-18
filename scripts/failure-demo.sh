#!/usr/bin/env bash
# Day 12 failure demo: drives mock-provider-a through the story arc from
# the devlog post: "Provider fails -> dependency detected -> risk
# calculated -> circuit opens -> traffic rerouted -> incident created ->
# provider recovers -> system restores itself."
#
# Run this AFTER `docker compose up -d` and after seeding routes (see README).
set -euo pipefail

API=${API:-http://localhost:8090}
PROVIDER_A=${PROVIDER_A:-http://localhost:9001}

step() { echo; echo "=== $1 ==="; }
show() { echo "--> $1"; curl -s "$2" | (command -v jq >/dev/null && jq . || cat); echo; }

step "1. Baseline: provider-a is healthy"
curl -s -X POST "$PROVIDER_A/mode" -d '{"mode":"NORMAL"}' >/dev/null
sleep 6
show "health" "$API/health"
show "risk scores" "$API/risk"

step "2. Fail provider-a (simulating a Paystack-style outage)"
curl -s -X POST "$PROVIDER_A/mode" -d '{"mode":"DOWN"}' >/dev/null
echo "Waiting for the health monitor to detect the failure and the breaker to trip..."
sleep 15
show "health (should show provider-a DOWN)" "$API/health"
show "breakers (should show provider-a OPEN)" "$API/breakers"
show "cascading impact" "$API/graph/impact?service=provider-a"
show "incidents (should show a new INC-xxx)" "$API/incidents"

step "3. Route a simulated request through the breaker + fallback"
show "simulate call (should reroute to provider-b)" ""
curl -s -X POST "$API/simulate/call" \
  -H "Content-Type: application/json" \
  -d '{"dependency":"provider-a","idempotency_key":"demo-key-001"}' | (command -v jq >/dev/null && jq . || cat)
echo

step "4. Recover provider-a"
curl -s -X POST "$PROVIDER_A/mode" -d '{"mode":"NORMAL"}' >/dev/null
echo "Waiting for the health monitor + half-open trial to close the breaker..."
sleep 25
show "health (should show provider-a HEALTHY again)" "$API/health"
show "breakers (should show provider-a CLOSED)" "$API/breakers"
show "incidents (should show the incident RESOLVED)" "$API/incidents"

echo
echo "Demo complete."
