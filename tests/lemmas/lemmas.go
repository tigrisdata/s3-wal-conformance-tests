// Package lemmas runs the paper-shaped workload and checks the LogDrive
// lemmas from Vickers et al. (OSDI '26), Appendix A:
//
//   - LD.1 (Tail-Range Guarantee): a weakTail(K) executing over [t_start,
//     t_stop] observes a contiguous tail T_obs ∈ [T(t_start), T(t_stop)].
//   - LD.2 (Window-Scan Refinement): under the K-window discipline, reading
//     the non-contiguous tail linearizably and scanning only the K preceding
//     addresses is equivalent to some full, unordered, non-atomic scan.
//
// The workload follows the paper's S3 LogDrive construction (§3.2): each
// address is one key, reverse-encoded so higher addresses sort first, which
// makes a single LIST yield the highest written address. Writers append
// under the K-window discipline (address s may be issued only once every
// address ≤ s−K is acknowledged), each address written exactly once — the
// paper's single-value model, under which retries of the same value are
// harmless.
//
// Ground truth is tracked from the client side with sound bounds: an
// address acknowledged before t_start is certainly written at t_start, so
// the true T(t_start) is at least the first un-acked address (LB); an
// address not yet issued at t_stop is certainly unwritten, so the true
// T(t_stop) is at most the first un-issued address (UB). LD.1 then demands
// LB ≤ T_obs ≤ UB — any excursion outside is a real violation, never a
// test-harness race. Sequential weakTails must also observe non-decreasing
// tails (true tails are monotone under write-once semantics, and LD.1
// brackets each observation between them).
package lemmas

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

type Config struct {
	Stores []*store.Store
	Prefix string
	Rng    *rand.Rand

	Addresses     int // log length per run (default 200)
	K             int // window size (default 16, the paper's value)
	Writers       int // concurrent writers (default 6)
	PayloadBytes  int // object size; default 5120 ≈ the paper's compressed batch
	FullScanEvery int // every Nth weakTail also runs the LD.2 full-scan check (default 4)
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) defaults() {
	if c.Addresses == 0 {
		c.Addresses = 200
	}
	if c.K == 0 {
		c.K = 16
	}
	if c.Writers == 0 {
		c.Writers = 6
	}
	if c.PayloadBytes == 0 {
		c.PayloadBytes = 5120
	}
	if c.FullScanEvery == 0 {
		c.FullScanEvery = 4
	}
}

// Reverse encoding per the paper's S3 LogDrive: address 0 is
// lexicographically last, so LIST order is descending addresses and the
// first listed key is the highest written address.
const maxAddr = 99_999_999

func (c *Config) key(a int) string {
	return fmt.Sprintf("%slog/%08d", c.Prefix, maxAddr-a)
}

func (c *Config) addrOf(key string) (int, bool) {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n < 0 || n > maxAddr {
		return 0, false
	}
	return maxAddr - n, true
}

func Run(ctx context.Context, cfg *Config) report.GroupResult {
	cfg.defaults()
	g := report.GroupResult{Name: "lemmas", Clause: "LD.1/LD.2"}
	ld1, ld2 := workload(ctx, cfg)
	g.Add(ld1)
	g.Add(ld2)
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

// ---- ground truth ---------------------------------------------------------

type truth struct {
	mu     sync.Mutex
	issued []int64 // ns since workload start; 0 = not yet issued
	acked  []int64 // ns since workload start; 0 = not yet acked
	wm     int     // watermark: first address not yet acked (contiguous prefix)
}

func newTruth(n int) *truth {
	return &truth{issued: make([]int64, n), acked: make([]int64, n)}
}

func (t *truth) watermark() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.wm
}

func (t *truth) setIssued(a int, at int64) {
	t.mu.Lock()
	t.issued[a] = at
	t.mu.Unlock()
}

func (t *truth) setAcked(a int, at int64) {
	t.mu.Lock()
	t.acked[a] = at
	for t.wm < len(t.acked) && t.acked[t.wm] != 0 {
		t.wm++
	}
	t.mu.Unlock()
}

