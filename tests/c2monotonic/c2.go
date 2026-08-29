// Package c2monotonic tests contract clause C2: monotonicity of existence,
// as observed. A writer creates keys (never deleting any); a pool of readers
// concurrently observes via GET and full LIST scans. Once any reader has
// observed a key as present, no reader may ever observe it as absent — via
// either observation path. This is the precondition of the paper's
// tail-range lemma (LD.1), and it runs on its own key prefix so deletes in
// other groups can never read as violations.
package c2monotonic

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

type Config struct {
	Stores []*store.Store
	Prefix string
	Rng    *rand.Rand

	Keys    int // keys the writer creates (default 100)
	Readers int // concurrent readers, spread across vantages (default 4)
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) defaults() {
	if c.Keys == 0 {
		c.Keys = 100
	}
	if c.Readers == 0 {
		c.Readers = 4
	}
}

func Run(ctx context.Context, cfg *Config) report.GroupResult {
	cfg.defaults()
	g := report.GroupResult{Name: "c2_monotonic_existence", Clause: "C2"}
	g.Add(absentAfterPresent(ctx, cfg))
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

// seenTracker records which keys have been observed present, by any reader,
// through any path. Committed-but-unobserved keys are C1's business; C2
// triggers only after an observation.
type seenTracker struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (t *seenTracker) mark(key string) {
	t.mu.Lock()
	t.seen[key] = true
	t.mu.Unlock()
}

func (t *seenTracker) has(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[key]
}

func (t *seenTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.seen))
	for k := range t.seen {
		out = append(out, k)
	}
	return out
}

func absentAfterPresent(ctx context.Context, cfg *Config) report.CheckResult {
	name := "absent_after_present"
	arena := cfg.Prefix + "keys/"

	tracker := &seenTracker{seen: map[string]bool{}}
	var committedKeys []string // keys whose PUT was acknowledged, in order
	var committedMu sync.Mutex
	writerDone := make(chan struct{})
	var violation atomic.Value // string

	failNow := func(format string, args ...any) {
		violation.CompareAndSwap(nil, fmt.Sprintf(format, args...))
	}

	// Writer: create keys sequentially, never delete.
	go func() {
		defer close(writerDone)
		for i := 0; i < cfg.Keys; i++ {
			if violation.Load() != nil {
				return
			}
			key := fmt.Sprintf("%sk-%05d", arena, i)
			if _, out := cfg.store(0).Put(ctx, key, []byte("v")); out == store.OK {
				committedMu.Lock()
				committedKeys = append(committedKeys, key)
				committedMu.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for r := 0; r < cfg.Readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(cfg.Rng.Int63() + int64(r)))
			st := cfg.store(r)
			finalPass := false
			for {
				if violation.Load() != nil {
					return
				}
				select {
				case <-writerDone:
					if finalPass {
						return
					}
					finalPass = true // one more sweep after the writer stops
				default:
				}

				if rng.Intn(100) < 70 {
					// GET path.
					committedMu.Lock()
					n := len(committedKeys)
					var key string
					if n > 0 {
						key = committedKeys[rng.Intn(n)]
					}
					committedMu.Unlock()
					if key == "" {
						continue
					}
					_, _, out := st.Get(ctx, key)
					switch out {
					case store.OK:
						tracker.mark(key)
					case store.NotFound:
						if tracker.has(key) {
							failNow("reader %d (vantage %s) observed key %s as ABSENT via GET after it had been observed present — existence went backwards", r, st.Vantage, key)
							return
						}
					}
				} else {
					// LIST path: keys observed present BEFORE the scan began
					// are stable for its whole duration (nothing is ever
					// deleted), so a full scan must return every one of them.
					before := tracker.snapshot()
					returned := map[string]bool{}
					token := ""
					ok := true
					for {
						page, out := st.List(ctx, arena, token, 0)
						if out != store.OK {
							ok = false
							break
						}
						for _, k := range page.Keys {
							returned[k] = true
							tracker.mark(k)
						}
						if !page.Truncated {
							break
						}
						token = page.NextToken
					}
					if ok {
						for _, k := range before {
							if !returned[k] {
								failNow("reader %d (vantage %s): key %s observed present before a full LIST scan was missing from it — existence went backwards on the LIST path", r, st.Vantage, k)
								return
							}
						}
					}
				}
			}
		}(r)
	}
	<-writerDone
	wg.Wait()

	if v := violation.Load(); v != nil {
		return fail(name, "%s", v.(string))
	}
	observed := len(tracker.snapshot())
	return pass(name, "%d keys created, %d observed present by %d readers via GET+LIST: no absent-after-present observation",
		cfg.Keys, observed, cfg.Readers)
}

func pass(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
