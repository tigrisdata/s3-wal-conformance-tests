// Package c3listing tests contract clause C3: strongly consistent, ordered
// listing with monotone-cursor pagination.
//
// This is the clause where the LogDrive authors found a real production bug,
// so the checks are adversarial rather than assertion-by-specification: a
// stable keyspace is scanned under concurrent insert/delete churn, page sizes
// sweep across keyspace-size boundaries, and a deterministic attack plants
// keys just ahead of and just behind the continuation cursor between pages.
package c3listing

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

type Config struct {
	Stores []*store.Store
	Prefix string // key prefix owned by this group
	Rng    *rand.Rand

	StableCount  int     // stable keyspace size (default 50)
	PageSizes    []int32 // sizes to sweep; 0 = provider default (defaults straddle StableCount)
	ScansPerSize int     // churn scans per page size (default 2)
	ChurnWorkers int     // concurrent churn workers (default 3)
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) defaults() {
	if c.StableCount == 0 {
		c.StableCount = 50
	}
	if len(c.PageSizes) == 0 {
		n := int32(c.StableCount)
		c.PageSizes = []int32{1, 2, 7, n - 1, n, n + 1, 0}
	}
	if c.ScansPerSize == 0 {
		c.ScansPerSize = 2
	}
	if c.ChurnWorkers == 0 {
		c.ChurnWorkers = 3
	}
}

