package registrar

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	asregistrar "github.com/mkowalski/scion-k8s-operator/internal/registrar"
)

// DefaultTimeout is the default HTTP client timeout for registrar calls.
// The registrar serializes PUTs, so in the worst case a request waits behind
// one in-flight topology reload and then runs its own: 2*ReloadTimeout, plus
// headroom for request handling and network latency.
const DefaultTimeout = 2*asregistrar.ReloadTimeout + 10*time.Second

// HTTP registers SIGs against the AS-side registrar service (internal/registrar):
// PUT {Endpoint}/v1/sigs with a Bearer token; 204 means the topology was
// patched and the control service reloaded.
type HTTP struct {
	// Endpoint is the service base URL, e.g. https://as-host:8642.
	Endpoint string
	// Token is sent as "Authorization: Bearer <Token>".
	Token string
	// CABundle optionally holds PEM certificates trusted for the
	// registrar's TLS endpoint (typically a private AS CA or the
	// self-signed server certificate). When empty, the system roots are
	// used. Invalid PEM is an error, never a silent fallback.
	CABundle []byte
	// Client is optional; when nil, a client with DefaultTimeout (and
	// CABundle, if set) is used.
	Client *http.Client
}

// client returns the configured client or builds the default one.
func (h *HTTP) client() (*http.Client, error) {
	if h.Client != nil {
		return h.Client, nil
	}
	c := &http.Client{Timeout: DefaultTimeout}
	if len(h.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(h.CABundle) {
			return nil, errors.New("registrar CA bundle contains no valid PEM certificates")
		}
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	}
	return c, nil
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

	c, err := h.client()
	if err != nil {
		return err
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
