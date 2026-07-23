# SCION Kubernetes Operator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every OpenShift node a first-class SCION endhost with transparent bidirectional IP-over-SCION, delivered as an operator-managed per-node agent, per `docs/superpowers/specs/2026-07-23-scion-k8s-operator-design.md`.

**Architecture:** Three Go binaries in one module: `scion-node-agent` (DaemonSet: bootstrap + embedded SCION daemon connector + embedded SCION-IP gateway), `scion-operator` (reconciles the `ScionNetwork` CRD into DaemonSet/RBAC/SCC + registrar controller), `scion-registrar` (small HTTP service run beside an OSS SCION control service to patch `topology.json` `sigs` entries).

**Tech Stack:** Go 1.26, `github.com/scionproto/scion v0.15.0` (embedded as a library: `gateway`, `pkg/daemon` standalone connector), controller-runtime + controller-gen (no kubebuilder scaffolding), client-go, distroless images, kustomize manifests + OLM bundle.

**Key API facts (verified against scionproto/scion v0.15.0, clone at /tmp/opencode/scion):**
- `pkg/daemon.NewStandaloneConnector(ctx, asinfo, opts...) (daemon.Connector, error)` (`pkg/daemon/standalone.go:109`) runs path lookup/trust in-process — no sciond gRPC service needed for the gateway. `pkg/daemon.LoadASInfoFromFile(topoFile)` (`standalone.go:90`); options `WithCertsDir(dir)`, `WithPeriodicCleanup()`, `WithMetrics()`.
- `gateway.Gateway` struct (`gateway/gateway.go:143`) with `Run(ctx) error` (`:206`) does everything: tun creation (`routemgr.FixedTunnelName`), Linux route programming (`routemgr.Linux`), SGRP, sessions. Embedder wiring is copied from `gateway/cmd/gateway/main.go:56-185`.
- Advertised/accepted prefixes are controlled by the **IP routing policy file** (`gateway/routing`, text format `<action> <from-ia> <to-ia> <prefixes...>`) and the **traffic policy JSON** (`{"ASes":{"<ia>":{"Nets":[...]}},"ConfigVersion":N}`, sample `dist/conffiles/gateway.json`). Route guardrails = generated accept rules, not custom netlink.
- `Gateway.ConfigReloadTrigger chan struct{}` re-reads both policy files — the agent regenerates files and signals this channel; no gateway restart on config change.
- Bootstrap protocol (netsec-ethz/bootstrapper): HTTP GET `<base>/topology` and `<base>/trcs` + `<base>/trcs/isd{I}-b{B}-s{S}` (executor MUST verify exact paths against `netsec-ethz/bootstrapper` fetcher source in Task 4).

**Scope notes (deviations to carry back into spec if accepted):**
- Localhost sciond gRPC API (`:30255`) for node-local SCION-native apps is provided by replicating the daemon `realMain` wiring — implemented in Task 8, feature-gated `--enable-daemon-api` (default on).
- Bootstrap modes `url` and `dns` are fully implemented; `dhcp` and `mdns` are implemented minimally in Task 6 (option 72 via `insomniacslk/dhcp`, mDNS via `grandcat/zeroconf`).

**Repository:** `/home/kmateusz/git/github.com/scion-k8s-operator` (github.com/mkowalski/scion-k8s-operator). Module path `github.com/mkowalski/scion-k8s-operator`.

**File structure (final state):**

```
cmd/agent/main.go                     # scion-node-agent entrypoint
cmd/operator/main.go                  # scion-operator entrypoint
cmd/registrar/main.go                 # scion-registrar entrypoint
api/v1alpha1/scionnetwork_types.go    # CRD Go types
api/v1alpha1/groupversion_info.go
internal/agent/config/config.go       # agent env/flag config
internal/agent/bootstrap/bootstrap.go # Discoverer interface + orchestration
internal/agent/bootstrap/url.go       # url mode fetcher
internal/agent/bootstrap/dns.go       # DNS-SD SRV discovery
internal/agent/bootstrap/dhcp.go      # DHCP option 72 discovery
internal/agent/bootstrap/mdns.go      # mDNS discovery
internal/agent/kube/node.go           # read own Node podCIDR/IP
internal/agent/policy/policy.go       # routing-policy + traffic-policy file generation, guardrails
internal/agent/sig/sig.go             # gateway.Gateway wiring
internal/agent/daemonapi/daemonapi.go # optional sciond gRPC service on :30255
internal/agent/health/health.go       # /healthz /readyz
internal/operator/controller/scionnetwork_controller.go
internal/operator/render/render.go    # DaemonSet/SA/RBAC/SCC/CM object builders
internal/operator/registrar/registrar.go   # backend interface + manual
internal/operator/registrar/http.go        # http backend client
internal/operator/registrar/anapaya.go     # anapaya stub backend
internal/registrar/server.go          # registrar HTTP service (AS-side)
internal/registrar/topology.go        # topology.json sigs patching
config/crd/                           # generated CRD manifests
config/manifests/                     # kustomize: ns, operator deploy, RBAC
bundle/                               # OLM bundle (generated)
build/Dockerfile.agent build/Dockerfile.operator build/Dockerfile.registrar
hack/dev-scion-topology/              # docker-compose SCION AS for integration tests
test/integration/ test/e2e/
Makefile
```

---

### Task 1: Repository scaffolding

**Files:**
- Create: `go.mod`, `Makefile`, `.gitignore`, `hack/boilerplate.go.txt`

- [ ] **Step 1: Initialize module and pin scion**

```bash
cd /home/kmateusz/git/github.com/scion-k8s-operator
go mod init github.com/mkowalski/scion-k8s-operator
go get github.com/scionproto/scion@v0.15.0
go get sigs.k8s.io/controller-runtime@latest k8s.io/client-go@latest k8s.io/api@latest k8s.io/apimachinery@latest
```

Expected: `go.mod` created with `go 1.26` (or the toolchain scion v0.15.0 requires; if `go get` complains about the Go version, set `toolchain` accordingly).

- [ ] **Step 2: Write Makefile**

```make
IMG_REGISTRY ?= quay.io/mkowalski
VERSION ?= 0.1.0-dev

.PHONY: build test lint images manifests

build:
	CGO_ENABLED=0 go build -o bin/agent ./cmd/agent
	CGO_ENABLED=0 go build -o bin/operator ./cmd/operator
	CGO_ENABLED=0 go build -o bin/registrar ./cmd/registrar

test:
	go test ./... -count=1

lint:
	go vet ./...

manifests: controller-gen
	$(CONTROLLER_GEN) crd rbac:roleName=scion-operator paths=./api/... paths=./internal/operator/... output:crd:dir=config/crd

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen:
	test -x $(CONTROLLER_GEN) || GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

images:
	podman build -f build/Dockerfile.agent -t $(IMG_REGISTRY)/scion-node-agent:$(VERSION) .
	podman build -f build/Dockerfile.operator -t $(IMG_REGISTRY)/scion-operator:$(VERSION) .
	podman build -f build/Dockerfile.registrar -t $(IMG_REGISTRY)/scion-registrar:$(VERSION) .
```

- [ ] **Step 3: Write .gitignore (`bin/`, `*.sqlite`, `dist/`), commit**

```bash
git add -A && git commit -s -m "Scaffold Go module, Makefile

Assisted-By: Claude Fable 5"
```

---

### Task 2: Agent configuration package

**Files:**
- Create: `internal/agent/config/config.go`
- Test: `internal/agent/config/config_test.go`

- [ ] **Step 1: Write failing test**

```go
package config

import "testing"

func TestFromEnv(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-0")
	t.Setenv("SCION_BOOTSTRAP_MODE", "url")
	t.Setenv("SCION_DISCOVERY_URL", "http://as-infra:8041")
	t.Setenv("SCION_TUN_NAME", "scion0")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeName != "worker-0" || c.DiscoveryURL != "http://as-infra:8041" || c.TunName != "scion0" {
		t.Fatalf("bad config: %+v", c)
	}
}

func TestFromEnvMissingNode(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for missing NODE_NAME")
	}
}
```

- [ ] **Step 2: Run to verify failure**: `go test ./internal/agent/config/ -v` → FAIL (undefined `FromEnv`).

- [ ] **Step 3: Implement**

```go
// Package config loads scion-node-agent configuration from environment
// variables (set by the operator on the DaemonSet from ScionNetwork spec).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	NodeName        string        // downward API
	BootstrapMode   string        // url|dns|dhcp|mdns
	DiscoveryURL    string        // required for mode=url
	DNSDomain       string        // for mode=dns; default: system search domain
	RefreshInterval time.Duration // topology/TRC re-fetch
	StateDir        string        // hostPath cache, default /var/lib/scion-node-agent
	TunName         string        // default scion0
	AdvertisePodCIDR bool
	AdvertiseNodeIP  bool
	EnableDaemonAPI  bool   // expose sciond gRPC on 127.0.0.1:30255
	AcceptISDASes    string // comma-separated remote ISD-ASes allowed by SGRP accept policy
	ForbiddenCIDRs   string // comma-separated cluster/service/machine CIDRs (guardrails)
	MetricsAddr      string // default :9465
}

func FromEnv() (Config, error) {
	c := Config{
		NodeName:         os.Getenv("NODE_NAME"),
		BootstrapMode:    getenv("SCION_BOOTSTRAP_MODE", "url"),
		DiscoveryURL:     os.Getenv("SCION_DISCOVERY_URL"),
		DNSDomain:        os.Getenv("SCION_DNS_DOMAIN"),
		StateDir:         getenv("SCION_STATE_DIR", "/var/lib/scion-node-agent"),
		TunName:          getenv("SCION_TUN_NAME", "scion0"),
		AdvertisePodCIDR: getenvBool("SCION_ADVERTISE_POD_CIDR", true),
		AdvertiseNodeIP:  getenvBool("SCION_ADVERTISE_NODE_IP", true),
		EnableDaemonAPI:  getenvBool("SCION_ENABLE_DAEMON_API", true),
		AcceptISDASes:    os.Getenv("SCION_ACCEPT_ISD_ASES"),
		ForbiddenCIDRs:   os.Getenv("SCION_FORBIDDEN_CIDRS"),
		MetricsAddr:      getenv("SCION_METRICS_ADDR", ":9465"),
	}
	var err error
	if c.RefreshInterval, err = time.ParseDuration(getenv("SCION_REFRESH_INTERVAL", "1h")); err != nil {
		return c, fmt.Errorf("SCION_REFRESH_INTERVAL: %w", err)
	}
	if c.NodeName == "" {
		return c, fmt.Errorf("NODE_NAME is required")
	}
	if c.BootstrapMode == "url" && c.DiscoveryURL == "" {
		return c, fmt.Errorf("SCION_DISCOVERY_URL required for bootstrap mode url")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
```

