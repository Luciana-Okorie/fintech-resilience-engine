package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis holds fast-changing shared state: current health snapshot and
// circuit breaker states, so multiple API replicas could (in a future
// iteration) agree on breaker state instead of each holding its own.
// Day 12 runs a single API instance, so this is used mainly to demonstrate
// Test 5 ("kill Redis - does the app die?") - it must NOT.
type Redis struct {
	client *redis.Client
}

func NewRedis(addr string) *Redis {
	return &Redis{
		client: redis.NewClient(&redis.Options{
			Addr:         addr,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  1 * time.Second,
			WriteTimeout: 1 * time.Second,
		}),
	}
}

func (r *Redis) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error { return r.client.Close() }

// SetBreakerState caches a breaker's current state with a short TTL.
// Errors are returned (not swallowed) so callers can decide to
// log-and-continue - this is intentionally NOT on the request hot path for
// Allow()/RecordFailure(), which stay purely in-process for latency and for
// Test 5 (Redis failure must not take down request handling).
func (r *Redis) SetBreakerState(ctx context.Context, dependency, state string) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return r.client.Set(ctx, breakerKey(dependency), state, 5*time.Minute).Err()
}

func (r *Redis) GetBreakerState(ctx context.Context, dependency string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return r.client.Get(ctx, breakerKey(dependency)).Result()
}

// CacheHealthSnapshot stores a JSON snapshot of health stats for fast reads
// by dashboards without hammering the in-process monitor lock.
func (r *Redis) CacheHealthSnapshot(ctx context.Context, snapshot any) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, "health:snapshot", b, 30*time.Second).Err()
}

func breakerKey(dependency string) string {
	return fmt.Sprintf("breaker:%s", dependency)
}