// lowerTail: first address not certainly written at time ts — every address
// below it was acked before ts, so the true contiguous tail T(ts) ≥ this.
func (t *truth) lowerTail(ts int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for a := range t.acked {
		if t.acked[a] == 0 || t.acked[a] > ts {
			return a
		}
	}
	return len(t.acked)
}

// upperTail: first address certainly unwritten at time ts (write not yet
// issued), so the true contiguous tail T(ts) ≤ this.
func (t *truth) upperTail(ts int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for a := range t.issued {
		if t.issued[a] == 0 || t.issued[a] > ts {
			return a
		}
	}
	return len(t.issued)
}

func (t *truth) ackTime(a int) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acked[a]
}

// ---- the workload ---------------------------------------------------------

func workload(ctx context.Context, cfg *Config) (report.CheckResult, report.CheckResult) {
	const ld1Name, ld2Name = "ld1_tail_range", "ld2_window_scan"

	payloads := make([][]byte, cfg.Addresses)
	for a := range payloads {
		p := make([]byte, cfg.PayloadBytes)
		cfg.Rng.Read(p)
		copy(p, []byte(fmt.Sprintf("addr-%08d-", a)))
		payloads[a] = p
	}

	tr := newTruth(cfg.Addresses)
	start := time.Now()
	clock := func() int64 { return time.Since(start).Nanoseconds() }

	var next atomic.Int64
	var writeErr atomic.Value
	writersDone := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < cfg.Writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st := cfg.store(w)
			for {
				s := int(next.Add(1) - 1)
				if s >= cfg.Addresses || writeErr.Load() != nil {
					return
				}
				// K-window discipline: issue s only once all ≤ s−K acked.
				for tr.watermark()+cfg.K <= s {
					if writeErr.Load() != nil || ctx.Err() != nil {
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
				tr.setIssued(s, clock())
				ok := false
				for attempt := 0; attempt < 6; attempt++ {
					// Retrying the same value is safe: single-value model.
					if _, out := st.Put(ctx, cfg.key(s), payloads[s]); out == store.OK {
						ok = true
						break
					}
					time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
				}
				if !ok {
					writeErr.CompareAndSwap(nil, fmt.Sprintf("address %d unwritable after 6 attempts", s))
					return
				}
				tr.setAcked(s, clock())
			}
		}(w)
	}
	go func() { wg.Wait(); close(writersDone) }()

	scanner := cfg.store(len(cfg.Stores) - 1) // last vantage: differs from writer 0 when multi-vantage
	var scans, fullScans, holesSeen, skipped int
	lastTobs := -1
	ld1Fail, ld2Fail := "", ""

	oneScan := func(withFull bool) {
		tStart := clock()
		nStar, holes, ok := weakTail(ctx, cfg, scanner)
		tStop := clock()
		if !ok {
			skipped++
			return
		}
		scans++
		holesSeen += len(holes)

		tObs := nStar
		for _, h := range holes {
			if h < tObs {
				tObs = h
			}
		}
		lb, ub := tr.lowerTail(tStart), tr.upperTail(tStop)
		if ld1Fail == "" && (tObs < lb || tObs > ub) {
			ld1Fail = fmt.Sprintf("scan %d observed contiguous tail %d outside the true tail range [%d,%d] for its interval (N*=%d, %d holes) — LD.1 violated",
				scans, tObs, lb, ub, nStar, len(holes))
		}
		if ld1Fail == "" && tObs < lastTobs {
			ld1Fail = fmt.Sprintf("scan %d observed tail %d after an earlier scan observed %d — observed tails regressed", scans, tObs, lastTobs)
		}
		if tObs > lastTobs {
			lastTobs = tObs
		}

		if withFull && ld2Fail == "" {
			fullScans++
			windowStart := max(0, nStar-cfg.K)
			for _, h := range holes {
				if h < windowStart || h >= nStar {
					ld2Fail = fmt.Sprintf("window scan reported hole %d outside its own window [%d,%d)", h, windowStart, nStar)
					return
				}
			}
			written, ok := fullScan(ctx, cfg, scanner)
			if !ok {
				return
			}
			for a := 0; a < windowStart; a++ {
				if !written[a] {
					ld2Fail = fmt.Sprintf("address %d below the window (start %d) read UNWRITTEN in a full scan — weakTail's implicit 'written' claim for the ignored prefix is false (address was acked at t=%dms); LD.2's window/full-scan equivalence fails",
						a, windowStart, tr.ackTime(a)/1e6)
					return
				}
			}
		}
	}

	running := true
	for running {
		select {
		case <-writersDone:
			running = false
		default:
			oneScan(scans%cfg.FullScanEvery == 0)
		}
	}
	if v := writeErr.Load(); v != nil {
		f := failNamed(ld1Name, "workload aborted: %s", v.(string))
		return f, failNamed(ld2Name, "workload aborted: %s", v.(string))
	}

	// Quiescent close: every address acked, so LB = UB = Addresses and the
	// final observation must be exact with no holes.
	tStart := clock()
	nStar, holes, ok := weakTail(ctx, cfg, scanner)
	if ok {
		scans++
		tObs := nStar
		for _, h := range holes {
			if h < tObs {
				tObs = h
			}
		}
		lb, ub := tr.lowerTail(tStart), tr.upperTail(clock())
		if ld1Fail == "" && (tObs != cfg.Addresses || lb != cfg.Addresses || ub != cfg.Addresses) {
			ld1Fail = fmt.Sprintf("quiescent scan after all %d writes acked observed tail %d with %d holes — must be exactly %d with none",
				cfg.Addresses, tObs, len(holes), cfg.Addresses)
		}
	}

	ld1 := passNamed(ld1Name, "%d writes under K=%d discipline by %d writers (%d B payloads); %d weakTails all within true tail ranges, observed tails monotone, quiescent tail exact (%d holes seen live, %d scans skipped on read errors)",
		cfg.Addresses, cfg.K, cfg.Writers, cfg.PayloadBytes, scans, holesSeen, skipped)
	if ld1Fail != "" {
		ld1 = failNamed(ld1Name, "%s", ld1Fail)
	}
	ld2 := passNamed(ld2Name, "%d window scans cross-checked against full scans: ignored prefix fully written every time, all holes inside the K-window",
		fullScans)
	if ld2Fail != "" {
		ld2 = failNamed(ld2Name, "%s", ld2Fail)
	}
	return ld1, ld2
}

