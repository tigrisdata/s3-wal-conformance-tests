// Package c4cas tests contract clause C4: conditional write / compare-and-swap.
//
// Verdicts are outcome-based, never status-code tallies. Each contender's
// result is classified won / lost / ambiguous from its response, and the
// final verdict comes from read-back of the key's actual state — a lost 200
// or a replayed request must not turn into a false failure (or false pass).
package c4cas

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sync"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

type Config struct {
	// Stores are the client vantages; contenders are spread round-robin
	// across them. One store is enough for regional evidence.
	Stores     []*store.Store
	Prefix     string // key prefix owned by this group
	Contenders int
	Rounds     int
	Rng        *rand.Rand
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) body(tag string, i int) []byte {
	b := make([]byte, 64)
	c.Rng.Read(b)
	return append([]byte(fmt.Sprintf("%s-%d-", tag, i)), b...)
}

// Run executes the group and returns its result. Best-effort cleanup of the
// group's key prefix happens before returning.
func Run(ctx context.Context, cfg *Config) report.GroupResult {
	g := report.GroupResult{Name: "c4_cas", Clause: "C4"}
	g.Add(contendedCAS(ctx, cfg))
	g.Add(contendedCreate(ctx, cfg))
	g.Add(staleEtagRejected(ctx, cfg))
	g.Add(chainedRegister(ctx, cfg))
	g.Add(etagFreshness(ctx, cfg))
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

type contenderResult struct {
	outcome store.Outcome
	newEtag string
}

// contendedCAS: all contenders CAS from the same captured ETag. Exactly one
// logical winner; losers get 412 with no mutation; a concurrent sampler must
// never observe a loser's payload.
func contendedCAS(ctx context.Context, cfg *Config) report.CheckResult {
	name := "contended_cas"
	for round := 0; round < cfg.Rounds; round++ {
		key := fmt.Sprintf("%scontended-cas/r%04d", cfg.Prefix, round)
		initial := cfg.body("initial", round)
		if _, out := cfg.store(0).Put(ctx, key, initial); out != store.OK {
			return fail(name, "setup PUT failed: %s", out)
		}
		_, etag0, out := cfg.store(0).Get(ctx, key)
		if out != store.OK {
			return fail(name, "setup GET failed: %s", out)
		}

		bodies := make([][]byte, cfg.Contenders)
		for i := range bodies {
			bodies[i] = cfg.body("contender", i)
		}

		// Sampler: concurrent GETs during the round. Everything it observes
		// must be the initial value or the eventual winner — a loser's
		// payload observable at any moment is a 412-mutation violation.
		samplerStop := make(chan struct{})
		var sampled [][]byte
		var samplerWg sync.WaitGroup
		samplerWg.Add(1)
		go func() {
			defer samplerWg.Done()
			for {
				select {
				case <-samplerStop:
					return
				default:
				}
				if b, _, out := cfg.store(0).Get(ctx, key); out == store.OK {
					sampled = append(sampled, b)
				}
			}
		}()

		results := make([]contenderResult, cfg.Contenders)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < cfg.Contenders; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				newEtag, out := cfg.store(i).PutIfMatch(ctx, key, etag0, bodies[i])
				results[i] = contenderResult{outcome: out, newEtag: newEtag}
			}(i)
		}
		close(start)
		wg.Wait()
		close(samplerStop)
		samplerWg.Wait()

		if res := judgeContention(ctx, cfg, name, key, initial, bodies, results, true); res != nil {
			return *res
		}
		for _, b := range sampled {
			if err := mustBeInitialOrFinal(ctx, cfg, key, initial, b); err != "" {
				return fail(name, "round %d: sampler observed a payload that is neither the initial value nor the final state (%s) — a losing conditional write was observable", round, err)
			}
		}
	}
	return pass(name, "%d rounds × %d contenders: exactly one logical winner per round, losers unobservable", cfg.Rounds, cfg.Contenders)
}

// contendedCreate: contenders race If-None-Match:* on a fresh key.
func contendedCreate(ctx context.Context, cfg *Config) report.CheckResult {
	name := "contended_create"
	for round := 0; round < cfg.Rounds; round++ {
		key := fmt.Sprintf("%scontended-create/r%04d", cfg.Prefix, round)
		if _, _, out := cfg.store(0).Get(ctx, key); out != store.NotFound {
			return fail(name, "round %d: key unexpectedly present before create race", round)
		}
		bodies := make([][]byte, cfg.Contenders)
		for i := range bodies {
			bodies[i] = cfg.body("creator", i)
		}
		results := make([]contenderResult, cfg.Contenders)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < cfg.Contenders; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				etag, out := cfg.store(i).PutIfAbsent(ctx, key, bodies[i])
				results[i] = contenderResult{outcome: out, newEtag: etag}
			}(i)
		}
		close(start)
		wg.Wait()
		if res := judgeContention(ctx, cfg, name, key, nil, bodies, results, false); res != nil {
			return *res
		}
	}
	return pass(name, "%d rounds × %d contenders: exactly one create wins", cfg.Rounds, cfg.Contenders)
}

