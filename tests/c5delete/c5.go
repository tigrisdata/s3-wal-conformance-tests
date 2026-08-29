// Package c5delete tests contract clause C5: delete durability. Once a
// DELETE completes and nothing recreates the key, no client may ever observe
// it as present again.
//
// Resurrection is historically triggered by recovery and anti-entropy paths,
// which need long time horizons and backend faults to fire. This group
// therefore layers three time scales:
//
//   - within-run: delete keys, then hammer GET+LIST from every vantage for an
//     observation window;
//   - trim-shaped: delete advancing prefixes of a log keyspace and assert the
//     survivor set is exact after every trim (a resurrected trimmed entry is
//     precisely what corrupts log replay);
//   - across runs: a manifest under the artifacts dir records every key this
//     group ever deleted in the target bucket; each later run re-verifies
//     none has resurfaced, giving arbitrary wall-clock gaps across runs.
//
// Full C5 conformance additionally requires provider-internal backend fault
// injection (see the spec's Conformance section); this group is the workload
// those runs execute.
package c5delete

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

// PersistentPrefix hosts keys that must stay checkable across runs. It is
// deliberately NOT under the seed-scoped run prefix: every key ever created
// here is deleted before the group returns, so the prefix must always list
// empty — in this run and every future one.
const PersistentPrefix = "s3-wal-conformance-c5-persistent/"

type Config struct {
	Stores []*store.Store
	Prefix string // seed-scoped prefix for the trim and recreate checks
	Rng    *rand.Rand

	ArtifactsDir string        // manifest location (default "conformance-artifacts")
	Keys         int           // keys for delete-then-hammer (default 60)
	Readers      int           // hammer readers (default 4)
	ObserveFor   time.Duration // hammer window after deletes (default 15s)
	LogKeys      int           // trim check keyspace (default 50)
	TrimBatch    int           // keys per trim round (default 10)
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) defaults() {
	if c.ArtifactsDir == "" {
		c.ArtifactsDir = "conformance-artifacts"
	}
	if c.Keys == 0 {
		c.Keys = 60
	}
	if c.Readers == 0 {
		c.Readers = 4
	}
	if c.ObserveFor == 0 {
		c.ObserveFor = 15 * time.Second
	}
	if c.LogKeys == 0 {
		c.LogKeys = 50
	}
	if c.TrimBatch == 0 {
		c.TrimBatch = 10
	}
}