// weakTail implements the paper's S3 LogDrive weakTail (§3.2, LD.2): one
// LIST for the highest written address (linearizable non-contiguous tail
// via reverse encoding), then an unordered scan of the K preceding
// addresses via point GETs. Returns N* and the observed hole set.
func weakTail(ctx context.Context, cfg *Config, st *store.Store) (int, []int, bool) {
	page, out := st.List(ctx, cfg.Prefix+"log/", "", 1)
	if out != store.OK {
		return 0, nil, false
	}
	if len(page.Keys) == 0 {
		return 0, nil, true
	}
	high, ok := cfg.addrOf(page.Keys[0])
	if !ok {
		return 0, nil, false
	}
	nStar := high + 1
	windowStart := max(0, nStar-cfg.K)

	type res struct {
		addr int
		out  store.Outcome
	}
	results := make(chan res, nStar-windowStart)
	var wg sync.WaitGroup
	for a := windowStart; a < nStar; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			_, _, out := st.Get(ctx, cfg.key(a))
			results <- res{a, out}
		}(a)
	}
	wg.Wait()
	close(results)

	var holes []int
	for r := range results {
		switch r.out {
		case store.OK:
		case store.NotFound:
			holes = append(holes, r.addr)
		default:
			return 0, nil, false // read error: scan is invalid, not a verdict
		}
	}
	return nStar, holes, true
}

// fullScan pages through the entire log prefix and returns the written set.
func fullScan(ctx context.Context, cfg *Config, st *store.Store) (map[int]bool, bool) {
	written := map[int]bool{}
	token := ""
	for range 10000 {
		page, out := st.List(ctx, cfg.Prefix+"log/", token, 0)
		if out != store.OK {
			return nil, false
		}
		for _, k := range page.Keys {
			if a, ok := cfg.addrOf(k); ok {
				written[a] = true
			}
		}
		if !page.Truncated {
			return written, true
		}
		token = page.NextToken
	}
	return nil, false
}

func passNamed(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func failNamed(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
