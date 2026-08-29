# Conformance Harness Design

**Status:** design draft — no code yet. Build order: C4 first, then C3, then the
rest, then fault injection — the two clauses most likely to surface surprises come
first.

## Design constraints

1. **Provider-neutral, and able to fail against its author.** The suite is maintained
   by Tigris Data. Configuration is endpoint + creds + bucket only: no provider
   detection, no provider-specific code paths, no test tuned to pass on any
   implementation, Tigris's included. A suite only its author passes is marketing and
   will be read as such.
2. **Tests derive from the paper's lemmas and clauses**, not invented properties. Every
   test names the clause (C1–C5) or lemma (LD.1, LD.2) it checks.
3. **Correctness verdicts are binary and machine-checkable.** Latency is reported
   alongside, never part of pass/fail.
4. **Runnable in three modes** with the same test code: local (against any live
   endpoint), CI (short randomized runs), and as a workload inside a provider's own
   fault-injection environment (for Tigris: the store running inside Antithesis —
   provider-internal, not a dependency of this suite).

## Language and key dependencies

Go. Rationale: official AWS S3 SDK, the open-source Antithesis Go SDK (no-op outside
that environment — see Fault injection), and [porcupine](https://github.com/anishathalye/porcupine) — the standard
linearizability checker — which C1 needs. HDR histograms for latency capture.

## Repository structure

```
s3-wal-conformance-tests/
├── cmd/conformance/        # CLI: run groups, emit report
├── internal/
│   ├── store/              # thin S3 client wrapper: typed ops + op-history recording
│   ├── history/            # concurrent-history capture, porcupine model defs
│   ├── report/             # pass/fail per clause + HDR latency distributions → JSON + human summary
│   └── faults/             # fault-injection hooks (Antithesis SDK; no-ops elsewhere)
├── tests/
│   ├── c1_linearizability/
│   ├── c2_monotonic_existence/
│   ├── c3_listing/
│   ├── c4_cas/
│   ├── c5_delete_durability/
│   └── lemmas/             # LD.1 tail-range, LD.2 window-scan
└── docs/
```

Config via env/flags only: `ENDPOINT`, `ACCESS_KEY`/`SECRET_KEY`, `BUCKET`, plus
workload knobs (client count, duration, page size, seed). Every run logs its seed;
every failure reproduces from endpoint + seed + the recorded op history.

**Vantage topology.** The contract's clauses are global claims, so the runner accepts
a *list* of client vantage points — region-tagged endpoints, or coordinated worker
processes launched in different regions — and records each operation's vantage in the
history and the report. A single-vantage run is stamped **REGIONAL EVIDENCE ONLY** in
the report and cannot support a cross-region conformance claim; the expected C4
failure demonstration on a Global bucket likewise requires ≥2 vantages to manifest.

## Test groups

### C1 — Linearizability (porcupine-checked histories)

N concurrent clients (ideally spread across regions for multi-region claims) issue
randomized GET/PUT/DELETE against a small hot keyspace (contention is the point).
Every operation is recorded with invocation/response timestamps and values; porcupine
checks the history against a per-key register model. Fail ⇒ emit the minimal
non-linearizable history for the report.

### C2 — Monotonic existence

Writer creates keys; a pool of readers (multi-region where applicable) polls
GET + LIST. Assert: no reader observes absent-after-present for an undeleted key.
This directly validates the precondition of LD.1 and is the cheapest test to run
continuously as a background invariant during all other groups — on its own dedicated
key prefix, disjoint from the delete-exercising groups (C5, trim workloads), so a
legitimate delete elsewhere can never read as a C2 violation.

### C3 — Listing (adversarial pagination)

The bug-hunting group — the paper's authors found their real violation here.

- **Snapshot inclusion:** LIST issued after PUT completion includes the key.
- **Ordering:** results lexicographic within and across pages.
- **Pagination adversary:** long paginated scans over a keyspace sized at page-size
  multiples (and ±1) while concurrent writers insert keys before, at, and after the
  current continuation-token position, and deleters remove keys mid-scan. Assert: no
  key present for the entire scan is skipped or duplicated. Sweep page sizes (1, 2,
  1000, provider default) and prefix/delimiter variants.

### C4 — Compare-and-swap

- **Contended CAS:** M clients capture the same ETag, all PUT with `If-Match`, SDK
  auto-retry disabled. Verdicts are outcome-based, never status-code tallies (a lost
  200 or an SDK replay turns exact-count assertions into false failures): classify
  each contender as won / lost / ambiguous from its response, then assert by
  read-back that the final state is exactly one contender's value, that at most one
  contender classified as won, and that ambiguous outcomes resolve to cleanly-won or
  cleanly-lost. Status-code counts are recorded as diagnostics only.
- **Contended create:** M clients race `If-None-Match: *` on a fresh key; exactly one
  wins, judged the same outcome-based way.
- **412 non-mutation:** losers' payloads never observable, at any point, by any reader.
- **Chained register:** a sequence of CAS updates modeling the VirtualLog membership
  register (the paper's §5.1 use case); assert the register's version history is a
  chain with no forks.
- **Stale-ETag rejection** and mixed CAS/unconditional-PUT interleavings.

### C5 — Delete durability

Create → delete → then hammer GET/LIST for the key across regions, across the fault
schedule, and across long wall-clock gaps (resurrection is often triggered by
recovery/anti-entropy, so this group needs long Antithesis runs, not short CI runs).
Include delete-of-prefix (trim-shaped: delete keys `log/00000`–`log/NNNNN`, then scan).

### Lemma tests (end-to-end workload)

- **LD.1 tail-range:** writers append under a K-sized window discipline (a writer may
  write address a only if a − K is populated); a scanner concurrently computes the
  observed contiguous tail; assert every observed tail falls within the true tail range
  over the scan interval (bounds computed from the recorded history).
- **LD.2 window-scan:** assert a bounded scan of the K addresses preceding the
  non-contiguous tail yields the same populated-set as a full scan.

These run last: they exercise C1–C4 jointly in the exact shape the paper proves things
about, so a lemma failure with all clause groups green means the contract statement
itself is missing something — the most valuable outcome the suite can produce.

## Fault injection

Two layers, deliberately separated. A black-box harness cannot inject faults into a
provider's backend — no external tool can — so the public suite tests the faults a
client can actually cause and observe, and backend faults are a provider-internal
concern.

**Public suite — client-observable faults (in this repo):**

- **Ambiguous outcomes:** kill the connection mid-request and verify the store stays
  consistent with *either* outcome — a timed-out CAS must have either fully won or
  fully lost (read-back decides), never a torn state.
- **Retry safety:** replay a retried `If-None-Match: *` or `If-Match` after an
  ambiguous failure; assert a single logical create/CAS can never succeed twice —
  judged by read-back of final state, not by status codes.
- **Client crash-restart** mid-scan and mid-write-window (LD.1 discipline must
  survive a writer dying and resuming from observed state).
- **Network chaos** via a toxiproxy mode: latency, resets, partial reads — runnable on
  a laptop against any endpoint.

**Provider-internal — backend faults (not in this repo's runtime deps):**

Regional partitions, node kills/restarts, recovery, clock skew require operating the
store itself under a fault injector. For Tigris that means running the store inside
Antithesis **with this public suite, verbatim, as the workload**. The
`internal/faults` hooks use the open-source Antithesis Go SDK, whose
`always`/`sometimes` assertions compile to no-ops outside that environment, so the
hooks live harmlessly in the public repo and no fork is needed. Published results
state which claims (C5 especially) rest on these internal runs, with methodology.
Other providers can validate the same way against their own stacks with their own
tooling; the suite doesn't care what drives the faults.

## Report

One JSON document per run: endpoint (anonymized on request), bucket configuration
label, suite version, seed, per-clause verdict with counterexample histories on
failure, and HDR latency distributions (p50/p95/p99/p999) per operation type. A small
renderer produces the human-readable summary. Published results are this JSON plus
methodology — reproducible by anyone with credentials against any endpoint.

## Build order

1. `internal/store` + `report` skeleton, then **C4** (most likely to surprise, smallest
   surface), then **C3 pagination adversary**.
2. C1/C2 with porcupine histories.
3. C5 + lemma tests.
4. Provider-internal fault-injection packaging (for Tigris: the Antithesis workload);
   run long; fix what it finds before publishing any results.
