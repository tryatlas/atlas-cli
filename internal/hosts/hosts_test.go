package hosts

import "testing"

// Oracle: evidence-auth.md §1.1 (desktop tests.rs:405-425).
func TestAuthLadder(t *testing.T) {
	t.Setenv("ATLAS_AUTH_URL", "http://localhost:8787/api/auth/")
	if got := Auth(); got != "http://localhost:8787/api/auth" {
		t.Fatalf("env: %q", got)
	}
	t.Setenv("ATLAS_AUTH_URL", "   ")
	old := buildAuth
	buildAuth = "https://staging.example/api/auth"
	defer func() { buildAuth = old }()
	if got := Auth(); got != "https://staging.example/api/auth" {
		t.Fatalf("blank env falls through to build: %q", got)
	}
	buildAuth = ""
	if got := Auth(); got != DefaultAuth {
		t.Fatalf("default: %q", got)
	}
}

func TestIngestDefault(t *testing.T) {
	t.Setenv("ATLAS_INGEST_URL", "")
	if got := Ingest(); got != "https://ingest.tryatlas.cc" {
		t.Fatalf("%q", got)
	}
	t.Setenv("ATLAS_INGEST_URL", " https://ingest.local// ")
	if got := Ingest(); got != "https://ingest.local" {
		t.Fatalf("%q", got)
	}
}
