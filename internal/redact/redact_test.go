package redact

import (
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Fake secrets are generated at run time so no secret-shaped literal sits in
// the source for scanners to trip on.

const (
	base62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	base32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func rnd(seed int64, n int, alphabet string) string {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func cat(parts ...string) string { return strings.Join(parts, "") }

type fixture struct {
	name   string
	in     string
	want   string
	counts Counts
}

func positives() []fixture {
	anthropic := cat("sk-", "ant-api03-", rnd(1, 93, base62+"_-"), "AA")
	aws := cat("AK", "IA", rnd(2, 16, base32))
	random := rnd(3, 32, base62)
	jwt := cat("ey", "J", rnd(4, 30, base62), ".ey", "J", rnd(5, 40, base62), ".", rnd(6, 43, base62))
	pem := cat("-----BEGIN ", "RSA PRIVATE KEY-----\n", rnd(7, 64, base62), "\n", rnd(8, 64, base62), "\n-----END RSA PRIVATE KEY-----")
	return []fixture{
		{"entropy token in prose", "the value is " + random + " today", "the value is [REDACTED] today", Counts{Entropy: 1}},
		{"vendor rule: anthropic key", "key " + anthropic + " ok", "key [REDACTED] ok", Counts{VendorRule: 1}},
		{"vendor rule: jwt", "cookie " + jwt + "\n", "cookie [REDACTED]\n", Counts{VendorRule: 1}},
		{"vendor rule: private key block", pem, "[REDACTED]", Counts{VendorRule: 1}},
		{"provider prefix: aws key quoted alone", `"` + aws + `"`, `"[REDACTED]"`, Counts{ProviderToken: 1}},
		{"credentialed uri", "postgres://app:Tr0ub4dor3@db.internal:5432/app", "postgres://app:[REDACTED]@db.internal:5432/app", Counts{CredentialedURI: 1}},
		{"connection string: semicolon", "Server=tcp:db.example.net;Database=app;User ID=app;Password=Hunter2Hunter2;", "Server=tcp:db.example.net;Database=app;User ID=app;Password=[REDACTED];", Counts{ConnectionString: 1}},
		{"connection string: jdbc", "jdbc:mysql://db:3306/app?user=app&password=Tr0ub4dor3", "jdbc:mysql://db:3306/app?user=app&password=[REDACTED]", Counts{ConnectionString: 1}},
		{"connection string: libpq dsn", "host=db port=5432 dbname=app user=app password=Tr0ub4dor3", "host=db port=5432 dbname=app user=app password=[REDACTED]", Counts{ConnectionString: 1}},
		{"credential value: env assignment", "DB_PASSWORD=hunter2hunter", "DB_PASSWORD=[REDACTED]", Counts{CredentialValue: 1}},
		{"credential value: json-ish text", `{"api_key": "abcd1234efgh"} trailing`, `{"api_key": "[REDACTED]"} trailing`, Counts{CredentialValue: 1}},
		{"credential value: yaml", "auth:\n  client_secret: hunter2hunter\n", "auth:\n  client_secret: [REDACTED]\n", Counts{CredentialValue: 1}},
		{"credential value: cli flag", "deploy --token abcdef123456 --force", "deploy --token [REDACTED] --force", Counts{CredentialValue: 1}},
		{"credential value: auth header keeps scheme", "Authorization: Bearer abc.def-ghi_jkl123", "Authorization: Bearer [REDACTED]", Counts{CredentialValue: 1}},
		{"credential value: camelCase key", "clientSecret = 'p4ssw0rd!!'", "clientSecret = '[REDACTED]'", Counts{CredentialValue: 1}},
		{"two layers on one span count once", "export OPENAI_KEY=" + random, "export OPENAI_KEY=[REDACTED]", Counts{Entropy: 1}},
	}
}

func TestFlatLayers(t *testing.T) {
	for _, f := range positives() {
		t.Run(f.name, func(t *testing.T) {
			got, err := Flat(f.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != f.want {
				t.Errorf("text\n got: %q\nwant: %q", got.Text, f.want)
			}
			if !equalCounts(got.Counts, f.counts) {
				t.Errorf("counts = %v, want %v", got.Counts, f.counts)
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	for _, f := range positives() {
		t.Run(f.name, func(t *testing.T) {
			once, _ := Flat(f.in)
			twice, _ := Flat(once.Text)
			if twice.Text != once.Text || twice.Counts.Total() != 0 {
				t.Errorf("second pass changed %q to %q (%v)", once.Text, twice.Text, twice.Counts)
			}
			auto1, _ := Auto(f.in)
			auto2, _ := Auto(auto1.Text)
			if auto2.Text != auto1.Text || auto2.Counts.Total() != 0 {
				t.Errorf("second auto pass changed %q to %q (%v)", auto1.Text, auto2.Text, auto2.Counts)
			}
		})
	}
}

// Over-redaction is permanent data loss: these must come through untouched.
func TestHoldOuts(t *testing.T) {
	cases := []string{
		"API_KEY=${API_KEY}",
		"password: $DB_PASSWORD",
		"token: <your-token>",
		"api_key = YOUR_API_KEY",
		"password=****",
		"secret: xxxxxxxx",
		"password: [REDACTED]",
		"token: {{ secrets.DEPLOY_TOKEN }}",
		"set %APPDATA% and %USERPROFILE%",
		"commit 3d539d9 and " + rnd(9, 40, "0123456789abcdef"),
		"sha256 " + rnd(10, 64, "0123456789abcdef"),
		"token_count=4096",
		"cache_key=abc123def456ghi",
		"PWD=/Users/eemong/Codes/atlas-cli",
		"input_tokens: 12345, max_tokens=8192",
		"password: str",
		"token = get_token()",
		"secret = settings.SECRET_KEY",
		"if password == other_password {",
		"secret_name: prod-db",
		"auth_url: https://auth.tryatlas.cc/api/auth",
		"author: Ada Lovelace",
		"request 014df517-39d1-4453-b7b3-9930c563627c done",
		"func TestRedactAutoWalksJSONLines2(t *testing.T)",
		"getUserAccountSettings2024Handler",
		"github.com/tryatlas/atlas-cli/internal/redact/vendor_rules.json",
		"node-v24.11.0-darwin-arm64.tar.gz",
		"AWS docs use " + cat("AK", "IA", "IOSFODNN7", "EXAMPLE"),
		"postgres://user:${DB_PASS}@localhost/app",
		"see https://example.com/path?page=2&sort=desc",
		"Pasted text",
		"",
		// Regressions from scanning this repo's docs (false positives found 2026-10-10).
		"as `Authorization: Bearer <session_token>` only",
		"Bearer auth: `Authorization: Bearer <token>` on every request",
		"env: [ATLAS_MCP_TOKEN=<token>] }",
		"5. On a token: write the session file",
		"Bearer = session token.",
		"(`commands/auth.rs:156-211`) and `core.rs:1343-1358`",
		"`AuthCore::new` normalises again",
		"secret := text[s:e]",
		"var credentialPairs = map[[2]string]bool{",
		"- auth: `POST /device/code`, `POST /device/token`",
		"(serialised token: `end_turn`, `max_tokens`)",
		"see `docs/adr/0008-agent-start-path-is-bounded.md`",
		"refuses `http://127.0.0.1:1@example.com`",
		"github.com/spf13/cobra v1.10.2 h1:" + rnd(13, 43, base62) + "=",
		`"integrity": "sha512-` + rnd(14, 86, base62) + `=="`,
		// Live regression (Phase 6): memory writer URIs in an MCP result.
		`"sources":["atlas-session:claude-code/5e2c2672-ebd1-4520-8d81-d83a16ead192"]`,
	}
	for _, in := range cases {
		got, err := Flat(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Text != in || got.Counts.Total() != 0 {
			t.Errorf("held-out text changed:\n  in: %q\n out: %q (%v)", in, got.Text, got.Counts)
		}
	}
}

func TestAutoWalksJSON(t *testing.T) {
	secret := rnd(11, 32, base62)
	in := `  {
  "id": "as-` + rnd(12, 32, "0123456789abcdef") + `",
  "path": "src/` + secret + `.go",
  "api_key": "` + secret + `",
  "headers": {"Authorization": "Bearer ` + secret + `"},
  "url": "https://u:pw12345678@h.example/x?token=abc123456789&q=1",
  "keys": {"api_keys": ["` + secret + `", "${KEY}"]},
  "note": "uses ` + secret + `",
  "n": 1.50, "ok": true, "none": null,
  "html": "<a&b>", "sep": "a b"
}
`
	got, err := Auto(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"` + jsonField(in, "id") + `","path":"src/` + secret + `.go","api_key":"[REDACTED]",` +
		`"headers":{"Authorization":"Bearer [REDACTED]"},` +
		`"url":"https://u:[REDACTED]@h.example/x?token=[REDACTED]&q=1",` +
		`"keys":{"api_keys":["[REDACTED]","${KEY}"]},"note":"uses [REDACTED]",` +
		`"n":1.50,"ok":true,"none":null,"html":"<a&b>","sep":"a b"}`
	if got.Text != want {
		t.Fatalf("text\n got: %s\nwant: %s", got.Text, want)
	}
	wantCounts := Counts{CredentialValue: 4, CredentialedURI: 1, Entropy: 1}
	if !equalCounts(got.Counts, wantCounts) {
		t.Fatalf("counts = %v, want %v", got.Counts, wantCounts)
	}
}

func jsonField(doc, key string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		panic(err)
	}
	return m[key].(string)
}

func TestAutoWalksJSONLines(t *testing.T) {
	in := "{\"password\": \"hunter2hunter\"}\n\n[1, 2]\n"
	got, _ := Auto(in)
	want := "{\"password\":\"[REDACTED]\"}\n\n[1,2]\n"
	if got.Text != want {
		t.Fatalf("got %q, want %q", got.Text, want)
	}
}

func TestAutoFallsBackToFlat(t *testing.T) {
	for _, in := range []string{
		"{not json} DB_PASSWORD=hunter2hunter",
		"[1,2]\nDB_PASSWORD=hunter2hunter",
		"\"just a string\" DB_PASSWORD=hunter2hunter",
	} {
		got, _ := Auto(in)
		flat, _ := Flat(in)
		if got.Text != flat.Text || !strings.Contains(got.Text, "DB_PASSWORD=[REDACTED]") {
			t.Errorf("%q: auto %q, flat %q", in, got.Text, flat.Text)
		}
	}
}

func TestAutoSingleLineJSONIsNotJSONL(t *testing.T) {
	got, _ := Auto(`{"a": "b"}`)
	if got.Text != `{"a":"b"}` {
		t.Fatalf("got %q", got.Text)
	}
}

func TestJSONStringEscaping(t *testing.T) {
	got, _ := Auto("{\"s\":\"q\\\" b\\\\ \\n \\u0001   é <>&\"}")
	want := "{\"s\":\"q\\\" b\\\\ \\n \\u0001   é <>&\"}"
	if got.Text != want {
		t.Fatalf("got %q, want %q", got.Text, want)
	}
}

func TestCountsJSON(t *testing.T) {
	b, err := json.Marshal(Counts{VendorRule: 2, Entropy: 1, CredentialValue: 0})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"entropy":1,"vendor_rule":2}` {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal(Counts{})
	if string(b) != `{}` {
		t.Fatalf("got %s", b)
	}
}

func TestVendorTable(t *testing.T) {
	tbl := vendorRules()
	if len(tbl.Rules) < 300 {
		t.Fatalf("only %d vendored rules", len(tbl.Rules))
	}
	if !strings.HasPrefix(tbl.Source, "betterleaks/betterleaks@") {
		t.Fatalf("source = %q", tbl.Source)
	}
	lowerKW := regexp.MustCompile(`^[^A-Z]+$`)
	for _, r := range tbl.Rules {
		if _, err := regexp.Compile(r.Regex); err != nil {
			t.Errorf("%s: %v", r.ID, err)
		}
		if len(r.Keywords) == 0 {
			t.Errorf("%s: no keywords", r.ID)
		}
		for _, k := range r.Keywords {
			if !lowerKW.MatchString(k) {
				t.Errorf("%s: keyword %q is not lower case", r.ID, k)
			}
		}
	}
}

func TestCredentialKey(t *testing.T) {
	yes := []string{"password", "DB_PASSWORD", "api_key", "apiKey", "X-Api-Key", "AWS_SECRET_ACCESS_KEY",
		"clientSecret", "auth_token", "Authorization", "GITHUB_TOKEN", "private_key", "db.pwd", "passphrase"}
	no := []string{"PWD", "token_count", "input_tokens", "max_tokens", "tokenizer", "cache_key", "author",
		"secret_name", "auth_url", "password_length", "keyboard", "monkey", "id", "path"}
	for _, k := range yes {
		if !credentialKey(k) {
			t.Errorf("credentialKey(%q) = false", k)
		}
	}
	for _, k := range no {
		if credentialKey(k) {
			t.Errorf("credentialKey(%q) = true", k)
		}
	}
}

func equalCounts(a, b Counts) bool {
	if a.Total() != b.Total() {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

func BenchmarkAutoToolResult(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("func handler(w http.ResponseWriter, r *http.Request) { // line of ordinary code\n")
	}
	text := sb.String()
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		if _, err := Auto(text); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAutoKeywordDense uses prose full of rule keywords (token, auth,
// key, secret, Bearer): the slow path for the vendored rules.
func BenchmarkAutoKeywordDense(b *testing.B) {
	raw, err := os.ReadFile("../../docs/atlas-reference/evidence-auth.md")
	if err != nil {
		b.Skip(err)
	}
	text := string(raw)
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		if _, err := Auto(text); err != nil {
			b.Fatal(err)
		}
	}
}

// A panic inside the redactor must surface as an error so the caller stores
// nothing for that content (SPEC §6.4).
func TestPanicBecomesError(t *testing.T) {
	f := func() (err error) {
		defer recoverInto(&err)
		panic("boom")
	}
	var re *Error
	if err := f(); !errors.As(err, &re) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestMatcherFindsOverlappingKeywords(t *testing.T) {
	m := newMatcher([]string{"he", "she", "his", "hers", "key"})
	seen := make([]bool, 5)
	m.present("ushers", seen)
	want := []bool{true, true, false, true, false}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen = %v, want %v", seen, want)
		}
	}
}
