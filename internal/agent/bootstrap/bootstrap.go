// Package bootstrap fetches SCION endhost configuration (topology.json and
// TRCs) from a discovery server, following the netsec-ethz/bootstrapper
// protocol, and caches it into a config directory consumable by
// pkg/daemon.LoadASInfoFromFile / WithCertsDir.
//
// Protocol (verified against netsec-ethz/bootstrapper fetcher/scion_openapi.go):
//   - GET <base>/topology                     -> topology.json contents
//   - GET <base>/trcs                         -> JSON array of {"id":{"isd":N,"base_number":N,"serial_number":N}}
//   - GET <base>/trcs/isd{I}-b{B}-s{S}/blob   -> raw TRC bytes
//
// TRCs are stored as certs/ISD{I}-B{B}-S{S}.trc, matching upstream naming.
package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Discoverer resolves the base URL of a SCION discovery server.
type Discoverer interface {
	// BaseURLs returns candidate discovery server base URLs, in order.
	BaseURLs(ctx context.Context) ([]string, error)
}

type trcID struct {
	ISD    int `json:"isd"`
	Base   int `json:"base_number"`
	Serial int `json:"serial_number"`
}
type trcEntry struct {
	ID trcID `json:"id"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Fetch downloads topology.json and all TRCs into dir (creating dir/certs).
// Files are written atomically (tmp + rename) so a running daemon never sees
// partial content. Candidate base URLs are tried in order; the whole
// configuration is downloaded from a single base before anything is written,
// so a failed base never leaves a mixed TRC set on disk.
func Fetch(ctx context.Context, d Discoverer, dir string) error {
	urls, err := d.BaseURLs(ctx)
	if err != nil {
		return err
	}
	if len(urls) == 0 {
		return fmt.Errorf("discoverer returned no discovery server URLs")
	}
	var lastErr error
	for _, base := range urls {
		if err := fetchFrom(ctx, strings.TrimRight(base, "/"), dir); err != nil {
			lastErr = fmt.Errorf("%s: %w", base, err)
			continue
		}
		return nil
	}
	return fmt.Errorf("all discovery servers failed: %w", lastErr)
}

func fetchFrom(ctx context.Context, base, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o755); err != nil {
		return err
	}
	topo, err := httpGet(ctx, base+"/topology")
	if err != nil {
		return err
	}
	var probe struct {
		IA string `json:"isd_as"`
	}
	if err := json.Unmarshal(topo, &probe); err != nil {
		return fmt.Errorf("invalid topology from %s: %w", base, err)
	}
	if probe.IA == "" {
		return fmt.Errorf("topology from %s has no isd_as", base)
	}
	list, err := httpGet(ctx, base+"/trcs")
	if err != nil {
		return err
	}
	var entries []trcEntry
	if err := json.Unmarshal(list, &entries); err != nil {
		return fmt.Errorf("invalid trc list: %w", err)
	}
	// Download everything before writing anything, so a mid-download failure
	// (followed by a fallback to another base) never leaves a mixed TRC set.
	trcs := make(map[string][]byte, len(entries))
	for _, e := range entries {
		raw, err := httpGet(ctx, fmt.Sprintf("%s/trcs/isd%d-b%d-s%d/blob", base, e.ID.ISD, e.ID.Base, e.ID.Serial))
		if err != nil {
			return err
		}
		trcs[fmt.Sprintf("ISD%d-B%d-S%d.trc", e.ID.ISD, e.ID.Base, e.ID.Serial)] = raw
	}
	// Pinning guard: a TRC whose name is pinned (Secret mounted by the
	// operator at <dir>/../pinned-trcs) must match the pin file's bytes
	// exactly. Check all pins before writing anything, so a violation leaves
	// the cache intact.
	for name, raw := range trcs {
		pin, err := os.ReadFile(filepath.Join(dir, "..", "pinned-trcs", name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if string(pin) != string(raw) {
			return fmt.Errorf("pinned TRC %s: discovery server %s served different bytes than pin; refusing to update", name, base)
		}
	}
	// Write TRCs before topology: the daemon reacts to topology.json, so the
	// TRCs it needs must already be in place when it appears.
	for name, raw := range trcs {
		if err := atomicWrite(filepath.Join(dir, "certs", name), raw); err != nil {
			return err
		}
	}
	return atomicWrite(filepath.Join(dir, "topology.json"), topo)
}

// Run fetches once immediately, then re-fetches every interval until ctx is
// done. On every fetch that changed topology.json content, it sends a
// non-blocking notification on changed (used to trigger gateway policy
// regeneration / reload).
func Run(ctx context.Context, d Discoverer, dir string, interval time.Duration,
	changed chan<- struct{}) error {

	fetch := func() {
		before, _ := os.ReadFile(filepath.Join(dir, "topology.json"))
		if err := Fetch(ctx, d, dir); err != nil {
			// Log and keep serving cached material; endhost bootstrap
			// failures must not crash a running data plane.
			fmt.Fprintf(os.Stderr, "bootstrap: fetch failed: %v\n", err)
			return
		}
		after, _ := os.ReadFile(filepath.Join(dir, "topology.json"))
		if string(before) != string(after) {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
	fetch()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			fetch()
		}
	}
}

// maxBodySize bounds discovery server responses; topologies and TRCs are
// small (KBs), 4 MiB is generous.
const maxBodySize = 4 << 20

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodySize {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxBodySize)
	}
	return body, nil
}

// atomicWrite writes data to path via a unique temp file in the same
// directory, fsyncs it, and renames it into place. This guarantees readers
// never observe partial content; durability across power loss is best-effort
// (the containing directory is not fsynced).
func atomicWrite(path string, data []byte) error {
	dir, base := filepath.Split(path)
	f, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
