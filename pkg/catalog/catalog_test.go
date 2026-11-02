package catalog

import (
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	for cmd, want := range map[string]Requirement{
		"npm":     {Kind: Toolchain, Name: "node"},
		"mvn":     {Kind: Tool, Name: "maven", OnImage: "maven"},
		"gh":      {Kind: Tool, Name: "gh", OnImage: "github cli"},
		"jq":      {Kind: Package, Name: "jq", OnImage: "jq"},
		"cc":      {Kind: Package, Name: "gcc", OnImage: "gcc"},
		"pwsh":    {Kind: Tool, Name: "powershell", OnImage: "powershell"},
		"kotlinc": {Kind: Tool, Name: "kotlin", OnImage: "kotlin"},
	} {
		if got, ok := c.Command(cmd); !ok || got != want {
			t.Errorf("%s: %+v, want %+v", cmd, got, want)
		}
	}
	if a := c.Action("Actions/Setup-Node"); a == nil || a.Setup != "node" {
		t.Errorf("setup-node: %+v", a)
	}
	if c.Requires("maven") != "java" || c.Title("node") != "Node.js" {
		t.Error("requires/title")
	}
	if c.Wrapper("sudo") == nil || c.Wrapper("timeout").SkipArgs != 1 || !c.Wrapper("env").SkipAssignments || !c.IsOpaque("make") {
		t.Error("wrappers/opaque")
	}
	if vf := c.VersionFile(".nvmrc"); vf.Format != FormatLine {
		t.Errorf(".nvmrc: %+v", vf)
	}
	for using, want := range map[string]string{"node16": "20", "node20": "20", "node24": "24", "node26": "26"} {
		if got, err := c.NodeMajor(using); err != nil || got != want {
			t.Errorf("%s: %s %v", using, got, err)
		}
	}
	if _, err := c.NodeMajor("deno2"); err == nil {
		t.Error("deno2: expected an error")
	}
}

func TestPlatformKnowledge(t *testing.T) {
	c := Default()
	for label, want := range map[string]string{
		"ubuntu-latest": "linux", "Ubuntu-24.04": "linux", "ubuntu-24.04-arm": "linux", "linux": "linux",
		"windows-2025": "windows", "macos-15": "macos", "macos": "macos",
		"self-hosted": "", "gpu": "", "ubuntuish": "",
	} {
		if got := c.OSOfLabel(label); got != want {
			t.Errorf("OSOfLabel(%q) = %q, want %q", label, got, want)
		}
	}
	if !c.OSAvailable("linux") || c.OSAvailable("macos") || c.OSAvailable("") {
		t.Error("availability")
	}
	if got := c.OSUnavailable("macos"); !strings.Contains(got, "Mac") {
		t.Errorf("macos message %q", got)
	}
	if got := c.OSUnavailable("plan9"); got != "no plan9 runner available" {
		t.Errorf("unknown os message %q", got)
	}
	if c.ImageForLabel("Ubuntu-22.04") != "ubuntu22" || c.ImageForLabel("ubuntu-latest") != "" {
		t.Error("ImageForLabel")
	}
	if got := c.ImageNames(); strings.Join(got, ",") != "ubuntu22,ubuntu24,ubuntu26" {
		t.Errorf("images %v", got)
	}

	w := c.CompatWarnings[0]
	if got := c.CompatMessage(w, "ubuntu24", ""); !strings.Contains(got, "Debian's 'trixie' here, not Ubuntu's 'noble'") {
		t.Errorf("message %q", got)
	}
	if got := c.CompatMessage(w, "", ""); !strings.Contains(got, "not Ubuntu's 'Ubuntu'") {
		t.Errorf("message without an image %q", got)
	}

	if got := c.Installs("apt-get", []string{"-y", "install", "jq", "--no-install-recommends", "curl"}); strings.Join(got, ",") != "jq,curl" {
		t.Errorf("Installs = %v", got)
	}
	if c.Installs("apt-get", []string{"update"}) != nil || c.Installs("npm", []string{"install", "x"}) != nil {
		t.Error("Installs should ignore other verbs and commands")
	}
	a := c.Action("dtolnay/rust-toolchain")
	if !a.IsVersionRef("1.80.0") || !a.IsVersionRef("stable") || a.IsVersionRef("v1") || a.IsVersionRef("master") || a.IsVersionRef("") {
		t.Error("IsVersionRef")
	}
}

