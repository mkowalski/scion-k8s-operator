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
// partial content.
func Fetch(ctx context.Context, d Discoverer, dir string) error {
	urls, err := d.BaseURLs(ctx)
	if err != nil {
		return err
	}
	var lastErr error
	for _, base := range urls {
		if err := fetchFrom(ctx, base, dir); err != nil {
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
	if err := json.Unmarshal(topo, &probe); err != nil || probe.IA == "" {
		return fmt.Errorf("invalid topology from %s: %v", base, err)
	}
	list, err := httpGet(ctx, base+"/trcs")
	if err != nil {
		return err
	}
	var entries []trcEntry
	if err := json.Unmarshal(list, &entries); err != nil {
		return fmt.Errorf("invalid trc list: %w", err)
	}
	for _, e := range entries {
		raw, err := httpGet(ctx, fmt.Sprintf("%s/trcs/isd%d-b%d-s%d/blob", base, e.ID.ISD, e.ID.Base, e.ID.Serial))
		if err != nil {
			return err
		}
		name := fmt.Sprintf("ISD%d-B%d-S%d.trc", e.ID.ISD, e.ID.Base, e.ID.Serial)
		if err := atomicWrite(filepath.Join(dir, "certs", name), raw); err != nil {
			return err
		}
	}
	return atomicWrite(filepath.Join(dir, "topology.json"), topo)
}

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
	return io.ReadAll(resp.Body)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
