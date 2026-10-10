package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The registry (evidence-acp.md §1.1–1.2, SPEC §8.2).
const (
	RegistryURL      = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"
	registryTimeout  = 30 * time.Second
	registryStale    = 60 * time.Minute
	iconBase         = "https://raw.githubusercontent.com/agentclientprotocol/registry/main/"
	httpConnTimeout  = 15 * time.Second
	httpReadTimeout  = 60 * time.Second
	dataDirName      = "dev.atlas.ide"
	externalAgentDir = "external-agents"
)

// DataDir is $XDG_DATA_HOME/dev.atlas.ide (default ~/.local/share/dev.atlas.ide).
func DataDir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, dataDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", dataDirName), nil
}

// Entry is one registry agent.
type Entry struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Repository   string       `json:"repository,omitempty"`
	Website      string       `json:"website,omitempty"`
	Icon         string       `json:"icon,omitempty"`
	Distribution Distribution `json:"distribution"`
}

// Distribution says how an agent is installed.
type Distribution struct {
	Binary map[string]BinaryTarget `json:"binary,omitempty"`
	Npx    *NpxDistribution        `json:"npx,omitempty"`
}

// BinaryTarget is one platform's archive.
type BinaryTarget struct {
	Archive string            `json:"archive"`
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	SHA256  string            `json:"sha256,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// NpxDistribution is an npm package run with the managed Node.
type NpxDistribution struct {
	Package string            `json:"package"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Registry is the parsed catalogue.
type Registry struct {
	Version string
	Agents  []Entry
	Dropped int // entries that failed to parse
}

// PlatformKey is this machine's registry key, or "" when unsupported.
func PlatformKey() string { return platformKey(runtime.GOOS, runtime.GOARCH) }

func platformKey(goos, goarch string) string {
	var os, arch string
	switch goos {
	case "darwin", "linux", "windows":
		os = goos
	default:
		return ""
	}
	switch goarch {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	default:
		return ""
	}
	return os + "-" + arch
}

// Kind is how an entry runs on this platform.
type Kind int

// Distribution kinds.
const (
	KindNone Kind = iota
	KindBinary
	KindNpx
)

// KindOn picks binary when it has a target for the platform, else npx
// (evidence-acp.md §1.2).
func (e Entry) KindOn(platform string) Kind {
	if _, ok := e.Distribution.Binary[platform]; ok && platform != "" {
		return KindBinary
	}
	if e.Distribution.Npx != nil && e.Distribution.Npx.Package != "" {
		return KindNpx
	}
	return KindNone
}

// Supported reports whether the entry runs on this platform.
func (e Entry) Supported() bool { return e.KindOn(PlatformKey()) != KindNone }

// IconURL resolves the icon: absolute http(s) as is, else under the
// registry repository.
func (e Entry) IconURL() string {
	if e.Icon == "" {
		return ""
	}
	if strings.HasPrefix(e.Icon, "http://") || strings.HasPrefix(e.Icon, "https://") {
		return e.Icon
	}
	return iconBase + e.ID + "/" + strings.TrimPrefix(e.Icon, "./")
}

// Find returns the entry with an id.
func (r *Registry) Find(id string) (Entry, bool) {
	for _, e := range r.Agents {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// ParseRegistry parses the top level strictly and each entry on its own; a
// bad entry is dropped, not fatal.
func ParseRegistry(b []byte) (*Registry, error) {
	var top struct {
		Version string            `json:"version"`
		Agents  []json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("unreadable registry: %w", err)
	}
	r := &Registry{Version: top.Version}
	for _, raw := range top.Agents {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil || !e.valid() {
			r.Dropped++
			continue
		}
		r.Agents = append(r.Agents, e)
	}
	return r, nil
}

// valid checks the required fields.
func (e Entry) valid() bool {
	if e.ID == "" || e.Name == "" || e.Version == "" {
		return false
	}
	for _, t := range e.Distribution.Binary {
		if t.Archive == "" || t.Cmd == "" {
			return false
		}
	}
	return e.Distribution.Npx == nil || e.Distribution.Npx.Package != ""
}

// Catalog is the cached registry with its refresh rule.
type Catalog struct {
	Dir       string // <data dir>/external-agents/registry
	URL       string
	UserAgent string
	HTTP      *http.Client
	Now       func() time.Time
}

// NewCatalog uses the data dir and the real registry URL.
func NewCatalog(version string) (*Catalog, error) {
	d, err := DataDir()
	if err != nil {
		return nil, err
	}
	return &Catalog{Dir: filepath.Join(d, externalAgentDir, "registry"), URL: RegistryURL,
		UserAgent: "atlas/" + version, HTTP: newHTTPClient(registryTimeout), Now: time.Now}, nil
}

// newHTTPClient has the desktop's 15 s connect timeout and an overall limit.
func newHTTPClient(total time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = httpReadTimeout
	tr.TLSHandshakeTimeout = httpConnTimeout
	return &http.Client{Timeout: total, Transport: tr}
}

func (c *Catalog) path() string { return filepath.Join(c.Dir, "registry.json") }

// Load returns the registry cache-first. It refreshes when the cache is
// missing, older than an hour, or force is set; a failed refresh keeps the
// previous catalogue and returns the fetch error alongside it.
func (c *Catalog) Load(ctx context.Context, force bool) (*Registry, error) {
	cached, cerr := c.cached()
	fresh := false
	if cerr == nil {
		if info, err := os.Stat(c.path()); err == nil && c.Now().Sub(info.ModTime()) < registryStale {
			fresh = true
		}
	}
	if fresh && !force {
		return cached, nil
	}
	b, ferr := c.fetch(ctx)
	if ferr == nil {
		r, perr := ParseRegistry(b)
		if perr == nil {
			_ = writeAtomic(c.path(), b, 0o644)
			return r, nil
		}
		ferr = perr
	}
	if cached != nil {
		return cached, fmt.Errorf("registry refresh failed, using the cached catalogue: %w", ferr)
	}
	return nil, fmt.Errorf("could not fetch the agent registry: %w", ferr)
}

func (c *Catalog) cached() (*Registry, error) {
	b, err := os.ReadFile(c.path())
	if err != nil {
		return nil, err
	}
	return ParseRegistry(b)
}

func (c *Catalog) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("registry answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// writeAtomic writes via a temp file and rename.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ErrNotInRegistry is an id the registry does not list.
var ErrNotInRegistry = errors.New("not in the agent registry")
