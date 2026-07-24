// Package health serves /healthz (liveness: process up) and /readyz
// (readiness: bootstrap succeeded AND gateway running), plus /metrics
// (Prometheus default registry, incl. embedded scion component metrics).
package health

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Health struct {
	mu    sync.Mutex
	ready map[string]bool
}

func New() *Health { return &Health{ready: map[string]bool{}} }

func (h *Health) SetReady(component string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready[component] = ok
}

func (h *Health) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range []string{"bootstrap", "gateway"} {
			if !h.ready[c] {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}
