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
	"time"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/report"
	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c1linear"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c2monotonic"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c3listing"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c4cas"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/c5delete"
	"github.com/tigrisdata/s3-wal-conformance-tests/tests/lemmas"
)

type endpointList []string

func (e *endpointList) String() string     { return strings.Join(*e, ",") }
func (e *endpointList) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	var endpoints, proxies endpointList
	flag.Var(&endpoints, "endpoint", "S3 endpoint URL, optionally label=url; repeat for multiple vantages")
	flag.Var(&proxies, "proxy", "route a vantage through a proxy, as label=proxy-url (socks5://, http://, https://); e.g. an `ssh -N -D` tunnel to a host in the target region")
	regionHeader := flag.String("region-header", "X-Tigris-Served-From", "response header naming the region that served each request; recorded per vantage and used to verify multi-vantage evidence (empty disables)")
	bucket := flag.String("bucket", "", "bucket to test (required; contents under the run prefix will be created and deleted)")
	groups := flag.String("groups", "c1,c2,c3,c4,c5,lemmas", "comma-separated test groups (available: c1, c2, c3, c4, c5, lemmas)")
	payload := flag.Int("payload", 5120, "lemmas: object size in bytes for the log workload (paper nominal ~5KB)")
	observe := flag.Duration("observe", 15*time.Second, "c5: observation window hammering deleted keys")
	artifacts := flag.String("artifacts", "conformance-artifacts", "directory for failure artifacts and the c5 cross-run manifest")
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

	proxyByLabel := map[string]string{}
	for _, p := range proxies {
		label, u, ok := strings.Cut(p, "=")
		if !ok {
			fatal("-proxy %q: want label=proxy-url", p)
		}
		proxyByLabel[label] = u
	}

	var stores []*store.Store
	var vantages []report.VantageInfo
	for i, e := range endpoints {
		label, endpoint := splitEndpoint(e, i)
		st, err := store.New(ctx, store.Options{
			Endpoint:     endpoint,
			Region:       *region,
			Bucket:       *bucket,
			Vantage:      label,
			PathStyle:    *pathStyle,
			ProxyURL:     proxyByLabel[label],
			RegionHeader: *regionHeader,
		}, rec)
		if err != nil {
			fatal("building client for %s: %v", endpoint, err)
		}
		delete(proxyByLabel, label)
		stores = append(stores, st)
		vantages = append(vantages, report.VantageInfo{Label: label, Endpoint: endpoint, Proxy: st.Proxy})
	}
	for label := range proxyByLabel {
		fatal("-proxy %q does not match any -endpoint label", label)
	}

	rep := report.New(vantages, *regionHeader, *bucket, *seed)
	runPrefix := fmt.Sprintf("s3-wal-conformance/%d/", *seed)
	rng := mrand.New(mrand.NewSource(*seed))

	for _, g := range strings.Split(*groups, ",") {
		switch strings.TrimSpace(g) {
		case "c1":
			rep.Groups = append(rep.Groups, c1linear.Run(ctx, &c1linear.Config{
				Stores:       stores,
				Prefix:       runPrefix + "c1/",
				Rng:          rng,
				Clients:      *contenders,
				ArtifactsDir: *artifacts,
			}))
		case "c2":
			rep.Groups = append(rep.Groups, c2monotonic.Run(ctx, &c2monotonic.Config{
				Stores: stores,
				Prefix: runPrefix + "c2/",
				Rng:    rng,
			}))
		case "c3":
			rep.Groups = append(rep.Groups, c3listing.Run(ctx, &c3listing.Config{
				Stores: stores,
				Prefix: runPrefix + "c3/",
				Rng:    rng,
			}))
		case "c4":
			rep.Groups = append(rep.Groups, c4cas.Run(ctx, &c4cas.Config{
				Stores:     stores,
				Prefix:     runPrefix + "c4/",
				Contenders: *contenders,
				Rounds:     *rounds,
				Rng:        rng,
			}))
		case "c5":
			rep.Groups = append(rep.Groups, c5delete.Run(ctx, &c5delete.Config{
				Stores:       stores,
				Prefix:       runPrefix + "c5/",
				Rng:          rng,
				ArtifactsDir: *artifacts,
				ObserveFor:   *observe,
			}))
		case "lemmas":
			rep.Groups = append(rep.Groups, lemmas.Run(ctx, &lemmas.Config{
				Stores:       stores,
				Prefix:       runPrefix + "lemmas/",
				Rng:          rng,
				Writers:      *contenders,
				PayloadBytes: *payload,
			}))
		case "":
		default:
			fatal("unknown group %q (available: c1, c2, c3, c4, c5, lemmas)", g)
		}
	}

	rep.Latency = rec.Summary()
	for i, st := range stores {
		rep.Vantages[i].RegionsObserved = st.RegionsObserved()
	}
	rep.FinalizeScope()
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