func Run(ctx context.Context, cfg *Config) report.GroupResult {
	cfg.defaults()
	g := report.GroupResult{Name: "c3_listing", Clause: "C3"}
	g.Add(listAfterWriteDelete(ctx, cfg))
	g.Add(orderedStableScan(ctx, cfg))
	g.Add(paginationChurn(ctx, cfg))
	g.Add(boundaryAttack(ctx, cfg))
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

// Stable keys use indices spaced by 10 so churn keys can always land
// lexicographically between two neighbors (fixed-width zero padding keeps
// byte order equal to numeric order).
func stableKey(arena string, i int) string {
	return fmt.Sprintf("%sk-%06d", arena, i*10)
}

func setupStable(ctx context.Context, cfg *Config, arena string) ([]string, string) {
	keys := make([]string, cfg.StableCount)
	for i := range keys {
		keys[i] = stableKey(arena, i)
		if _, out := cfg.store(0).Put(ctx, keys[i], []byte("stable")); out != store.OK {
			return nil, fmt.Sprintf("setup PUT %s failed: %s", keys[i], out)
		}
	}
	sort.Strings(keys)
	return keys, ""
}

// scan runs a full paginated listing and returns the pages.
func scan(ctx context.Context, st *store.Store, arena string, pageSize int32) ([][]string, string) {
	var pages [][]string
	token := ""
	for range 10000 {
		page, out := st.List(ctx, arena, token, pageSize)
		if out != store.OK {
			return nil, fmt.Sprintf("LIST failed: %s", out)
		}
		pages = append(pages, page.Keys)
		if !page.Truncated {
			return pages, ""
		}
		token = page.NextToken
	}
	return nil, "scan exceeded 10000 pages without terminating"
}

func flatten(pages [][]string) []string {
	var all []string
	for _, p := range pages {
		all = append(all, p...)
	}
	return all
}

// ascending asserts strict lexicographic order within and across pages —
// which also implies no key appears twice in one scan.
func ascending(pages [][]string) string {
	prev := ""
	for pi, p := range pages {
		for _, k := range p {
			if prev != "" && k <= prev {
				return fmt.Sprintf("key %q on page %d does not follow %q — ordering violated (or duplicate)", k, pi, prev)
			}
			prev = k
		}
	}
	return ""
}

// listAfterWriteDelete checks the single-request rule: a LIST issued after a
// completed PUT includes the key; after a completed DELETE it does not.
func listAfterWriteDelete(ctx context.Context, cfg *Config) report.CheckResult {
	name := "list_after_write_delete"
	const iters = 10
	for i := range iters {
		arena := fmt.Sprintf("%slaw/%03d/", cfg.Prefix, i)
		key := arena + "k"
		if _, out := cfg.store(0).Put(ctx, key, []byte("x")); out != store.OK {
			return fail(name, "PUT failed: %s", out)
		}
		page, out := cfg.store(i).List(ctx, arena, "", 0)
		if out != store.OK {
			return fail(name, "LIST failed: %s", out)
		}
		if !contains(page.Keys, key) {
			return fail(name, "LIST issued after PUT of %s completed did not include it — list-after-write violated", key)
		}
		if out := cfg.store(0).Delete(ctx, key); out != store.OK {
			return fail(name, "DELETE failed: %s", out)
		}
		page, out = cfg.store(i).List(ctx, arena, "", 0)
		if out != store.OK {
			return fail(name, "LIST failed: %s", out)
		}
		if contains(page.Keys, key) {
			return fail(name, "LIST issued after DELETE of %s completed still included it — list-after-delete violated", key)
		}
	}
	return pass(name, "%d iterations: LIST reflects completed PUTs and DELETEs", iters)
}

// orderedStableScan: with no churn, every page size must return exactly the
// stable keyspace, in order.
func orderedStableScan(ctx context.Context, cfg *Config) report.CheckResult {
	name := "ordered_stable_scan"
	arena := cfg.Prefix + "stable/"
	expected, errs := setupStable(ctx, cfg, arena)
	if errs != "" {
		return fail(name, "%s", errs)
	}
	for _, size := range cfg.PageSizes {
		pages, errs := scan(ctx, cfg.store(0), arena, size)
		if errs != "" {
			return fail(name, "page size %d: %s", size, errs)
		}
		if v := ascending(pages); v != "" {
			return fail(name, "page size %d: %s", size, v)
		}
		got := flatten(pages)
		if len(got) != len(expected) {
			return fail(name, "page size %d: returned %d keys, expected %d (skip or duplicate in a quiescent keyspace)", size, len(got), len(expected))
		}
		for i := range got {
			if got[i] != expected[i] {
				return fail(name, "page size %d: position %d returned %q, expected %q", size, i, got[i], expected[i])
			}
		}
	}
	return pass(name, "%d stable keys exact and ordered across page sizes %v", cfg.StableCount, cfg.PageSizes)
}

// churner continuously inserts keys into the gaps between stable keys and
// deletes some of its earlier inserts. Each churner owns its keys: stable
// keys are never touched.
func churner(ctx context.Context, st *store.Store, arena string, id int, seed int64, stop <-chan struct{}, stableCount int) {
	rng := rand.New(rand.NewSource(seed))
	var mine []string
	for {
		select {
		case <-stop:
			return
		default:
		}
		gap := rng.Intn(stableCount)
		key := fmt.Sprintf("%sk-%06d-c%d-%04d", arena, gap*10+1+rng.Intn(8), id, rng.Intn(10000))
		if _, out := st.Put(ctx, key, []byte("churn")); out == store.OK {
			mine = append(mine, key)
		}
		if len(mine) > 0 && rng.Intn(100) < 30 {
			i := rng.Intn(len(mine))
			st.Delete(ctx, mine[i])
			mine = append(mine[:i], mine[i+1:]...)
		}
	}
}

// paginationChurn: paginated scans under live insert/delete churn. Stable
// keys — present for every scan's whole duration — must appear exactly once
// per scan; order must stay strict; churn keys may come and go but can never
// duplicate (ascending covers it).
func paginationChurn(ctx context.Context, cfg *Config) report.CheckResult {
	name := "pagination_churn"
	arena := cfg.Prefix + "churn/"
	expected, errs := setupStable(ctx, cfg, arena)
	if errs != "" {
		return fail(name, "%s", errs)
	}
	stableSet := map[string]bool{}
	for _, k := range expected {
		stableSet[k] = true
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range cfg.ChurnWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			churner(ctx, cfg.store(w+1), arena, w, cfg.Rng.Int63(), stop, cfg.StableCount)
		}(w)
	}
	defer func() { close(stop); wg.Wait() }()

	scans := 0
	for _, size := range cfg.PageSizes {
		for range cfg.ScansPerSize {
			scans++
			pages, errs := scan(ctx, cfg.store(0), arena, size)
			if errs != "" {
				return fail(name, "page size %d under churn: %s", size, errs)
			}
			if v := ascending(pages); v != "" {
				return fail(name, "page size %d under churn: %s", size, v)
			}
			seen := map[string]int{}
			for _, k := range flatten(pages) {
				seen[k]++
			}
			for _, k := range expected {
				switch seen[k] {
				case 1:
				case 0:
					return fail(name, "page size %d under churn: stable key %q skipped — present for the whole scan but never returned", size, k)
				default:
					return fail(name, "page size %d under churn: stable key %q returned %d times", size, k, seen[k])
				}
			}
			for k := range seen {
				if !stableSet[k] && !strings.HasPrefix(k, arena+"k-") {
					return fail(name, "page size %d under churn: unexpected key %q outside the arena keyspace", size, k)
				}
			}
		}
	}
	return pass(name, "%d scans across page sizes %v under %d-worker churn: no stable key skipped or duplicated, order strict",
		scans, cfg.PageSizes, cfg.ChurnWorkers)
}

