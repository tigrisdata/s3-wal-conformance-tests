package report

import "testing"

func TestFinalize(t *testing.T) {
	g := GroupResult{Name: "g", Clause: "C0"}
	g.Add(CheckResult{Name: "a", Status: Pass})
	g.Add(CheckResult{Name: "b", Status: Info})
	g.Finalize()
	if !g.Passed {
		t.Fatal("group with only pass+info checks must pass")
	}
	g.Add(CheckResult{Name: "c", Status: Fail})
	g.Finalize()
	if g.Passed {
		t.Fatal("group with a failed check must fail")
	}
}

func v(label string, regions ...string) VantageInfo {
	m := map[string]int64{}
	for _, r := range regions {
		m[r]++
	}
	if len(m) == 0 {
		m = nil
	}
	return VantageInfo{Label: label, Endpoint: "https://e", RegionsObserved: m}
}

func TestComputeScope(t *testing.T) {
	cases := []struct {
		name    string
		vs      []VantageInfo
		capture bool
		want    string
	}{
		{"single vantage", []VantageInfo{v("a", "iad")}, true, ScopeRegional},
		{"two vantages, two regions", []VantageInfo{v("a", "iad"), v("b", "sjc")}, true, ScopeMultiVerified},
		{"two vantages, one observed region", []VantageInfo{v("a", "sjc"), v("b", "sjc")}, true, ScopeMultiCollapsed},
		{"two vantages, capture disabled", []VantageInfo{v("a"), v("b")}, false, ScopeMultiUnproven},
		{"two vantages, header never present", []VantageInfo{v("a"), v("b")}, true, ScopeMultiUnproven},
		{"mixed: one vantage saw two regions", []VantageInfo{v("a", "iad", "sjc"), v("b", "sjc")}, true, ScopeMultiVerified},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeScope(c.vs, c.capture); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestReportScopeLifecycle(t *testing.T) {
	r := New([]VantageInfo{{Label: "a", Endpoint: "e1"}, {Label: "b", Endpoint: "e2"}}, "X-Region", "b", 1)
	if r.EvidenceScope != ScopeRegional {
		t.Fatalf("pre-finalize scope %q, want provisional %q", r.EvidenceScope, ScopeRegional)
	}
	r.Vantages[0].RegionsObserved = map[string]int64{"iad": 5}
	r.Vantages[1].RegionsObserved = map[string]int64{"sjc": 7}
	r.FinalizeScope()
	if r.EvidenceScope != ScopeMultiVerified {
		t.Fatalf("post-finalize scope %q, want %q", r.EvidenceScope, ScopeMultiVerified)
	}
}

func TestAllPassed(t *testing.T) {
	r := New([]VantageInfo{{Label: "a", Endpoint: "e"}}, "", "b", 1)
	g := GroupResult{Name: "g"}
	g.Add(CheckResult{Status: Pass})
	g.Finalize()
	r.Groups = append(r.Groups, g)
	if !r.AllPassed() {
		t.Fatal("all-pass report must report AllPassed")
	}
	bad := GroupResult{Name: "h"}
	bad.Add(CheckResult{Status: Fail})
	bad.Finalize()
	r.Groups = append(r.Groups, bad)
	if r.AllPassed() {
		t.Fatal("report with a failed group must not report AllPassed")
	}
}
