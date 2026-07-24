package registrar

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// maxBodyBytes caps PUT bodies; a full sigs set for a large cluster is
// well under this.
const maxBodyBytes = 1 << 20

// Server exposes the sigs registration API:
//
//	PUT /v1/sigs  — replace the operator-managed sigs set (body:
//	                map[name]SIG, names without prefix) and trigger Reload.
//	GET /v1/sigs  — return the current managed set, derived from the
//	                topology file (not in-memory state) so it is accurate
//	                across registrar restarts.
//
// All requests require "Authorization: Bearer <Token>". An empty Token
// fails closed: every request is rejected.
type Server struct {
	TopologyFile string
	Prefix       string
	Token        string
	// Reload is invoked after a successful topology patch. Exec wiring
	// (e.g. systemctl kill -s HUP scion-control) lives in cmd/registrar.
	Reload func() error
	Log    *slog.Logger

	// mu serializes the sigs handlers: PUT is a read-modify-write of the
	// topology file, so concurrent PUTs could otherwise interleave and
	// produce a mixed set (lost update). With the mutex the last
	// completed PUT wins wholesale.
	mu sync.Mutex
}

// Handler returns the HTTP handler for the service.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sigs", s.auth(s.sigs))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := []byte("Bearer " + s.Token)
		got := []byte(r.Header.Get("Authorization"))
		// Fail closed on empty token; constant-time compare otherwise.
		if s.Token == "" || subtle.ConstantTimeCompare(want, got) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) sigs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		s.getSigs(w, r)
	case http.MethodPut:
		s.putSigs(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getSigs(w http.ResponseWriter, _ *http.Request) {
	managed, err := ManagedSigs(s.TopologyFile, s.Prefix)
	if err != nil {
		s.logError("reading topology", err)
		http.Error(w, "reading topology", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(managed)
}

func (s *Server) putSigs(w http.ResponseWriter, r *http.Request) {
	var desired map[string]SIG
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&desired); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := PatchSigs(s.TopologyFile, desired, s.Prefix); err != nil {
		s.logError("patching topology", err)
		http.Error(w, "patching topology", http.StatusInternalServerError)
		return
	}
	if err := s.Reload(); err != nil {
		s.logError("reloading control service", err)
		http.Error(w, "topology updated but reload failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) logError(msg string, err error) {
	if s.Log != nil {
		s.Log.Error(msg, "err", err)
	}
}