// boundaryAttack plants keys deterministically at the continuation cursor
// between pages:
//   - a key just AHEAD of the cursor, committed before the next page is
//     requested, MUST appear later in the scan (each page reflects completed
//     PUTs for its range);
//   - a key inserted ahead and DELETED before the next page MUST NOT appear;
//   - a key just BEHIND the cursor MUST NOT appear in any later page (the
//     cursor only advances).
func boundaryAttack(ctx context.Context, cfg *Config) report.CheckResult {
	name := "boundary_attack"
	arena := cfg.Prefix + "boundary/"
	if _, errs := setupStable(ctx, cfg, arena); errs != "" {
		return fail(name, "%s", errs)
	}
	const pageSize = 5

	type planted struct {
		key       string
		afterPage int
	}
	var mustAppear, mustNotAppear, behind []planted
	var pages [][]string
	token := ""
	for pi := 0; pi < 10000; pi++ {
		page, out := cfg.store(0).List(ctx, arena, token, pageSize)
		if out != store.OK {
			return fail(name, "LIST failed: %s", out)
		}
		pages = append(pages, page.Keys)
		if !page.Truncated {
			break
		}
		if n := len(page.Keys); n > 0 {
			last := page.Keys[n-1]
			ahead := fmt.Sprintf("%s-a%03d", last, pi)
			if _, out := cfg.store(0).Put(ctx, ahead, []byte("ahead")); out != store.OK {
				return fail(name, "planting PUT failed: %s", out)
			}
			mustAppear = append(mustAppear, planted{ahead, pi})

			gone := fmt.Sprintf("%s-ad%03d", last, pi)
			if _, out := cfg.store(0).Put(ctx, gone, []byte("gone")); out != store.OK {
				return fail(name, "planting PUT failed: %s", out)
			}
			if out := cfg.store(0).Delete(ctx, gone); out != store.OK {
				return fail(name, "planting DELETE failed: %s", out)
			}
			mustNotAppear = append(mustNotAppear, planted{gone, pi})

			if n >= 2 {
				b := fmt.Sprintf("%s-b%03d", page.Keys[n-2], pi)
				if _, out := cfg.store(0).Put(ctx, b, []byte("behind")); out != store.OK {
					return fail(name, "planting PUT failed: %s", out)
				}
				behind = append(behind, planted{b, pi})
			}
		}
		token = page.NextToken
	}
	if v := ascending(pages); v != "" {
		return fail(name, "%s", v)
	}
	appearedAfter := func(p planted) bool {
		for pi := p.afterPage + 1; pi < len(pages); pi++ {
			if contains(pages[pi], p.key) {
				return true
			}
		}
		return false
	}
	for _, p := range mustAppear {
		if !appearedAfter(p) {
			return fail(name, "key %q committed ahead of the cursor after page %d never appeared — a completed PUT in the unscanned range was skipped", p.key, p.afterPage)
		}
	}
	for _, p := range mustNotAppear {
		if appearedAfter(p) {
			return fail(name, "key %q deleted before page %d was served still appeared — LIST did not reflect a completed DELETE", p.key, p.afterPage+1)
		}
	}
	for _, p := range behind {
		if appearedAfter(p) {
			return fail(name, "key %q inserted behind the cursor after page %d appeared later — the cursor went backwards", p.key, p.afterPage)
		}
	}
	return pass(name, "%d pages: %d ahead-plants all appeared, %d deleted plants and %d behind-plants never did",
		len(pages), len(mustAppear), len(mustNotAppear), len(behind))
}

func contains(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func pass(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