func TestProvisioningKnowledge(t *testing.T) {
	c := Default()
	if c.Toolchains["node"].Installer != "node" || c.ToolcacheName("go") != "go" || c.ToolcacheName("python") != "Python" || c.ToolcacheName("ruby") != "ruby" {
		t.Error("installer / toolcache names")
	}
	if c.DebianName("dnsutils") != "bind9-dnsutils" || c.DebianName("netcat") != "netcat-openbsd" || c.DebianName("jq") != "jq" || c.DebianName("unknown") != "unknown" {
		t.Error("DebianName")
	}
	if got := c.CommandsOf(Package, "dnsutils"); strings.Join(got, ",") != "dig,nslookup" {
		t.Errorf("package commands %v", got)
	}
	if got := c.CommandsOf(Tool, "maven"); strings.Join(got, ",") != "mvn" {
		t.Errorf("tool commands %v", got)
	}
	if got := c.CommandsOf(Tool, "zstd"); strings.Join(got, ",") != "zstd" {
		t.Errorf("a tool without commands is its own command: %v", got)
	}
	if _, err := c.Overlay([]byte("toolchains:\n  node: {installer: 'x; rm -rf /'}\n")); err == nil {
		t.Error("an installer name with shell characters must be rejected")
	}
}

func TestOverlay(t *testing.T) {
	c, err := Default().Overlay([]byte(`
runtimes:
  node20: "24"          # GitHub moved node20 actions to node24
toolchains:
  zig:
    title: Zig
    commands: [zig]
    manifests: [build.zig]
tools:
  zstd: null            # removed
  just: {image: just}   # newly preinstalled (and no longer only opaque)
actions:
  mlugg/setup-zig: {setup: zig, version-input: version}
opaque: [make, task]
`))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := c.Command("zig"); !ok || r.Name != "zig" {
		t.Errorf("zig: %+v", r)
	}
	if _, ok := c.Command("zstd"); ok {
		t.Error("zstd should be removed")
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("overlay lost the default toolchains")
	}
	if m, _ := c.NodeMajor("node20"); m != "24" {
		t.Errorf("node20 runs on %s", m)
	}
	if c.IsOpaque("just") || !c.IsOpaque("task") {
		t.Error("opaque list should be replaced")
	}
	if Default().Action("mlugg/setup-zig") != nil {
		t.Error("overlay modified the default catalog")
	}
}

func TestValidation(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"unknown key":         {"tools:\n  x: {imgae: x}\n", "imgae"},
		"duplicate command":   {"tools:\n  y: {commands: [npm]}\n", `command "npm" is claimed`},
		"unknown toolchain":   {"tools:\n  y: {requires: haskell}\n", "unknown toolchain"},
		"setup unknown":       {"actions:\n  a/b: {setup: haskell, version-input: v}\n", "unknown toolchain"},
		"no role":             {"actions:\n  a/b: {}\n", "exactly one of"},
		"version on provider": {"actions:\n  a/b: {provides: x, version-input: v}\n", "only apply to setup"},
		"bad format":          {"version-files:\n  x.lock: {format: toml}\n", "unknown format"},
		"json without fields": {"version-files:\n  x.json: {format: json}\n", "needs fields"},
		"bad runtime":         {"runtimes:\n  node30: latest\n", "major version"},
		"image source":        {"toolchains:\n  x: {image: {default: {tool: X}}}\n", "needs a section"},
		// What a poisoned catalog could try, if a remote one is ever trusted.
		"option injection":    {"packages:\n  \"-o=APT::Update::Pre-Invoke::=sh\": {commands: [x]}\n", "aren't allowed"},
		"debian name":         {"packages:\n  ok: {debian: \"x --allow-unauthenticated\"}\n", "aren't allowed"},
		"command with space":  {"tools:\n  y: {commands: [\"a b\"]}\n", "aren't allowed"},
		"toolset path":        {"images:\n  u: {label: l, toolset: ../../x.json}\n", "must be a file name"},
		"manifest path":       {"toolchains:\n  z: {manifests: [../../etc/passwd]}\n", "root"},
		"default file path":   {"actions:\n  a/b: {setup: node, version-input: v, default-files: [../x]}\n", "inside the repo"},
		"bad label regexp":    {"runner-labels:\n  - {pattern: '(', os: linux}\n", "runner-labels[0]"},
		"label unknown os":    {"runner-labels:\n  - {pattern: '^x$', os: plan9}\n", "unknown operating system"},
		"duplicate label":     {"images:\n  other: {label: UBUNTU-24.04, toolset: t.json}\n", "same label"},
		"unknown placeholder": {"compat-warnings:\n  - {commands: [x], message: 'on {planet}'}\n", "unknown placeholder"},
		"arg without prefix":  {"compat-warnings:\n  - {commands: [x], message: 'got {arg}'}\n", "needs arg-prefix"},
		"ref-ignore alone":    {"actions:\n  a/b: {setup: node, version-input: v, ref-ignore: x}\n", "needs version-from-ref"},
		"installer no verbs":  {"installers:\n  yum: {}\n", "needs verbs"},
		"newer schema":        {"schema: 99\nfuture-key: 1\n", "needs a newer raccoonci"},
	} {
		_, err := Default().Overlay([]byte(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}
