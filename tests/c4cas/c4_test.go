package c4cas

import (
	"testing"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

func bodies(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte{byte('a' + i)}
	}
	return out
}

func outcomes(o ...store.Outcome) []contenderResult {
	rs := make([]contenderResult, len(o))
	for i, x := range o {
		rs[i] = contenderResult{outcome: x}
	}
	return rs
}

func TestJudgeOutcomes(t *testing.T) {
	initial := []byte("init")
	b := bodies(3)
	ok, pf, amb := store.OK, store.PreconditionFailed, store.Ambiguous

	cases := []struct {
		name           string
		results        []contenderResult
		final          []byte
		initialAllowed bool
		wantConform    bool
	}{
		{"clean winner", outcomes(pf, ok, pf), b[1], true, true},
		{"winner but final is another contender (lost update)", outcomes(pf, ok, pf), b[0], true, false},
		{"winner but final is initial (mutation lost)", outcomes(pf, ok, pf), initial, true, false},
		{"two acknowledged winners", outcomes(ok, ok, pf), b[0], true, false},
		{"all 412 with no interference (no consensus progress)", outcomes(pf, pf, pf), initial, true, false},
		{"ambiguous resolved as the winner", outcomes(pf, amb, pf), b[1], true, true},
		{"ambiguous unresolved, state unchanged", outcomes(pf, amb, pf), initial, true, true},
		{"final belongs to a 412'd contender (412 mutated)", outcomes(pf, amb, pf), b[0], true, false},
		{"create race: ambiguous unresolved but key must hold a value", outcomes(pf, amb, pf), []byte("who-wrote-this"), false, false},
		{"create race: ambiguous resolved", outcomes(pf, amb, pf), b[1], false, true},
		{"unexpected outcome classified", outcomes(pf, store.NotFound, pf), initial, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var init []byte
			if c.initialAllowed {
				init = initial
			}
			res := judgeOutcomes("t", init, b, c.results, c.final, c.initialAllowed)
			if got := res == nil; got != c.wantConform {
				detail := ""
				if res != nil {
					detail = res.Details
				}
				t.Fatalf("conform=%v, want %v (%s)", got, c.wantConform, detail)
			}
		})
	}
}

func TestEpochRoundtrip(t *testing.T) {
	for _, n := range []int{0, 1, 41, 1 << 30} {
		got, ok := epochOf(epochBody(n))
		if !ok || got != n {
			t.Fatalf("epoch %d roundtripped to %d, ok=%v", n, got, ok)
		}
	}
	for _, bad := range [][]byte{nil, []byte("epoch:"), []byte("nonsense-14byte"), epochBody(1)[:10]} {
		if _, ok := epochOf(bad); ok {
			t.Fatalf("epochOf(%q) accepted invalid body", bad)
		}
	}
}