// judgeContention reads back the key's final state and applies the pure
// outcome-based verdict. Returns nil if the round conforms.
func judgeContention(ctx context.Context, cfg *Config, name, key string, initial []byte, bodies [][]byte, results []contenderResult, initialAllowed bool) *report.CheckResult {
	final, _, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		f := fail(name, "read-back failed: %s", out)
		return &f
	}
	return judgeOutcomes(name, initial, bodies, results, final, initialAllowed)
}

// judgeOutcomes is the pure verdict over one contention round: contender
// outcomes plus the read-back final state. initial is nil for create races
// (the key did not exist beforehand).
func judgeOutcomes(name string, initial []byte, bodies [][]byte, results []contenderResult, final []byte, initialAllowed bool) *report.CheckResult {
	wins, ambiguous := 0, 0
	winner := -1
	for i, r := range results {
		switch r.outcome {
		case store.OK:
			wins++
			winner = i
		case store.Ambiguous:
			ambiguous++
		case store.PreconditionFailed:
			// expected for losers
		default:
			f := fail(name, "contender %d: unexpected outcome %s", i, r.outcome)
			return &f
		}
	}
	if wins > 1 {
		f := fail(name, "%d contenders acknowledged as winners for one predicate state — CAS admitted multiple writes", wins)
		return &f
	}
	matchIdx := -1
	for i, b := range bodies {
		if string(b) == string(final) {
			matchIdx = i
			break
		}
	}
	switch {
	case wins == 1:
		if matchIdx != winner {
			f := fail(name, "acknowledged winner %d but final state is %s — lost update", winner, describeState(matchIdx, initial, final))
			return &f
		}
	case ambiguous > 0:
		// No acknowledged winner: the state must be the initial value or
		// exactly one ambiguous contender's value (its lost success).
		if matchIdx >= 0 {
			if results[matchIdx].outcome != store.Ambiguous {
				f := fail(name, "final state belongs to contender %d whose outcome was %s, not ambiguous — a 412'd write mutated state", matchIdx, results[matchIdx].outcome)
				return &f
			}
		} else if !initialAllowed || string(final) != string(initial) {
			f := fail(name, "no winner acknowledged, final state matches no contender%s", initialClause(initialAllowed))
			return &f
		}
	default:
		// No winner, no ambiguity, no interference: per spec C4, exactly one
		// of the matching contenders MUST succeed.
		f := fail(name, "all %d contenders failed with 412 against the current state — store made no consensus progress", len(results))
		return &f
	}
	return nil
}

// staleEtagRejected: CAS against a superseded ETag must 412 and not mutate.
func staleEtagRejected(ctx context.Context, cfg *Config) report.CheckResult {
	name := "stale_etag_rejected"
	key := cfg.Prefix + "stale-etag"
	v1 := cfg.body("v1", 0)
	if _, out := cfg.store(0).Put(ctx, key, v1); out != store.OK {
		return fail(name, "setup PUT v1 failed: %s", out)
	}
	_, etag1, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		return fail(name, "setup GET failed: %s", out)
	}
	v2 := cfg.body("v2", 0)
	if _, out := cfg.store(0).Put(ctx, key, v2); out != store.OK {
		return fail(name, "setup PUT v2 failed: %s", out)
	}
	v3 := cfg.body("v3", 0)
	_, casOut := cfg.store(0).PutIfMatch(ctx, key, etag1, v3)
	if casOut == store.OK {
		return fail(name, "CAS with a superseded ETag was acknowledged — conditions not evaluated against latest state")
	}
	final, _, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		return fail(name, "read-back failed: %s", out)
	}
	if string(final) != string(v2) {
		return fail(name, "state mutated by a failed conditional write (CAS outcome %s)", casOut)
	}
	return pass(name, "superseded ETag rejected with %s, no mutation", casOut)
}

