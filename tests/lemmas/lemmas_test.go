package lemmas

import "testing"

func TestAddressEncoding(t *testing.T) {
	cfg := &Config{Prefix: "p/"}
	for _, a := range []int{0, 1, 199, maxAddr} {
		got, ok := cfg.addrOf(cfg.key(a))
		if !ok || got != a {
			t.Fatalf("address %d roundtripped to %d, ok=%v", a, got, ok)
		}
	}
	// Reverse encoding: higher addresses sort lexicographically FIRST, so a
	// single ascending LIST yields the highest written address up front.
	if !(cfg.key(5) < cfg.key(4) && cfg.key(100) < cfg.key(99)) {
		t.Fatal("higher addresses must sort before lower ones")
	}
	if _, ok := cfg.addrOf("p/log-without-slashes"); ok {
		t.Fatal("addrOf accepted a malformed key")
	}
	if _, ok := cfg.addrOf("p/log/notanumber"); ok {
		t.Fatal("addrOf accepted a non-numeric key")
	}
}

func TestTruthBounds(t *testing.T) {
	tr := newTruth(5)

	// Nothing issued: everything certainly unwritten, tail bounds at 0.
	if lb, ub := tr.lowerTail(100), tr.upperTail(100); lb != 0 || ub != 0 {
		t.Fatalf("empty truth: lb=%d ub=%d, want 0,0", lb, ub)
	}

	tr.setIssued(0, 10)
	tr.setAcked(0, 20)
	tr.setIssued(1, 15) // in flight, never acked

	// At t=5 nothing had been issued: upper bound 0. At t=12, address 0 was
	// issued but 1 was not: upper bound 1. At t=30 both issued: upper 2.
	for _, c := range []struct{ ts, want int64 }{{5, 0}, {12, 1}, {30, 2}} {
		if got := tr.upperTail(c.ts); int64(got) != c.want {
			t.Fatalf("upperTail(%d)=%d, want %d", c.ts, got, c.want)
		}
	}
	// At t=15 address 0 was not yet acked: lower bound 0. At t=25 it was:
	// lower bound 1 (address 1 unacked).
	for _, c := range []struct{ ts, want int64 }{{15, 0}, {25, 1}} {
		if got := tr.lowerTail(c.ts); int64(got) != c.want {
			t.Fatalf("lowerTail(%d)=%d, want %d", c.ts, got, c.want)
		}
	}
}

func TestWatermarkContiguity(t *testing.T) {
	tr := newTruth(4)
	tr.setAcked(1, 10) // out of order: 0 not acked yet
	if w := tr.watermark(); w != 0 {
		t.Fatalf("watermark advanced past an unacked address: %d", w)
	}
	tr.setAcked(0, 12)
	if w := tr.watermark(); w != 2 {
		t.Fatalf("watermark=%d after 0 and 1 acked, want 2", w)
	}
	tr.setAcked(3, 14)
	if w := tr.watermark(); w != 2 {
		t.Fatalf("watermark jumped a hole: %d", w)
	}
	tr.setAcked(2, 16)
	if w := tr.watermark(); w != 4 {
		t.Fatalf("watermark=%d after all acked, want 4", w)
	}
}
