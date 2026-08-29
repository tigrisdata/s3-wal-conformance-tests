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
// single vantage point cannot support a cross-region conformance verdict.
const (
	ScopeRegional = "REGIONAL EVIDENCE ONLY"
	ScopeMulti    = "MULTI-VANTAGE"
)

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
	SuiteVersion  string                           `json:"suite_version"`
	StartedAt     time.Time                        `json:"started_at"`
	Endpoints     []string                         `json:"endpoints"`
	Vantages      []string                         `json:"vantages"`
	EvidenceScope string                           `json:"evidence_scope"`
	Bucket        string                           `json:"bucket"`
	Seed          int64                            `json:"seed"`
	Groups        []GroupResult                    `json:"groups"`
	Latency       map[string]store.LatencySummary  `json:"latency_us"`
}

func New(endpoints, vantages []string, bucket string, seed int64) *Report {
	scope := ScopeRegional
	if len(vantages) > 1 {
		scope = ScopeMulti
	}
	return &Report{
		SuiteVersion:  SuiteVersion,
		StartedAt:     time.Now().UTC(),
		Endpoints:     endpoints,
		Vantages:      vantages,
		EvidenceScope: scope,
		Bucket:        bucket,
		Seed:          seed,
	}
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
	fmt.Fprintf(&b, "s3-wal-conformance %s  bucket=%s  seed=%d  scope=%s\n",
		r.SuiteVersion, r.Bucket, r.Seed, r.EvidenceScope)
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