// chainedRegister models the VirtualLog membership register: workers advance
// an epoch counter via CAS. Sound invariants under ambiguity:
//   - no two acknowledged CAS successes target the same epoch (fork / lost update)
//   - with no ambiguous outcomes, final epoch == acknowledged successes
//   - each worker's observed epochs never decrease
func chainedRegister(ctx context.Context, cfg *Config) report.CheckResult {
	name := "chained_register"
	key := cfg.Prefix + "register"
	if _, out := cfg.store(0).Put(ctx, key, epochBody(0)); out != store.OK {
		return fail(name, "setup PUT failed: %s", out)
	}

	type ack struct{ worker, epoch int }
	var mu sync.Mutex
	var acks []ack
	ambiguous := 0
	violation := ""

	attempts := cfg.Rounds * 4
	var wg sync.WaitGroup
	for w := 0; w < cfg.Contenders; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st := cfg.store(w)
			lastSeen := -1
			for a := 0; a < attempts; a++ {
				body, etag, out := st.Get(ctx, key)
				if out != store.OK {
					continue
				}
				epoch, ok := epochOf(body)
				if !ok {
					mu.Lock()
					violation = fmt.Sprintf("worker %d read a register body that is not a valid epoch", w)
					mu.Unlock()
					return
				}
				if epoch < lastSeen {
					mu.Lock()
					violation = fmt.Sprintf("worker %d observed epoch %d after epoch %d — register went backwards", w, epoch, lastSeen)
					mu.Unlock()
					return
				}
				lastSeen = epoch
				_, casOut := st.PutIfMatch(ctx, key, etag, epochBody(epoch+1))
				mu.Lock()
				switch casOut {
				case store.OK:
					acks = append(acks, ack{worker: w, epoch: epoch + 1})
				case store.Ambiguous:
					ambiguous++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if violation != "" {
		return fail(name, "%s", violation)
	}

	seen := map[int]int{}
	for _, a := range acks {
		if prev, dup := seen[a.epoch]; dup {
			return fail(name, "workers %d and %d were both acknowledged for epoch %d — the register forked", prev, a.worker, a.epoch)
		}
		seen[a.epoch] = a.worker
	}
	finalBody, _, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		return fail(name, "final read-back failed: %s", out)
	}
	finalEpoch, ok := epochOf(finalBody)
	if !ok {
		return fail(name, "final register body is not a valid epoch")
	}
	if ambiguous == 0 && finalEpoch != len(acks) {
		return fail(name, "final epoch %d but %d acknowledged CAS successes — lost or phantom update", finalEpoch, len(acks))
	}
	if finalEpoch < len(acks) {
		return fail(name, "final epoch %d is lower than %d acknowledged successes — acknowledged writes lost", finalEpoch, len(acks))
	}
	return pass(name, "%d workers advanced the register to epoch %d (%d acknowledged, %d ambiguous), no forks",
		cfg.Contenders, finalEpoch, len(acks), ambiguous)
}

// etagFreshness is informational: the spec's C4 requires the conformance
// report to state whether the store returns a distinct ETag for every write,
// or derives ETags from content (in which case register values must be kept
// byte-distinct by the client — the ABA caveat).
func etagFreshness(ctx context.Context, cfg *Config) report.CheckResult {
	name := "etag_freshness"
	key := cfg.Prefix + "etag-freshness"
	body := cfg.body("same-bytes", 0)
	if _, out := cfg.store(0).Put(ctx, key, body); out != store.OK {
		return fail(name, "setup PUT failed: %s", out)
	}
	_, e1, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		return fail(name, "GET failed: %s", out)
	}
	if _, out := cfg.store(0).Put(ctx, key, body); out != store.OK {
		return fail(name, "second PUT failed: %s", out)
	}
	_, e2, out := cfg.store(0).Get(ctx, key)
	if out != store.OK {
		return fail(name, "GET failed: %s", out)
	}
	if e1 != e2 {
		return report.CheckResult{Name: name, Status: report.Info,
			Details: "store returns a DISTINCT ETag for byte-identical rewrites — If-Match CAS is ABA-safe as-is"}
	}
	return report.CheckResult{Name: name, Status: report.Info,
		Details: "store returns the SAME ETag for byte-identical rewrites (content-derived) — register clients MUST keep successive values byte-distinct (embed an epoch), per spec C4"}
}

func epochBody(n int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(n))
	return append([]byte("epoch:"), b...)
}

func epochOf(body []byte) (int, bool) {
	if len(body) != 14 || string(body[:6]) != "epoch:" {
		return 0, false
	}
	return int(binary.BigEndian.Uint64(body[6:])), true
}

func mustBeInitialOrFinal(ctx context.Context, cfg *Config, key string, initial, observed []byte) string {
	if string(observed) == string(initial) {
		return ""
	}
	final, _, out := cfg.store(0).Get(ctx, key)
	if out == store.OK && string(observed) == string(final) {
		return ""
	}
	return fmt.Sprintf("%d bytes observed", len(observed))
}

func describeState(matchIdx int, initial, final []byte) string {
	if matchIdx >= 0 {
		return fmt.Sprintf("contender %d's value", matchIdx)
	}
	if string(final) == string(initial) {
		return "the initial value"
	}
	return "an unknown value"
}

func initialClause(initialAllowed bool) string {
	if initialAllowed {
		return " and is not the initial value"
	}
	return " (create race: key must hold a contender's value)"
}

func pass(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
