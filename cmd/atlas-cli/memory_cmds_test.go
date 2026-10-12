package main

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/memory/record"
	"github.com/tryatlas/cli/internal/memory/server"
)

func bridgeIn(t *testing.T, msgs ...string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(strings.Join(msgs, "\n") + "\n")
	t.Cleanup(func() { stdin = old })
}

func TestMCPBridgeThenVerifyExportImport(t *testing.T) {
	root := gitRepo(t)
	srv := server.New("test")
	defer srv.Close()
	tokens := server.NewTokens()
	ts := httptest.NewServer(server.NewHTTPHandler(srv, tokens.Auth))
	defer ts.Close()
	t.Setenv("ATLAS_MCP_TOKEN", tokens.Mint(server.Grant{SessionID: server.UUID(), Agent: "codex", Cwd: root}))
	bridgeIn(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_remember","arguments":{"kind":"decision","content":"Use Postgres for the ledger"}}}`,
	)
	code, out, e := runCmd(t, "mcp-bridge", ts.URL+"/mcp")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 || lines[0]["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("bridge output: %s", out)
	}
	if !strings.Contains(out, `\"outcome\":\"inserted\"`) {
		t.Fatalf("remember: %s", out)
	}

	if code, out, e := runCmd(t, "memory", "list", "--kind", "decision"); code != 0 || !strings.Contains(out, "\tdecision\tactive\tUse Postgres for the ledger") {
		t.Fatalf("list %d: %q %s", code, out, e)
	}
	if code, out, _ := runCmd(t, "memory", "list", "--search", "ledger"); code != 0 || !strings.Contains(out, "Postgres") {
		t.Fatalf("search %d: %q", code, out)
	}
	code, out, e = runCmd(t, "memory", "verify")
	if code != 0 || !strings.Contains(out, "Revision chain intact: 1 revisions") {
		t.Fatalf("verify %d: %s %s", code, out, e)
	}
	dir := filepath.Join(t.TempDir(), "amr")
	if code, out, e := runCmd(t, "memory", "export", dir); code != 0 || !strings.Contains(out, "Exported 1 memories") {
		t.Fatalf("export %d: %s %s", code, out, e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "decisions.md")); !strings.Contains(string(b), "- Use Postgres for the ledger [source: atlas-session:codex/") {
		t.Fatalf("decisions.md: %s", b)
	}
	// Import into another repository: the line lands as a candidate.
	other := gitRepo(t)
	if code, out, e := runCmd(t, "memory", "import", dir); code != 0 || !strings.Contains(out, "Imported 1 memories as candidates") {
		t.Fatalf("import %d: %s %s", code, out, e)
	}
	st, _ := record.Open(other, false)
	defer st.Close()
	es, _ := st.List(t.Context(), "", 10)
	if len(es) != 1 || es[0].State != record.StateCandidate || es[0].Source != "import:amr" {
		t.Fatalf("%+v", es)
	}
}

func TestMemoryVerifyReportsABrokenChain(t *testing.T) {
	root := gitRepo(t)
	st, _ := record.Open(root, true)
	_, _, _ = st.Remember(t.Context(), record.Write{Kind: record.KindFact, Content: "one", Source: "user", Confidence: 1, At: 1})
	_ = st.Close()
	db := record.Path(root)
	if err := tamper(db); err != nil {
		t.Fatal(err)
	}
	code, _, e := runCmd(t, "memory", "verify")
	if code != 1 || !strings.Contains(e, "broken at revision 1") {
		t.Fatalf("exit %d: %s", code, e)
	}
}
