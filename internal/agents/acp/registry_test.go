package acp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureRegistry = `{"version":"1.0.0","agents":[
 {"id":"bin-agent","name":"Bin","version":"1.2.0","description":"d","icon":"./icon.svg",
  "distribution":{"binary":{"darwin-aarch64":{"archive":"https://github.com/o/r/releases/download/v1/a.tar.gz","cmd":"./bin/agent","args":["--acp"]},
                            "linux-x86_64":{"archive":"https://x/a.zip","cmd":"agent"}},
                  "npx":{"package":"bin-agent@1.2.0"}}},
 {"id":"npx-agent","name":"Npx","version":"0.81.2","description":"d","icon":"https://cdn/x.svg",
  "distribution":{"npx":{"package":"@scope/npx-agent@0.81.2","args":["--acp"],"env":{"K":"V"}}}},
 {"id":"broken","name":"No version","description":"d","distribution":{}},
 {"id":"bad-shape","name":"x","version":"1","description":"d","distribution":{"binary":"nope"}},
 {"id":"nothing","name":"x","version":"1","description":"d","distribution":{}}
]}`

func TestParseRegistryDropsBadEntries(t *testing.T) {
	r, err := ParseRegistry([]byte(fixtureRegistry))
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "1.0.0" || len(r.Agents) != 3 || r.Dropped != 2 {
		t.Fatalf("%+v", r)
	}
	if _, err := ParseRegistry([]byte(`{"agents":{}}`)); err == nil {
		t.Fatal("the top level is strict")
	}
}

func TestPlatformKeysAndKinds(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}: "darwin-aarch64", {"darwin", "amd64"}: "darwin-x86_64",
		{"linux", "arm64"}: "linux-aarch64", {"linux", "amd64"}: "linux-x86_64",
		{"windows", "amd64"}: "windows-x86_64", {"freebsd", "amd64"}: "", {"linux", "386"}: "",
	}
	for in, want := range cases {
		if got := platformKey(in[0], in[1]); got != want {
			t.Errorf("%v: %q want %q", in, got, want)
		}
	}
	r, _ := ParseRegistry([]byte(fixtureRegistry))
	bin, _ := r.Find("bin-agent")
	if bin.KindOn("darwin-aarch64") != KindBinary || bin.KindOn("darwin-x86_64") != KindNpx {
		t.Fatal("binary wins only with a target for this platform")
	}
	if n, _ := r.Find("nothing"); n.KindOn("darwin-aarch64") != KindNone {
		t.Fatal("neither distribution")
	}
	if bin.IconURL() != "https://raw.githubusercontent.com/agentclientprotocol/registry/main/bin-agent/icon.svg" {
		t.Fatal(bin.IconURL())
	}
	if npx, _ := r.Find("npx-agent"); npx.IconURL() != "https://cdn/x.svg" {
		t.Fatal(npx.IconURL())
	}
}

func TestCatalogIsCacheFirst(t *testing.T) {
	hits := 0
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("User-Agent") != "atlas/test" {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(fixtureRegistry))
	}))
	defer srv.Close()
	now := time.Now()
	c := &Catalog{Dir: t.TempDir(), URL: srv.URL, UserAgent: "atlas/test", HTTP: srv.Client(), Now: func() time.Time { return now }}
	ctx := context.Background()
	if r, err := c.Load(ctx, false); err != nil || len(r.Agents) != 3 || hits != 1 {
		t.Fatalf("%v %d", err, hits)
	}
	if _, err := c.Load(ctx, false); err != nil || hits != 1 {
		t.Fatal("a fresh cache is not refetched")
	}
	now = now.Add(61 * time.Minute)
	fail = true
	r, err := c.Load(ctx, false)
	if hits != 2 || err == nil || r == nil || len(r.Agents) != 3 {
		t.Fatalf("a failed refresh keeps the catalogue and reports: %v", err)
	}
}

func TestInstalledMapRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "installed.json")
	in := `{"codex":{"type":"extension","env":{"SOME_TOKEN":"x"},"favorite_config_option_values":{"m":["a"]},"zz_future":1},` +
		`"mine":{"type":"custom","command":"~/bin/agent","args":["--acp"],"default_config_options":{}},"weird":{"type":"other"}}`
	_ = os.WriteFile(p, []byte(in), 0o644)
	m := LoadInstalled(p)
	if len(m) != 2 || m["codex"].Type != TypeRegistry || m["codex"].Env["SOME_TOKEN"] != "x" {
		t.Fatalf("%+v", m)
	}
	home, _ := os.UserHomeDir()
	if m["mine"].CommandPath() != filepath.Join(home, "bin/agent") {
		t.Fatalf("~ expanded: %s", m["mine"].CommandPath())
	}
	m["claude-acp"] = &Installed{Type: TypeRegistry}
	if err := m.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := `{
  "claude-acp": {
    "type": "registry"
  },
  "codex": {
    "type": "registry",
    "env": {
      "SOME_TOKEN": "x"
    },
    "favorite_config_option_values": {
      "m": [
        "a"
      ]
    },
    "zz_future": 1
  },
  "mine": {
    "type": "custom",
    "command": "~/bin/agent",
    "args": [
      "--acp"
    ]
  }
}`
	if string(b) != want {
		t.Fatalf("got\n%s", b)
	}
	if LoadInstalled(filepath.Join(dir, "missing.json")) == nil || !strings.HasSuffix(p, "installed.json") {
		t.Fatal("missing is empty")
	}
}
