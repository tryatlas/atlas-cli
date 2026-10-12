package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/update"
)

func fakeRelease(t *testing.T, tag string, bin []byte) {
	t.Helper()
	var arch bytes.Buffer
	zw := gzip.NewWriter(&arch)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "atlas", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(bin)
	_ = tw.Close()
	_ = zw.Close()
	sum := sha256.Sum256(arch.Bytes())
	name := update.Archive(tag, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, `{"tag_name":%q}`, tag) })
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		case name:
			_, _ = w.Write(arch.Bytes())
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := updateSource
	updateSource = func() update.Source {
		return update.Source{API: srv.URL + "/api", Base: srv.URL + "/dl", HTTP: srv.Client()}
	}
	t.Cleanup(func() { updateSource = old })
}

func TestUpdate(t *testing.T) {
	fakeRelease(t, "v9.0.0", []byte("atlas 9"))
	exe := filepath.Join(t.TempDir(), "atlas")
	_ = os.WriteFile(exe, []byte("atlas old"), 0o755)
	oldTarget, oldVersion := updateTarget, version
	updateTarget = func() (string, error) { return exe, nil }
	t.Cleanup(func() { updateTarget, version = oldTarget, oldVersion })
	version = "0.4.0"

	code, out, e := runCmd(t, "update", "--check")
	if code != 0 || !strings.Contains(out, "atlas v9.0.0 is available") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	if b, _ := os.ReadFile(exe); string(b) != "atlas old" {
		t.Fatal("--check must not replace")
	}
	code, out, e = runCmd(t, "update")
	if code != 0 || !strings.Contains(out, "Updated atlas 0.4.0 → v9.0.0.") || !strings.Contains(out, "Verified:") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	if b, _ := os.ReadFile(exe); string(b) != "atlas 9" {
		t.Fatalf("binary = %q", b)
	}
	version = "9.0.0"
	if code, out, _ := runCmd(t, "update"); code != 0 || !strings.Contains(out, "atlas 9.0.0 is up to date.") {
		t.Fatal(out)
	}
	if code, _, e := runCmd(t, "update", "--version", "8.0.0"); code != 1 || !strings.Contains(e, "atlas_8.0.0_") {
		t.Fatalf("a missing release must fail: %d %s", code, e)
	}
}
