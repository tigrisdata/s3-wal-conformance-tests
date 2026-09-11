// Package c1linear tests contract clause C1: per-key linearizable read/write.
//
// Concurrent clients issue randomized GET/PUT/DELETE against a small hot
// keyspace (contention is the point). Every operation is recorded with
// invocation/response timestamps and values, and each key's history is
// checked against a register model with the porcupine linearizability
// checker. Ambiguous writes (timeouts, transport failures) stay in the
// history with an unbounded return time, per standard practice: the checker
// is free to linearize them anywhere after invocation, including "never
// observed before the end of the test".
package c1linear

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anishathalye/porcupine"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

type Config struct {
	Stores []*store.Store
	Prefix string
	Rng    *rand.Rand

	Clients      int    // concurrent clients (default 8)
	OpsPerClient int    // operations per client (default 60)
	HotKeys      int    // keyspace size; small = contended (default 5)
	ArtifactsDir string // where failing histories are written (default "conformance-artifacts")
}

func (c *Config) store(i int) *store.Store { return c.Stores[i%len(c.Stores)] }

func (c *Config) defaults() {
	if c.Clients == 0 {
		c.Clients = 8
	}
	if c.OpsPerClient == 0 {
		c.OpsPerClient = 60
	}
	if c.HotKeys == 0 {
		c.HotKeys = 5
	}
	if c.ArtifactsDir == "" {
		c.ArtifactsDir = "conformance-artifacts"
	}
}

type opKind uint8

const (
	opGet opKind = iota
	opPut
	opDelete
)

type input struct {
	Kind  opKind
	Value string // for PUT
}

type output struct {
	Present bool
	Value   string
}

type regState struct {
	Present bool
	Value   string
}

// registerModel is a per-key read/write/delete register.
var registerModel = porcupine.Model{
	Init: func() any { return regState{} },
	Step: func(state, in, out any) (bool, any) {
		st := state.(regState)
		i := in.(input)
		switch i.Kind {
		case opPut:
			return true, regState{Present: true, Value: i.Value}
		case opDelete:
			return true, regState{}
		default: // GET
			o := out.(output)
			ok := o.Present == st.Present && (!o.Present || o.Value == st.Value)
			return ok, st
		}
	},
}

type recorded struct {
	key       string
	client    int
	region    string // serving region, when the store captures one
	in        input
	out       output
	call      int64
	ret       int64
	unbounded bool // ambiguous write: return time becomes end-of-history
}

func Run(ctx context.Context, cfg *Config) report.GroupResult {
	cfg.defaults()
	g := report.GroupResult{Name: "c1_linearizability", Clause: "C1"}
	g.Add(linearizableRegisters(ctx, cfg))
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)
	g.Finalize()
	return g
}

