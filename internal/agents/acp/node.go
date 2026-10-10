package acp

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The managed Node runtime (evidence-acp.md §1.5). There is no system Node
// fallback.
const (
	NodeVersion      = "v24.11.0"
	nodeDist         = "https://nodejs.org/dist/"
	nodeDownloadTime = 900 * time.Second
	shasumsTimeout   = 30 * time.Second
	npmInstallTime   = 1800 * time.Second
	npmOtherTime     = 600 * time.Second
)

// Progress receives install status lines ("Downloading Node.js…").
type Progress func(string)

func (p Progress) say(s string) {
	if p != nil {
		p(s)
	}
}

// nodePlatform is Node's os-arch naming.
func nodePlatform(goos, goarch string) (string, error) {
	var o, a string
	switch goos {
	case "darwin", "linux":
		o = goos
	case "windows":
		o = "win"
	default:
		return "", fmt.Errorf("no managed Node for %s", goos)
	}
	switch goarch {
	case "amd64":
		a = "x64"
	case "arm64":
		a = "arm64"
	default:
		return "", fmt.Errorf("no managed Node for %s", goarch)
	}
	return o + "-" + a, nil
}

// Node is the managed runtime under <data dir>/node/node-v24.11.0-<os>-<arch>/.
type Node struct {
	Root string // <data dir>/node
	Dist string // base URL (tests)
	HTTP *http.Client
}

// NewNode uses the data dir.
func NewNode() (*Node, error) {
	d, err := DataDir()
	if err != nil {
		return nil, err
	}
	return &Node{Root: filepath.Join(d, "node"), Dist: nodeDist, HTTP: newHTTPClient(nodeDownloadTime)}, nil
}

func (n *Node) name() (string, error) {
	p, err := nodePlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return "node-" + NodeVersion + "-" + p, nil
}

// Dir is the runtime's directory.
func (n *Node) Dir() string {
	name, _ := n.name()
	return filepath.Join(n.Root, name)
}

// Bin is the node binary.
func (n *Node) Bin() string { return filepath.Join(n.Dir(), "bin", "node") }

// npmCLI is bin/npm, which node runs as a script.
func (n *Node) npmCLI() string { return filepath.Join(n.Dir(), "bin", "npm") }

// Ensure downloads and verifies the runtime when it is missing or broken.
func (n *Node) Ensure(ctx context.Context, progress Progress) error {
	if n.alive(ctx) {
		return nil
	}
	if err := os.MkdirAll(n.Root, 0o755); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(n.Root, ".install.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if n.alive(ctx) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, nodeDownloadTime)
	defer cancel()
	name, err := n.name()
	if err != nil {
		return err
	}
	progress.say("Downloading Node.js…")
	want, err := n.shasum(ctx, name+".tar.gz")
	if err != nil {
		return fmt.Errorf("the Node.js checksums: %w", err)
	}
	tmp, err := os.CreateTemp(n.Root, ".node-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if err := download(ctx, n.HTTP, n.Dist+NodeVersion+"/"+name+".tar.gz", io.MultiWriter(tmp, h), 2<<30); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("the Node.js download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the Node.js download does not match SHASUMS256.txt (%s, want %s); not installed", got, want)
	}
	stage, err := os.MkdirTemp(n.Root, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := extractTarGz(tmp.Name(), stage, 4<<30); err != nil {
		return err
	}
	_ = os.RemoveAll(n.Dir())
	if err := os.Rename(filepath.Join(stage, name), n.Dir()); err != nil {
		return err
	}
	if err := n.writeRCs(); err != nil {
		return err
	}
	if !n.alive(ctx) {
		return errors.New("the managed Node.js does not run")
	}
	return nil
}

// shasum finds a file's sha256 in SHASUMS256.txt.
func (n *Node) shasum(ctx context.Context, file string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, shasumsTimeout)
	defer cancel()
	var b strings.Builder
	if err := download(ctx, n.HTTP, n.Dist+NodeVersion+"/SHASUMS256.txt", &b, 1<<20); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == file {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed", file)
}

// writeRCs writes the blank npmrc files npm is pinned to.
func (n *Node) writeRCs() error {
	if err := os.WriteFile(filepath.Join(n.Dir(), "blank_user_npmrc"), []byte("update-notifier=false\n"), 0o644); err != nil { //nolint:gosec // G306: no secret
		return err
	}
	return os.WriteFile(filepath.Join(n.Dir(), "blank_global_npmrc"), nil, 0o644) //nolint:gosec // G306: no secret
}

// alive runs `npm --version` through the managed node.
func (n *Node) alive(ctx context.Context) bool {
	if _, err := os.Stat(n.Bin()); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(n.Dir(), "blank_user_npmrc")); err != nil {
		if n.writeRCs() != nil {
			return false
		}
	}
	err := n.npm(ctx, "", "--version")
	return err == nil
}

// npmArgs builds `<npm-cli> [--prefix dir] <sub> <pinned flags> <args…>`.
func (n *Node) npmArgs(prefix, sub string, args ...string) []string {
	out := []string{n.npmCLI()}
	if prefix != "" {
		out = append(out, "--prefix", prefix)
	}
	out = append(out, sub)
	out = append(out,
		"--cache="+filepath.Join(n.Dir(), "cache"),
		"--userconfig", filepath.Join(n.Dir(), "blank_user_npmrc"),
		"--globalconfig", filepath.Join(n.Dir(), "blank_global_npmrc"),
		"--no-audit", "--no-fund",
		"--fetch-timeout", "300000", "--fetch-retries", "2",
		"--fetch-retry-mintimeout", "2000", "--fetch-retry-maxtimeout", "10000")
	return append(out, args...)
}

// npm runs npm with the pinned flags. install gets 1800 s, everything else
// 600 s; a spawn or I/O failure is retried once, a timeout never.
func (n *Node) npm(ctx context.Context, prefix, sub string, args ...string) error {
	limit := npmOtherTime
	if sub == "install" {
		limit = npmInstallTime
	}
	var out []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		c, cancel := context.WithTimeout(ctx, limit)
		cmd := exec.CommandContext(c, n.Bin(), n.npmArgs(prefix, sub, args...)...) //nolint:gosec // G204: the managed node
		cmd.Env = npmEnv(os.Environ(), n.binDir())
		out, err = cmd.CombinedOutput()
		timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
		cancel()
		var ee *exec.ExitError
		if err == nil || timedOut || errors.As(err, &ee) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("npm %s: %w: %s", sub, err, tail(out, 2000))
	}
	return nil
}

