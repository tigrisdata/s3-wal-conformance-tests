# The Object-Store Contract for Shared-Log Workloads

**Version:** 0.1-draft
**Status:** Draft for review. Not yet published.

## Abstract

A class of distributed systems — shared logs, consensus protocols, state machine
replication, and the streaming and database systems built on them — can delegate all
durability and coordination to a commodity object store, eliminating any separate
metadata database, provided the object store meets a small set of testable guarantees.
This document specifies those guarantees as five clauses over the standard
S3-style API (PUT, GET, LIST, DELETE, conditional PUT). It is derived from the
specification and proofs in Vickers et al., *"The LogDrive: Composable Durability for
Cloud-Based Shared Logs"* (OSDI '26). A companion open-source conformance suite tests
each clause against any S3-compatible endpoint; the core of a conformance claim can be
verified by any consumer independently, with backend fault coverage a
provider-published extension (see Conformance).

The contract is deliberately minimal. It requires **no** multi-key transactions, **no**
multi-object atomicity, **no** server-side sequencing, **no** append operation, and
**no** non-standard APIs. Any provider exposing the standard API can conform, and any
consumer can independently verify provisional conformance (see Conformance).

## Terminology

- **Object store**: a service exposing at minimum PutObject, GetObject, ListObjectsV2,
  DeleteObject, and conditional PutObject via `If-Match` / `If-None-Match`.
- **Key**: an object name within a bucket. **Completed** operation: one for which the
  client received a success response.
- **Real-time precedence**: operation A precedes B if A's response was received before
  B's request was issued, by any clients.
- The key words MUST, MUST NOT, and MAY are to be interpreted as in RFC 2119.
- **Scope of conformance**: a provider declares conformance for a specific *bucket
  configuration* (e.g. a replication/location type), not for the service as a whole.
  All clauses are global claims: they MUST hold across all regions and endpoints from
  which the bucket is accessible, not merely per-region.

## The contract

### C1 — Per-key linearizable read and write

For each key, there MUST exist a total order over all completed GET, PUT, and DELETE
operations, consistent with real-time precedence, such that every GET returns the value
written by the most recent preceding PUT in that order (or absence, if the most recent
preceding operation is a DELETE or no PUT precedes it).

*Why it's needed:* every safety argument below composes with C1. Readers of a log built
on the store reconstruct state from object reads; a stale read reorders history.

### C2 — Monotonicity of existence, as observed

Once any client observes key K as present (via GET or LIST), no client subsequently
observes K as absent, unless a DELETE of K completed *after* that observation and no
subsequent PUT has since recreated K. This MUST hold across all clients and all
regions. "Deleted" here means a completed client-issued DELETE; provider-initiated
removal (lifecycle expiry, TTL) is out of scope — conformance is claimed for buckets
with such rules disabled.

*Why it's needed:* log readers compute the contiguous tail of the log by observing which
addresses are populated. A key that flickers from present to absent invalidates the
lower bound of the observed tail (paper Lemma LD.1) and can cause a reader to conclude
the log is shorter than it is — an acknowledged-write loss from the reader's view. C2 is
*not* implied by C1: C1 orders only GET/PUT/DELETE responses, while C2 also constrains
observations made through LIST, which C1 does not cover. It is also the clause that
eventually-consistent cross-region reads and negative-result caches break in practice.

### C3 — Strongly consistent, ordered listing

A single LIST request MUST reflect every PUT and every DELETE completed before the
request was issued: every key so created (and not since deleted) within the requested
range appears, and no key so deleted (and not since recreated) appears. Results MUST
be returned in lexicographic key order.

A *paginated listing* is a sequence of LIST requests linked by continuation tokens. It
is NOT required to form a snapshot of a single point in time. It MUST provide
monotone-cursor semantics: each page individually satisfies the single-request rule
above for its own key range at the time that page is served; pages advance strictly in
key order; and across the entire paginated listing no key that existed for its whole
duration is skipped or duplicated, regardless of concurrent writes. A key created or
deleted mid-listing MAY appear or not, according to whether the change lands ahead of
or behind the cursor position.

