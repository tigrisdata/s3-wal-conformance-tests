package c1linear

import (
	"testing"

	"github.com/anishathalye/porcupine"
)

// step drives the register model one operation at a time.
func step(t *testing.T, state any, in input, out output) (bool, any) {
	t.Helper()
	ok, next := registerModel.Step(state, in, out)
	return ok, next
}

func TestRegisterModel(t *testing.T) {
	s0 := registerModel.Init()

	// Reading the initial state: absent is legal, any value is not.
	if ok, _ := step(t, s0, input{Kind: opGet}, output{Present: false}); !ok {
		t.Fatal("reading absent on fresh state must be legal")
	}
	if ok, _ := step(t, s0, input{Kind: opGet}, output{Present: true, Value: "x"}); ok {
		t.Fatal("reading a value on fresh state must be illegal")
	}

	// PUT then GET.
	_, s1 := step(t, s0, input{Kind: opPut, Value: "v1"}, output{})
	if ok, _ := step(t, s1, input{Kind: opGet}, output{Present: true, Value: "v1"}); !ok {
		t.Fatal("reading the written value must be legal")
	}
	if ok, _ := step(t, s1, input{Kind: opGet}, output{Present: true, Value: "v2"}); ok {
		t.Fatal("reading a different value must be illegal")
	}
	if ok, _ := step(t, s1, input{Kind: opGet}, output{Present: false}); ok {
		t.Fatal("reading absent after a put must be illegal")
	}

	// DELETE then GET.
	_, s2 := step(t, s1, input{Kind: opDelete}, output{})
	if ok, _ := step(t, s2, input{Kind: opGet}, output{Present: false}); !ok {
		t.Fatal("reading absent after delete must be legal")
	}
	if ok, _ := step(t, s2, input{Kind: opGet}, output{Present: true, Value: "v1"}); ok {
		t.Fatal("reading the deleted value must be illegal")
	}
}

// End-to-end sanity: porcupine accepts a legal history and rejects the
// read-after-delete shape the suite caught live (GET returns a value whose
// DELETE completed before the GET was invoked).
func TestCheckerOnKnownHistories(t *testing.T) {
	legal := []porcupine.Operation{
		{ClientId: 0, Input: input{Kind: opPut, Value: "a"}, Output: output{}, Call: 0, Return: 10},
		{ClientId: 1, Input: input{Kind: opGet}, Output: output{Present: true, Value: "a"}, Call: 11, Return: 20},
		{ClientId: 0, Input: input{Kind: opDelete}, Output: output{}, Call: 21, Return: 30},
		{ClientId: 1, Input: input{Kind: opGet}, Output: output{Present: false}, Call: 31, Return: 40},
	}
	if res := porcupine.CheckOperations(registerModel, legal); res != true {
		t.Fatal("legal history rejected")
	}

	staleReadAfterDelete := []porcupine.Operation{
		{ClientId: 0, Input: input{Kind: opPut, Value: "a"}, Output: output{}, Call: 0, Return: 10},
		{ClientId: 0, Input: input{Kind: opDelete}, Output: output{}, Call: 11, Return: 20},
		// Invoked after the delete completed, yet reads the deleted value.
		{ClientId: 1, Input: input{Kind: opGet}, Output: output{Present: true, Value: "a"}, Call: 21, Return: 30},
	}
	if res := porcupine.CheckOperations(registerModel, staleReadAfterDelete); res != false {
		t.Fatal("read-after-delete violation accepted")
	}
}
