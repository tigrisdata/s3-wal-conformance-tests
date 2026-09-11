package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapturingTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tigris-Served-From", "iad")
	}))
	defer srv.Close()

	client, err := buildHTTPClient("", "X-Tigris-Served-From")
	if err != nil {
		t.Fatal(err)
	}
	ctx, rc := ContextWithRegionCapture(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if rc.Value() != "iad" {
		t.Fatalf("captured %q, want iad", rc.Value())
	}

	// Without a capture in context, requests still work and capture nothing.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
}

func TestBuildHTTPClientProxyValidation(t *testing.T) {
	if _, err := buildHTTPClient("socks5://localhost:1080", ""); err != nil {
		t.Fatalf("socks5 proxy rejected: %v", err)
	}
	if _, err := buildHTTPClient("ftp://nope", ""); err == nil {
		t.Fatal("unsupported proxy scheme accepted")
	}
	if _, err := buildHTTPClient("://bad", ""); err == nil {
		t.Fatal("malformed proxy URL accepted")
	}
}

func TestRegionTally(t *testing.T) {
	var tally regionTally
	tally.add("iad")
	tally.add("iad")
	tally.add("sjc")
	tally.add("") // unobserved: never counted
	got := tally.snapshot()
	if got["iad"] != 2 || got["sjc"] != 1 || len(got) != 2 {
		t.Fatalf("tally = %v", got)
	}
}
