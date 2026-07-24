package registrar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTopo(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func readSigs(t *testing.T, f string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	sigs, _ := out["sigs"].(map[string]any)
	return sigs
}

func TestPatchSigs(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","sigs":{"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := writeTopo(t, topo)

	sigs := map[string]SIG{
		"worker-0": {CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"},
	}
	if err := PatchSigs(f, sigs, "managed-"); err != nil {
		t.Fatal(err)
	}
	got := readSigs(t, f)
	if _, ok := got["managed-worker-0"]; !ok {
		t.Fatalf("managed sig missing: %v", got)
	}
	if _, ok := got["old-sig"]; !ok {
		t.Fatalf("unmanaged sig must be preserved: %v", got)
	}
}

func TestPatchSigsRemovesStaleManaged(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","sigs":{
		"managed-gone":{"ctrl_addr":"192.0.2.2:30256","data_addr":"192.0.2.2:30056"},
		"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := writeTopo(t, topo)

	err := PatchSigs(f, map[string]SIG{
		"worker-0": {CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"},
	}, "managed-")
	if err != nil {
		t.Fatal(err)
	}
	got := readSigs(t, f)
	if _, ok := got["managed-gone"]; ok {
		t.Fatalf("stale managed sig must be removed: %v", got)
	}
	if _, ok := got["managed-worker-0"]; !ok {
		t.Fatalf("desired managed sig missing: %v", got)
	}
	if _, ok := got["old-sig"]; !ok {
		t.Fatalf("unmanaged sig must be preserved: %v", got)
	}
}

func TestPatchSigsEmptyDesired(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","sigs":{
		"managed-gone":{"ctrl_addr":"192.0.2.2:30256","data_addr":"192.0.2.2:30056"},
		"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := writeTopo(t, topo)

	if err := PatchSigs(f, nil, "managed-"); err != nil {
		t.Fatal(err)
	}
	got := readSigs(t, f)
	if len(got) != 1 {
		t.Fatalf("want only old-sig, got %v", got)
	}
	if _, ok := got["old-sig"]; !ok {
		t.Fatalf("unmanaged sig must be preserved: %v", got)
	}
}

func TestPatchSigsPreservesOtherFields(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","mtu":1472,
		"control_service":{"cs-1":{"addr":"192.0.2.1:30252"}},
		"border_routers":{"br-1":{"internal_addr":"192.0.2.1:30042"}}}`
	f := writeTopo(t, topo)

	err := PatchSigs(f, map[string]SIG{
		"worker-0": {CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"},
	}, "k8s-")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	var orig map[string]any
	json.Unmarshal([]byte(topo), &orig)
	for _, k := range []string{"isd_as", "mtu", "control_service", "border_routers"} {
		if !reflect.DeepEqual(out[k], orig[k]) {
			t.Errorf("field %q changed: got %v want %v", k, out[k], orig[k])
		}
	}
	// missing sigs key handled: sigs was absent in input, must exist now
	got := readSigs(t, f)
	if _, ok := got["k8s-worker-0"]; !ok {
		t.Fatalf("managed sig missing: %v", got)
	}
}

func TestManagedSigs(t *testing.T) {
	topo := `{"isd_as":"1-ff00:0:112","sigs":{
		"k8s-worker-0":{"ctrl_addr":"192.0.2.11:30256","data_addr":"192.0.2.11:30056"},
		"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := writeTopo(t, topo)

	got, err := ManagedSigs(f, "k8s-")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SIG{
		"worker-0": {CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
