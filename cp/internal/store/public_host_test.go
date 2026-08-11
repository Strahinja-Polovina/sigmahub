package store

import "testing"

// SIGMA-351. Pure functions, so they are unit-tested here rather than in the
// integration package — nothing below touches a database.

func TestPublicLabelIsAlwaysDNSShaped(t *testing.T) {
	cases := []struct {
		name, kind, id, want string
	}{
		{"Shop API", "app", "res_1a2b3c4d5e6f", "shop-api-3c4d5e6f"},
		{"shop", "app", "res_1a2b3c4d5e6f", "shop-3c4d5e6f"},
		// Punctuation collapses to single hyphens and never leads or trails.
		{"  My!!App  ", "app", "res_1a2b3c4d5e6f", "my-app-3c4d5e6f"},
		// A name with no ASCII letters reduces to nothing, so the kind carries it
		// rather than producing a hostname that starts with a hyphen.
		{"日本語", "postgres", "res_1a2b3c4d5e6f", "postgres-3c4d5e6f"},
		{"...", "", "res_1a2b3c4d5e6f", "app-3c4d5e6f"},
	}
	for _, c := range cases {
		if got := PublicLabel(c.name, c.kind, c.id); got != c.want {
			t.Errorf("PublicLabel(%q, %q) = %q, want %q", c.name, c.kind, got, c.want)
		}
	}

	// The stem is capped so the whole label stays well inside a DNS label's 63
	// bytes, and the cap must not leave a trailing hyphen behind.
	long := PublicLabel(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbbbbbb", "app", "res_1a2b3c4d5e6f")
	if len(long) > 63 {
		t.Errorf("label %q is %d bytes, over the 63-byte DNS label limit", long, len(long))
	}
	if long[publicLabelStemMax-1] == '-' && long[publicLabelStemMax] == '-' {
		t.Errorf("label %q has a doubled hyphen at the cap", long)
	}
}

func TestPublicHostPrefersTheConfiguredWildcard(t *testing.T) {
	got := PublicHost("shop-3c4d5e6f", "apps.example.com", "203.0.113.7:22")
	if want := "shop-3c4d5e6f.apps.example.com"; got != want {
		t.Fatalf("PublicHost = %q, want %q", got, want)
	}
	// A trailing dot on the configured domain is an operator typo, not a
	// different domain.
	if got := PublicHost("shop-3c4d5e6f", "apps.example.com.", ""); got != "shop-3c4d5e6f.apps.example.com" {
		t.Errorf("a trailing dot changed the host: %q", got)
	}
}

func TestPublicHostFallsBackToSslip(t *testing.T) {
	// No wildcard configured: a fresh self-hosted install still gets a URL on its
	// first deploy, which is the whole point of the fallback.
	got := PublicHost("shop-3c4d5e6f", "", "203.0.113.7:22")
	if want := "shop-3c4d5e6f-203-0-113-7.sslip.io"; got != want {
		t.Fatalf("PublicHost = %q, want %q", got, want)
	}
	// A bare address, no port.
	if got := PublicHost("shop-3c4d5e6f", "", "203.0.113.7"); got != "shop-3c4d5e6f-203-0-113-7.sslip.io" {
		t.Errorf("bare address produced %q", got)
	}
}

func TestPublicHostRefusesAnAddressNobodyCanReach(t *testing.T) {
	// Answering with a hostname that resolves to a private or absent address
	// would put a URL on screen that silently never connects — the exact failure
	// this change exists to end, re-created one layer down.
	for _, endpoint := range []string{"", "10.0.0.5", "192.168.1.10:22", "127.0.0.1", "not-an-ip", "::1"} {
		if got := PublicHost("shop-3c4d5e6f", "", endpoint); got != "" {
			t.Errorf("endpoint %q produced host %q; want none", endpoint, got)
		}
	}
	// And an empty label is never a host, whatever the domain says.
	if got := PublicHost("", "apps.example.com", "203.0.113.7"); got != "" {
		t.Errorf("empty label produced %q", got)
	}
}
