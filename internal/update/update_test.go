package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
)

// Fake builds a release server for tag holding bin; corrupt breaks the
// checksum.
func Fake(t *testing.T, tag string, bin []byte, corrupt bool) Source {
	t.Helper()
	var arch bytes.Buffer
	zw := gzip.NewWriter(&arch)
	tw := tar.NewWriter(zw)
	for name, body := range map[string][]byte{"README.md": []byte("readme"), "atlas-cli": bin} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = zw.Close()
	sum := sha256.Sum256(arch.Bytes())
	hexsum := hex.EncodeToString(sum[:])
	if corrupt {
		hexsum = strings.Repeat("0", 64)
	}
	name := Archive(tag, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/dl/"+tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n%s  atlas-cli_x_plan9_mips.tar.gz\n", hexsum, name, strings.Repeat("1", 64))
	})
	mux.HandleFunc("/dl/"+tag+"/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(arch.Bytes()) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return Source{API: srv.URL + "/api", Base: srv.URL + "/dl", HTTP: srv.Client()}
}

func TestDownloadVerifiesAndExtracts(t *testing.T) {
	ctx := context.Background()
	src := Fake(t, "v9.9.9", []byte("new binary"), false)
	tag, err := src.Latest(ctx)
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("%q %v", tag, err)
	}
	bin, err := src.Download(ctx, tag)
	if err != nil || string(bin) != "new binary" {
		t.Fatalf("%q %v", bin, err)
	}
	if _, err := Fake(t, "v9.9.9", []byte("x"), true).Download(ctx, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a bad checksum must refuse: %v", err)
	}
	if _, err := src.Download(ctx, "v1.0.0"); err == nil {
		t.Fatal("an unknown tag must fail")
	}
}

func TestReplaceKeepsTheModeAndSwapsAtomically(t *testing.T) {
	p := filepath.Join(t.TempDir(), "atlas")
	_ = os.WriteFile(p, []byte("old"), 0o700)
	if err := Replace(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(b) != "new" || fi.Mode().Perm() != 0o711 {
		t.Fatalf("%q %v", b, fi.Mode())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".atlas-update-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		cur, tag string
		want     bool
	}{
		{"0.3.4", "v0.4.0", true}, {"0.4.0", "v0.4.0", false}, {"0.4.1", "v0.4.0", false},
		{"0.4.0-dev", "v0.4.0", true}, {"dev", "v0.1.0", true}, {"0.10.0", "v0.9.9", false},
		{"1.0.0", "v1.0.1", true}, {"0.4.0", "nonsense", false},
	} {
		if got := Newer(c.cur, c.tag); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.cur, c.tag, got)
		}
	}
}
