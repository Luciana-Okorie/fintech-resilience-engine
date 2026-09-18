package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/api"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/graph"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/health"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/incident"
	"github.com/luciana-okorie/fintech-resilience-engine/internal/store"
)

var (
	dependencyHealthGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dependency_health_state",
		Help: "Current health state of each dependency (1=HEALTHY,0.5=DEGRADED,0=DOWN/COMPROMISED,-1=UNKNOWN)",
	}, []string{"dependency"})

	circuitBreakerStateGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "circuit_breaker_state",
		Help: "Current circuit breaker state (0=CLOSED,1=HALF_OPEN,2=OPEN)",
	}, []string{"dependency"})

	dependencyErrorRate = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dependency_error_rate",
		Help: "Rolling-window error rate per dependency",
	}, []string{"dependency"})

	openIncidentsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "open_incidents_total",
		Help: "Number of currently open (non-resolved) incidents",
	})
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	postgresDSN := getenv("POSTGRES_DSN", "postgres://resilience:resilience@postgres:5432/resilience?sslmode=disable")
	redisAddr := getenv("REDIS_ADDR", "redis:6379")
	httpAddr := getenv("HTTP_ADDR", ":8090")
	providerAURL := getenv("PROVIDER_A_URL", "http://mock-provider-a:9001/health")
	providerBURL := getenv("PROVIDER_B_URL", "http://mock-provider-b:9002/health")

	g := graph.New()

	// --- Part 1: seed the dependency graph from the Day 12 spec's example ---
	g.Register(graph.Service{Name: "checkout", DependsOn: []string{"order-service"}, Criticality: 5})
	g.Register(graph.Service{Name: "order-service", DependsOn: []string{"payment-service", "kyc-service"}, Criticality: 5})
	g.Register(graph.Service{Name: "payment-service", DependsOn: []string{"paystack", "postgres", "provider-a"}, Criticality: 5})
	g.Register(graph.Service{Name: "kyc-service", DependsOn: []string{"identity-provider", "nibss"}, Criticality: 4})
	g.Register(graph.Service{Name: "paystack", DependsOn: []string{}, Criticality: 5})
	g.Register(graph.Service{Name: "identity-provider", DependsOn: []string{}, Criticality: 3})
	g.Register(graph.Service{Name: "nibss", DependsOn: []string{}, Criticality: 4})
	g.Register(graph.Service{Name: "postgres", DependsOn: []string{}, Criticality: 5})
	g.Register(graph.Service{Name: "provider-a", DependsOn: []string{}, Criticality: 4})
	g.Register(graph.Service{Name: "provider-b", DependsOn: []string{}, Criticality: 3})

	// --- Postgres (durability - see internal/store/postgres.go for the
	// "must not be on the hot path" rationale, Test 6) ---
	pg, err := store.NewPostgres(postgresDSN)
	if err != nil {
		log.Printf("[postgres] could not initialize client: %v (continuing without persistence)", err)
		pg = nil
	} else if err := pg.Ping(ctx); err != nil {
		log.Printf("[postgres] not reachable at startup: %v (continuing - Test 6 requires we survive this)", err)
	}

	// --- Redis (Test 5 - must not be on the hot path either) ---
	rdb := store.NewRedis(redisAddr)
	if err := rdb.Ping(ctx); err != nil {
		log.Printf("[redis] not reachable at startup: %v (continuing - Test 5 requires we survive this)", err)
	}

	// --- Part 3: health monitor against the two mock providers ---
	monitor := health.NewMonitor(g, health.DefaultThresholds(), 50, func(dep string, from, to graph.HealthState) {
		log.Printf("[health] %s: %s -> %s", dep, from, to)
	})
	monitor.Register("provider-a", health.NewHTTPChecker(providerAURL))
	monitor.Register("provider-b", health.NewHTTPChecker(providerBURL))

	incidentEngine := incident.New()

	srv := api.NewServer(g, monitor, incidentEngine, pg, rdb)
	// --- Part 6: default fallback routing, provider-a -> provider-b ---
	srv.Routes["provider-a"] = []string{"provider-b"}

	go monitor.Run(ctx, 5*time.Second)
	go publishMetrics(ctx, g, srv, monitor, incidentEngine)

	mux := http.NewServeMux()
	mux.Handle("/", srv.Routes_())
	mux.Handle("/metrics", promhttp.Handler())

	httpServer := &http.Server{Addr: httpAddr, Handler: mux}

	go func() {
		log.Printf("resilience engine listening on %s", httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if pg != nil {
		_ = pg.Close()
	}
	_ = rdb.Close()
}

// publishMetrics periodically pushes current in-memory state into the
// Prometheus gauges above, for Grafana to render.
func publishMetrics(ctx context.Context, g *graph.Graph, srv *api.Server, monitor *health.Monitor, incEngine *incident.Engine) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, svc := range g.Services() {
				state, _ := g.Health(svc)
				dependencyHealthGauge.WithLabelValues(svc).Set(healthToFloat(state))
			}
			for dep, stats := range monitor.AllStats() {
				dependencyErrorRate.WithLabelValues(dep).Set(stats.ErrorRate)
			}
			for dep, state := range srv.BreakerStates() {
				circuitBreakerStateGauge.WithLabelValues(dep).Set(breakerStateToFloat(state))
			}
			openCount := 0
			for _, inc := range incEngine.All() {
				if inc.Status != incident.StatusResolved {
					openCount++
				}
			}
			openIncidentsGauge.Set(float64(openCount))
		}
	}
}

func breakerStateToFloat(state string) float64 {
	switch state {
	case "CLOSED":
		return 0
	case "HALF_OPEN":
		return 1
	case "OPEN":
		return 2
	default:
		return -1
	}
}

func healthToFloat(s graph.HealthState) float64 {
	switch s {
	case graph.Healthy:
		return 1
	case graph.Degraded:
		return 0.5
	case graph.Down, graph.Compromised:
		return 0
	default:
		return -1
	}
}
