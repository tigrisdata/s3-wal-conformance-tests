package main

import "testing"

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in            string
		wantLabel     string
		wantEndpoint  string
	}{
		{"iad=https://t3.storage.dev", "iad", "https://t3.storage.dev"},
		{"https://t3.storage.dev", "v0-t3.storage.dev", "https://t3.storage.dev"},
		{"not-a-url", "v0", "not-a-url"},
	}
	for _, c := range cases {
		label, endpoint := splitEndpoint(c.in, 0)
		if label != c.wantLabel || endpoint != c.wantEndpoint {
			t.Fatalf("splitEndpoint(%q) = (%q,%q), want (%q,%q)", c.in, label, endpoint, c.wantLabel, c.wantEndpoint)
		}
	}
}
