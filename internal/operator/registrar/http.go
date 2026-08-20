package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTP registers SIGs against the AS-side registrar service (internal/registrar):
// PUT {Endpoint}/v1/sigs with a Bearer token; 204 means the topology was
// patched and the control service reloaded.
type HTTP struct {
	// Endpoint is the service base URL, e.g. http://as-host:8642.
	Endpoint string
	// Token is sent as "Authorization: Bearer <Token>".
	Token string
	// Client is optional; when nil, a client with a 45s timeout is used.
	Client *http.Client
}

// Ensure replaces the full managed SIG set on the AS side.
func (h *HTTP) Ensure(ctx context.Context, sigs []SIG) error {
	body := make(map[string]SIG, len(sigs))
	for _, s := range sigs {
		body[s.Name] = s
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sigs: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.Endpoint+"/v1/sigs", bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.Token)

	c := h.Client
	if c == nil {
		// The registrar may spend up to 30s running its topology reload command.
		// Leave headroom for request handling so a valid reload cannot strand
		// subsequent PUTs behind the registrar's serialization lock.
		c = &http.Client{Timeout: 45 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("put sigs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Include a bounded slice of the body for diagnostics; the body
		// may be empty (e.g. some 502s).
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("put sigs: status %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), bytes.TrimSpace(b))
	}
	return nil
}
