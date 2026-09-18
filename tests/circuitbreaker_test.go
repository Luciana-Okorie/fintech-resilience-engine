package tests

import (
	"testing"
	"time"

	"github.com/luciana-okorie/fintech-resilience-engine/internal/circuitbreaker"
)

func TestBreaker_OpensOnConsecutiveFailures(t *testing.T) {
	cfg := circuitbreaker.DefaultConfig()
	cfg.FailureThreshold = 3
	cfg.OpenTimeout = 50 * time.Millisecond

	b := circuitbreaker.New("test-dep", cfg, nil)

	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("expected CLOSED to allow requests, got err: %v", err)
		}
		b.RecordFailure()
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("expected still CLOSED after 2 failures, got %s", b.State())
	}

	b.RecordFailure() // 3rd consecutive failure -> should open
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN after reaching failure threshold, got %s", b.State())
	}

	if err := b.Allow(); err != circuitbreaker.ErrOpen {
		t.Fatalf("expected ErrOpen immediately after opening, got %v", err)
	}
}

func TestBreaker_HalfOpenRecovery(t *testing.T) {
	cfg := circuitbreaker.DefaultConfig()
	cfg.FailureThreshold = 1
	cfg.OpenTimeout = 20 * time.Millisecond
	cfg.HalfOpenSuccessesToClose = 2

	b := circuitbreaker.New("test-dep", cfg, nil)

	b.RecordFailure() // opens immediately (threshold=1)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN, got %s", b.State())
	}

	time.Sleep(30 * time.Millisecond) // wait past OpenTimeout

	if err := b.Allow(); err != nil {
		t.Fatalf("expected trial request to be allowed after timeout, got %v", err)
	}
	if b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("expected HALF_OPEN after timeout elapses, got %s", b.State())
	}

	b.RecordSuccess()
	if b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("expected still HALF_OPEN after 1 of 2 required successes, got %s", b.State())
	}

	b.RecordSuccess()
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("expected CLOSED after required successive successes, got %s", b.State())
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	cfg := circuitbreaker.DefaultConfig()
	cfg.FailureThreshold = 1
	cfg.OpenTimeout = 10 * time.Millisecond

	b := circuitbreaker.New("test-dep", cfg, nil)
	b.RecordFailure() // OPEN
	time.Sleep(15 * time.Millisecond)
	_ = b.Allow() // transitions to HALF_OPEN

	b.RecordFailure() // trial fails -> back to OPEN
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN again after a failed trial request, got %s", b.State())
	}
}

func TestBreaker_ErrorRateThresholdTripsWithoutConsecutiveFailures(t *testing.T) {
	cfg := circuitbreaker.DefaultConfig()
	cfg.FailureThreshold = 1000 // effectively disable the consecutive-failure rule
	cfg.MinRequestsForRate = 10
	cfg.ErrorRateThreshold = 0.2

	b := circuitbreaker.New("test-dep", cfg, nil)

	// Interleave successes/failures so consecutive-failure count never
	// climbs high, but the rolling error rate exceeds 20%.
	pattern := []bool{true, true, true, true, false, true, true, true, false, false}
	for _, success := range pattern {
		_ = b.Allow()
		if success {
			b.RecordSuccess()
		} else {
			b.RecordFailure()
		}
	}

	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN once rolling error rate exceeded threshold, got %s", b.State())
	}
}