func (n *Node) binDir() string { return filepath.Join(n.Dir(), "bin") }

// npmEnv strips every npm_config_* (any case) and NODE_ENV, and puts the
// managed node first on PATH.
func npmEnv(base []string, nodeBin string) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToLower(k), "npm_config_") || k == "NODE_ENV" || k == "PATH" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+nodeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// CommandEnv is npm_command_env: the managed node's bin first on PATH, and
// NODE_EXTRA_CA_CERTS passed through.
func (n *Node) CommandEnv() map[string]string {
	env := map[string]string{"PATH": n.binDir() + string(os.PathListSeparator) + os.Getenv("PATH")}
	if v := os.Getenv("NODE_EXTRA_CA_CERTS"); v != "" {
		env["NODE_EXTRA_CA_CERTS"] = v
	}
	return env
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// ── shared helpers ───────────────────────────────────────────────────────────

// download streams a URL into w, at most limit bytes.
func download(ctx context.Context, c *http.Client, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("download larger than %d bytes", limit)
	}
	return nil
}

// extractTarGz expands a .tar.gz into dir.
func extractTarGz(path, dir string, limit int64) error {
	f, err := os.Open(path) //nolint:gosec // G304: our temp file
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	return extractTar(tar.NewReader(gz), dir, limit, false)
}

// extractTar writes entries inside dir; paths that escape are refused.
// mask0755 forces every file to 0755 (agent archives).
func extractTar(tr *tar.Reader, dir string, limit int64, mask0755 bool) error {
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := within(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(h.Mode&0o777) | 0o600
			if mask0755 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // G304: inside the staging dir
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, limit-total+1))
			_ = out.Close()
			if err != nil {
				return err
			}
			total += n
			if total > limit {
				return fmt.Errorf("archive expands past %d bytes", limit)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) {
				return fmt.Errorf("absolute symlink %s in archive", h.Name)
			}
			if _, err := within(dir, filepath.Join(filepath.Dir(h.Name), h.Linkname)); err != nil { //nolint:gosec // G305: this is the containment check
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// within joins name under dir and refuses anything that escapes it.
func within(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(dir, filepath.FromSlash(name)))
	if clean != dir && !strings.HasPrefix(clean, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the install directory", name)
	}
	return clean, nil
}

// lockFile takes an exclusive flock, waiting for another installer.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // G304: our data dir
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
