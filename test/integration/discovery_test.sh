#!/usr/bin/env bash
# discovery_test.sh: protocol check for hack/dev-scion-topology/serve-discovery.py.
# Creates a fake gen/AS dir and trcs dir, starts the server, and asserts the
# three endpoints (/topology, /trcs, /trcs/<id>/blob) return the expected
# shapes. Requires only python3 and curl; no SCION topology needed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
serve="$here/../../hack/dev-scion-topology/serve-discovery.py"

command -v python3 >/dev/null || { echo "FATAL: python3 not found" >&2; exit 1; }
command -v curl >/dev/null || { echo "FATAL: curl not found" >&2; exit 1; }

tmp="$(mktemp -d)"
pid=""
cleanup() {
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
    rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/gen/ASff00_0_112" "$tmp/gen/trcs"
printf '{"isd_as":"1-ff00:0:112"}' > "$tmp/gen/ASff00_0_112/topology.json"
printf 'FAKE-TRC-BYTES-1-1-1' > "$tmp/gen/trcs/ISD1-B1-S1.trc"
printf 'FAKE-TRC-BYTES-2-1-3' > "$tmp/gen/trcs/ISD2-B1-S3.trc"
printf 'not a trc' > "$tmp/gen/trcs/README"

port=$(( (RANDOM % 10000) + 20000 ))
python3 "$serve" "$tmp/gen/ASff00_0_112" "$tmp/gen/trcs" "$port" &
pid=$!

base="http://127.0.0.1:$port"
for _ in $(seq 1 50); do
    curl -fsS -o /dev/null "$base/trcs" 2>/dev/null && break
    kill -0 "$pid" 2>/dev/null || { echo "FATAL: server exited early" >&2; exit 1; }
    sleep 0.1
done

fail=0
check() {
    local desc="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc: got '$got', want '$want'" >&2
        fail=1
    fi
}

# /topology returns the topology.json bytes verbatim.
check "/topology" "$(curl -fsS "$base/topology")" '{"isd_as":"1-ff00:0:112"}'

# /trcs returns a JSON array of {"id":{...}} for *.trc files only, and each
# entry has the three integer fields the agent parses.
trcs_json="$(curl -fsS "$base/trcs")"
check "/trcs entry count" \
    "$(python3 -c 'import json,sys; print(len(json.load(sys.stdin)))' <<<"$trcs_json")" "2"
check "/trcs ids" \
    "$(python3 -c '
import json, sys
ids = sorted((e["id"]["isd"], e["id"]["base_number"], e["id"]["serial_number"])
             for e in json.load(sys.stdin))
print(ids)' <<<"$trcs_json")" "[(1, 1, 1), (2, 1, 3)]"

# /trcs/isd{I}-b{B}-s{S}/blob returns raw TRC bytes.
check "/trcs/isd1-b1-s1/blob" "$(curl -fsS "$base/trcs/isd1-b1-s1/blob")" 'FAKE-TRC-BYTES-1-1-1'
check "/trcs/isd2-b1-s3/blob" "$(curl -fsS "$base/trcs/isd2-b1-s3/blob")" 'FAKE-TRC-BYTES-2-1-3'

# Unknown paths and missing TRCs 404.
check "unknown path 404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/nope")" "404"
check "missing trc 404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/trcs/isd9-b9-s9/blob")" "404"
check "no /blob suffix 404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/trcs/isd1-b1-s1")" "404"

if [[ "$fail" -ne 0 ]]; then
    echo "discovery_test.sh: FAILED" >&2
    exit 1
fi
echo "discovery_test.sh: all checks passed"
