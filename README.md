# s3-wal-conformance-tests

A conformance suite — and a draft specification — for the **object-store contract for
shared-log workloads**: the minimal set of guarantees an S3-compatible object store
must provide to host a shared log, consensus, and state machine replication with no
external metadata database.

The contract and the tests are derived from Vickers et al.,
[*"The LogDrive: Composable Durability for Cloud-Based Shared Logs"*](https://www.usenix.org/conference/osdi26/presentation/vickers)
(OSDI '26). The paper proves that a small set of storage properties is *sufficient*
to host consensus; this repository turns its formal specification (Appendix A) and
lemmas (LD.1, LD.2) into a contract other providers can implement and a test suite
anyone can run. The contract is deliberately **stronger** than the paper's proven minimum —
a provider can't verify a client's write-once discipline, so the contract requires
properties a black-box suite can actually test, which imply the paper's. The spec's
*Relationship to the LogDrive paper* section states precisely what is the paper's
and what is this project's.

The suite runs against **any S3-compatible endpoint** given only an endpoint URL,
credentials, and a bucket. Output is a pass/fail report per contract clause plus
observed latency distributions. Correctness verdicts are binary; latency is reported,
never judged.

## The contract in one paragraph

Per-key linearizable read/write (C1); a key observed present is never later observed
absent unless deleted after that observation (C2); each LIST page is consistent and
ordered, and a paginated scan never skips or duplicates a key stable for the whole
scan (C3); conditional PUT / compare-and-swap where exactly one concurrent contender
wins and losers get 412 with no mutation (C4); a deleted key never resurfaces (C5).
Explicitly **not** required: transactions, multi-object atomicity, append, server-side
sequencing, cross-key ordering, snapshot-isolated listings, bounded staleness, or any
non-standard API. This paragraph is a gloss — [the spec](docs/spec-draft.md) is
authoritative.

## Contents

- [`docs/spec-draft.md`](docs/spec-draft.md) — the contract: five clauses (C1–C5),
  explicit non-requirements, and two conformance levels (provisional, which anyone
  can verify independently; full, which adds provider-run backend fault injection
  with published methodology).
- [`docs/harness-design.md`](docs/harness-design.md) — the test suite design: one
  test group per clause, lemma-derived workload tests, multi-vantage topology for
  cross-region claims, and client-observable fault injection.

## Status

Early. The spec and harness design are drafted; all five clause groups are
implemented: **C1** (porcupine-checked linearizability histories), **C2**
(monotonic existence via GET+LIST observation), **C3** (listing, with adversarial
pagination — churn scans and a deterministic cursor boundary attack), **C4**
(conditional write / CAS with outcome-based verdicts), and **C5** (delete
durability: delete-then-hammer, log-trim shape, and a cross-run manifest that
re-verifies every previously deleted key on later runs, accumulating wall-clock
gap evidence). The **lemma workload** is also implemented: the paper's S3 LogDrive
construction (reverse-encoded addresses, K-window write discipline, `weakTail` via
LIST + window scan) with LD.1 checked against sound client-side tail bounds and
LD.2 cross-checked against full scans, at a configurable object size (`-payload`).
Remaining: fault modes, multi-vantage orchestration.

## Running

```
go build ./cmd/conformance
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
  ./conformance -endpoint https://<s3-endpoint> -bucket <test-bucket> -groups c4
```

The suite creates and deletes objects under a `s3-wal-conformance/<seed>/` prefix in
the given bucket. Every run prints its seed and is reproducible from it. Add
`-json report.json` for the machine-readable report; repeat `-endpoint label=url`
for multi-vantage runs (a single-vantage report is stamped `REGIONAL EVIDENCE ONLY`).

## Neutrality

This suite is built and maintained by [Tigris Data](https://www.tigrisdata.com). It
is deliberately provider-neutral: configuration is endpoint + credentials + bucket,
there are no provider-specific code paths, and no test is tuned to any
implementation. The suite can and does fail against Tigris's own bucket
configurations that don't meet the contract — a conformance suite its author cannot
fail is marketing, and this one is built to be able to.

## License

[Apache License 2.0](LICENSE).
