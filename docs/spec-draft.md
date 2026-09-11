# The Object-Store Contract for Shared-Log Workloads

**Version:** 0.1 (draft)
**Status:** Published draft, open for review. The clause set (C1–C5) is stable;
wording, conformance-level details, and test derivations may change before 1.0.
Feedback via issues on this repository.

## Abstract

A class of distributed systems — shared logs, consensus protocols, state machine
replication, and the streaming and database systems built on them — can delegate all
durability and coordination to a commodity object store, eliminating any separate
metadata database, provided the object store meets a small set of testable guarantees.
This document specifies those guarantees as five clauses over the standard
S3-style API (PUT, GET, LIST, DELETE, conditional PUT). It is derived from the
formal specification (Appendix A) and lemmas of Vickers et al., *"The LogDrive:
Composable Durability for Cloud-Based Shared Logs"* (OSDI '26), deliberately
strengthened into clauses a provider can offer and a black-box suite can verify —
the section *Relationship to the LogDrive paper* states exactly which parts are the
paper's and which are this contract's. A companion open-source conformance suite tests
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
as a hole in the log; the paper's authors' own simulation testing surfaced a
pagination-induced linearizability violation in their S3 implementation (§4.1) that
their API-level tests had not caught. Conformance testing of this clause MUST be adversarial (writes landing mid-scan,
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

## Relationship to the LogDrive paper

This contract is derived from the paper but is deliberately **stronger** than the
paper's proven minimum. The differences are intentional:

- **The paper's log entries are write-once; C1 covers arbitrary overwrites.** The
  paper's §A.1 specifies linearizable READ/WRITE — a total order of completed
  operations consistent with real-time precedence — but under a *single-value
  assumption*: if two writes propose different values to one address, behavior is
  unspecified. Its `weakTail` is explicitly *not* linearizable — only equivalent to
  a deterministic function over an unordered, non-atomic scan (Lemmas LD.1, LD.2).
  A provider cannot observe or enforce a client's single-value discipline, so C1
  requires linearizability under arbitrary overwrites and deletes — the stronger,
  black-box-testable property, which implies §A.1's requirement on well-formed
  executions.
- **C2 generalizes §A.1's write-once monotonicity.** The paper states monotonicity
  of written-ness as a first-class axiom: once written, an address is never
  unwritten. C2 is its observation-side counterpart for stores with deletion — once
  observed present, never observed absent unless deleted after that observation.
- **The paper tolerated weaker listing than C3.** The authors' simulation testing
  surfaced a pagination-induced linearizability violation in their S3 LogDrive;
  their resolution was the insight that the AtomicLog layered above remains
  linearizable even when `weakTail` is not (§4.1). A provider contract cannot assume
  every consumer builds that exact layer on top, so C3 requires strongly consistent,
  ordered, monotone-cursor listing outright.
- **C5 formalizes what the paper leaves implicit.** The Loglet API includes
  `prefixTrim` (§4, Fig. 5) and the paper's systems checkpoint and trim aggressively
  (§5.1), but the paper states no delete-durability requirement. C5 is this
  contract's formalization of what safe trimming assumes.
- **C4 matches the paper.** One conditionally-written register is the only source of
  consensus in the paper's system — the VirtualLog membership register (§5.1); log
  entries themselves need no conditional writes (the paper notes that S3 before
  conditional-write support sufficed for entries). The paper's production system
  keeps that register in DynamoDB, a choice made when S3 lacked conditional writes;
  conditional PUT is now standard across major providers, which is precisely what
  makes this contract satisfiable on object storage alone.
- The explicit non-requirements restate the paper's design premise: no append API,
  no transactions, no server-side sequencing, composition over commodity
  put/get/list (§1, §3).

The implication runs one way: a store satisfying C1–C5 supports the paper's
constructions — LogDrive, AtomicLog, VirtualLog — with margin. The converse is not
claimed; the paper's minimum is genuinely weaker, and a store could host a LogDrive
while failing this contract.

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