func linearizableRegisters(ctx context.Context, cfg *Config) report.CheckResult {
	name := "linearizable_registers"
	keys := make([]string, cfg.HotKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("%sreg-%02d", cfg.Prefix, i)
	}
	// The model's initial state is "absent": make it true even if a prior
	// aborted run left keys behind under the same seed-derived prefix.
	cfg.store(0).DeletePrefix(ctx, cfg.Prefix)

	start := time.Now()
	clock := func() int64 { return time.Since(start).Nanoseconds() }

	var mu sync.Mutex
	var history []recorded
	record := func(r recorded) {
		mu.Lock()
		history = append(history, r)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for c := 0; c < cfg.Clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(cfg.Rng.Int63() + int64(c)))
			st := cfg.store(c)
			for n := 0; n < cfg.OpsPerClient; n++ {
				key := keys[rng.Intn(len(keys))]
				opCtx, capture := store.ContextWithRegionCapture(ctx)
				r := recorded{key: key, client: c, call: clock()}
				switch p := rng.Intn(100); {
				case p < 40: // GET
					body, _, out := st.Get(opCtx, key)
					r.ret = clock()
					r.in = input{Kind: opGet}
					switch out {
					case store.OK:
						r.out = output{Present: true, Value: string(body)}
					case store.NotFound:
						r.out = output{Present: false}
					default:
						continue // failed read: no effect, no information
					}
				case p < 80: // PUT
					v := fmt.Sprintf("c%d-%d", c, n)
					_, out := st.Put(opCtx, key, []byte(v))
					r.ret = clock()
					r.in = input{Kind: opPut, Value: v}
					switch out {
					case store.OK:
					case store.Ambiguous:
						r.unbounded = true
					default:
						continue // definite failure: not applied
					}
				default: // DELETE
					out := st.Delete(opCtx, key)
					r.ret = clock()
					r.in = input{Kind: opDelete}
					switch out {
					case store.OK:
					case store.Ambiguous:
						r.unbounded = true
					default:
						continue
					}
				}
				r.region = capture.Value()
				record(r)
			}
		}(c)
	}
	wg.Wait()

	// Ambiguous writes may take effect at any time: give them a return time
	// after every other operation so the checker can place them freely.
	var maxRet int64
	for _, r := range history {
		if !r.unbounded && r.ret > maxRet {
			maxRet = r.ret
		}
	}
	perKey := map[string][]recorded{}
	ambiguous := 0
	for i := range history {
		if history[i].unbounded {
			history[i].ret = maxRet + 1
			ambiguous++
		}
		perKey[history[i].key] = append(perKey[history[i].key], history[i])
	}

	total := 0
	for key, recs := range perKey {
		total += len(recs)
		ops := make([]porcupine.Operation, len(recs))
		for i, r := range recs {
			ops[i] = porcupine.Operation{
				ClientId: r.client,
				Input:    r.in,
				Output:   r.out,
				Call:     r.call,
				Return:   r.ret,
			}
		}
		res, info := porcupine.CheckOperationsVerbose(registerModel, ops, 30*time.Second)
		switch res {
		case porcupine.Ok:
		case porcupine.Unknown:
			return fail(name, "key %s: checker inconclusive after 30s on %d ops — not a violation; rerun with a smaller workload before drawing conclusions", key, len(ops))
		default:
			artifacts := dumpFailure(cfg, key, recs, info)
			return fail(name, "key %s: history of %d ops is NOT linearizable — no total order consistent with real-time precedence explains the observed reads (%s; reproduce with this run's seed)", key, len(ops), artifacts)
		}
	}
	return pass(name, "%d ops by %d clients over %d hot keys (%d ambiguous writes): every per-key history linearizable",
		total, cfg.Clients, cfg.HotKeys, ambiguous)
}

// dumpFailure writes the failing history as JSON (each op annotated with the
// serving region when captured) and as porcupine's interactive HTML
// visualization; returns a description of what was written.
func dumpFailure(cfg *Config, key string, recs []recorded, info porcupine.LinearizationInfo) string {
	if err := os.MkdirAll(cfg.ArtifactsDir, 0o755); err != nil {
		return fmt.Sprintf("could not write artifacts: %v", err)
	}
	base := filepath.Join(cfg.ArtifactsDir, "c1-"+filepath.Base(key))

	type jsonOp struct {
		Client  int    `json:"client"`
		Kind    string `json:"kind"`
		Value   string `json:"value,omitempty"`
		Present *bool  `json:"present,omitempty"`
		Read    string `json:"read,omitempty"`
		Region  string `json:"served_from,omitempty"`
		CallNs  int64  `json:"call_ns"`
		RetNs   int64  `json:"return_ns"`
	}
	kinds := map[opKind]string{opGet: "get", opPut: "put", opDelete: "delete"}
	dump := make([]jsonOp, 0, len(recs))
	for _, r := range recs {
		j := jsonOp{Client: r.client, Kind: kinds[r.in.Kind], Value: r.in.Value, Region: r.region, CallNs: r.call, RetNs: r.ret}
		if r.in.Kind == opGet {
			j.Present = &r.out.Present
			j.Read = r.out.Value
		}
		dump = append(dump, j)
	}
	if data, err := json.MarshalIndent(dump, "", " "); err == nil {
		_ = os.WriteFile(base+".json", data, 0o644)
	}
	viz := "visualization failed"
	if f, err := os.Create(base + ".html"); err == nil {
		if porcupine.Visualize(registerModel, info, f) == nil {
			viz = base + ".html"
		}
		f.Close()
	}
	return fmt.Sprintf("history: %s.json, visualization: %s", base, viz)
}

func pass(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Pass, Details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) report.CheckResult {
	return report.CheckResult{Name: name, Status: report.Fail, Details: fmt.Sprintf(format, args...)}
}
