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

func TestEvidenceScope(t *testing.T) {
	single := New([]string{"e1"}, []string{"v1"}, "b", 1)
	if single.EvidenceScope != ScopeRegional {
		t.Fatalf("single vantage scope %q, want %q", single.EvidenceScope, ScopeRegional)
	}
	multi := New([]string{"e1", "e2"}, []string{"v1", "v2"}, "b", 1)
	if multi.EvidenceScope != ScopeMulti {
		t.Fatalf("multi vantage scope %q, want %q", multi.EvidenceScope, ScopeMulti)
	}
}

func TestAllPassed(t *testing.T) {
	r := New([]string{"e"}, []string{"v"}, "b", 1)
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