- [ ] **Step 4: Run tests**: `go test ./internal/agent/config/ -v` → PASS.

- [ ] **Step 5: Commit**: `git add -A && git commit -s -m "agent: env-based configuration package

Assisted-By: Claude Fable 5"`

---

### Task 3: Bootstrap module — interface and URL discoverer

**Files:**
- Create: `internal/agent/bootstrap/bootstrap.go`, `internal/agent/bootstrap/url.go`
- Test: `internal/agent/bootstrap/url_test.go`

Purpose: obtain `topology.json` + TRC files and cache them under `<StateDir>/etc/scion/{topology.json,certs/}` so `pkg/daemon.LoadASInfoFromFile` and `WithCertsDir` can consume them.

- [ ] **Step 1: Write failing test (httptest server serving topology + TRCs)**

```go
package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestURLDiscovererFetch(t *testing.T) {
	topo := []byte(`{"isd_as": "1-ff00:0:112", "mtu": 1472}`)
	trc := []byte("fake-trc-der-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) { w.Write(topo) })
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":{"base_number":1,"isd":1,"serial_number":1}}]`))
	})
	mux.HandleFunc("/trcs/isd1-b1-s1", func(w http.ResponseWriter, r *http.Request) { w.Write(trc) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	d := &URLDiscoverer{BaseURL: srv.URL}
	if err := Fetch(context.Background(), d, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "topology.json"))
	if err != nil || string(got) != string(topo) {
		t.Fatalf("topology not cached: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "certs", "isd1-b1-s1.trc")); err != nil {
		t.Fatalf("trc not cached: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**: `go test ./internal/agent/bootstrap/ -v` → FAIL (undefined types).

- [ ] **Step 3: Verify protocol against upstream.** Clone `https://github.com/netsec-ethz/bootstrapper` to `/tmp/opencode/bootstrapper`; read `fetcher/fetcher.go` (or equivalent) and confirm the discovery-server HTTP paths (`/topology`, `/trcs`, `/trcs/isd{I}-b{B}-s{S}`) and the TRC-list JSON shape. Adjust the test and the code in Step 4 to the verified paths; record the verified paths in a comment.

- [ ] **Step 4: Implement**

```go
// Package bootstrap fetches SCION endhost configuration (topology.json and
// TRCs) from a discovery server, following the netsec-ethz/bootstrapper
// protocol, and caches it into a config directory consumable by
// pkg/daemon.LoadASInfoFromFile / WithCertsDir.
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
	// Sanity check before persisting.
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
		name := fmt.Sprintf("isd%d-b%d-s%d", e.ID.ISD, e.ID.Base, e.ID.Serial)
		raw, err := httpGet(ctx, base+"/trcs/"+name)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "certs", name+".trc"), raw); err != nil {
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
```

`internal/agent/bootstrap/url.go`:

```go
package bootstrap

import "context"

// URLDiscoverer is the "url" bootstrap mode: a fixed discovery server URL.
type URLDiscoverer struct {
	BaseURL string
}

func (d *URLDiscoverer) BaseURLs(context.Context) ([]string, error) {
	return []string{d.BaseURL}, nil
}
```

- [ ] **Step 5: Run tests**: `go test ./internal/agent/bootstrap/ -v` → PASS.

- [ ] **Step 6: Commit**: `git add -A && git commit -s -m "agent: bootstrap fetcher with url discoverer

Assisted-By: Claude Fable 5"`

---

### Task 4: Bootstrap TRC verification and refresh loop

**Files:**
- Modify: `internal/agent/bootstrap/bootstrap.go`
- Test: `internal/agent/bootstrap/refresh_test.go`

- [ ] **Step 1: Write failing test**

```go
package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRefreshes(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"isd_as":"1-ff00:0:112"}`))
	})
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	changed := make(chan struct{}, 16)
	_ = Run(ctx, &URLDiscoverer{BaseURL: srv.URL}, t.TempDir(), 50*time.Millisecond, changed)
	if hits.Load() < 2 {
		t.Fatalf("expected at least 2 fetches, got %d", hits.Load())
	}
	select {
	case <-changed:
	default:
		t.Fatal("expected change notification after first fetch")
	}
}
```

- [ ] **Step 2: Run to verify failure**: `go test ./internal/agent/bootstrap/ -run TestRunRefreshes -v` → FAIL (undefined `Run`).

- [ ] **Step 3: Implement — append to `bootstrap.go`**

```go
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
```

Note on TRC verification: TRC chain validation happens inside the scion trust
engine when the standalone connector loads `certs/`; the bootstrap module only
does the structural sanity check on `topology.json`. Pinned-TRC support: if
`<StateDir>/pinned-trcs/` exists (mounted from a Secret/ConfigMap by the
operator), `Fetch` must refuse to overwrite existing TRC files that differ —
add this check in `fetchFrom` before `atomicWrite` of each TRC:

```go
		dst := filepath.Join(dir, "certs", name+".trc")
		if existing, err := os.ReadFile(dst); err == nil && len(existing) > 0 &&
			!bytesEqual(existing, raw) && isPinned(dir, name) {
			return fmt.Errorf("TRC %s differs from pinned copy; refusing update", name)
		}
```

with helpers:

```go
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func isPinned(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, "..", "pinned-trcs", name+".trc"))
	return err == nil
}
```

- [ ] **Step 4: Run tests**: `go test ./internal/agent/bootstrap/ -v` → PASS.

- [ ] **Step 5: Commit**: `git add -A && git commit -s -m "agent: bootstrap refresh loop with TRC pinning guard

Assisted-By: Claude Fable 5"`

---

### Task 5: DNS discoverer

**Files:**
- Create: `internal/agent/bootstrap/dns.go`
- Test: `internal/agent/bootstrap/dns_test.go`

- [ ] **Step 1: Write failing test (inject resolver func)**

```go
package bootstrap

import (
	"context"
	"net"
	"testing"
)

func TestDNSDiscoverer(t *testing.T) {
	d := &DNSDiscoverer{
		Domain: "example.org",
		lookupSRV: func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
			if service != "sciondiscovery" || proto != "tcp" || name != "example.org" {
				t.Fatalf("unexpected query: %s %s %s", service, proto, name)
			}
			return "", []*net.SRV{{Target: "ds.example.org.", Port: 8041}}, nil
		},
	}
	urls, err := d.BaseURLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "http://ds.example.org:8041" {
		t.Fatalf("got %v", urls)
	}
}
```

- [ ] **Step 2: Run to verify failure**: `go test ./internal/agent/bootstrap/ -run TestDNSDiscoverer -v` → FAIL.

- [ ] **Step 3: Implement**

```go
package bootstrap

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// DNSDiscoverer is the "dns" bootstrap mode: SRV lookup of
// _sciondiscovery._tcp.<domain> per the endhost-bootstrap design
// (doc/dev/design/endhost-bootstrap.rst in scionproto/scion).
type DNSDiscoverer struct {
	Domain    string
	lookupSRV func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

func (d *DNSDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	lookup := d.lookupSRV
	if lookup == nil {
		lookup = net.DefaultResolver.LookupSRV
	}
	_, srvs, err := lookup(ctx, "sciondiscovery", "tcp", d.Domain)
	if err != nil {
		return nil, fmt.Errorf("SRV _sciondiscovery._tcp.%s: %w", d.Domain, err)
	}
	urls := make([]string, 0, len(srvs))
	for _, s := range srvs {
		urls = append(urls, fmt.Sprintf("http://%s:%d", strings.TrimSuffix(s.Target, "."), s.Port))
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("no SRV records for _sciondiscovery._tcp.%s", d.Domain)
	}
	return urls, nil
}
```

- [ ] **Step 4: Run tests** → PASS. **Step 5: Commit** `git commit -s -am "agent: dns bootstrap discoverer

Assisted-By: Claude Fable 5"`

---

### Task 6: DHCP and mDNS discoverers

**Files:**
- Create: `internal/agent/bootstrap/dhcp.go`, `internal/agent/bootstrap/mdns.go`
- Test: `internal/agent/bootstrap/dhcp_test.go`

These modes cannot be fully exercised in unit tests (need L2 network); test the pure parsing, keep network calls thin.

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/insomniacslk/dhcp/dhcpv4@latest github.com/grandcat/zeroconf@latest
```

- [ ] **Step 2: Write failing test for option-72 parsing**

```go
package bootstrap

import (
	"net"
	"testing"
)

func TestURLsFromOption72(t *testing.T) {
	got := urlsFromWWWServers([]net.IP{net.IPv4(192, 0, 2, 10)})
	if len(got) != 1 || got[0] != "http://192.0.2.10:8041" {
		t.Fatalf("got %v", got)
	}
}
```

- [ ] **Step 3: Run to verify failure**, then implement `dhcp.go`:

```go
package bootstrap

import (
	"context"
	"fmt"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

const discoveryPort = 8041 // default discovery server port, netsec-ethz/bootstrapper

// DHCPDiscoverer is the "dhcp" bootstrap mode: DHCPINFORM requesting
// option 72 (WWW server) per the endhost-bootstrap design.
type DHCPDiscoverer struct {
	Interface string // host interface to query on, e.g. br-ex or eno1
}

func urlsFromWWWServers(ips []net.IP) []string {
	urls := make([]string, 0, len(ips))
	for _, ip := range ips {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, discoveryPort))
	}
	return urls
}

