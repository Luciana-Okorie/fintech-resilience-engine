// Package store holds persistence adapters (PostgreSQL, Redis).
//
// Design note (Test 6 - "database failure" from the Day 12 spec): the API
// must keep serving graph/health/circuit-breaker reads from in-memory state
// even if Postgres is down. Postgres here is for durability of the
// dependency registry and incident history, NOT the hot path for health
// checks or circuit breaker decisions. Every write method returns an error
// instead of panicking, and callers (see internal/api) log-and-continue on
// failure rather than taking the process down.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/lib/pq"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/incident"
)

type Postgres struct {
	db *sql.DB
}

func NewPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Postgres{db: db}, nil
}

func (p *Postgres) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.db.PingContext(ctx)
}

func (p *Postgres) Close() error { return p.db.Close() }

// SaveDependency upserts a service's dependency-graph registration.
func (p *Postgres) SaveDependency(ctx context.Context, service string, dependsOn []string, criticality int) error {
	payload, err := json.Marshal(dependsOn)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO dependencies (service, depends_on, criticality, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (service) DO UPDATE
		SET depends_on = EXCLUDED.depends_on,
		    criticality = EXCLUDED.criticality,
		    updated_at = now()
	`, service, payload, criticality)
	return err
}

// LoadDependencies reads back the full registry, e.g. on API startup so the
// in-memory graph survives a restart.
func (p *Postgres) LoadDependencies(ctx context.Context) (map[string][]string, map[string]int, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT service, depends_on, criticality FROM dependencies`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	deps := make(map[string][]string)
	crit := make(map[string]int)
	for rows.Next() {
		var service string
		var raw []byte
		var criticality int
		if err := rows.Scan(&service, &raw, &criticality); err != nil {
			return nil, nil, err
		}
		var dependsOn []string
		if err := json.Unmarshal(raw, &dependsOn); err != nil {
			return nil, nil, err
		}
		deps[service] = dependsOn
		crit[service] = criticality
	}
	return deps, crit, rows.Err()
}

// SaveIncident upserts an incident row (called on open, fallback, resolve).
func (p *Postgres) SaveIncident(ctx context.Context, inc *incident.Incident) error {
	affected, err := json.Marshal(inc.AffectedServices)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO incidents (id, severity, dependency, failure_reason, affected_services,
		                        current_action, fallback, status, started_at, resolved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO UPDATE
		SET severity = EXCLUDED.severity,
		    affected_services = EXCLUDED.affected_services,
		    current_action = EXCLUDED.current_action,
		    fallback = EXCLUDED.fallback,
		    status = EXCLUDED.status,
		    resolved_at = EXCLUDED.resolved_at
	`, inc.ID, inc.Severity, inc.Dependency, inc.FailureReason, affected,
		inc.CurrentAction, inc.Fallback, inc.Status, inc.StartedAt, inc.ResolvedAt)
	return err
}

// RecentIncidentCount returns how many incidents opened for `dependency`
// within the given window - fed into the risk score's "recent_incidents" factor.
func (p *Postgres) RecentIncidentCount(ctx context.Context, dependency string, since time.Duration) (int, error) {
	var count int
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM incidents
		WHERE dependency = $1 AND started_at >= now() - $2::interval
	`, dependency, fmt.Sprintf("%d seconds", int(since.Seconds()))).Scan(&count)
	return count, err
}
