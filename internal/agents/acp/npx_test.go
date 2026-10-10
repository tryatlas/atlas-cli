package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCeilingSpec(t *testing.T) {
	for in, want := range map[string]string{
		"@zed-industries/codex-acp@0.81.2": "@zed-industries/codex-acp@0.0.0 - 0.81.2",
		"pkg@1.2.3":                        "pkg@0.0.0 - 1.2.3",
		"@scope/pkg":                       "@scope/pkg",
		"pkg":                              "pkg",
	} {
		if got := CeilingSpec(in); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}

func TestNpmArgsAreThePinnedFlags(t *testing.T) {
	n := &Node{Root: "/d/node"}
	got := strings.Join(n.npmArgs("/d/x", "install", "pkg@0.0.0 - 1", "--save-exact", "--prefer-online"), " ")
	dir := n.Dir()
	want := dir + "/bin/npm --prefix /d/x install --cache=" + dir + "/cache --userconfig " + dir + "/blank_user_npmrc --globalconfig " +
		dir + "/blank_global_npmrc --no-audit --no-fund --fetch-timeout 300000 --fetch-retries 2 --fetch-retry-mintimeout 2000 " +
		"--fetch-retry-maxtimeout 10000 pkg@0.0.0 - 1 --save-exact --prefer-online"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if !strings.HasPrefix(filepath.Base(dir), "node-v24.11.0-") {
		t.Fatal(dir)
	}
}

func TestNpmEnvStripsConfig(t *testing.T) {
	env := npmEnv([]string{"npm_config_registry=x", "NPM_CONFIG_CACHE=y", "NODE_ENV=production", "HOME=/h", "PATH=/usr/bin"}, "/n/bin")
	j := strings.Join(env, " ")
	if strings.Contains(j, "npm_config") || strings.Contains(j, "NPM_CONFIG") || strings.Contains(j, "NODE_ENV") || !strings.Contains(j, "HOME=/h") ||
		!strings.Contains(j, "PATH=/n/bin:") {
		t.Fatal(j)
	}
}

func TestBinResolution(t *testing.T) {
	for _, c := range []struct{ name, bin, want string }{
		{"x", `"dist/index.js"`, "dist/index.js"},
		{"@scope/claude-agent-acp", `{"claude-agent-acp":"dist/a.js","other":"b.js"}`, "dist/a.js"},
		{"@scope/x", `{"only":"o.js"}`, "o.js"},
		{"@scope/x", `{"a":"a.js","b":"b.js"}`, ""},
	} {
		if got := binOf(&packageJSON{Name: c.name, Bin: []byte(c.bin)}); got != c.want {
			t.Errorf("%s %s: %q", c.name, c.bin, got)
		}
	}
}

func TestInstallNeeded(t *testing.T) {
	dir := t.TempDir()
	spec := "@s/agent@1.2.0"
	if why, need := installNeeded(dir, spec); !need || why != "no package.json" {
		t.Fatal(why)
	}
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	write("package.json", "{}")
	write(".atlas-wanted", spec)
	write("node_modules/@s/agent/package.json", `{"name":"@s/agent","version":"1.3.0","bin":"a.js","optionalDependencies":{"@s/agent-darwin-x64":"1","@s/agent-linux-arm64":"1"}}`)
	if why, need := installNeeded(dir, spec); !need || why != "installed version above the ceiling" {
		t.Fatal(why)
	}
	write("node_modules/@s/agent/package.json", `{"name":"@s/agent","version":"1.1.0","bin":"a.js","optionalDependencies":{"@s/agent-plat":"1"}}`)
	write("node_modules/.package-lock.json", `{"packages":{"node_modules/@s/agent-plat":{"os":["`+nodeOSName()+`"],"cpu":["`+nodeCPUName()+`"]}}}`)
	if why, need := installNeeded(dir, spec); !need || !strings.HasPrefix(why, "platform package") {
		t.Fatal(why)
	}
	write("node_modules/@s/agent-plat/package.json", `{}`)
	if why, need := installNeeded(dir, spec); need {
		t.Fatal(why)
	}
	write(".atlas-wanted", "@s/agent@1.1.9")
	if _, need := installNeeded(dir, spec); !need {
		t.Fatal("a changed sidecar reinstalls")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.2.3", "1.2.3", 0}, {"v2", "2.0.0", 0}, {"1.10.0", "1.9.9", 1}, {"1.0.0-beta", "1.0.0", -1}, {"0.81.2", "0.81.10", -1}} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s: %d", c.a, c.b, got)
		}
	}
}

func nodeOSName() string {
	p, _ := nodePlatform(runtime.GOOS, runtime.GOARCH)
	o, _, _ := strings.Cut(p, "-")
	if o == "win" {
		return "win32"
	}
	return o
}

func nodeCPUName() string {
	p, _ := nodePlatform(runtime.GOOS, runtime.GOARCH)
	_, c, _ := strings.Cut(p, "-")
	return c
}
