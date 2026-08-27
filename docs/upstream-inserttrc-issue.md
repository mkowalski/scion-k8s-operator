# Upstream issue draft — scionproto/scion

Status: **filed as [scionproto/scion#4977](https://github.com/scionproto/scion/issues/4977)**
(2026-08-27). Kept as the record of the verified reproducer. Reproducer
verified against v0.15.1; code confirmed unchanged on master at filing
time. Downstream follow-up: once a release ships a typed conflict error,
narrow the `db.ErrWriteFailed` match in
`internal/agent/daemonapi.resetStaleTrustCache`.

---

Title: **trust: conflicting TRC with reused ID surfaces as raw SQLite
constraint error instead of a typed conflict**

## Description

The `trcs` table's primary key is `(isd_id, base, serial)`, but
`InsertTRC`'s idempotency guard dedupes on `(isd_id, base, serial,
fingerprint)` (`private/storage/trust/sqlite/{schema.go,db.go}`). A TRC
that reuses an existing ID with **different content** therefore falls
through the `WHERE NOT EXISTS` guard and hits the primary key:

```
db: write failed: UNIQUE constraint failed: trcs.isd_id, trcs.base, trcs.serial
```

This case is semantically meaningful — either a re-bootstrapped
test/dev AS or a TRC substitution attempt — but callers cannot
distinguish it: the most specific classification available is
`errors.Is(err, db.ErrWriteFailed)`, which also matches disk-full and
every other write failure. Via `trust.LoadTRCs`, the control service
and daemon fail startup with this unclassified error; an endhost whose
AS re-mints trust material under a reused TRC ID crash-loops until its
trust database is deleted by hand.

## Steps to reproduce

Mint the same test topology twice (same TRC ID, different keys), then
insert both TRCs:

```sh
cat > topo.yaml <<'EOF'
ASes:
  "1-ff00:0:110": { core: true, voting: true, authoritative: true, issuing: true }
EOF
scion-pki testcrypto -t topo.yaml -o gen1
scion-pki testcrypto -t topo.yaml -o gen2
```

```go
tdb, _ := storage.NewTrustStorage(storage.DBConfig{Connection: "trust.db"})
tdb.InsertTRC(ctx, load("gen1/trcs/ISD1-B1-S1.trc")) // inserted=true
_, err := tdb.InsertTRC(ctx, load("gen2/trcs/ISD1-B1-S1.trc"))
// err = db: write failed: UNIQUE constraint failed: trcs.isd_id, trcs.base, trcs.serial
// errors.Is(err, db.ErrWriteFailed) is the only possible classification
```

(`load` = `os.ReadFile` + PEM decode + `cppki.DecodeSignedTRC`.)

## Expected behavior

`InsertTRC` returns a typed error (e.g. `ErrTRCConflict`, ideally
carrying the existing and offered fingerprints) so callers can decide
policy: fail closed on possible substitution, or reset a stale cache
for a re-keyed AS. `trust.LoadTRCs` would classify it the same way it
already maps the identical-TRC case to `ErrAlreadyExists`. The primary
key itself is correct — only the error classification is missing; no
auto-overwrite is proposed.

## Context

Hit in the wild by scion-k8s-operator, which embeds the daemon on
Kubernetes nodes with a persistent trust DB: a re-bootstrapped dev AS
took down the agent on every node. Downstream workaround (matching the
catch-all `db.ErrWriteFailed`):
<https://github.com/mkowalski/scion-k8s-operator/commit/23f9a11>.
Happy to send a PR for the typed error if the approach is agreeable.