func (d *DHCPDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	c, err := nclient4.New(d.Interface)
	if err != nil {
		return nil, fmt.Errorf("dhcp client on %s: %w", d.Interface, err)
	}
	defer c.Close()
	// Request option 72 via a full DORA; INFORM support varies by server.
	offer, err := c.DiscoverOffer(ctx,
		dhcpv4.WithRequestedOptions(dhcpv4.OptionWWWServer))
	if err != nil {
		return nil, fmt.Errorf("dhcp discover: %w", err)
	}
	ips := dhcpv4.GetIPs(dhcpv4.OptionWWWServer, offer.Options)
	if len(ips) == 0 {
		return nil, fmt.Errorf("dhcp: no option 72 (WWW server) in offer")
	}
	return urlsFromWWWServers(ips), nil
}
```

and `mdns.go`:

```go
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/grandcat/zeroconf"
)

// MDNSDiscoverer is the "mdns" bootstrap mode: browse for
// _sciondiscovery._tcp via multicast DNS.
type MDNSDiscoverer struct{}

func (d *MDNSDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	browseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := resolver.Browse(browseCtx, "_sciondiscovery._tcp", "local.", entries); err != nil {
		return nil, err
	}
	var urls []string
	for e := range entries {
		for _, ip := range e.AddrIPv4 {
			urls = append(urls, fmt.Sprintf("http://%s:%d", ip, e.Port))
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("mdns: no _sciondiscovery._tcp services found")
	}
	return urls, nil
}
```

Then add the mode selector in `bootstrap.go`:

```go
// NewDiscoverer builds a Discoverer for the configured bootstrap mode.
func NewDiscoverer(mode, url, dnsDomain, iface string) (Discoverer, error) {
	switch mode {
	case "url":
		return &URLDiscoverer{BaseURL: url}, nil
	case "dns":
		return &DNSDiscoverer{Domain: dnsDomain}, nil
	case "dhcp":
		return &DHCPDiscoverer{Interface: iface}, nil
	case "mdns":
		return &MDNSDiscoverer{}, nil
	default:
		return nil, fmt.Errorf("unknown bootstrap mode %q", mode)
	}
}
```

(Add `SCION_DHCP_INTERFACE` env → `Config.DHCPInterface string` in `internal/agent/config/config.go`.)

- [ ] **Step 4: Run tests**: `go test ./internal/agent/bootstrap/ -v` → PASS. Verify the DHCP API compiles against the actual `nclient4` signatures (`go build ./...`) — adjust if the library differs.

- [ ] **Step 5: Commit** `git commit -s -am "agent: dhcp and mdns bootstrap discoverers

Assisted-By: Claude Fable 5"`

---

### Task 7: Kube node info (own podCIDR / node IP)

**Files:**
- Create: `internal/agent/kube/node.go`
- Test: `internal/agent/kube/node_test.go`

- [ ] **Step 1: Write failing test (fake clientset)**

```go
package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeInfo(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.128.2.0/23"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.11"},
		}},
	}
	cs := fake.NewSimpleClientset(node)
	info, err := GetNodeInfo(context.Background(), cs, "worker-0")
	if err != nil {
		t.Fatal(err)
	}
	if info.PodCIDRs[0] != "10.128.2.0/23" || info.InternalIP != "192.0.2.11" {
		t.Fatalf("got %+v", info)
	}
}
```

- [ ] **Step 2: Run to verify failure** → FAIL (undefined `GetNodeInfo`).

- [ ] **Step 3: Implement**

```go
// Package kube reads this node's identity from the Kubernetes API.
// RBAC required: get on nodes (cluster-scoped), nothing else.
package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type NodeInfo struct {
	PodCIDRs   []string
	InternalIP string
}

func GetNodeInfo(ctx context.Context, cs kubernetes.Interface, name string) (NodeInfo, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return NodeInfo{}, fmt.Errorf("get node %s: %w", name, err)
	}
	info := NodeInfo{PodCIDRs: node.Spec.PodCIDRs}
	if len(info.PodCIDRs) == 0 && node.Spec.PodCIDR != "" {
		info.PodCIDRs = []string{node.Spec.PodCIDR}
	}
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			info.InternalIP = a.Address
			break
		}
	}
	if info.InternalIP == "" {
		return info, fmt.Errorf("node %s has no InternalIP", name)
	}
	return info, nil
}
```

Caveat for executor: on OVN-Kubernetes, `node.spec.podCIDRs` may be empty; the
per-node subnet is then in the annotation `k8s.ovn.org/node-subnets`
(JSON: `{"default":["10.128.2.0/23"]}`). Add this fallback with a test:

```go
	if len(info.PodCIDRs) == 0 {
		if raw, ok := node.Annotations["k8s.ovn.org/node-subnets"]; ok {
			var m map[string][]string
			if err := json.Unmarshal([]byte(raw), &m); err == nil {
				info.PodCIDRs = m["default"]
			}
		}
	}
```

- [ ] **Step 4: Run tests** → PASS. **Step 5: Commit** `git commit -s -am "agent: node podCIDR/IP discovery incl. OVN-K annotation fallback

Assisted-By: Claude Fable 5"`

---

### Task 8: Policy generation (SGRP routing policy, traffic policy, guardrails)

**Files:**
- Create: `internal/agent/policy/policy.go`
- Test: `internal/agent/policy/policy_test.go`

The SIG's advertised and accepted prefixes are controlled by two files it
reads (and re-reads on `ConfigReloadTrigger`): the IP routing policy (text,
parsed by `gateway/routing`) and the traffic policy (JSON, sample in
`dist/conffiles/gateway.json`). Guardrails are implemented here: accept rules
never include forbidden CIDRs.

- [ ] **Step 1: Verify formats.** Read `/tmp/opencode/scion/gateway/routing/policy.go` and its tests plus `/tmp/opencode/scion/dist/conffiles/gateway.json`. Confirm: routing policy line format (`<action> <from-ia> <to-ia> <prefix[,prefix]>`, actions `accept`/`reject`/`advertise`, wildcard `0-0`), and traffic policy JSON shape. Adjust Step 2/3 content to the verified syntax.

- [ ] **Step 2: Write failing test**

```go
package policy

import (
	"strings"
	"testing"
)

func TestRenderRoutingPolicy(t *testing.T) {
	in := Input{
		LocalIA:        "1-ff00:0:112",
		AdvertisedNets: []string{"10.128.2.0/23", "192.0.2.11/32"},
		AcceptISDASes:  []string{"1-ff00:0:110"},
		ForbiddenCIDRs: []string{"10.128.0.0/14", "172.30.0.0/16"},
	}
	out, err := RenderRoutingPolicy(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "advertise 1-ff00:0:112 1-ff00:0:110 10.128.2.0/23") {
		t.Fatalf("missing advertise rule:\n%s", out)
	}
	if !strings.Contains(out, "accept 1-ff00:0:110 1-ff00:0:112") {
		t.Fatalf("missing accept rule:\n%s", out)
	}
}

func TestGuardrailRejectsOverlap(t *testing.T) {
	in := Input{
		LocalIA:        "1-ff00:0:112",
		AdvertisedNets: []string{"10.128.2.0/23"},
		AcceptISDASes:  []string{"1-ff00:0:110"},
		// Remote nets accepted must never overlap these:
		ForbiddenCIDRs: []string{"10.128.0.0/14"},
	}
	if err := ValidateNoOverlap([]string{"10.128.4.0/24"}, in.ForbiddenCIDRs); err == nil {
		t.Fatal("expected overlap error")
	}
	if err := ValidateNoOverlap([]string{"198.51.100.0/24"}, in.ForbiddenCIDRs); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
```

- [ ] **Step 3: Run to verify failure**, then implement

```go
// Package policy renders the SIG IP routing policy (SGRP advertise/accept
// rules) and traffic policy JSON, and enforces prefix guardrails: prefixes
// overlapping cluster/service/machine networks are never accepted, and the
// default route is never accepted.
package policy

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

type Input struct {
	LocalIA        string   // e.g. 1-ff00:0:112
	AdvertisedNets []string // this node's pod CIDR + node IP /32
	AcceptISDASes  []string // remote ASes we exchange prefixes with
	ForbiddenCIDRs []string // clusterNetwork, serviceNetwork, machineNetwork
}

// RenderRoutingPolicy produces the gateway/routing text policy:
//
//	advertise <local-ia> <remote-ia> <prefix>
//	accept    <remote-ia> <local-ia> 0.0.0.0/0   (narrowed by guardrails at
//	                                              acceptance time, see below)
//
// gateway/routing has no "except" syntax, so guardrails are enforced by
// accepting only explicit safe ranges: we accept everything except forbidden
// ranges by splitting 0.0.0.0/0 minus ForbiddenCIDRs into covering prefixes.
func RenderRoutingPolicy(in Input) (string, error) {
	var b strings.Builder
	allowed, err := subtractCIDRs("0.0.0.0/0", in.ForbiddenCIDRs)
	if err != nil {
		return "", err
	}
	for _, remote := range in.AcceptISDASes {
		for _, net := range in.AdvertisedNets {
			fmt.Fprintf(&b, "advertise %s %s %s\n", in.LocalIA, remote, net)
		}
		for _, net := range allowed {
			fmt.Fprintf(&b, "accept %s %s %s\n", remote, in.LocalIA, net)
		}
	}
	return b.String(), nil
}

// RenderTrafficPolicy produces the SIG traffic policy JSON mapping each
// remote ISD-AS to the prefixes we may send to it. Using the broad allowed
// set keeps egress open to any prefix the remote legitimately advertises;
// session-level prefix learning still narrows actual routes.
func RenderTrafficPolicy(in Input) (string, error) {
	allowed, err := subtractCIDRs("0.0.0.0/0", in.ForbiddenCIDRs)
	if err != nil {
		return "", err
	}
	type asEntry struct {
		Nets []string `json:"Nets"`
	}
	doc := struct {
		ASes          map[string]asEntry `json:"ASes"`
		ConfigVersion uint64             `json:"ConfigVersion"`
	}{ASes: map[string]asEntry{}, ConfigVersion: 1}
	for _, remote := range in.AcceptISDASes {
		doc.ASes[remote] = asEntry{Nets: allowed}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	return string(out), err
}

// ValidateNoOverlap returns an error if any of nets overlaps any forbidden
// CIDR (used both at render time and as a defense-in-depth runtime check).
func ValidateNoOverlap(nets, forbidden []string) error {
	for _, n := range nets {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			return fmt.Errorf("bad prefix %q: %w", n, err)
		}
		if p.Bits() == 0 {
			return fmt.Errorf("default route %q is never allowed", n)
		}
		for _, f := range forbidden {
			fp, err := netip.ParsePrefix(f)
			if err != nil {
				return fmt.Errorf("bad forbidden prefix %q: %w", f, err)
			}
			if p.Overlaps(fp) {
				return fmt.Errorf("prefix %s overlaps forbidden %s", n, f)
			}
		}
	}
	return nil
}

// subtractCIDRs computes base minus excludes as a minimal list of prefixes,
// by recursively splitting base and keeping halves that do not overlap any
// exclude.
func subtractCIDRs(base string, excludes []string) ([]string, error) {
	b, err := netip.ParsePrefix(base)
	if err != nil {
		return nil, err
	}
	ex := make([]netip.Prefix, 0, len(excludes))
	for _, e := range excludes {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("bad forbidden prefix %q: %w", e, err)
		}
		ex = append(ex, p)
	}
	var out []string
	var walk func(p netip.Prefix)
	walk = func(p netip.Prefix) {
		overlap := false
		for _, e := range ex {
			if e.Contains(p.Addr()) && e.Bits() <= p.Bits() {
				return // fully excluded
			}
			if p.Overlaps(e) {
				overlap = true
				break
			}
		}
		if !overlap {
			out = append(out, p.String())
			return
		}
		// split into two halves
		left := netip.PrefixFrom(p.Addr(), p.Bits()+1)
		rightAddr := lastHalfAddr(p)
		right := netip.PrefixFrom(rightAddr, p.Bits()+1)
		walk(left)
		walk(right)
	}
	walk(b)
	return out, nil
}

func lastHalfAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	bit := uint(31 - p.Bits())
	idx := 3 - bit/8
	a[idx] |= 1 << (bit % 8)
	return netip.AddrFrom4(a)
}
```

- [ ] **Step 4: Run tests**: `go test ./internal/agent/policy/ -v` → PASS. Also add a round-trip check: feed `RenderRoutingPolicy` output into the real parser `gateway/routing.LoadPolicy` (import `github.com/scionproto/scion/gateway/routing`) in a test to guarantee syntax correctness:

```go
func TestPolicyParsesUpstream(t *testing.T) {
	out, err := RenderRoutingPolicy(Input{
		LocalIA:        "1-ff00:0:112",
		AdvertisedNets: []string{"10.128.2.0/23"},
		AcceptISDASes:  []string{"1-ff00:0:110"},
		ForbiddenCIDRs: []string{"10.128.0.0/14"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "pol")
	os.WriteFile(f, []byte(out), 0o644)
	if _, err := routing.LoadPolicy(f); err != nil {
		t.Fatalf("upstream parser rejected our policy: %v", err)
	}
}
```

(Verify the exact upstream loader name — `routing.LoadPolicy` vs `routing.Policy.UnmarshalText` — in `/tmp/opencode/scion/gateway/routing/policy.go`, adjust.)

- [ ] **Step 5: Commit** `git commit -s -am "agent: SGRP/traffic policy rendering with prefix guardrails

Assisted-By: Claude Fable 5"`

---

### Task 9: SIG embedding (gateway.Gateway wiring)

**Files:**
- Create: `internal/agent/sig/sig.go`
- Test: compile-only + integration (Task 12); unit test for address derivation.

This replicates `gateway/cmd/gateway/main.go:56-185` with a
`NewStandaloneConnector` instead of a gRPC daemon connection.

- [ ] **Step 1: Implement**

```go
// Package sig embeds the SCION-IP gateway from scionproto/scion.
// Wiring replicated from gateway/cmd/gateway/main.go (v0.15.0).
package sig

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"

	"github.com/scionproto/scion/gateway"
	"github.com/scionproto/scion/gateway/dataplane"
	"github.com/scionproto/scion/pkg/daemon"
	"github.com/scionproto/scion/pkg/snet/addrutil"
)

type Params struct {
	ConfigDir         string // contains topology.json + certs/
	TrafficPolicyFile string
	RoutingPolicyFile string
	TunName           string        // scion0
	NodeIP            net.IP        // node InternalIP; SIG binds ctrl/data here
	ReloadTrigger     chan struct{} // signaled by policy regeneration
}

// Run blocks until ctx is cancelled or the gateway fails.
func Run(ctx context.Context, p Params) error {
	asinfo, err := daemon.LoadASInfoFromFile(filepath.Join(p.ConfigDir, "topology.json"))
	if err != nil {
		return fmt.Errorf("load AS info: %w", err)
	}
	conn, err := daemon.NewStandaloneConnector(ctx, asinfo,
		daemon.WithCertsDir(filepath.Join(p.ConfigDir, "certs")),
		daemon.WithPeriodicCleanup(),
		daemon.WithMetrics(),
	)
	if err != nil {
		return fmt.Errorf("standalone connector: %w", err)
	}
	defer conn.Close()

	localIA, err := conn.LocalIA(ctx)
	if err != nil {
		return err
	}

	ctrlAddr := &net.UDPAddr{IP: p.NodeIP, Port: 30256}
	dataAddr := &net.UDPAddr{IP: p.NodeIP, Port: 30056}
	probeAddr := &net.UDPAddr{IP: p.NodeIP, Port: 30856}
	// If NodeIP is unset, derive like upstream main.go does.
	if p.NodeIP == nil {
		ip, err := addrutil.DefaultLocalIP(ctx, daemon.TopoQuerier{Connector: conn})
		if err != nil {
			return fmt.Errorf("derive local IP: %w", err)
		}
		ctrlAddr.IP, dataAddr.IP, probeAddr.IP = ip, ip, ip
	}
	pathMonitorIP, _ := netip.AddrFromSlice(ctrlAddr.IP)

	rt := &dataplane.AtomicRoutingTable{}
	gw := &gateway.Gateway{
		ID:                       "scion-node-agent",
		TrafficPolicyFile:        p.TrafficPolicyFile,
		RoutingPolicyFile:        p.RoutingPolicyFile,
		ControlClientIP:          ctrlAddr.IP,
		ControlServerAddr:        ctrlAddr,
		ServiceDiscoveryClientIP: ctrlAddr.IP,
		PathMonitorIP:            pathMonitorIP,
		ProbeServerAddr:          probeAddr,
		ProbeClientIP:            probeAddr.IP,
		DataServerAddr:           dataAddr,
		DataClientIP:             dataAddr.IP,
		Daemon:                   conn,
		TunnelName:               p.TunName,
		RoutingTableReader:       rt,
		RoutingTableSwapper:      rt,
		ConfigReloadTrigger:      p.ReloadTrigger,
		HTTPServeMux:             http.NewServeMux(),
		Metrics:                  gateway.NewMetrics(localIA),
	}
	return gw.Run(ctx)
}
```

- [ ] **Step 2: Verify compile and fix API drift**

Run: `go build ./internal/agent/sig/`
The struct fields above are from v0.15.0 `gateway/gateway.go:143-204`; if the
compiler reports missing/renamed fields (e.g. `RpcConfig env.RPC`,
`HTTPEndpoints service.StatusPages` needing values, or `daemon.TopoQuerier`
living elsewhere), consult `/tmp/opencode/scion/gateway/cmd/gateway/main.go`
and mirror exactly what upstream main does. Zero values are acceptable for
`RpcConfig`/`HTTPEndpoints` if upstream main tolerates them — check.

- [ ] **Step 3: Commit** `git commit -s -am "agent: embed SCION-IP gateway with standalone daemon connector

Assisted-By: Claude Fable 5"`

---

### Task 10: Optional sciond gRPC API on :30255 + health/metrics

**Files:**
- Create: `internal/agent/daemonapi/daemonapi.go`, `internal/agent/health/health.go`
- Test: `internal/agent/health/health_test.go`

- [ ] **Step 1: daemonapi.** Replicate the daemon service wiring from
`/tmp/opencode/scion/daemon/cmd/daemon/main.go:86-367` into
`daemonapi.Run(ctx, configDir, listenAddr string) error`: `topology.NewLoader`
(private/topology), `storage.NewPathStorage`/`NewTrustStorage`/`NewRevocationStorage`
with sqlite files under the agent StateDir, `daemontrust.NewEngine`,
`fetcher.NewFetcher` (config struct at main.go:278-289), then
`sdpb.RegisterDaemonServiceServer(grpcServer, daemon.NewServer(daemon.ServerConfig{...}))`
listening on `127.0.0.1:30255`. This is a mechanical port — copy upstream
`realMain` body, replace launcher/env plumbing with our config values, keep
the same store paths (`sd.path.db`, `sd.trust.db`). Feature-gated by
`Config.EnableDaemonAPI`. Because this pulls many `private/` packages, isolate
every scionproto import in this one file so version bumps touch one place.

- [ ] **Step 2: health.** Write the test first:

```go
package health

import (
	"net/http/httptest"
	"testing"
)

func TestReadyz(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	if code := get(t, srv.URL+"/readyz"); code != 503 {
		t.Fatalf("not ready yet, want 503 got %d", code)
	}
	h.SetReady("bootstrap", true)
	h.SetReady("gateway", true)
	if code := get(t, srv.URL+"/readyz"); code != 200 {
		t.Fatalf("want 200 got %d", code)
	}
	if code := get(t, srv.URL+"/healthz"); code != 200 {
		t.Fatalf("healthz want 200 got %d", code)
	}
}
```

(`get` helper does `http.Get` and returns status code.) Run → FAIL. Implement:

```go
// Package health serves /healthz (liveness: process up) and /readyz
// (readiness: bootstrap succeeded AND gateway running).
package health

import (
	"net/http"
	"sync"
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
	return mux
}
```

Run tests → PASS. Prometheus metrics: the scionproto libraries register into
the default Prometheus registry; expose them by adding to `Mux()`:

```go
	mux.Handle("/metrics", promhttp.Handler())
```

(`github.com/prometheus/client_golang/prometheus/promhttp` — already an
indirect dependency via scion.)

- [ ] **Step 3: Commit** `git commit -s -am "agent: optional sciond gRPC API, health endpoints, metrics

Assisted-By: Claude Fable 5"`

---

### Task 11: Agent main

**Files:**
- Create: `cmd/agent/main.go`

- [ ] **Step 1: Implement**

```go
// scion-node-agent: makes this node a first-class SCION endhost.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mkowalski/scion-k8s-operator/internal/agent/bootstrap"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/config"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/daemonapi"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/health"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/kube"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/policy"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/sig"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := health.New()

	// Node identity.
	rc, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	node, err := kube.GetNodeInfo(ctx, cs, cfg.NodeName)
	if err != nil {
		return err
	}

	confDir := filepath.Join(cfg.StateDir, "etc")
	trafficPolicyFile := filepath.Join(cfg.StateDir, "gateway-traffic.json")
	routingPolicyFile := filepath.Join(cfg.StateDir, "gateway-routing.policy")
	reload := make(chan struct{}, 1)
	topoChanged := make(chan struct{}, 1)

	disc, err := bootstrap.NewDiscoverer(cfg.BootstrapMode, cfg.DiscoveryURL,
		cfg.DNSDomain, cfg.DHCPInterface)
	if err != nil {
		return err
	}
	// Initial bootstrap must succeed before the gateway starts.
	if err := bootstrap.Fetch(ctx, disc, confDir); err != nil {
		return fmt.Errorf("initial bootstrap: %w", err)
	}
	h.SetReady("bootstrap", true)

	// Render policies (advertise node pod CIDR + node IP).
	adv := []string{}
	if cfg.AdvertisePodCIDR {
		adv = append(adv, node.PodCIDRs...)
	}
	if cfg.AdvertiseNodeIP {
		adv = append(adv, node.InternalIP+"/32")
	}
	in := policy.Input{
		LocalIA:        mustLocalIA(confDir),
		AdvertisedNets: adv,
		AcceptISDASes:  split(cfg.AcceptISDASes),
		ForbiddenCIDRs: split(cfg.ForbiddenCIDRs),
	}
	if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return bootstrap.Run(ctx, disc, confDir, cfg.RefreshInterval, topoChanged)
	})
	g.Go(func() error { // re-render + reload gateway on topology change
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-topoChanged:
				if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
					fmt.Fprintln(os.Stderr, "policy re-render:", err)
					continue
				}
				select {
				case reload <- struct{}{}:
				default:
				}
			}
		}
	})
	g.Go(func() error {
		h.SetReady("gateway", true)
		defer h.SetReady("gateway", false)
		return sig.Run(ctx, sig.Params{
			ConfigDir:         confDir,
			TrafficPolicyFile: trafficPolicyFile,
			RoutingPolicyFile: routingPolicyFile,
			TunName:           cfg.TunName,
			NodeIP:            parseIP(node.InternalIP),
			ReloadTrigger:     reload,
		})
	})
	if cfg.EnableDaemonAPI {
		g.Go(func() error { return daemonapi.Run(ctx, confDir, "127.0.0.1:30255") })
	}
	g.Go(func() error {
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: h.Mux()}
		go func() { <-ctx.Done(); srv.Close() }()
		return srv.ListenAndServe()
	})
	return g.Wait()
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
```

Helpers `mustLocalIA` (read `isd_as` from `<confDir>/topology.json`),
`parseIP` (`net.ParseIP`), and `renderPolicies` (calls
`policy.RenderRoutingPolicy` + `policy.RenderTrafficPolicy`, writes both files
atomically) go in the same file:

```go
func renderPolicies(in policy.Input, trafficFile, routingFile string) error {
	rp, err := policy.RenderRoutingPolicy(in)
	if err != nil {
		return err
	}
	tp, err := policy.RenderTrafficPolicy(in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(routingFile, []byte(rp), 0o644); err != nil {
		return err
	}
	return os.WriteFile(trafficFile, []byte(tp), 0o644)
}
```

- [ ] **Step 2: Build**: `go build ./cmd/agent` → succeeds. `go vet ./...` clean.

- [ ] **Step 3: Commit** `git commit -s -am "agent: main wiring bootstrap, policies, gateway, daemon API

Assisted-By: Claude Fable 5"`

---

### Task 12: Agent container image + integration test against a local SCION topology

**Files:**
- Create: `build/Dockerfile.agent`, `hack/dev-scion-topology/README.md`, `test/integration/agent_test.sh`

- [ ] **Step 1: Dockerfile**

```dockerfile
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
# NET_ADMIN + /dev/net/tun are granted by the pod spec; binary itself is
# capability-agnostic. Runs as root only when the SCC requires it for tun.
COPY --from=build /agent /agent
ENTRYPOINT ["/agent"]
```

Build: `podman build -f build/Dockerfile.agent -t scion-node-agent:dev .` → succeeds.

- [ ] **Step 2: Dev SCION topology.** In `/tmp/opencode/scion` (or a fresh clone pinned to v0.15.0): `./scion.sh topology -c topology/tiny.topo && ./scion.sh run` brings up a local multi-AS topology (ASes `1-ff00:0:110..112`) with control services and routers on localhost. Document in `hack/dev-scion-topology/README.md`: how to start it, where per-AS `topology.json`/TRCs live (`gen/ASff00_0_112/`), and how to serve them with a one-line discovery server for the agent (`python3 -m http.server` exposing `/topology` and `/trcs` from a small script — include the script `hack/dev-scion-topology/serve-discovery.py` that maps `gen/ASff00_0_112/topology.json` → `/topology` and `gen/trcs/*.trc` → `/trcs`, `/trcs/<id>`).

`hack/dev-scion-topology/serve-discovery.py`:

```python
#!/usr/bin/env python3
"""Minimal SCION discovery server for dev: serves an AS's topology and TRCs
in the layout expected by the agent bootstrap module (Task 3)."""
import json, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

GEN = sys.argv[1]          # e.g. .../scion/gen/ASff00_0_112
TRCS = sys.argv[2]         # e.g. .../scion/gen/trcs
PORT = int(sys.argv[3]) if len(sys.argv) > 3 else 8041

def trc_ids():
    out = []
    for f in os.listdir(TRCS):
        m = re.match(r"ISD(\d+)-B(\d+)-S(\d+)\.trc", f, re.I)
        if m:
            out.append({"id": {"isd": int(m[1]), "base_number": int(m[2]),
                               "serial_number": int(m[3])}, "file": f})
    return out

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/topology":
            body = open(os.path.join(GEN, "topology.json"), "rb").read()
        elif self.path == "/trcs":
            body = json.dumps([{"id": t["id"]} for t in trc_ids()]).encode()
        elif self.path.startswith("/trcs/isd"):
            m = re.match(r"/trcs/isd(\d+)-b(\d+)-s(\d+)", self.path)
            fname = f"ISD{m[1]}-B{m[2]}-S{m[3]}.trc"
            body = open(os.path.join(TRCS, fname), "rb").read()
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200); self.end_headers(); self.wfile.write(body)

HTTPServer(("", PORT), H).serve_forever()
```

- [ ] **Step 3: Integration test script** `test/integration/agent_test.sh` (run manually / CI with the topology up): starts two agent instances in two network namespaces (`ip netns add sig-a/sig-b`), each with `NODE_NAME` faked and `kube.GetNodeInfo` bypassed via a `SCION_LOCAL_PREFIXES` env override (add this override to `cmd/agent/main.go`: if set, skip the Kubernetes client entirely and use the given comma-separated prefixes — this also keeps the agent runnable outside Kubernetes). Assert: `scion0` exists in each netns (`ip -n sig-a link show scion0`), routes for the peer's prefix appear, and `ping` between namespace-local addresses traverses the tunnel. Expected: ping succeeds; tearing one agent down removes its tun.

- [ ] **Step 4: Run it**: `bash test/integration/agent_test.sh` → PASS (requires the dev topology running; document `sudo` needs).

- [ ] **Step 5: Commit** `git commit -s -am "agent: container image, dev topology helpers, netns integration test

Assisted-By: Claude Fable 5"`

---

### Task 13: CRD types (`ScionNetwork`)

**Files:**
- Create: `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/scionnetwork_types.go`
- Generated: `config/crd/*.yaml`

- [ ] **Step 1: groupversion_info.go**

```go
// Package v1alpha1 contains the ScionNetwork API.
// +kubebuilder:object:generate=true
// +groupName=scion.mkowalski.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "scion.mkowalski.github.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
```

- [ ] **Step 2: scionnetwork_types.go**

```go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BootstrapSpec configures how node agents discover the SCION AS.
type BootstrapSpec struct {
	// +kubebuilder:validation:Enum=url;dns;dhcp;mdns
	Mode string `json:"mode"`
	// +optional
	DiscoveryURL string `json:"discoveryURL,omitempty"`
	// +optional
	DNSDomain string `json:"dnsDomain,omitempty"`
	// +optional
	DHCPInterface string `json:"dhcpInterface,omitempty"`
	// SecretRef optionally holds credentials for authenticated bootstrap
	// (Anapaya) and/or pinned TRCs under key "pinned-trcs".
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
	// +optional
	// +kubebuilder:default:="1h"
	RefreshInterval string `json:"refreshInterval,omitempty"`
}

type AdvertisementSpec struct {
	// +kubebuilder:default:=true
	PodCIDR *bool `json:"podCIDR,omitempty"`
	// +kubebuilder:default:=true
	NodeIP *bool `json:"nodeIP,omitempty"`
}

type AcceptPolicySpec struct {
	// ISDASes lists remote ISD-ASes to exchange prefixes with.
	// +kubebuilder:validation:MinItems=1
	ISDASes []string `json:"isdASes"`
	// ForbiddenCIDRs are never accepted from remotes; the operator always
	// appends clusterNetwork/serviceNetwork automatically.
	// +optional
	ForbiddenCIDRs []string `json:"forbiddenCIDRs,omitempty"`
}

type DataplaneSpec struct {
	// +kubebuilder:default:="scion0"
	TunName string `json:"tunName,omitempty"`
}

type RegistrarSpec struct {
	// +kubebuilder:validation:Enum=manual;http;anapaya
	// +kubebuilder:default:="manual"
	Backend string `json:"backend,omitempty"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

type ScionNetworkSpec struct {
	Bootstrap BootstrapSpec `json:"bootstrap"`
	// +optional
	Advertisement AdvertisementSpec `json:"advertisement,omitempty"`
	AcceptPolicy  AcceptPolicySpec  `json:"acceptPolicy"`
	// +optional
	Dataplane DataplaneSpec `json:"dataplane,omitempty"`
	// +optional
	Registrar RegistrarSpec `json:"registrar,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// AgentImage overrides the default agent image (set by operator build).
	// +optional
	AgentImage string `json:"agentImage,omitempty"`
}

type NodeSummary struct {
	Ready int32 `json:"ready"`
	Total int32 `json:"total"`
	// +optional
	Degraded []string `json:"degraded,omitempty"`
}

type RegistrarStatus struct {
	// +optional
	RegisteredNodes int32 `json:"registeredNodes,omitempty"`
	// DesiredSIGs is published for backend=manual so AS operators can copy it.
	// +optional
	DesiredSIGs []string `json:"desiredSIGs,omitempty"`
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// +optional
	LastError string `json:"lastError,omitempty"`
}

type ScionNetworkStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ISDAS string `json:"isdAS,omitempty"`
	// +optional
	Nodes NodeSummary `json:"nodes,omitempty"`
	// +optional
	Registrar RegistrarStatus `json:"registrar,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ScionNetwork is a singleton; metadata.name must be 'cluster'"
// +kubebuilder:printcolumn:name="ISD-AS",type=string,JSONPath=`.status.isdAS`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.nodes.ready`
type ScionNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ScionNetworkSpec   `json:"spec,omitempty"`
	Status            ScionNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ScionNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScionNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScionNetwork{}, &ScionNetworkList{})
}
```

Add CEL rule for bootstrap mode/url coupling on `BootstrapSpec` (place above the struct):

```go
// +kubebuilder:validation:XValidation:rule="self.mode != 'url' || size(self.discoveryURL) > 0",message="discoveryURL required when mode is url"
```

- [ ] **Step 3: Generate deepcopy + CRD**

```bash
GOBIN=$(pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
bin/controller-gen object paths=./api/...
bin/controller-gen crd paths=./api/... output:crd:dir=config/crd
```

Expected: `api/v1alpha1/zz_generated.deepcopy.go` and `config/crd/scion.mkowalski.github.io_scionnetworks.yaml` created; `go build ./...` passes.

- [ ] **Step 4: Commit** `git commit -s -am "api: ScionNetwork v1alpha1 CRD

Assisted-By: Claude Fable 5"`

---

### Task 14: Operator object rendering

**Files:**
- Create: `internal/operator/render/render.go`
- Test: `internal/operator/render/render_test.go`

- [ ] **Step 1: Write failing test**

```go
package render

import (
	"testing"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
)

func TestDaemonSet(t *testing.T) {
	sn := &v1alpha1.ScionNetwork{}
	sn.Name = "cluster"
	sn.Spec.Bootstrap.Mode = "url"
	sn.Spec.Bootstrap.DiscoveryURL = "http://ds:8041"
	sn.Spec.AcceptPolicy.ISDASes = []string{"1-ff00:0:110"}
	ds := DaemonSet(sn, "quay.io/mkowalski/scion-node-agent:0.1.0", []string{"10.128.0.0/14", "172.30.0.0/16"})
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Fatal("agent must be hostNetwork")
	}
	env := map[string]string{}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SCION_DISCOVERY_URL"] != "http://ds:8041" {
		t.Fatalf("env: %v", env)
	}
	if env["SCION_FORBIDDEN_CIDRS"] != "10.128.0.0/14,172.30.0.0/16" {
		t.Fatalf("forbidden cidrs env: %v", env)
	}
}
```

- [ ] **Step 2: Run to verify failure**, then implement builders for: DaemonSet
(hostNetwork, `NET_ADMIN` capability, `/dev/net/tun` hostPath, `/var/lib/scion-node-agent`
hostPath, downward-API `NODE_NAME`, env from spec, `system-node-critical`,
tolerations `operator: Exists`, probes hitting `:9465/healthz|readyz`),
ServiceAccount, ClusterRole (`get` nodes), ClusterRoleBinding, and the SCC
(as `unstructured.Unstructured` so vanilla k8s builds don't import OpenShift
types). Core of `render.go`:

```go
// Package render builds the Kubernetes objects owned by the operator for a
// ScionNetwork. Pure functions: input spec, output objects; no API calls.
package render

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
)

const (
	Namespace = "scion-system"
	agentName = "scion-node-agent"
)

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func DaemonSet(sn *v1alpha1.ScionNetwork, image string, forbiddenCIDRs []string) *appsv1.DaemonSet {
	hostPathChar := corev1.HostPathCharDev
	hostPathDir := corev1.HostPathDirectoryOrCreate
	priv := false
	env := []corev1.EnvVar{
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "SCION_BOOTSTRAP_MODE", Value: sn.Spec.Bootstrap.Mode},
		{Name: "SCION_DISCOVERY_URL", Value: sn.Spec.Bootstrap.DiscoveryURL},
		{Name: "SCION_DNS_DOMAIN", Value: sn.Spec.Bootstrap.DNSDomain},
		{Name: "SCION_DHCP_INTERFACE", Value: sn.Spec.Bootstrap.DHCPInterface},
		{Name: "SCION_REFRESH_INTERVAL", Value: sn.Spec.Bootstrap.RefreshInterval},
		{Name: "SCION_TUN_NAME", Value: sn.Spec.Dataplane.TunName},
		{Name: "SCION_ACCEPT_ISD_ASES", Value: strings.Join(sn.Spec.AcceptPolicy.ISDASes, ",")},
		{Name: "SCION_FORBIDDEN_CIDRS", Value: strings.Join(forbiddenCIDRs, ",")},
		{Name: "SCION_ADVERTISE_POD_CIDR", Value: boolStr(boolOr(sn.Spec.Advertisement.PodCIDR, true))},
		{Name: "SCION_ADVERTISE_NODE_IP", Value: boolStr(boolOr(sn.Spec.Advertisement.NodeIP, true))},
	}
	labels := map[string]string{"app": agentName}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: Namespace, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: agentName,
					HostNetwork:        true,
					PriorityClassName:  "system-node-critical",
					NodeSelector:       sn.Spec.NodeSelector,
					Tolerations: append(sn.Spec.Tolerations,
						corev1.Toleration{Operator: corev1.TolerationOpExists}),
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: image,
						Env:   env,
						SecurityContext: &corev1.SecurityContext{
							Privileged: &priv,
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{"NET_ADMIN"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "state", MountPath: "/var/lib/scion-node-agent"},
							{Name: "tun", MountPath: "/dev/net/tun"},
						},
						ReadinessProbe: probe("/readyz"),
						LivenessProbe:  probe("/healthz"),
					}},
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/lib/scion-node-agent", Type: &hostPathDir}}},
						{Name: "tun", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: "/dev/net/tun", Type: &hostPathChar}}},
					},
				},
			},
		},
	}
}
```

plus `probe(path string) *corev1.Probe` (HTTPGet on port 9465), `boolStr`,
`ServiceAccount()`, `ClusterRole()` (get nodes), `ClusterRoleBinding()`, and
`SCC()` returning an `*unstructured.Unstructured` with
`apiVersion: security.openshift.io/v1, kind: SecurityContextConstraints`,
`allowHostNetwork: true`, `allowedCapabilities: [NET_ADMIN]`,
`allowHostDirVolumePlugin: true`, `runAsUser: {type: RunAsAny}`,
`seLinuxContext: {type: RunAsAny}`, `users: [system:serviceaccount:scion-system:scion-node-agent]`.

- [ ] **Step 3: Run tests** → PASS. **Step 4: Commit** `git commit -s -am "operator: object renderers (DaemonSet, RBAC, SCC)

Assisted-By: Claude Fable 5"`

---

### Task 15: ScionNetwork controller

**Files:**
- Create: `internal/operator/controller/scionnetwork_controller.go`, `cmd/operator/main.go`
- Test: `internal/operator/controller/scionnetwork_controller_test.go` (envtest)

- [ ] **Step 1: Write failing envtest** (setup-envtest for kube-apiserver binaries):

```go
package controller

// Envtest: creates a ScionNetwork "cluster" and asserts the controller
// creates the DaemonSet and sets a Progressing condition.
func TestReconcileCreatesDaemonSet(t *testing.T) {
	// standard envtest boilerplate: testEnv.Start(), install CRD from
	// config/crd, start manager with SetupWithManager, defer Stop.
	sn := &v1alpha1.ScionNetwork{}
	sn.Name = "cluster"
	sn.Spec.Bootstrap.Mode = "url"
	sn.Spec.Bootstrap.DiscoveryURL = "http://ds:8041"
	sn.Spec.AcceptPolicy.ISDASes = []string{"1-ff00:0:110"}
	require.NoError(t, k8sClient.Create(ctx, sn))

	require.Eventually(t, func() bool {
		ds := &appsv1.DaemonSet{}
		return k8sClient.Get(ctx, types.NamespacedName{
			Namespace: "scion-system", Name: "scion-node-agent"}, ds) == nil
	}, 10*time.Second, 200*time.Millisecond)
}
```

(Write the full envtest suite file — `suite_test.go` with `TestMain` starting
envtest — following controller-runtime's standard pattern; `go get
sigs.k8s.io/controller-runtime/tools/setup-envtest` and add a `make envtest`
target that downloads binaries.)

- [ ] **Step 2: Run to verify failure**, then implement the reconciler:

```go
// Package controller reconciles ScionNetwork into the node-agent DaemonSet
// and supporting objects.
package controller

import (
	"context"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
)

type ScionNetworkReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	AgentImage string // default from env AGENT_IMAGE
	SCCAvail   bool   // discovered at startup: security.openshift.io present?
}

func (r *ScionNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sn := &v1alpha1.ScionNetwork{}
	if err := r.Get(ctx, req.NamespacedName, sn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	forbidden, err := r.clusterForbiddenCIDRs(ctx, sn)
	if err != nil {
		return ctrl.Result{}, err
	}

	image := sn.Spec.AgentImage
	if image == "" {
		image = r.AgentImage
	}

	objs := []client.Object{
		render.ServiceAccount(),
		render.ClusterRole(),
		render.ClusterRoleBinding(),
		render.DaemonSet(sn, image, forbidden),
	}
	if r.SCCAvail {
		objs = append(objs, render.SCC())
	}
	for _, o := range objs {
		if err := ctrl.SetControllerReference(sn, o, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.createOrUpdate(ctx, o); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply %T: %w", o, err)
		}
	}
	return ctrl.Result{}, r.updateStatus(ctx, sn)
}

// clusterForbiddenCIDRs merges spec.acceptPolicy.forbiddenCIDRs with the
// cluster's pod and service networks (read from the openshift
// network.config.openshift.io/cluster if present, else from spec only).
func (r *ScionNetworkReconciler) clusterForbiddenCIDRs(
	ctx context.Context, sn *v1alpha1.ScionNetwork) ([]string, error) {
	out := append([]string{}, sn.Spec.AcceptPolicy.ForbiddenCIDRs...)
	// OpenShift: read cluster network config via unstructured to avoid the
	// openshift/api dependency; on vanilla clusters this returns NotFound
	// and spec values are used as-is.
	nc, err := getOpenShiftNetworkConfig(ctx, r.Client) // helper below
	if err == nil {
		out = append(out, nc.ClusterCIDRs...)
		out = append(out, nc.ServiceCIDRs...)
	} else if !errors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return nil, err
	}
	return dedupe(out), nil
}
```

Include the remaining helpers in the same file: `createOrUpdate` (get; create
if absent; otherwise update Spec/data fields — use
`controllerutil.CreateOrUpdate` from controller-runtime), `updateStatus`
(list pods with label `app=scion-node-agent`, count ready vs total, set
`Available` = all ready & total>0, `Progressing` = DS generation mismatch or
total==0, `Degraded` = any pod unready > 5 min; set `status.nodes`),
`getOpenShiftNetworkConfig` (unstructured GET of
`network.config.openshift.io/v1, kind Network, name cluster`, extracting
`.spec.clusterNetwork[].cidr` and `.spec.serviceNetwork[]`), `dedupe`, and
`SetupWithManager` (For ScionNetwork, Owns DaemonSet, Watches Nodes →
enqueue "cluster").

`cmd/operator/main.go`: standard controller-runtime main — scheme with
clientgoscheme + v1alpha1, manager with LeaderElection true, discovery check
for `security.openshift.io/v1` to set `SCCAvail`, `AGENT_IMAGE` env (fail if
empty), health probes, metrics on `:8080`.

- [ ] **Step 3: Run tests**: `make envtest && go test ./internal/operator/... -v` → PASS.

- [ ] **Step 4: Commit** `git commit -s -am "operator: ScionNetwork reconciler with status aggregation

Assisted-By: Claude Fable 5"`

---

### Task 16: Registrar service (AS-side)

**Files:**
- Create: `internal/registrar/topology.go`, `internal/registrar/server.go`, `cmd/registrar/main.go`, `build/Dockerfile.registrar`
- Test: `internal/registrar/topology_test.go`, `internal/registrar/server_test.go`

- [ ] **Step 1: Write failing topology test**

```go
package registrar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPatchSigs(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","sigs":{"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := filepath.Join(t.TempDir(), "topology.json")
	os.WriteFile(f, []byte(topo), 0o644)

	sigs := map[string]SIG{
		"worker-0": {CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"},
	}
	if err := PatchSigs(f, sigs, "managed-"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f)
	var out map[string]any
	json.Unmarshal(raw, &out)
	got := out["sigs"].(map[string]any)
	if _, ok := got["managed-worker-0"]; !ok {
		t.Fatalf("managed sig missing: %v", got)
	}
	if _, ok := got["old-sig"]; !ok {
		t.Fatalf("unmanaged sig must be preserved: %v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**, then implement `topology.go`:

```go
// Package registrar patches sigs entries in a SCION AS topology.json.
// It manages only entries with the configured name prefix; operator-managed
// entries are fully reconciled (add/update/remove), others are untouched.
package registrar

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type SIG struct {
	CtrlAddr string `json:"ctrl_addr"`
	DataAddr string `json:"data_addr"`
}

func PatchSigs(topoFile string, desired map[string]SIG, prefix string) error {
	raw, err := os.ReadFile(topoFile)
	if err != nil {
		return err
	}
	var topo map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topo); err != nil {
		return fmt.Errorf("parse %s: %w", topoFile, err)
	}
	sigs := map[string]json.RawMessage{}
	if cur, ok := topo["sigs"]; ok {
		if err := json.Unmarshal(cur, &sigs); err != nil {
			return err
		}
	}
	for name := range sigs { // drop stale managed entries
		if strings.HasPrefix(name, prefix) {
			delete(sigs, name)
		}
	}
	for name, sig := range desired {
		b, _ := json.Marshal(sig)
		sigs[prefix+name] = b
	}
	b, _ := json.Marshal(sigs)
	topo["sigs"] = b
	out, err := json.MarshalIndent(topo, "", "  ")
	if err != nil {
		return err
	}
	tmp := topoFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, topoFile)
}
```

- [ ] **Step 3: Server test + implementation.** Test: PUT `/v1/sigs` with
bearer token replaces the managed set; missing/wrong token → 401; GET
`/v1/sigs` returns current managed set. Implement `server.go`:

```go
package registrar

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os/exec"
	"sync"
)

