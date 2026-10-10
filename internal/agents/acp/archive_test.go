package acp

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionDirFormula(t *testing.T) {
	h := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:])[:16] }
	got := VersionDir("1.2.0+build", "https://x/a.tgz", "ABCDEF")
	want := "v_1.2.0_build_" + h("1.2.0+build") + "_" + h("https://x/a.tgz\x00sha256:abcdef")
	if got != want {
		t.Fatalf("%s\n%s", got, want)
	}
	if VersionDir("1", "u", "") != "v_1_"+h("1")+"_"+h("u") {
		t.Fatal("no sha256: the url alone")
	}
}

func TestArchiveKinds(t *testing.T) {
	for u, k := range map[string]archiveKind{
		"https://x/a.zip": archiveZip, "https://x/a.TAR.GZ": archiveTarGz, "https://x/a.tgz": archiveTarGz,
		"https://x/a.tar.bz2": archiveTarBz2, "https://x/a.tbz2": archiveTarBz2, "https://x/a.dmg": archiveRefused,
		"https://x/a.tar.xz": archiveRefused, "https://x/a.gz": archiveRefused, "https://x/a.7z": archiveRefused,
		"https://x/agent": archiveRaw, "https://x/agent.exe?x=1": archiveRaw, "https://x/a.par": archiveRaw,
	} {
		if got := kindOf(u); got != k {
			t.Errorf("%s: %d want %d", u, got, k)
		}
	}
}

func TestResolveCmd(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "agent"), []byte("x"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "agent2"), []byte("x"), 0o755)
	if p, err := resolveCmd(dir, "./bin/agent"); err != nil || p != filepath.Join(dir, "bin", "agent") {
		t.Fatal(p, err)
	}
	if _, err := resolveCmd(dir, "agent2"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bin/agent", "/bin/sh", "../x", "./../x", "c:agent", "./missing"} {
		if _, err := resolveCmd(dir, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func tgz(_ *testing.T, files map[string]string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

func TestEnsureInstallsVerifiesAndMasks(t *testing.T) {
	good := tgz(t, map[string]string{"pkg/bin/agent": "#!/bin/sh\n"})
	sum := sha256.Sum256(good)
	zipped := func() []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create("agent-z")
		_, _ = w.Write([]byte("z"))
		_ = zw.Close()
		return b.Bytes()
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(good)
		case strings.HasSuffix(r.URL.Path, ".zip"):
			_, _ = w.Write(zipped)
		default:
			_, _ = w.Write([]byte("raw-binary"))
		}
	}))
	defer srv.Close()
	a := &Archives{Root: t.TempDir(), HTTP: srv.Client(), GitHub: srv.URL}
	ctx := context.Background()
	p, err := a.Ensure(ctx, "x", "1.0", BinaryTarget{Archive: srv.URL + "/a.tar.gz", Cmd: "./pkg/bin/agent", SHA256: hex.EncodeToString(sum[:])}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if _, err := a.Ensure(ctx, "y", "1.0", BinaryTarget{Archive: srv.URL + "/a.tar.gz", Cmd: "./pkg/bin/agent", SHA256: strings.Repeat("0", 64)}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
	if _, err := a.Ensure(ctx, "z", "1.0", BinaryTarget{Archive: srv.URL + "/a.zip", Cmd: "agent-z"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := a.Ensure(ctx, "r", "1.0", BinaryTarget{Archive: srv.URL + "/dl/amp-acp", Cmd: "./amp-acp"}, nil, nil); err != nil || filepath.Base(p) != "amp-acp" {
		t.Fatalf("raw binary: %s %v", p, err)
	}
	if _, err := a.Ensure(ctx, "d", "1.0", BinaryTarget{Archive: srv.URL + "/a.dmg", Cmd: "./x"}, nil, nil); err == nil {
		t.Fatal("dmg accepted")
	}
	// A new version replaces the old directory.
	_, _ = a.Ensure(ctx, "x", "1.1", BinaryTarget{Archive: srv.URL + "/a.tar.gz", Cmd: "./pkg/bin/agent"}, nil, nil)
	entries, _ := os.ReadDir(filepath.Join(a.Root, "x"))
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "v_") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d version dirs", n)
	}
}

func TestTarEscapeRefused(t *testing.T) {
	data := tgz(t, map[string]string{"../evil": "x"})
	p := filepath.Join(t.TempDir(), "a.tgz")
	_ = os.WriteFile(p, data, 0o644)
	if err := extractTarGz(p, t.TempDir(), 1<<20); err == nil {
		t.Fatal("path traversal accepted")
	}
}
