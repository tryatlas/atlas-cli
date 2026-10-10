package acp

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Registry binary installs (evidence-acp.md §1.4(b)).
const (
	maxDownload    = 2 << 30
	maxExpanded    = 4 << 30
	maxZipEntries  = 100_000
	archiveTimeout = 20 * time.Minute
)

// archiveKind by URL suffix.
type archiveKind int

const (
	archiveRaw archiveKind = iota
	archiveZip
	archiveTarGz
	archiveTarBz2
	archiveRefused
)

var refusedSuffixes = []string{".dmg", ".pkg", ".deb", ".rpm", ".msi", ".appimage", ".tar.xz", ".txz", ".tar", ".gz", ".bz2", ".xz", ".7z"}

// kindOf classifies an archive URL: zip, tar.gz/tgz, tar.bz2/tbz2 are
// accepted; the refused list is refused; any other suffix is a raw binary.
func kindOf(rawURL string) archiveKind {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		p = u.Path
	}
	p = strings.ToLower(p)
	switch {
	case strings.HasSuffix(p, ".zip"):
		return archiveZip
	case strings.HasSuffix(p, ".tar.gz"), strings.HasSuffix(p, ".tgz"):
		return archiveTarGz
	case strings.HasSuffix(p, ".tar.bz2"), strings.HasSuffix(p, ".tbz2"):
		return archiveTarBz2
	}
	for _, s := range refusedSuffixes {
		if strings.HasSuffix(p, s) {
			return archiveRefused
		}
	}
	return archiveRaw
}

func sha16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// VersionDir is v_<ver>_<sha256(ver)[..16]>_<sha256(url [+ "\0sha256:" +
// lowercase(sha)])[..16]>.
func VersionDir(version, archive, sum string) string {
	key := archive
	if sum != "" {
		key += "\x00sha256:" + strings.ToLower(sum)
	}
	return "v_" + sanitize(version) + "_" + sha16(version) + "_" + sha16(key)
}

// Archives installs binary distributions.
type Archives struct {
	Root   string // <data dir>/external-agents/registry
	HTTP   *http.Client
	GitHub string // API base (tests)
}

// NewArchives uses the data dir.
func NewArchives(data string) *Archives {
	return &Archives{Root: filepath.Join(data, externalAgentDir, "registry"), HTTP: newHTTPClient(archiveTimeout),
		GitHub: "https://api.github.com"}
}

// Ensure installs the target when its versioned directory is absent and
// returns the resolved cmd.
func (a *Archives) Ensure(ctx context.Context, id, version string, t BinaryTarget, node *Node, progress Progress) (string, error) {
	if t.Cmd == "node" {
		if err := node.Ensure(ctx, progress); err != nil {
			return "", err
		}
	}
	agentDir := filepath.Join(a.Root, sanitize(id))
	dir := filepath.Join(agentDir, VersionDir(version, t.Archive, t.SHA256))
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			return "", err
		}
		unlock, err := lockFile(filepath.Join(agentDir, ".install.lock"))
		if err != nil {
			return "", err
		}
		defer unlock()
		if _, err := os.Stat(dir); err != nil {
			progress.say(fmt.Sprintf("Downloading %s %s…", id, version))
			if err := a.install(ctx, agentDir, dir, t); err != nil {
				return "", err
			}
			a.gc(agentDir, filepath.Base(dir))
		}
	}
	if t.Cmd == "node" {
		return node.Bin(), nil
	}
	return resolveCmd(dir, t.Cmd)
}

// resolveCmd allows ./relative, .\relative, or a bare file name inside dir.
func resolveCmd(dir, cmd string) (string, error) {
	rel := cmd
	switch {
	case strings.HasPrefix(cmd, "./"), strings.HasPrefix(cmd, `.\`):
		rel = cmd[2:]
	case strings.ContainsAny(cmd, `/\:`) || strings.Contains(cmd, ".."):
		return "", fmt.Errorf("registry cmd %q must be ./relative or a bare file name", cmd)
	}
	rel = strings.ReplaceAll(rel, `\`, "/")
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("registry cmd %q escapes the install directory", cmd)
	}
	p, err := within(dir, rel)
	if err != nil {
		return "", err
	}
	if !executable(p) {
		return "", fmt.Errorf("registry cmd %q is not in the downloaded archive", cmd)
	}
	return p, nil
}

func (a *Archives) install(ctx context.Context, agentDir, dir string, t BinaryTarget) error {
	kind := kindOf(t.Archive)
	if kind == archiveRefused {
		return fmt.Errorf("refusing to install %s: unsupported archive type", path.Base(t.Archive))
	}
	want := strings.ToLower(t.SHA256)
	if want == "" {
		want = a.githubDigest(ctx, t.Archive) // "" when unverifiable
	}
	tmp, err := os.CreateTemp(agentDir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if err := download(ctx, a.HTTP, t.Archive, io.MultiWriter(tmp, h), maxDownload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		return fmt.Errorf("%s does not match its sha256 (%s, want %s); not installed", path.Base(t.Archive), got, want)
	}
	stage, err := os.MkdirTemp(agentDir, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	switch kind {
	case archiveZip:
		err = extractZip(tmp.Name(), stage)
	case archiveTarGz:
		err = extractTarFile(tmp.Name(), stage, func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) })
	case archiveTarBz2:
		err = extractTarFile(tmp.Name(), stage, func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil })
	case archiveRaw:
		name := path.Base(strings.TrimPrefix(strings.ReplaceAll(t.Cmd, `\`, "/"), "./"))
		err = copyFile(tmp.Name(), filepath.Join(stage, name), 0o755)
	}
	if err != nil {
		return err
	}
	return os.Rename(stage, dir)
}

// githubDigest reads the release asset's recorded digest for a
// github.com/<o>/<r>/releases/download/<tag>/<asset> URL.
func (a *Archives) githubDigest(ctx context.Context, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != "github.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 6 || parts[2] != "releases" || parts[3] != "download" {
		return ""
	}
	owner, repo, tag, asset := parts[0], parts[1], parts[4], parts[5]
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.GitHub+"/repos/"+owner+"/"+repo+"/releases/tags/"+url.PathEscape(tag), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rel) != nil {
		return ""
	}
	for _, as := range rel.Assets {
		if as.Name == asset {
			if d, ok := strings.CutPrefix(as.Digest, "sha256:"); ok {
				return strings.ToLower(d)
			}
		}
	}
	return ""
}

// gc removes older version directories.
func (a *Archives) gc(agentDir, keep string) {
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "v_") && e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(agentDir, e.Name()))
		}
	}
}

func extractTarFile(p, dir string, wrap func(io.Reader) (io.Reader, error)) error {
	f, err := os.Open(p) //nolint:gosec // G304: our temp file
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := wrap(f)
	if err != nil {
		return err
	}
	return extractTar(tar.NewReader(r), dir, maxExpanded, true)
}

func extractZip(p, dir string) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxZipEntries {
		return fmt.Errorf("archive has more than %d entries", maxZipEntries)
	}
	var total int64
	for _, f := range zr.File {
		target, err := within(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // symlinks in zips are not followed
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // G304: inside the staging dir
		if err != nil {
			_ = rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxExpanded-total+1))
		_ = rc.Close()
		_ = out.Close()
		if err != nil {
			return err
		}
		if total += n; total > maxExpanded {
			return fmt.Errorf("archive expands past %d bytes", int64(maxExpanded))
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: our temp file
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // G304: inside the staging dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
