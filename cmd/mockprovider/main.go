// cmd/mockprovider simulates an external dependency (e.g. Paystack, a KYC
// provider) whose behaviour can be toggled at runtime via POST /mode, so the
// failure scenarios in the Day 12 spec (timeout, 503, slow response,
// recovery) can be driven from a test script or curl during the demo.
package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

type Mode string

const (
	ModeNormal  Mode = "NORMAL"  // fast, always 200
	ModeDegraded Mode = "DEGRADED" // occasional errors + added latency
	ModeDown    Mode = "DOWN"    // always 503
	ModeTimeout Mode = "TIMEOUT" // hangs past the caller's timeout
	ModeSlow    Mode = "SLOW"    // always 200 but after an 8s delay (Test 7)
)

type state struct {
	mu   sync.RWMutex
	mode Mode
}

func (s *state) get() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

func (s *state) set(m Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = m
}

func main() {
	name := getenv("PROVIDER_NAME", "provider")
	addr := getenv("LISTEN_ADDR", ":9001")

	st := &state{mode: ModeNormal}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		switch st.get() {
		case ModeNormal:
			respond(w, http.StatusOK, name, "ok")
		case ModeDegraded:
			if rand.Float64() < 0.3 {
				time.Sleep(1500 * time.Millisecond)
				respond(w, http.StatusInternalServerError, name, "degraded: intermittent failure")
				return
			}
			time.Sleep(800 * time.Millisecond)
			respond(w, http.StatusOK, name, "ok (degraded latency)")
		case ModeDown:
			respond(w, http.StatusServiceUnavailable, name, "down")
		case ModeTimeout:
			// Sleep far longer than any reasonable client timeout; the
			// HTTPChecker's own context deadline will cut this off client-side.
			time.Sleep(30 * time.Second)
			respond(w, http.StatusOK, name, "ok (too late)")
		case ModeSlow:
			time.Sleep(8 * time.Second)
			respond(w, http.StatusOK, name, "ok (slow) - Test 7: technically available != healthy")
		}
	})

	// POST /mode {"mode": "DOWN"} - used by the failure-demo script.
	mux.HandleFunc("/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			respond(w, http.StatusOK, name, string(st.get()))
			return
		}
		var body struct {
			Mode Mode `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		st.set(body.Mode)
		log.Printf("[%s] mode set to %s", name, body.Mode)
		respond(w, http.StatusOK, name, "mode set to "+string(body.Mode))
	})

	log.Printf("mock provider %q listening on %s (mode=%s)", name, addr, st.get())
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func respond(w http.ResponseWriter, status int, provider, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"provider": provider,
		"message":  message,
	})
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
