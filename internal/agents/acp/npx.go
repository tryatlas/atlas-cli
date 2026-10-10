package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// npx agents (evidence-acp.md §1.4(c)): npm-installed into
// <data dir>/external-agents/registry/npx/<id>/ with the managed Node.

// splitSpec splits "name@version" (a scoped name keeps its leading @).
func splitSpec(spec string) (name, version string) {
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return spec, ""
	}
	return spec[:i], spec[i+1:]
}

// CeilingSpec turns pkg@1.2.3 into the range "pkg@0.0.0 - 1.2.3": a
// ceiling, not a pin. A spec without a version is used as is.
func CeilingSpec(spec string) string {
	name, ver := splitSpec(spec)
	if ver == "" {
		return spec
	}
	return name + "@0.0.0 - " + ver
}

// sanitize keeps [A-Za-z0-9._-] and replaces everything else with "_".
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// npxDir is an npx agent's install directory.
func npxDir(data, id string) string {
	return filepath.Join(data, externalAgentDir, "registry", "npx", sanitize(id))
}

// packageJSON is what the resolver reads from a package.json.
type packageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Bin                  json.RawMessage   `json:"bin"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

func readPackage(path string) (*packageJSON, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: our install dir
	if err != nil {
		return nil, err
	}
	var p packageJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// binOf resolves package.json "bin": a string, or the entry named after the
// unscoped package (else the only entry).
func binOf(p *packageJSON) string {
	if len(p.Bin) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Bin, &s) == nil {
		return s
	}
	var m map[string]string
	if json.Unmarshal(p.Bin, &m) != nil || len(m) == 0 {
		return ""
	}
	unscoped := p.Name
	if i := strings.LastIndex(unscoped, "/"); i >= 0 {
		unscoped = unscoped[i+1:]
	}
	if v, ok := m[unscoped]; ok {
		return v
	}
	if len(m) == 1 {
		for _, v := range m {
			return v
		}
	}
	return ""
}

// NpxInstall is a resolved npx agent.
type NpxInstall struct {
	Dir        string
	Executable string // absolute path of the package's bin script
}

// installNeeded applies the desktop's checks: no package.json, the installed
// version above the ceiling, no bin, a platform package missing, or a
// sidecar that names another spec.
func installNeeded(dir, spec string) (string, bool) {
	name, ver := splitSpec(spec)
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return "no package.json", true
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".atlas-wanted")); err != nil || strings.TrimSpace(string(b)) != spec { //nolint:gosec // G304: our install dir
		return "the wanted spec changed", true
	}
	p, err := readPackage(filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json"))
	if err != nil {
		return "package not installed", true
	}
	if ver != "" && compareVersions(p.Version, ver) > 0 {
		return "installed version above the ceiling", true
	}
	if binOf(p) == "" {
		return "package has no bin", true
	}
	if missing := missingPlatformPackage(dir, p); missing != "" {
		return "platform package " + missing + " missing", true
	}
	return "", false
}

// missingPlatformPackage checks the main package's optional dependencies
// that node_modules/.package-lock.json says are for this os/cpu: each must
// be present on disk (npm exits 0 when one fails to install).
func missingPlatformPackage(dir string, p *packageJSON) string {
	b, err := os.ReadFile(filepath.Join(dir, "node_modules", ".package-lock.json")) //nolint:gosec // G304: our install dir
	if err != nil {
		return ""
	}
	var lock struct {
		Packages map[string]struct {
			OS  []string `json:"os"`
			CPU []string `json:"cpu"`
		} `json:"packages"`
	}
	if json.Unmarshal(b, &lock) != nil {
		return ""
	}
	nodeOS, nodeCPU := runtime.GOOS, map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if nodeOS == "windows" {
		nodeOS = "win32"
	}
	for dep := range p.OptionalDependencies {
		entry, ok := lock.Packages["node_modules/"+dep]
		forUs := ok && matches(entry.OS, nodeOS) && matches(entry.CPU, nodeCPU)
		if !ok {
			// Not in the lock at all: npm skipped it. It matters only when
			// its name says it is for this platform.
			forUs = strings.Contains(dep, nodeOS) && strings.Contains(dep, nodeCPU)
		}
		if !forUs {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "node_modules", filepath.FromSlash(dep), "package.json")); err != nil {
			return dep
		}
	}
	return ""
}

func matches(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	allowed := false
	for _, x := range list {
		if strings.HasPrefix(x, "!") {
			if x[1:] == v {
				return false
			}
			allowed = true // only exclusions listed
			continue
		}
		if x == v {
			return true
		}
	}
	return allowed
}

// ResolveNpx installs when needed and returns the executable.
func ResolveNpx(ctx context.Context, n *Node, data, id string, d NpxDistribution, progress Progress) (*NpxInstall, error) {
	if err := n.Ensure(ctx, progress); err != nil {
		return nil, err
	}
	dir := npxDir(data, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(dir, ".install.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	name, ver := splitSpec(d.Package)
	if _, needed := installNeeded(dir, d.Package); needed {
		progress.say(fmt.Sprintf("Installing %s %s…", name, ver))
		if err := npmInstall(ctx, n, dir, d.Package); err != nil {
			return nil, err
		}
		if _, needed := installNeeded(dir, d.Package); needed {
			// Retry once from scratch.
			if err := npmInstall(ctx, n, dir, d.Package); err != nil {
				return nil, err
			}
			if why, needed := installNeeded(dir, d.Package); needed {
				return nil, fmt.Errorf("installing %s failed: %s", d.Package, why)
			}
		}
	}
	p, err := readPackage(filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json"))
	if err != nil {
		return nil, err
	}
	exe := filepath.Join(dir, "node_modules", filepath.FromSlash(name), filepath.FromSlash(binOf(p)))
	if _, err := os.Stat(exe); err != nil {
		return nil, fmt.Errorf("%s has no runnable bin: %w", d.Package, err)
	}
	return &NpxInstall{Dir: dir, Executable: exe}, nil
}

// npmInstall wipes node_modules and the lock, installs the ceiling spec
// (--prefer-online, then --prefer-offline), and writes the sidecar.
func npmInstall(ctx context.Context, n *Node, dir, spec string) error {
	_ = os.RemoveAll(filepath.Join(dir, "node_modules"))
	_ = os.Remove(filepath.Join(dir, "package-lock.json"))
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644); err != nil { //nolint:gosec // G306: no secret
			return err
		}
	}
	err := n.npm(ctx, dir, "install", CeilingSpec(spec), "--save-exact", "--prefer-online")
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return err
		}
		if err2 := n.npm(ctx, dir, "install", CeilingSpec(spec), "--save-exact", "--prefer-offline"); err2 != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, ".atlas-wanted"), []byte(spec), 0o644) //nolint:gosec // G306: no secret
}

// compareVersions compares semver-ish strings: a leading v is ignored,
// missing parts are zero, a pre-release sorts before its release, and
// non-numeric parts compare as strings.
func compareVersions(a, b string) int {
	pa, pre1 := versionParts(a)
	pb, pre2 := versionParts(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := comparePart(x, y); c != 0 {
			return c
		}
	}
	switch {
	case pre1 == pre2:
		return 0
	case pre1 == "":
		return 1
	case pre2 == "":
		return -1
	}
	return strings.Compare(pre1, pre2)
}

func versionParts(v string) ([]string, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	return strings.Split(core, "."), pre
}

func comparePart(x, y string) int {
	if x == "" {
		x = "0"
	}
	if y == "" {
		y = "0"
	}
	nx, ex := atoi(x)
	ny, ey := atoi(y)
	if ex == nil && ey == nil {
		switch {
		case nx < ny:
			return -1
		case nx > ny:
			return 1
		}
		return 0
	}
	return strings.Compare(x, y)
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// Sanitize is the directory name used for an agent id.
func Sanitize(id string) string { return sanitize(id) }
