#!/usr/bin/env python3
"""Minimal SCION discovery server for dev/test: serves an AS's topology.json
and TRCs in the netsec-ethz/bootstrapper HTTP layout used by the agent."""
import json, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

GEN = sys.argv[1]          # e.g. .../scion/gen/ASff00_0_112
TRCS = sys.argv[2]         # e.g. .../scion/gen/trcs
PORT = int(sys.argv[3]) if len(sys.argv) > 3 else 8041

def trc_ids():
    out = []
    for f in os.listdir(TRCS):
        m = re.match(r"ISD(\d+)-B(\d+)-S(\d+)\.trc$", f, re.I)
        if m:
            out.append({"id": {"isd": int(m[1]), "base_number": int(m[2]),
                               "serial_number": int(m[3])}, "file": f})
    return out

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            if self.path == "/topology":
                body = open(os.path.join(GEN, "topology.json"), "rb").read()
            elif self.path == "/trcs":
                body = json.dumps([{"id": t["id"]} for t in trc_ids()]).encode()
            else:
                m = re.match(r"^/trcs/isd(\d+)-b(\d+)-s(\d+)/blob$", self.path)
                if not m:
                    self.send_response(404); self.end_headers(); return
                body = open(os.path.join(TRCS, f"ISD{m[1]}-B{m[2]}-S{m[3]}.trc"), "rb").read()
        except FileNotFoundError:
            self.send_response(404); self.end_headers(); return
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

if __name__ == "__main__":
    HTTPServer(("", PORT), H).serve_forever()
