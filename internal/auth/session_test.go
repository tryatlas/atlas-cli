package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The example from SPEC §3.5 / evidence-auth.md §4.2, in serde_json's pretty
// layout (two-space indent, "key": value, no trailing newline).
const fullFile = `{
  "sessionToken": "tok-1",
  "savedAt": "2026-10-10T12:34:56.789012+00:00",
  "identity": {
    "id": "user_1",
    "name": "Ada Lovelace",
    "email": "ada@atlas.example",
    "avatarUrl": "https://example.test/photo.png",
    "avatarPath": "/tmp/avatar-0123456789abcdef.png",
    "orgs": [
      {
        "id": "org_1",
        "name": "Atlas",
        "role": "admin"
      },
      {
        "id": "org_2",
        "name": "Side",
        "role": null
      }
    ],
    "activeOrgId": "org_1"
  },
  "pinnedOrg": {
    "orgId": "org_1"
  }
}`

func TestFullFileRoundTripsByteForByte(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(fullFile), &s); err != nil {
		t.Fatal(err)
	}
	got, err := s.encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fullFile {
		t.Fatalf("round trip differs\n got:\n%s\nwant:\n%s", got, fullFile)
	}
}

func TestFreshGrantFileShape(t *testing.T) {
	s := freshSession("tok-2", "2026-10-10T12:00:00+00:00")
	got, _ := s.encode()
	want := `{
  "sessionToken": "tok-2",
  "savedAt": "2026-10-10T12:00:00+00:00",
  "identity": null,
  "pinnedOrg": null
}`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Minimal files the desktop loader accepts (evidence-auth.md §4.2).
func TestMinimalFilesLoad(t *testing.T) {
	for _, doc := range []string{
		`{"sessionToken":"session-tok","savedAt":"2026-07-22T00:00:00Z"}`,
		`{"sessionToken":"session-tok","savedAt":"2026-07-22T00:00:00Z",
		 "identity":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example","avatarUrl":null,"avatarPath":null}}`,
	} {
		var s Session
		if err := json.Unmarshal([]byte(doc), &s); err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if s.SessionToken != "session-tok" {
			t.Fatalf("token = %q", s.SessionToken)
		}
	}
}

func TestOrgsNullAndEmptyStayDistinct(t *testing.T) {
	for _, tc := range []struct{ doc, want string }{
		{`{"id":"u","name":"n","email":"e","orgs":null}`, `"orgs": null`},
		{`{"id":"u","name":"n","email":"e","orgs":[]}`, `"orgs": []`},
		{`{"id":"u","name":"n","email":"e"}`, `"orgs": null`},
	} {
		var id Identity
		if err := json.Unmarshal([]byte(tc.doc), &id); err != nil {
			t.Fatal(err)
		}
		s := Session{SessionToken: "t", SavedAt: "s", Identity: &id}
		b, _ := s.encode()
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("%s → %s, want %s", tc.doc, b, tc.want)
		}
	}
}

func TestPinnedOrgExplicitNone(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(`{"sessionToken":"t","savedAt":"s","pinnedOrg":{"orgId":null}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.PinnedOrg == nil || s.PinnedOrg.OrgID != nil {
		t.Fatalf("pin = %+v, want explicit none", s.PinnedOrg)
	}
	b, _ := s.encode()
	if !strings.Contains(string(b), "\"pinnedOrg\": {\n    \"orgId\": null\n  }") {
		t.Fatalf("explicit none not kept:\n%s", b)
	}
}

func TestUnknownRoleReadsAsNullAndOrgIsKept(t *testing.T) {
	var id Identity
	if err := json.Unmarshal([]byte(`{"id":"u","name":"n","email":"e","orgs":[{"id":"o","name":"O","role":"owner"}]}`), &id); err != nil {
		t.Fatal(err)
	}
	if id.Orgs == nil || len(*id.Orgs) != 1 || (*id.Orgs)[0].Role != nil {
		t.Fatalf("orgs = %+v", id.Orgs)
	}
}

// A field a newer desktop adds must survive a CLI rewrite.
func TestUnknownFieldsArePreserved(t *testing.T) {
	doc := `{"sessionToken":"t","savedAt":"s","futureTop":{"a":1},
	  "identity":{"id":"u","name":"n","email":"e","futureIdentity":true}}`
	var s Session
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatal(err)
	}
	b, _ := s.encode()
	for _, want := range []string{`"futureTop": {`, `"a": 1`, `"futureIdentity": true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	s := Session{SessionToken: "a<b>&c", SavedAt: "s"}
	b, _ := s.encode()
	if !strings.Contains(string(b), `"a<b>&c"`) {
		t.Fatalf("escaped: %s", b)
	}
}

func TestStoreSaveLoadClear(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev.atlas.ide")
	st := Store{Dir: dir}
	if _, ok := st.Load(); ok {
		t.Fatal("missing file should read as signed out")
	}
	s := freshSession("tok-3", "2026-10-10T12:00:00+00:00")
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "atlas-session.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	got, ok := st.Load()
	if !ok || got.SessionToken != "tok-3" {
		t.Fatalf("load = %+v, %v", got, ok)
	}
	if err := st.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := st.Clear(); err != nil {
		t.Fatalf("second clear must succeed: %v", err)
	}
	if _, ok := st.Load(); ok {
		t.Fatal("cleared file should read as signed out")
	}
}

func TestCorruptFileReadsAsSignedOut(t *testing.T) {
	dir := t.TempDir()
	st := Store{Dir: dir}
	for _, doc := range []string{`{not json`, `{"savedAt":"s"}`, `{"sessionToken":"","savedAt":"s"}`} {
		if err := os.WriteFile(st.Path(), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := st.Load(); ok {
			t.Errorf("%s should read as signed out", doc)
		}
	}
}

func TestActiveOrgResolution(t *testing.T) {
	org := func(id string) Org { return Org{ID: id, Name: id} }
	str := func(s string) *string { return &s }
	orgs := []Org{org("org_1"), org("org_2")}
	empty := []Org{}
	cases := []struct {
		name string
		id   *Identity
		want string
	}{
		{"no identity", nil, ""},
		{"orgs unknown", &Identity{}, ""},
		{"known none", &Identity{Orgs: &empty}, ""},
		{"chosen and listed", &Identity{Orgs: &orgs, ActiveOrgID: str("org_2")}, "org_2"},
		{"chosen but gone falls back to first", &Identity{Orgs: &orgs, ActiveOrgID: str("org_9")}, "org_1"},
		{"none chosen falls back to first", &Identity{Orgs: &orgs}, "org_1"},
	}
	for _, c := range cases {
		s := Session{SessionToken: "t", SavedAt: "s", Identity: c.id}
		if got := s.ActiveOrgID(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDirPerOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	switch runtime.GOOS {
	case "darwin":
		if got, _ := DefaultDir(); got != filepath.Join(home, "Library", "Application Support", "dev.atlas.ide") {
			t.Fatalf("darwin: %q", got)
		}
	case "linux":
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		if got, _ := DefaultDir(); got != "/xdg/dev.atlas.ide" {
			t.Fatalf("absolute XDG: %q", got)
		}
		t.Setenv("XDG_CONFIG_HOME", "relative/xdg")
		if got, _ := DefaultDir(); got != filepath.Join(home, ".config", "dev.atlas.ide") {
			t.Fatalf("relative XDG ignored: %q", got)
		}
	}
}
