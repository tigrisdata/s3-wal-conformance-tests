package c2monotonic

import "testing"

func TestSeenTracker(t *testing.T) {
	tr := &seenTracker{seen: map[string]bool{}}
	if tr.has("k") {
		t.Fatal("fresh tracker must not have observed anything")
	}
	tr.mark("k")
	if !tr.has("k") {
		t.Fatal("marked key not reported as seen")
	}
	tr.mark("j")
	snap := tr.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d keys, want 2", len(snap))
	}
	// The snapshot is a copy: mutating tracked state afterwards must not
	// change it (the LIST check depends on a stable pre-scan snapshot).
	tr.mark("l")
	if len(snap) != 2 {
		t.Fatal("snapshot aliased live state")
	}
}
