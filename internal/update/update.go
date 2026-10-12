// Package update replaces the running atlas binary with a release from
// GitHub: the same archive and checksums.txt the installer (install.sh)
// downloads, verified the same way, swapped in with a rename.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "tryatlas/atlas-cli"

// maxArchive bounds a download.
const maxArchive = 128 << 20

// Source is where releases are read from; tests point it at a fake.
type Source struct {
	API  string // https://api.github.com/repos/<repo>
	Base string // https://github.com/<repo>/releases/download
	HTTP *http.Client
}

// GitHub is the real source. ATLAS_UPDATE_BASE_URL replaces the download
// base, as ATLAS_BASE_URL does for the installer.
func GitHub() Source {
	s := Source{API: "https://api.github.com/repos/" + Repo, Base: "https://github.com/" + Repo + "/releases/download",
		HTTP: &http.Client{Timeout: 2 * time.Minute}}
	if b := os.Getenv("ATLAS_UPDATE_BASE_URL"); b != "" {
		s.Base = strings.TrimRight(b, "/")
	}
	return s
}

func (s Source) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "atlas-cli-update")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// Latest is the tag of the latest published release, "v0.4.0".
func (s Source) Latest(ctx context.Context) (string, error) {
	b, err := s.get(ctx, s.API+"/releases/latest", 1<<20)
	if err != nil {
		return "", err
	}
	var r struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Tag == "" {
		return "", errors.New("the latest release names no tag")
	}
	return r.Tag, nil
}

// Archive is the release archive for this platform.
func Archive(tag, goos, goarch string) string {
	return fmt.Sprintf("atlas_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// Download fetches this platform's archive for tag, checks it against the
// release's checksums.txt, and returns the atlas binary inside it.
func (s Source) Download(ctx context.Context, tag string) ([]byte, error) {
	name := Archive(tag, runtime.GOOS, runtime.GOARCH)
	sums, err := s.get(ctx, s.Base+"/"+tag+"/checksums.txt", 1<<20)
	if err != nil {
		return nil, err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s is not in the release's checksums.txt (no build for %s/%s?)", name, runtime.GOOS, runtime.GOARCH)
	}
	arch, err := s.get(ctx, s.Base+"/"+tag+"/"+name, maxArchive)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(arch)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s does not match its checksum; nothing was changed", name)
	}
	return binaryFrom(arch)
}

// binaryFrom extracts the "atlas" file from a tar.gz.
func binaryFrom(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the archive holds no atlas binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "atlas" {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// Replace swaps the binary at path for bin: written next to it, then
// renamed over it, so a running atlas keeps its open file and a failure
// leaves the old one in place.
func Replace(path string, bin []byte) error {
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm() | 0o111
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".atlas-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Executable is the running binary's real path.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Newer reports that release tag is newer than the running version. A
// development build ("dev", "0.4.0-dev") is older than its release.
func Newer(current, tag string) bool {
	c, cpre := parse(current)
	t, tpre := parse(tag)
	if c == nil {
		return true
	}
	if t == nil {
		return false
	}
	for i := range 3 {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return cpre && !tpre
}

// parse reads "v1.2.3[-pre]"; nil when it is not a version.
func parse(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, pre != ""
}
