package c3listing

import "testing"

func TestAscending(t *testing.T) {
	cases := []struct {
		name  string
		pages [][]string
		ok    bool
	}{
		{"empty", nil, true},
		{"single page ordered", [][]string{{"a", "b", "c"}}, true},
		{"ordered across pages", [][]string{{"a", "b"}, {"c"}, {"d", "e"}}, true},
		{"duplicate within page", [][]string{{"a", "a"}}, false},
		{"duplicate across pages", [][]string{{"a", "b"}, {"b", "c"}}, false},
		{"regression within page", [][]string{{"b", "a"}}, false},
		{"regression across pages", [][]string{{"a", "c"}, {"b"}}, false},
		{"empty middle page ok", [][]string{{"a"}, {}, {"b"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ascending(c.pages) == ""; got != c.ok {
				t.Fatalf("ascending=%v, want %v (%s)", got, c.ok, ascending(c.pages))
			}
		})
	}
}

func TestStableKeySpacing(t *testing.T) {
	// Churn keys land in the gaps: stable indices are multiples of 10, churn
	// uses offsets 1-8, so a churn key must sort strictly between neighbors
	// and never collide with a stable key.
	a, b := stableKey("x/", 3), stableKey("x/", 4)
	churn := "x/k-000031-c0-0001"
	if !(a < churn && churn < b) {
		t.Fatalf("churn key %q does not sort between %q and %q", churn, a, b)
	}
}

func TestFlatten(t *testing.T) {
	got := flatten([][]string{{"a"}, nil, {"b", "c"}})
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("flatten returned %v", got)
	}
}