*Why it's needed:* recovery and tail-finding scan the log by prefix. A skipped key reads
as a hole in the log; the paper's authors found a real linearizability violation in a
production implementation caused specifically by pagination behavior under concurrent
writes. Conformance testing of this clause MUST be adversarial (writes landing mid-scan,
at page boundaries, at page-size multiples), not assertion-by-specification.

### C4 — Conditional write (compare-and-swap)

The store MUST support:

- **Create-if-absent**: PUT with `If-None-Match: *` succeeds iff the key does not
  currently exist.
- **Compare-and-swap**: PUT with `If-Match: <etag>` succeeds iff the key's current ETag
  equals the supplied value.

A failed conditional PUT MUST return HTTP 412 and MUST NOT modify the object. Conditions
MUST be evaluated against the latest committed state of the key (per the C1 order).
Under concurrent conditional PUTs whose conditions match the same current state,
exactly one MUST succeed and every other MUST fail with 412 — a store that fails all
contenders provides no consensus progress and does not conform.

**ETag freshness.** If ETags are derived purely from content (e.g. MD5), two versions
of a key holding identical bytes share an ETag, and `If-Match` alone cannot
distinguish them — the classic ABA problem, fatal for a consensus register. A
conforming store SHOULD return a distinct ETag for every successful write, including
writes of byte-identical content. Where it does not, clients implementing a register
over C4 MUST keep successive register values byte-distinct (e.g. by embedding a
monotonically increasing epoch), and a conformance report MUST state which of the two
properties the tested store provides.

*Why it's needed:* this is the contract's only source of consensus. A single
conditionally-written register object is sufficient to implement membership changes,
leader election, and log sealing (the paper's VirtualLog register, §5.1) — the role
otherwise played by an external coordination database.

### C5 — Delete durability

Once a DELETE of key K completes and no subsequent PUT recreates K, no client
subsequently observes K as present, under any failure, partition, or recovery scenario.

*Why it's needed:* log trimming deletes a prefix. A resurrected trimmed entry reappears
as a valid log address and corrupts replay.

## Explicit non-requirements

The following are **not** required, and their absence is the point of the contract —
everything above is available today on commodity object storage:

- Multi-key or multi-object transactions
- Atomic rename, append, or server-side sequencing
- Cross-key ordering guarantees of any kind
- Snapshot isolation across a LIST and subsequent GETs
- Any API beyond standard PUT/GET/LIST/DELETE and standard conditional headers
- Bounded staleness or clock guarantees

A provider MAY offer stronger guarantees; the contract does not test for them.

## Conformance

Conformance is established by the companion suite — one test group per clause, plus
workload tests derived from the paper's lemmas (tail-range, LD.1; window-scan, LD.2) —
at one of two levels:

- **Provisional conformance:** the suite passes against the live endpoint, including
  its client-observable fault modes (connection loss mid-request, retried conditional
  writes, network chaos). Any consumer can establish this level independently, without
  the provider's cooperation.
- **Full conformance:** additionally, the suite passes while the store's own
  infrastructure is subjected to backend faults — at minimum: network partitions
  between replicas and regions, process and node crash-recovery, and any
  anti-entropy/repair mechanisms the store runs. Backend faults can only be injected
  by the store's operator, so full conformance is necessarily a provider-published
  claim; it MUST use the unmodified public suite as the workload and MUST be
  accompanied by the fault methodology. C5 in particular is only meaningfully
  established at this level — delete resurrection is historically triggered by
  recovery and repair paths that client-side chaos cannot reach.

The suite reports pass/fail per clause plus observed latency distributions; latency is
reported, not judged — the contract is a correctness contract.

## Notes on scope

This contract specifies sufficiency for hosting a shared log, not performance or price.
The conformance report contains correctness verdicts and observed latency
distributions only; whether a conforming store is *economical* for a given workload
(small objects at high request rates) is left to the reader to evaluate against
provider pricing — the contract and the suite take no position on cost.
