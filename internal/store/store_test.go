package store

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestClassifyWrite(t *testing.T) {
	if got := classifyWrite(nil); got != OK {
		t.Fatalf("nil error classified %s, want %s", got, OK)
	}
	// A definite 412 surfaced as a smithy API error.
	pf := &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "precondition failed"}
	if got := classifyWrite(pf); got != PreconditionFailed {
		t.Fatalf("412 API error classified %s, want %s", got, PreconditionFailed)
	}
	// Transport-level failures are ambiguous: the write may have applied.
	for _, err := range []error{
		errors.New("connection reset by peer"),
		context.DeadlineExceeded,
	} {
		if got := classifyWrite(err); got != Ambiguous {
			t.Fatalf("%v classified %s, want %s", err, got, Ambiguous)
		}
	}
}

func TestClassifyRead(t *testing.T) {
	if got := classifyRead(nil); got != OK {
		t.Fatalf("nil error classified %s, want %s", got, OK)
	}
	if got := classifyRead(&types.NoSuchKey{}); got != NotFound {
		t.Fatalf("NoSuchKey classified %s, want %s", got, NotFound)
	}
	if got := classifyRead(errors.New("boom")); got != Failed {
		t.Fatalf("generic read error classified %s, want %s", got, Failed)
	}
}

func TestRecorderSummary(t *testing.T) {
	r := NewRecorder()
	for i := 0; i < 100; i++ {
		r.Observe("PUT", 1000000) // 1ms
	}
	s := r.Summary()["PUT"]
	if s.Count != 100 {
		t.Fatalf("count=%d, want 100", s.Count)
	}
	if s.P50us < 900 || s.P50us > 1100 {
		t.Fatalf("p50=%dµs, want ~1000", s.P50us)
	}
}
