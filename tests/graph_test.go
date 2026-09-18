package tests

import (
	"testing"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
)

func buildTestGraph() *graph.Graph {
	g := graph.New()
	g.Register(graph.Service{Name: "checkout", DependsOn: []string{"order-service"}, Criticality: 5})
	g.Register(graph.Service{Name: "order-service", DependsOn: []string{"payment-service"}, Criticality: 5})
	g.Register(graph.Service{Name: "payment-service", DependsOn: []string{"paystack"}, Criticality: 5})
	g.Register(graph.Service{Name: "paystack", DependsOn: []string{}, Criticality: 5})
	return g
}

func TestCascadeImpact_PropagatesThroughChain(t *testing.T) {
	g := buildTestGraph()
	g.SetHealth("paystack", graph.Down)

	impact := g.CascadeImpact("paystack")

	affected := map[string]graph.Severity{}
	for _, node := range impact {
		affected[node.Service] = node.Severity
	}

	if len(affected) != 3 {
		t.Fatalf("expected 3 affected services (payment-service, order-service, checkout), got %d: %+v", len(affected), affected)
	}
	if affected["payment-service"] != graph.SeverityCritical {
		t.Errorf("expected payment-service CRITICAL (direct dependent of DOWN paystack), got %s", affected["payment-service"])
	}
	if affected["order-service"] != graph.SeverityWarning {
		t.Errorf("expected order-service WARNING (transitively behind the failure), got %s", affected["order-service"])
	}
	if affected["checkout"] != graph.SeverityWarning {
		t.Errorf("expected checkout WARNING (transitively behind the failure), got %s", affected["checkout"])
	}
}

func TestCascadeImpact_NoImpactWhenHealthy(t *testing.T) {
	g := buildTestGraph()
	g.SetHealth("paystack", graph.Healthy)

	impact := g.CascadeImpact("paystack")
	if len(impact) != 0 {
		t.Fatalf("expected no cascading impact from a HEALTHY dependency, got %+v", impact)
	}
}

func TestCascadeImpact_DegradedProducesWarningOnly(t *testing.T) {
	g := buildTestGraph()
	g.SetHealth("paystack", graph.Degraded)

	impact := g.CascadeImpact("paystack")
	for _, node := range impact {
		if node.Severity == graph.SeverityCritical {
			t.Errorf("did not expect CRITICAL severity from a DEGRADED (not DOWN) dependency: %+v", node)
		}
	}
}

func TestRegister_UpdatesReverseEdgesOnRedefinition(t *testing.T) {
	g := graph.New()
	g.Register(graph.Service{Name: "a", DependsOn: []string{"b"}})
	if got := g.Dependents("b"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected b to have dependent 'a', got %v", got)
	}

	// Re-register 'a' without depending on 'b' anymore.
	g.Register(graph.Service{Name: "a", DependsOn: []string{"c"}})
	if got := g.Dependents("b"); len(got) != 0 {
		t.Fatalf("expected b to have no dependents after 'a' dropped it, got %v", got)
	}
	if got := g.Dependents("c"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected c to have dependent 'a', got %v", got)
	}
}
