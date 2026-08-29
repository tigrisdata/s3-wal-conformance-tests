// Command conformance runs the shared-log contract conformance suite against
// any S3-compatible endpoint.
//
// Credentials come from the standard AWS chain (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY, shared config, etc.). Example:
//
//	conformance -endpoint https://t3.storage.dev -bucket my-test-bucket -groups c4
//
// Repeat -endpoint (optionally as label=url) for multi-vantage runs; a
// single-vantage run is stamped REGIONAL EVIDENCE ONLY in the report and
// cannot support a cross-region conformance claim.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	mrand "math/rand"
	"net/url"
	"os"
	"strings"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c4cas"
)

type endpointList []string

func (e *endpointList) String() string     { return strings.Join(*e, ",") }
func (e *endpointList) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	var endpoints endpointList
	flag.Var(&endpoints, "endpoint", "S3 endpoint URL, optionally label=url; repeat for multiple vantages")
	bucket := flag.String("bucket", "", "bucket to test (required; contents under the run prefix will be created and deleted)")
	groups := flag.String("groups", "c4", "comma-separated test groups (available: c4)")
	region := flag.String("region", "auto", "region string for the SDK (S3-compatible endpoints usually accept any)")
	seed := flag.Int64("seed", 0, "random seed; 0 derives one and prints it (every run is reproducible from its seed)")
	contenders := flag.Int("contenders", 8, "concurrent contenders per race")
	rounds := flag.Int("rounds", 20, "rounds per contended check")
	pathStyle := flag.Bool("path-style", false, "use path-style addressing")
	jsonOut := flag.String("json", "", "write the JSON report to this file")
	flag.Parse()

	if *bucket == "" || len(endpoints) == 0 {
		fmt.Fprintln(os.Stderr, "usage: conformance -endpoint URL -bucket NAME [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *seed == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			fatal("deriving seed: %v", err)
		}
		*seed = int64(binary.BigEndian.Uint64(b[:]) >> 1)
	}

	ctx := context.Background()
	rec := store.NewRecorder()

	var stores []*store.Store
	var urls, vantages []string
	for i, e := range endpoints {
		label, endpoint := splitEndpoint(e, i)
		st, err := store.New(ctx, store.Options{
			Endpoint:  endpoint,
			Region:    *region,
			Bucket:    *bucket,
			Vantage:   label,
			PathStyle: *pathStyle,
		}, rec)
		if err != nil {
			fatal("building client for %s: %v", endpoint, err)
		}
		stores = append(stores, st)
		urls = append(urls, endpoint)
		vantages = append(vantages, label)
	}

	rep := report.New(urls, vantages, *bucket, *seed)
	runPrefix := fmt.Sprintf("s3-wal-conformance/%d/", *seed)
	rng := mrand.New(mrand.NewSource(*seed))

	for _, g := range strings.Split(*groups, ",") {
		switch strings.TrimSpace(g) {
		case "c4":
			rep.Groups = append(rep.Groups, c4cas.Run(ctx, &c4cas.Config{
				Stores:     stores,
				Prefix:     runPrefix + "c4/",
				Contenders: *contenders,
				Rounds:     *rounds,
				Rng:        rng,
			}))
		case "":
		default:
			fatal("unknown group %q (available: c4)", g)
		}
	}

	rep.Latency = rec.Summary()
	fmt.Println(rep.Human())
	if *jsonOut != "" {
		data, err := rep.JSON()
		if err != nil {
			fatal("encoding report: %v", err)
		}
		if err := os.WriteFile(*jsonOut, data, 0o644); err != nil {
			fatal("writing report: %v", err)
		}
	}
	if !rep.AllPassed() {
		os.Exit(1)
	}
}

func splitEndpoint(v string, i int) (label, endpoint string) {
	if pre, rest, ok := strings.Cut(v, "="); ok && !strings.Contains(pre, "://") {
		return pre, rest
	}
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		return fmt.Sprintf("v%d-%s", i, u.Host), v
	}
	return fmt.Sprintf("v%d", i), v
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
