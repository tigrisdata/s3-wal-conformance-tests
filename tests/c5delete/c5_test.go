package c5delete

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tigrisdata/s3-wal-conformance-tests/internal/store"
)

func TestManifestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Stores:       []*store.Store{{Bucket: "unit-test-bucket"}},
		ArtifactsDir: dir,
	}
	if m, err := loadManifest(cfg); err != nil || m != nil {
		t.Fatalf("missing manifest should load as nil,nil; got %v,%v", m, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := appendManifest(cfg, []manifestEntry{{Key: "a", DeletedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := appendManifest(cfg, []manifestEntry{{Key: "b", DeletedAt: now}}); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(cfg)
	if err != nil || m == nil {
		t.Fatalf("load failed: %v", err)
	}
	if m.Bucket != "unit-test-bucket" || len(m.Entries) != 2 || m.Entries[0].Key != "a" || m.Entries[1].Key != "b" {
		t.Fatalf("manifest content wrong: %+v", m)
	}
	if got := cfg.manifestPath(); filepath.Dir(got) != dir {
		t.Fatalf("manifest path %q not under artifacts dir", got)
	}
}

func TestManifestCap(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Stores: []*store.Store{{Bucket: "b"}}, ArtifactsDir: dir}
	entries := make([]manifestEntry, 6000)
	for i := range entries {
		entries[i] = manifestEntry{Key: "k", DeletedAt: time.Now()}
	}
	if err := appendManifest(cfg, entries); err != nil {
		t.Fatal(err)
	}
	m, _ := loadManifest(cfg)
	if len(m.Entries) != 5000 {
		t.Fatalf("manifest not capped: %d entries", len(m.Entries))
	}
}

func TestDiagnoseTrim(t *testing.T) {
	all := []string{"log/0", "log/1", "log/2", "log/3"}
	if got := diagnoseTrim([]string{"log/1", "log/2", "log/3"}, all, 2); got == "" ||
		got[:7] != "trimmed" {
		t.Fatalf("resurfaced trimmed entry not diagnosed: %q", got)
	}
	if got := diagnoseTrim([]string{"log/2"}, all, 2); got != "surviving entries missing from the scan" {
		t.Fatalf("missing survivor misdiagnosed: %q", got)
	}
}