func Run(ctx context.Context, cfg *Config) report.GroupResult {
	cfg.defaults()
	g := report.GroupResult{Name: "c5_delete_durability", Clause: "C5"}
	g.Add(revisitPriorDeletes(ctx, cfg))
	g.Add(deleteThenHammer(ctx, cfg))
	g.Add(trimPrefix(ctx, cfg))
	g.Add(recreateCycle(ctx, cfg))
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

// ---- manifest -------------------------------------------------------------

type manifestEntry struct {
	Key       string    `json:"key"`
	DeletedAt time.Time `json:"deleted_at"`
}

type manifest struct {
	Bucket  string          `json:"bucket"`
	Entries []manifestEntry `json:"entries"`
}

func (c *Config) manifestPath() string {
	return filepath.Join(c.ArtifactsDir, "c5-deleted-manifest-"+c.store(0).Bucket+".json")
}

func loadManifest(cfg *Config) (*manifest, error) {
	data, err := os.ReadFile(cfg.manifestPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func appendManifest(cfg *Config, entries []manifestEntry) error {
	m, err := loadManifest(cfg)
	if err != nil || m == nil {
		m = &manifest{Bucket: cfg.store(0).Bucket}
	}
	m.Entries = append(m.Entries, entries...)
	const keep = 5000
	if len(m.Entries) > keep {
		m.Entries = m.Entries[len(m.Entries)-keep:]
	}
	if err := os.MkdirAll(cfg.ArtifactsDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.manifestPath(), data, 0o644)
}

// revisitPriorDeletes re-verifies every key a previous run deleted in this
// bucket: the persistent prefix must list empty from every vantage, and a
// sample of manifest keys must GET NotFound. The manifest gap is real
// wall-clock time, so this is the long-horizon resurrection check.
func revisitPriorDeletes(ctx context.Context, cfg *Config) report.CheckResult {
	name := "revisit_prior_deletes"
	m, err := loadManifest(cfg)
	if err != nil {
		return fail(name, "manifest unreadable: %v", err)
	}
	if m == nil || len(m.Entries) == 0 {
		return report.CheckResult{Name: name, Status: report.Info,
			Details: "no prior-run manifest for this bucket yet — cross-run gap evidence starts accumulating after this run"}
	}
	for v := range cfg.Stores {
		st := cfg.store(v)
		token := ""
		for {
			page, out := st.List(ctx, PersistentPrefix, token, 0)
			if out != store.OK {
				return fail(name, "LIST failed at vantage %s: %s", st.Vantage, out)
			}
			if len(page.Keys) > 0 {
				return fail(name, "vantage %s: key %s deleted by a previous run has RESURFACED in LIST (%d keys under the persistent prefix)",
					st.Vantage, page.Keys[0], len(page.Keys))
			}
			if !page.Truncated {
				break
			}
			token = page.NextToken
		}
	}
	sample := m.Entries
	if len(sample) > 50 {
		idx := cfg.Rng.Perm(len(m.Entries))[:50]
		sample = make([]manifestEntry, 0, 50)
		for _, i := range idx {
			sample = append(sample, m.Entries[i])
		}
	}
	oldest := time.Duration(0)
	for _, e := range sample {
		st := cfg.store(cfg.Rng.Intn(len(cfg.Stores)))
		if _, _, out := st.Get(ctx, e.Key); out == store.OK {
			return fail(name, "key %s deleted %s ago has RESURFACED via GET at vantage %s",
				e.Key, time.Since(e.DeletedAt).Round(time.Second), st.Vantage)
		}
		if age := time.Since(e.DeletedAt); age > oldest {
			oldest = age
		}
	}
	return pass(name, "%d previously deleted keys (%d sampled by GET, oldest deleted %s ago) still absent from every vantage",
		len(m.Entries), len(sample), oldest.Round(time.Second))
}

// deleteThenHammer creates keys, observes them present, deletes them, then
// hammers GET+LIST from every vantage for the observation window.
func deleteThenHammer(ctx context.Context, cfg *Config) report.CheckResult {
	name := "delete_then_hammer"
	arena := fmt.Sprintf("%srun-%d/", PersistentPrefix, cfg.Rng.Int63())

	keys := make([]string, cfg.Keys)
	for i := range keys {
		keys[i] = fmt.Sprintf("%sk-%05d", arena, i)
		if _, out := cfg.store(0).Put(ctx, keys[i], []byte("doomed")); out != store.OK {
			return fail(name, "setup PUT failed: %s", out)
		}
	}
	// Observe present first: C5 violations are strongest when the key was
	// demonstrably visible before its delete.
	for i, k := range keys {
		if _, _, out := cfg.store(i).Get(ctx, k); out != store.OK {
			return fail(name, "setup GET of %s failed: %s", k, out)
		}
	}
	deletedAt := make([]time.Time, len(keys))
	var entries []manifestEntry
	for i, k := range keys {
		if out := cfg.store(0).Delete(ctx, k); out != store.OK {
			return fail(name, "DELETE of %s failed: %s", k, out)
		}
		deletedAt[i] = time.Now()
		entries = append(entries, manifestEntry{Key: k, DeletedAt: deletedAt[i]})
	}

	var violation atomic.Value
	deadline := time.Now().Add(cfg.ObserveFor)
	var wg sync.WaitGroup
	for r := 0; r < cfg.Readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(r) * 7919))
			st := cfg.store(r)
			for time.Now().Before(deadline) && violation.Load() == nil {
				if rng.Intn(100) < 70 {
					i := rng.Intn(len(keys))
					if _, _, out := st.Get(ctx, keys[i]); out == store.OK {
						violation.CompareAndSwap(nil, fmt.Sprintf(
							"key %s observed present via GET at vantage %s, %s after its DELETE completed",
							keys[i], st.Vantage, time.Since(deletedAt[i]).Round(time.Millisecond)))
						return
					}
				} else {
					token := ""
					for {
						page, out := st.List(ctx, arena, token, 0)
						if out != store.OK {
							break
						}
						if len(page.Keys) > 0 {
							violation.CompareAndSwap(nil, fmt.Sprintf(
								"key %s observed present via LIST at vantage %s after all deletes completed",
								page.Keys[0], st.Vantage))
							return
						}
						if !page.Truncated {
							break
						}
						token = page.NextToken
					}
				}
			}
		}(r)
	}
	wg.Wait()
	if v := violation.Load(); v != nil {
		return fail(name, "%s", v.(string))
	}
	if err := appendManifest(cfg, entries); err != nil {
		return fail(name, "observation clean but manifest write failed: %v", err)
	}
	return pass(name, "%d deleted keys hammered by %d readers for %s via GET+LIST: none resurfaced (manifest updated for future runs)",
		len(keys), cfg.Readers, cfg.ObserveFor)
}

