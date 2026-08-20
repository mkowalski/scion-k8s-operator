// Package registrar implements the AS-side sigs registration service:
// a small HTTP server run next to an open-source SCION control service
// that reconciles operator-managed `sigs` entries in the AS topology.json.
package registrar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SIG is one gateway entry in the topology `sigs` map. Field names match
// scion v0.15.1 private/topology/json/json.go GatewayInfo (ctrl_addr,
// data_addr; the optional probe_addr and allow_interfaces are not managed
// by the operator).
type SIG struct {
	CtrlAddr string `json:"ctrl_addr"`
	DataAddr string `json:"data_addr"`
}

// PatchSigs rewrites the `sigs` map in topoFile: every existing entry whose
// name starts with prefix is dropped, then each desired entry is added as
// prefix+name. Entries without the prefix and all other topology fields are
// preserved (the document is handled as map[string]json.RawMessage so
// unknown fields survive untouched). The file is replaced atomically via
// tmp+rename.
func PatchSigs(topoFile string, desired map[string]SIG, prefix string) error {
	raw, err := os.ReadFile(topoFile)
	if err != nil {
		return err
	}
	var topo map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topo); err != nil {
		return fmt.Errorf("parsing %s: %w", topoFile, err)
	}

	sigs := map[string]json.RawMessage{}
	if rawSigs, ok := topo["sigs"]; ok {
		if err := json.Unmarshal(rawSigs, &sigs); err != nil {
			return fmt.Errorf("parsing sigs in %s: %w", topoFile, err)
		}
	}
	for name := range sigs {
		if strings.HasPrefix(name, prefix) {
			delete(sigs, name)
		}
	}
	for name, sig := range desired {
		enc, err := json.Marshal(sig)
		if err != nil {
			return err
		}
		sigs[prefix+name] = enc
	}
	encSigs, err := json.Marshal(sigs)
	if err != nil {
		return err
	}
	topo["sigs"] = encSigs

	out, err := json.MarshalIndent(topo, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	st, err := os.Stat(topoFile)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(topoFile), ".topology-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	// Mirror the original file's mode rather than hardcoding one.
	if err := tmp.Chmod(st.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), topoFile)
}

// ManagedSigs returns the operator-managed sigs currently in topoFile,
// keyed by name with prefix stripped. Deriving this from the file (rather
// than in-memory state) survives registrar restarts.
func ManagedSigs(topoFile string, prefix string) (map[string]SIG, error) {
	raw, err := os.ReadFile(topoFile)
	if err != nil {
		return nil, err
	}
	var topo struct {
		Sigs map[string]SIG `json:"sigs"`
	}
	if err := json.Unmarshal(raw, &topo); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", topoFile, err)
	}
	out := map[string]SIG{}
	for name, sig := range topo.Sigs {
		if strings.HasPrefix(name, prefix) {
			out[strings.TrimPrefix(name, prefix)] = sig
		}
	}
	return out, nil
}
