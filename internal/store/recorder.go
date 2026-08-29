package store

import (
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// Recorder captures per-operation latency distributions. Latency is reported,
// never judged: nothing here feeds a pass/fail verdict.
type Recorder struct {
	mu    sync.Mutex
	hists map[string]*hdrhistogram.Histogram
}

func NewRecorder() *Recorder {
	return &Recorder{hists: make(map[string]*hdrhistogram.Histogram)}
}

func (r *Recorder) Observe(op string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hists[op]
	if !ok {
		// 1µs .. 5min, 3 significant digits.
		h = hdrhistogram.New(1, int64(5*time.Minute/time.Microsecond), 3)
		r.hists[op] = h
	}
	// Out-of-range values are clamped by RecordValue's error, which we ignore:
	// a >5min observation still counts at the cap.
	_ = h.RecordValue(min(d.Microseconds(), h.HighestTrackableValue()))
}

// LatencySummary is the published shape: microsecond quantiles per operation.
type LatencySummary struct {
	Count int64   `json:"count"`
	P50us int64   `json:"p50_us"`
	P95us int64   `json:"p95_us"`
	P99us int64   `json:"p99_us"`
	P999us int64  `json:"p999_us"`
	MaxUs int64   `json:"max_us"`
}

func (r *Recorder) Summary() map[string]LatencySummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]LatencySummary, len(r.hists))
	for op, h := range r.hists {
		out[op] = LatencySummary{
			Count:  h.TotalCount(),
			P50us:  h.ValueAtQuantile(50),
			P95us:  h.ValueAtQuantile(95),
			P99us:  h.ValueAtQuantile(99),
			P999us: h.ValueAtQuantile(99.9),
			MaxUs:  h.Max(),
		}
	}
	return out
}