// trimPrefix is the log-trim shape: create log/00000..N, delete advancing
// prefixes in batches, and after every trim assert a full scan returns
// exactly the surviving suffix — in order, no trimmed key resurfacing.
func trimPrefix(ctx context.Context, cfg *Config) report.CheckResult {
	name := "trim_prefix"
	arena := cfg.Prefix + "log/"

	keys := make([]string, cfg.LogKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s%08d", arena, i)
		if _, out := cfg.store(0).Put(ctx, keys[i], []byte("entry")); out != store.OK {
			return fail(name, "setup PUT failed: %s", out)
		}
	}
	for trimmed := 0; trimmed < cfg.LogKeys; {
		batch := min(cfg.TrimBatch, cfg.LogKeys-trimmed)
		for i := trimmed; i < trimmed+batch; i++ {
			if out := cfg.store(0).Delete(ctx, keys[i]); out != store.OK {
				return fail(name, "trim DELETE of %s failed: %s", keys[i], out)
			}
		}
		trimmed += batch

		st := cfg.store(trimmed) // rotate vantages across rounds
		var got []string
		token := ""
		for {
			page, out := st.List(ctx, arena, token, 7) // small pages on purpose
			if out != store.OK {
				return fail(name, "post-trim LIST failed: %s", out)
			}
			got = append(got, page.Keys...)
			if !page.Truncated {
				break
			}
			token = page.NextToken
		}
		want := keys[trimmed:]
		if len(got) != len(want) {
			return fail(name, "after trimming %d entries the scan returned %d keys, want %d — %s",
				trimmed, len(got), len(want), diagnoseTrim(got, keys, trimmed))
		}
		for i := range want {
			if got[i] != want[i] {
				return fail(name, "after trimming %d entries, scan position %d is %q, want %q", trimmed, i, got[i], want[i])
			}
		}
		if trimmed > 0 {
			boundary := keys[trimmed-1]
			if _, _, out := st.Get(ctx, boundary); out == store.OK {
				return fail(name, "trimmed boundary entry %s still readable after its trim round", boundary)
			}
		}
	}
	return pass(name, "%d-entry log trimmed to empty in %d-entry batches: survivor scan exact after every round, no trimmed entry resurfaced",
		cfg.LogKeys, cfg.TrimBatch)
}

func diagnoseTrim(got, all []string, trimmed int) string {
	inTrimmed := map[string]bool{}
	for _, k := range all[:trimmed] {
		inTrimmed[k] = true
	}
	for _, k := range got {
		if inTrimmed[k] {
			return fmt.Sprintf("trimmed entry %s RESURFACED in the scan", k)
		}
	}
	return "surviving entries missing from the scan"
}

// recreateCycle exercises the C2/C5 escape hatch: delete → recreate →
// delete again, verifying each transition lands.
func recreateCycle(ctx context.Context, cfg *Config) report.CheckResult {
	name := "recreate_cycle"
	key := cfg.Prefix + "phoenix"
	const cycles = 5
	for i := range cycles {
		v := fmt.Sprintf("life-%d", i)
		if _, out := cfg.store(i).Put(ctx, key, []byte(v)); out != store.OK {
			return fail(name, "cycle %d: PUT failed: %s", i, out)
		}
		body, _, out := cfg.store(i).Get(ctx, key)
		if out != store.OK {
			return fail(name, "cycle %d: GET after recreate returned %s, want the recreated object", i, out)
		}
		if string(body) != v {
			return fail(name, "cycle %d: GET after recreate returned value %q, want %q — a stale generation surfaced", i, string(body), v)
		}
		if out := cfg.store(i).Delete(ctx, key); out != store.OK {
			return fail(name, "cycle %d: DELETE failed: %s", i, out)
		}
		if _, _, out := cfg.store(i).Get(ctx, key); out != store.NotFound {
			return fail(name, "cycle %d: GET after DELETE returned %s, want not_found", i, out)
		}
	}
	return pass(name, "%d create/delete cycles: every recreate visible with the fresh value, every delete final", cycles)
}

func pass(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
