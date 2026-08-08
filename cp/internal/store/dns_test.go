package store

import "testing"

func TestIsApexDomain(t *testing.T) {
	// The distinction matters: a CNAME is illegal at the apex, so getting this
	// wrong would have us suggest a record the registrar refuses.
	for _, tc := range []struct {
		domain string
		apex   bool
	}{
		{"example.com", true},
		{"example.co", true},
		{"app.example.com", false},
		{"a.b.example.com", false},
		// Trailing dots are legal in DNS input and must not change the answer.
		{"example.com.", true},
		{"app.example.com.", false},
	} {
		if got := isApexDomain(tc.domain); got != tc.apex {
			t.Errorf("isApexDomain(%q) = %v, want %v", tc.domain, got, tc.apex)
		}
	}
}

func TestApexOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"app.example.com", "example.com"},
		{"example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
	} {
		if got := apexOf(tc.in); got != tc.want {
			t.Errorf("apexOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPublicHostStripsTheMeshPort(t *testing.T) {
	// The stored endpoint is ip:port where the port is the agent's mesh
	// handshake port — pasting it into a DNS record would be nonsense.
	ep := "203.0.113.10:51820"
	if got := publicHost(&ep); got != "203.0.113.10" {
		t.Fatalf("publicHost = %q, want the bare address", got)
	}
	bare := "203.0.113.10"
	if got := publicHost(&bare); got != bare {
		t.Fatalf("publicHost(%q) = %q", bare, got)
	}
	v6 := "[2001:db8::1]:51820"
	if got := publicHost(&v6); got != "2001:db8::1" {
		t.Fatalf("publicHost(v6) = %q", got)
	}
	if got := publicHost(nil); got != "" {
		t.Fatalf("publicHost(nil) = %q, want empty", got)
	}
	empty := ""
	if got := publicHost(&empty); got != "" {
		t.Fatalf("publicHost(empty) = %q", got)
	}
}
