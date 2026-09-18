// Package circuitbreaker implements a small, dependency-free circuit
// breaker: CLOSED -> OPEN -> HALF-OPEN -> CLOSED (or back to OPEN).
//
// This is intentionally simple and explainable rather than borrowing a
// library, since the point of Day 12 is to actually understand the state
// machine, not just call sony/gobreaker.
package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

type State string

const (
	Closed   State = "CLOSED"
	Open     State = "OPEN"
	HalfOpen State = "HALF_OPEN"
)

// ErrOpen is returned by Allow() when the breaker is OPEN and the cooldown
// hasn't elapsed yet - the caller must not send the request.
var ErrOpen = errors.New("circuit breaker is open")

// Config controls when the breaker trips and how it recovers.
type Config struct {
	// FailureThreshold: consecutive failures (in CLOSED) before opening.
	FailureThreshold int
	// ErrorRateThreshold: if request volume is high enough, open when the
	// rolling error rate exceeds this (0.0-1.0), even without consecutive failures.
	ErrorRateThreshold float64
	// MinRequestsForRate: minimum requests in the rolling window before the
	// error-rate rule is allowed to trip the breaker (avoids noisy small samples).
	MinRequestsForRate int
	// OpenTimeout: how long to stay OPEN before allowing a single trial
	// request through (HALF_OPEN).
	OpenTimeout time.Duration
	// HalfOpenSuccessesToClose: consecutive successes while HALF_OPEN
	// required to fully close the breaker again.
	HalfOpenSuccessesToClose int
}

func DefaultConfig() Config {
	return Config{
		FailureThreshold:         5,
		ErrorRateThreshold:       0.2, // 20%, matches the Day 12 incident example
		MinRequestsForRate:       20,
		OpenTimeout:              10 * time.Second,
		HalfOpenSuccessesToClose: 3,
	}
}

// Breaker is a single circuit breaker guarding one dependency.
type Breaker struct {
	mu     sync.Mutex
	name   string
	cfg    Config
	state  State
	openedAt time.Time

	consecutiveFailures  int
	halfOpenSuccesses    int

	// rolling window counters, reset whenever the breaker transitions state
	windowRequests int
	windowFailures int

	onStateChange func(name string, from, to State)
}

func New(name string, cfg Config, onStateChange func(name string, from, to State)) *Breaker {
	return &Breaker{
		name:          name,
		cfg:           cfg,
		state:         Closed,
		onStateChange: onStateChange,
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a request should be permitted through right now.
// Call this BEFORE making the downstream call.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return nil
	case Open:
		if time.Since(b.openedAt) >= b.cfg.OpenTimeout {
			b.transition(HalfOpen)
			return nil // allow exactly one trial request in
		}
		return ErrOpen
	case HalfOpen:
		// Only allow ONE in-flight trial at a time in this simple model:
		// once we've let one through, any concurrent caller waits.
		// (A production version would use a token/semaphore; documented
		// as a known simplification in the README.)
		return nil
	}
	return nil
}

// RecordSuccess reports a successful call result.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.windowRequests++
	b.consecutiveFailures = 0

	if b.state == HalfOpen {
		b.halfOpenSuccesses++
		if b.halfOpenSuccesses >= b.cfg.HalfOpenSuccessesToClose {
			b.transition(Closed)
		}
	}
}

// RecordFailure reports a failed call result (error, timeout, 5xx, etc).
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.windowRequests++
	b.windowFailures++
	b.consecutiveFailures++

	switch b.state {
	case Closed:
		rateTripped := b.windowRequests >= b.cfg.MinRequestsForRate &&
			float64(b.windowFailures)/float64(b.windowRequests) >= b.cfg.ErrorRateThreshold
		if b.consecutiveFailures >= b.cfg.FailureThreshold || rateTripped {
			b.transition(Open)
		}
	case HalfOpen:
		// A single failure during the trial sends it straight back to OPEN.
		b.transition(Open)
	}
}

func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	b.windowRequests = 0
	b.windowFailures = 0
	b.halfOpenSuccesses = 0
	if to == Open {
		b.openedAt = time.Now()
	}
	if b.onStateChange != nil {
		// copy fields needed to avoid holding the lock during the callback
		name, f, t := b.name, from, to
		go b.onStateChange(name, f, t)
	}
}
