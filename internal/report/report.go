// Package report defines the suite's output: binary correctness verdicts per
// contract clause, plus observed latency distributions (reported, not judged).
package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

const SuiteVersion = "0.1.0-dev"

// Evidence scope: the contract's clauses are global claims, so a run from a
// single vantage point cannot support a cross-region conformance verdict —
// and a second endpoint URL is not a second vantage unless it demonstrably
// routed elsewhere (anycast fronts collapse hostnames onto one region).
const (
	ScopeRegional       = "REGIONAL EVIDENCE ONLY"
	ScopeMultiVerified  = "MULTI-VANTAGE (verified)"
	ScopeMultiUnproven  = "MULTI-VANTAGE (unverified: no serving-region evidence)"
	ScopeMultiCollapsed = "REGIONAL EVIDENCE ONLY (multiple endpoints, but every vantage was served from one region)"
)

// VantageInfo records one vantage and the serving-region evidence behind it.
type VantageInfo struct {
	Label           string           `json:"label"`
	Endpoint        string           `json:"endpoint"`
	Proxy           string           `json:"proxy,omitempty"`
	RegionsObserved map[string]int64 `json:"regions_observed,omitempty"`
}

// ComputeScope derives the evidence stamp from what was actually observed.
// captureEnabled says whether a serving-region header was being recorded.
func ComputeScope(vantages []VantageInfo, captureEnabled bool) string {
	if len(vantages) < 2 {
		return ScopeRegional
	}
	if !captureEnabled {
		return ScopeMultiUnproven
	}
	distinct := map[string]bool{}
	for _, v := range vantages {
		for r := range v.RegionsObserved {
			distinct[r] = true
		}
	}
	switch {
	case len(distinct) >= 2:
		return ScopeMultiVerified
	case len(distinct) == 1:
		return ScopeMultiCollapsed
	default:
		// Capture was on but the provider never sent the header.
		return ScopeMultiUnproven
	}
}

type CheckStatus string

const (
	Pass CheckStatus = "pass"
	Fail CheckStatus = "fail"
	// Info checks report a property without judging it (e.g. which ETag
	// freshness property the store provides, per spec C4).
	Info CheckStatus = "info"
)

type CheckResult struct {
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Details string      `json:"details,omitempty"`
}

type GroupResult struct {
	Name   string        `json:"name"`
	Clause string        `json:"clause"`
	Passed bool          `json:"passed"`
	Checks []CheckResult `json:"checks"`
}

func (g *GroupResult) Add(c CheckResult) {
	g.Checks = append(g.Checks, c)
}

// Finalize computes Passed: every non-info check passed.
func (g *GroupResult) Finalize() {
	g.Passed = true
	for _, c := range g.Checks {
		if c.Status == Fail {
			g.Passed = false
		}
	}
}

type Report struct {
	SuiteVersion  string                          `json:"suite_version"`
	StartedAt     time.Time                       `json:"started_at"`
	Vantages      []VantageInfo                   `json:"vantages"`
	EvidenceScope string                          `json:"evidence_scope"`
	RegionHeader  string                          `json:"region_header,omitempty"`
	Bucket        string                          `json:"bucket"`
	Seed          int64                           `json:"seed"`
	Groups        []GroupResult                   `json:"groups"`
	Latency       map[string]store.LatencySummary `json:"latency_us"`
}

func New(vantages []VantageInfo, regionHeader, bucket string, seed int64) *Report {
	return &Report{
		SuiteVersion:  SuiteVersion,
		StartedAt:     time.Now().UTC(),
		Vantages:      vantages,
		RegionHeader:  regionHeader,
		EvidenceScope: ScopeRegional, // finalized after the run via FinalizeScope
		Bucket:        bucket,
		Seed:          seed,
	}
}

// FinalizeScope recomputes the evidence stamp from post-run vantage
// observations (call after updating Vantages[i].RegionsObserved).
func (r *Report) FinalizeScope() {
	r.EvidenceScope = ComputeScope(r.Vantages, r.RegionHeader != "")
}

func (r *Report) AllPassed() bool {
	for _, g := range r.Groups {
		if !g.Passed {
			return false
		}
	}
	return true
}

func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Human renders the terminal summary.
func (r *Report) Human() string {
	var b strings.Builder
	fmt.Fprintf(&b, "s3-wal-conformance %s  bucket=%s  seed=%d\nscope: %s\n",
		r.SuiteVersion, r.Bucket, r.Seed, r.EvidenceScope)
	for _, v := range r.Vantages {
		fmt.Fprintf(&b, "vantage %-10s %s", v.Label, v.Endpoint)
		if v.Proxy != "" {
			fmt.Fprintf(&b, " via %s", v.Proxy)
		}
		if len(v.RegionsObserved) > 0 {
			fmt.Fprintf(&b, "  served-from:")
			for region, n := range v.RegionsObserved {
				fmt.Fprintf(&b, " %s×%d", region, n)
			}
		}
		b.WriteString("\n")
	}
	for _, g := range r.Groups {
		verdict := "PASS"
		if !g.Passed {
			verdict = "FAIL"
		}
		fmt.Fprintf(&b, "\n[%s] %s (%s)\n", verdict, g.Name, g.Clause)
		for _, c := range g.Checks {
			mark := map[CheckStatus]string{Pass: "  ✓", Fail: "  ✗", Info: "  i"}[c.Status]
			fmt.Fprintf(&b, "%s %s", mark, c.Name)
			if c.Details != "" {
				fmt.Fprintf(&b, " — %s", c.Details)
			}
			b.WriteString("\n")
		}
	}
	if len(r.Latency) > 0 {
		fmt.Fprintf(&b, "\nlatency (µs)          count      p50      p95      p99    p99.9      max\n")
		for op, s := range r.Latency {
			fmt.Fprintf(&b, "%-16s %10d %8d %8d %8d %8d %8d\n",
				op, s.Count, s.P50us, s.P95us, s.P99us, s.P999us, s.MaxUs)
		}
	}
	return b.String()
}