type Server struct {
	TopoFile   string
	Prefix     string   // e.g. "k8s-"
	Token      string   // bearer token
	ReloadCmd  []string // e.g. ["systemctl", "reload", "scion-control"]
	mu         sync.Mutex
	lastSet    map[string]SIG
}

func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/sigs", s.auth(s.putSigs))
	mux.HandleFunc("GET /v1/sigs", s.auth(s.getSigs))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.Token
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) putSigs(w http.ResponseWriter, r *http.Request) {
	var desired map[string]SIG
	if err := json.NewDecoder(r.Body).Decode(&desired); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := PatchSigs(s.TopoFile, desired, s.Prefix); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(s.ReloadCmd) > 0 {
		if err := exec.Command(s.ReloadCmd[0], s.ReloadCmd[1:]...).Run(); err != nil {
			http.Error(w, "reload failed: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	s.lastSet = desired
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSigs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	json.NewEncoder(w).Encode(s.lastSet)
}
```

`cmd/registrar/main.go`: flags `--topology`, `--prefix` (default `k8s-`),
`--listen :8642`, `--reload-cmd "systemctl reload scion-control"`, token from
env `REGISTRAR_TOKEN`; serve with `http.ListenAndServe`. VERIFY during
implementation whether the OSS control service reloads topology on SIGHUP or
needs a restart (check `private/app` SIGHUP handling in scionproto for the
control service); set the documented default reload command accordingly.

- [ ] **Step 4: Run tests** → PASS. **Step 5: Dockerfile.registrar** (same
two-stage pattern as the agent, entrypoint `/registrar`). **Step 6: Commit**
`git commit -s -am "registrar: AS-side sigs registration service

Assisted-By: Claude Fable 5"`

---

### Task 17: Operator registrar controller + backends

**Files:**
- Create: `internal/operator/registrar/registrar.go`, `internal/operator/registrar/http.go`, `internal/operator/registrar/anapaya.go`
- Modify: `internal/operator/controller/scionnetwork_controller.go` (call backend in Reconcile, fill `status.registrar`)
- Test: `internal/operator/registrar/http_test.go`

- [ ] **Step 1: Backend interface + manual backend** (`registrar.go`):

```go
// Package registrar syncs the cluster's node SIG set to the AS side.
package registrar

import "context"

type SIG struct {
	Name     string `json:"-"`
	CtrlAddr string `json:"ctrl_addr"`
	DataAddr string `json:"data_addr"`
}

// Backend reconciles the full desired SIG set on the AS side.
type Backend interface {
	Ensure(ctx context.Context, sigs []SIG) error
}

// Manual is a no-op backend; the desired set is only published in status.
type Manual struct{}

func (Manual) Ensure(context.Context, []SIG) error { return nil }
```

- [ ] **Step 2: HTTP backend with failing test first** — test uses `httptest`
asserting a PUT to `/v1/sigs` with the bearer token and JSON body keyed by
node name; implementation:

```go
package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type HTTP struct {
	Endpoint string // http://as-host:8642
	Token    string
	Client   *http.Client
}

func (h *HTTP) Ensure(ctx context.Context, sigs []SIG) error {
	body := map[string]SIG{}
	for _, s := range sigs {
		body[s.Name] = s
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		h.Endpoint+"/v1/sigs", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/json")
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("registrar: %s", resp.Status)
	}
	return nil
}
```

- [ ] **Step 3: Anapaya stub backend** (`anapaya.go`): implements `Backend`,
returns a typed `ErrNotImplemented` with a doc comment referencing the
OpenAPI client models in `Anapaya/ansible-collections`
(`appliance_api_client`); the controller surfaces this in
`status.registrar.lastError`. Interface compliance test only.

- [ ] **Step 4: Wire into Reconcile**: build the desired SIG list from Nodes
matching `spec.nodeSelector` (InternalIP + ports 30256/30056), select backend
from `spec.registrar.backend` (token from `credentialsSecretRef` key `token`),
call `Ensure`, set `status.registrar` (registeredNodes, desiredSIGs for
manual mode as `"name=ctrl,data"` strings, lastSyncTime, lastError). Node
watch (already added in Task 15) re-triggers on node add/remove. Extend the
envtest: add a Node, expect `status.registrar.desiredSIGs` to contain it.

- [ ] **Step 5: Run all tests** → PASS. **Commit** `git commit -s -am "operator: registrar controller with manual/http/anapaya backends

Assisted-By: Claude Fable 5"`

---

### Task 18: Deploy manifests, monitoring, operator image

**Files:**
- Create: `config/manifests/namespace.yaml`, `config/manifests/operator.yaml`, `config/manifests/rbac.yaml`, `config/manifests/monitoring.yaml`, `config/manifests/kustomization.yaml`, `build/Dockerfile.operator`

- [ ] **Step 1: Manifests.**
  - `namespace.yaml`: Namespace `scion-system` with PSA labels
    `pod-security.kubernetes.io/enforce: privileged` (needed for hostNetwork
    agent pods on vanilla k8s; harmless on OpenShift).
  - `rbac.yaml`: operator ServiceAccount `scion-operator`; ClusterRole:
    `scionnetworks{,/status,/finalizers}` all verbs; `daemonsets`,
    `serviceaccounts`, `clusterroles`, `clusterrolebindings`,
    `securitycontextconstraints` create/update/get/list/watch/delete;
    `nodes`, `pods` get/list/watch; `secrets` get (registrar token);
    `networks.config.openshift.io` get; leader-election leases.
  - `operator.yaml`: Deployment, 1 replica, image
    `quay.io/mkowalski/scion-operator:VERSION`, env
    `AGENT_IMAGE=quay.io/mkowalski/scion-node-agent:VERSION`, resource
    requests 50m/64Mi, runAsNonRoot, seccompProfile RuntimeDefault.
  - `monitoring.yaml`: Service (port 9465, selector `app=scion-node-agent`) +
    `ServiceMonitor` + `PrometheusRule` with two alerts:

```yaml
      - alert: ScionNodeAgentDown
        expr: kube_daemonset_status_number_unavailable{daemonset="scion-node-agent",namespace="scion-system"} > 0
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "SCION node agent unavailable on {{ $value }} node(s)"
      - alert: ScionNetworkDegraded
        expr: max(scion_operator_scionnetwork_degraded) > 0
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "ScionNetwork is Degraded"
```

  (Expose the `scion_operator_scionnetwork_degraded` gauge from the
  controller via controller-runtime's metrics registry when setting the
  Degraded condition.)
  - `kustomization.yaml` listing all of the above + `../crd`.

- [ ] **Step 2: Dockerfile.operator** (same two-stage pattern as the agent,
builds `./cmd/operator`, distroless nonroot).

- [ ] **Step 3: Smoke check**: `kubectl kustomize config/manifests | kubectl apply --dry-run=client -f -` → no errors (needs a kubeconfig; alternatively `kubeconform`).

- [ ] **Step 4: Commit** `git commit -s -am "deploy: kustomize manifests, monitoring, operator image

Assisted-By: Claude Fable 5"`

---

### Task 19: OLM bundle

**Files:**
- Create: `bundle/` (generated), `config/manifests/bases/scion-operator.clusterserviceversion.yaml`

- [ ] **Step 1: Install operator-sdk** (`curl -L` release binary to `./bin/operator-sdk`, or dnf).

- [ ] **Step 2: Write the CSV base** with: displayName "SCION Operator",
`installModes` AllNamespaces only, `owned` CRD `scionnetworks.scion.mkowalski.github.io`,
permissions copied from `config/manifests/rbac.yaml`, deployment spec copied
from `operator.yaml`, `minKubeVersion`, categories Networking, and an
`alm-examples` annotation containing a minimal ScionNetwork:

```json
[{"apiVersion":"scion.mkowalski.github.io/v1alpha1","kind":"ScionNetwork",
  "metadata":{"name":"cluster"},
  "spec":{"bootstrap":{"mode":"url","discoveryURL":"http://scion-ds.example.org:8041"},
           "acceptPolicy":{"isdASes":["1-ff00:0:110"]}}}]
```

- [ ] **Step 3: Generate + validate**

```bash
bin/operator-sdk generate bundle --input-dir config/manifests --version 0.1.0
bin/operator-sdk bundle validate ./bundle
```

Expected: `bundle validate` passes.

- [ ] **Step 4: Commit** `git commit -s -am "olm: bundle for operator 0.1.0

Assisted-By: Claude Fable 5"`

---

### Task 20: E2E on OpenShift

**Files:**
- Create: `test/e2e/e2e_test.sh`, `test/e2e/README.md`

Prerequisites (documented in README): an OpenShift 5.x cluster (`KUBECONFIG`
set), a reachable dev SCION AS (Task 12 topology on a VM reachable from
nodes, or a SCIONLab user AS), the registrar service (Task 16) running next
to that AS's control service, images pushed to a registry the cluster can
pull.

- [ ] **Step 1: Write `e2e_test.sh`** with phases (bash, `set -euo pipefail`,
each phase a function, `oc` CLI):
  1. `deploy`: `oc apply -k config/manifests`; wait for operator Deployment
     Available.
  2. `configure`: apply a ScionNetwork with `bootstrap.mode=url` pointing at
     the dev discovery server, `registrar.backend=http` pointing at the
     registrar, acceptPolicy for the dev remote AS.
  3. `assert_agents`: wait until DaemonSet `desiredNumberScheduled ==
     numberReady`; assert `oc get scionnetwork cluster -o
     jsonpath='{.status.conditions[?(@.type=="Available")].status}'` is True.
  4. `assert_dataplane`: `oc debug node/<first-node> -- chroot /host ip link
     show scion0` succeeds; ping from a test pod to an IP behind the remote
     SIG (dev topology exposes a target netns IP; documented in README)
     succeeds; from the remote side, ping the pod IP of the test pod
     (inbound path).
  5. `assert_registration`: `curl -H "Authorization: Bearer $TOKEN"
     $REGISTRAR/v1/sigs` lists every node.
  6. `churn`: delete one agent pod → recreated, routes return; scale test:
     `oc scale`-style node add is environment-specific, so instead delete a
     Node object registration assertion is re-run after pod churn.
  7. `undeploy`: delete ScionNetwork → DaemonSet gone, `scion0` gone from
     nodes, registrar shows empty managed set.

- [ ] **Step 2: Run against a real cluster**, fix fallout. Expected final
output: all phases print `OK`.

- [ ] **Step 3: Commit** `git commit -s -am "e2e: OpenShift end-to-end suite

Assisted-By: Claude Fable 5"`

---

### Task 21: Documentation and upstream follow-ups

**Files:**
- Create: `README.md`, `docs/install.md`, `docs/as-registration.md`
- Modify: `docs/superpowers/specs/2026-07-23-scion-k8s-operator-design.md` (record accepted deviations)

- [ ] **Step 1: README.md**: what/why (one paragraph), architecture diagram
(from spec §3), quick start (kustomize apply + ScionNetwork example),
requirements (OpenShift 5.x, AS with SIG registration, UDP reachability
30056/30256/30856 node↔remote), status/metrics, license (Apache-2.0 — add
`LICENSE` file; scionproto code is Apache-2.0, embedding is compatible).

- [ ] **Step 2: docs/as-registration.md**: manual runbook (copy
`status.registrar.desiredSIGs` into `topology.json` `sigs`), registrar
service install unit (systemd example), Anapaya note (backend stub, appliance
API pointer), and the planned upstream proposal for TTL-based dynamic SIG
self-registration (file as issue against scionproto/scion; link once filed).

- [ ] **Step 3: Spec sync**: record in the spec's scope notes any deviations
that materialized (bootstrap protocol details, `HTTPEndpoints`/`RpcConfig`
zero-value findings, control-service reload behavior).

- [ ] **Step 4: Commit** `git commit -s -am "docs: README, install and AS-registration guides

Assisted-By: Claude Fable 5"`

---

## Self-review notes

- **Spec coverage**: bootstrap modes (Tasks 3-6), daemon API :30255 (Task 10),
  SIG + tun + SGRP + routes (Tasks 8-9), guardrails (Task 8), node identity
  incl. OVN-K annotation fallback (Task 7), CRD spec/status incl. registrar
  fields (Task 13), operator + SCC gating for vanilla-k8s path (Tasks 14-15),
  registrar service + 3 backends + heartbeat caveat (Tasks 16-17; heartbeat
  expiry from the spec is NOT implemented — registrar reconciles full desired
  set on every sync instead, which covers unclean node removal as long as the
  operator lives; unclean *cluster* removal still leaves stale entries:
  documented in docs/as-registration.md, follow-up issue),
  monitoring + alerts (Task 18), OLM (Task 19), e2e incl. inbound and churn
  (Task 20), docs + upstream proposal (Task 21).
- **Known verification points for the executor** (marked in tasks):
  bootstrapper HTTP paths (Task 3), gateway struct zero-values (Task 9),
  `routing.LoadPolicy` name (Task 8), nclient4 API (Task 6), control-service
  reload semantics (Task 16), OVN-K `node-subnets` annotation shape (Task 7).
- **Type consistency**: `SIG{CtrlAddr,DataAddr}` duplicated in
  `internal/registrar` and `internal/operator/registrar` intentionally (no
  shared dependency between AS-side service and operator); JSON keys match
  (`ctrl_addr`/`data_addr`).
