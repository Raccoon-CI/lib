// Package catalog holds Raccoon CI's knowledge of the ecosystem as data
// (catalog.yaml): toolchains, tools, apt packages, setup actions, version
// files, runner labels and images, and Ubuntu-to-Debian pitfalls. It can
// follow new tools without code changes, and Source pulls updates from a URL.
// See catalog.yaml for the format.
//
// A catalog may come from a remote URL, so it holds knowledge only: names to
// look up and install through the provisioner's own installers, never
// commands or download locations, and every name is checked against a strict
// character set.
package catalog

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var defaultYAML []byte

// SchemaVersion is the catalog format this code understands. A catalog with
// a higher schema needs a newer raccoonci and is rejected with ErrNewerSchema,
// instead of being half-understood.
const SchemaVersion = 1

var ErrNewerSchema = errors.New("catalog needs a newer raccoonci")

type Kind int

const (
	Toolchain Kind = iota // node, python, go, ...: installed into the toolcache like setup-* would
	Package               // an apt package
	Tool                  // another tool with its own installer (docker, yarn, kubectl, ...)
)

func (k Kind) String() string {
	return [...]string{"toolchain", "package", "tool"}[k]
}

type Catalog struct {
	Schema                int                      `yaml:"schema"`
	Platform              Platform                 `yaml:"platform"`
	OperatingSystems      map[string]*OSDef        `yaml:"operating-systems"`
	RunnerLabels          []LabelRule              `yaml:"runner-labels"`
	Images                map[string]*ImageDef     `yaml:"images"`
	ImageAlias            string                   `yaml:"image-alias"`
	Runtimes              map[string]string        `yaml:"runtimes"`
	Toolchains            map[string]*ToolchainDef `yaml:"toolchains"`
	Tools                 map[string]*ToolDef      `yaml:"tools"`
	Packages              map[string]*PackageDef   `yaml:"packages"`
	Wrappers              map[string]*WrapperDef   `yaml:"wrappers"`
	Installers            map[string]*InstallerDef `yaml:"installers"`
	Opaque                []string                 `yaml:"opaque"`
	Actions               map[string]*ActionDef    `yaml:"actions"`
	VersionFiles          map[string]*VersionFile  `yaml:"version-files"`
	CompatWarnings        []CompatWarning          `yaml:"compat-warnings"`
	ImageSoftwareSections []string                 `yaml:"image-software-sections"`
	// SkipDirs are directory names that hold installed, vendored or VCS data
	// rather than the project's own sources (node_modules, .git, ...). A walk
	// that looks for a repository's projects does not enter them.
	SkipDirs []string `yaml:"skip-dirs"`

	commands map[string]Requirement
}

// Platform describes the machines Raccoon CI runs jobs on.
type Platform struct {
	Distro   string `yaml:"distro"`
	Version  string `yaml:"version"`
	Codename string `yaml:"codename"`
}

func (p Platform) String() string { return strings.TrimSpace(p.Distro + " " + p.Version) }

// OSDef says whether Raccoon CI can run jobs on an operating system, and what
// to tell the user when it can't.
type OSDef struct {
	Available   bool   `yaml:"available"`
	Unavailable string `yaml:"unavailable"`
}

// LabelRule maps runs-on labels (matched lower-cased) to an operating
// system; the first matching rule wins.
type LabelRule struct {
	Pattern string `yaml:"pattern"`
	OS      string `yaml:"os"`
	re      *regexp.Regexp
}

// ImageDef is one GitHub-hosted runner image whose preinstalled software we
// track.
type ImageDef struct {
	Label    string `yaml:"label"`    // the runs-on label, e.g. ubuntu-24.04
	Codename string `yaml:"codename"` // the Ubuntu release, for messages
	Toolset  string `yaml:"toolset"`  // its file in runner-images' toolsets/
}

type ToolchainDef struct {
	Title string `yaml:"title"`
	// Installer names one of Raccoon CI's built-in installers (the catalog
	// can pick one, never define one: where things are downloaded from
	// stays in code). Toolcache is the directory name under the tool cache
	// that setup-* actions look in (node, go, Python, ...); it defaults to
	// the toolchain's name.
	Installer string `yaml:"installer"`
	// ImplicitOnly says the toolchain's setup action installs it by itself
	// (rustup, setup-java), so the installer is only for jobs that use the
	// tool without setting it up. Prefilling the tool cache for the action
	// would download something it never looks at.
	ImplicitOnly bool     `yaml:"implicit-only"`
	Toolcache    string   `yaml:"toolcache"`
	Commands     []string `yaml:"commands"`
	Manifests    []string `yaml:"manifests"`
	// Lockfiles pin the dependencies exactly. When none of them changes the
	// installed dependencies are the same, which is what lets an environment
	// snapshot be reused. A toolchain without any cannot be snapshotted.
	Lockfiles []string `yaml:"lockfiles"`
	// WorkspaceKind names the kind of workspace this toolchain's projects can
	// form (npm, cargo): several packages in one repository whose dependencies
	// the root's one lockfile pins. The kind is the name package affected
	// gives such packages; empty means the toolchain has no workspaces.
	WorkspaceKind string       `yaml:"workspace-kind"`
	Asdf          []string     `yaml:"asdf"`
	LossyFloats   []string     `yaml:"lossy-floats"`
	Image         *ImageSource `yaml:"image"`
	// Probe says how to read the version of a copy already installed on the
	// machine (a provider's template may ship one), so it can be used instead
	// of downloading another. Without it the toolchain is always downloaded.
	Probe *Probe `yaml:"probe"`
}

// Probe runs Command with Args and takes the version from the first capture
// group of Pattern in its output. Command must be one of the toolchain's
// commands, and Args are plain words, never a shell line.
type Probe struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Pattern string   `yaml:"pattern"`

	re *regexp.Regexp
}

// Version extracts the version from the probe command's output.
func (p *Probe) Version(output string) (string, bool) {
	m := p.re.FindStringSubmatch(output)
	if len(m) < 2 || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// ImageSource says where the runner-images software report states the
// toolchain's default and cached versions.
type ImageSource struct {
	Default struct {
		// Section and Tool name a tool entry whose version is the default.
		Section string `yaml:"section"`
		Tool    string `yaml:"tool"`
		// TableMarker: the default is the row of Section's table that ends
		// with this marker, e.g. "17.0.20+1 (default)".
		TableMarker string `yaml:"table-marker"`
		// Toolset: the default is the version pattern of this toolcache
		// entry in the image's toolset, resolved against the cached versions.
		Toolset string `yaml:"toolset"`
	} `yaml:"default"`
	// Cached is the tool's name under "Cached Tools".
	Cached string `yaml:"cached"`
	// Required makes a report without this default an error: the report
	// format has most likely changed.
	Required bool `yaml:"required"`
}

type ToolDef struct {
	Commands []string `yaml:"commands"`
	Image    string   `yaml:"image"`
	Requires string   `yaml:"requires"`
}

type PackageDef struct {
	Commands []string `yaml:"commands"`
	Debian   string   `yaml:"debian"`
}

// WrapperDef describes a command that runs the command after it, and which
// of its own arguments to skip (besides -flags).
type WrapperDef struct {
	SkipAssignments bool `yaml:"skip-assignments"` // NAME=value words, as in env
	SkipArgs        int  `yaml:"skip-args"`        // leading positional arguments, as timeout's duration
}

// InstallerDef is a command that installs packages for the job itself: a
// verb ("install") after which the non-flag arguments are package names.
type InstallerDef struct {
	Verbs []string `yaml:"verbs"`
}

type ActionDef struct {
	Setup          string   `yaml:"setup"`
	VersionInput   string   `yaml:"version-input"`
	FileInput      string   `yaml:"file-input"`
	DefaultFiles   []string `yaml:"default-files"`
	VersionFromRef bool     `yaml:"version-from-ref"`
	// RefIgnore matches refs that are not versions (v4, master).
	RefIgnore   string `yaml:"ref-ignore"`
	Provides    string `yaml:"provides"`
	NeedsDocker bool   `yaml:"needs-docker"`
	Checkout    bool   `yaml:"checkout"`

	refIgnore *regexp.Regexp
}

type VersionFile struct {
	Format string   `yaml:"format"`
	Fields []string `yaml:"fields"`
}

// CompatWarning flags a command that works on GitHub's Ubuntu images but is
// likely to misbehave on Raccoon CI's machines. Message may use {platform},
// {distro}, {codename} (the machine's), {image-codename} (the GitHub image's
// Ubuntu release) and {arg}.
type CompatWarning struct {
	Commands []string `yaml:"commands"`
	// ArgPrefix limits the warning to calls with an argument starting so;
	// {arg} is that argument.
	ArgPrefix string `yaml:"arg-prefix"`
	Message   string `yaml:"message"`
}

// Version file formats the code knows how to read.
const (
	FormatLine         = "line"
	FormatJSON         = "json"
	FormatGoMod        = "go.mod"
	FormatToolVersions = "tool-versions"
)

// Requirement is what running a command needs.
type Requirement struct {
	Kind Kind
	Name string // toolchain, tool or package name
	// OnImage is how the runner-images report names it: the apt package or
	// the lower-cased software name. "" for toolchains.
	OnImage string
}

var (
	defaultOnce sync.Once
	defaultCat  *Catalog
	defaultErr  error
)

// Default returns the embedded catalog. Its validity is a test, so a broken
// catalog.yaml never ships; Default panics if it somehow does.
func Default() *Catalog {
	defaultOnce.Do(func() { defaultCat, defaultErr = Parse(defaultYAML) })
	if defaultErr != nil {
		panic("embedded catalog.yaml: " + defaultErr.Error())
	}
	return defaultCat
}

// Parse parses and validates a complete catalog.
func Parse(src []byte) (*Catalog, error) {
	c, err := decode(src)
	if err != nil {
		return nil, err
	}
	return c, c.validate()
}

func decode(src []byte) (*Catalog, error) {
	// Read the schema first and leniently, so a catalog from the future is
	// refused as a whole instead of failing on its first unknown key.
	var header struct {
		Schema int `yaml:"schema"`
	}
	if err := yaml.Unmarshal(src, &header); err == nil && header.Schema > SchemaVersion {
		return nil, fmt.Errorf("%w (schema %d, this one understands %d)", ErrNewerSchema, header.Schema, SchemaVersion)
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	c := &Catalog{}
	if err := dec.Decode(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Overlay returns c with the catalog file src applied on top: entries with
// the same name replace c's (an empty value, `name: null`, removes one), new
// entries are added, and top-level lists (runner-labels, compat-warnings,
// opaque, ...) and single values replace c's when given.
func (c *Catalog) Overlay(src []byte) (*Catalog, error) {
	o, err := decode(src)
	if err != nil {
		return nil, err
	}
	out := &Catalog{
		Schema:                SchemaVersion,
		Platform:              c.Platform,
		ImageAlias:            c.ImageAlias,
		OperatingSystems:      overlayPtr(c.OperatingSystems, o.OperatingSystems),
		Images:                overlayPtr(c.Images, o.Images),
		Runtimes:              overlay(c.Runtimes, o.Runtimes),
		Toolchains:            overlayPtr(c.Toolchains, o.Toolchains),
		Tools:                 overlayPtr(c.Tools, o.Tools),
		Packages:              overlayPtr(c.Packages, o.Packages),
		Wrappers:              overlayPtr(c.Wrappers, o.Wrappers),
		Installers:            overlayPtr(c.Installers, o.Installers),
		Actions:               overlayPtr(lowerKeys(c.Actions), lowerKeys(o.Actions)),
		VersionFiles:          overlayPtr(c.VersionFiles, o.VersionFiles),
		RunnerLabels:          pick(c.RunnerLabels, o.RunnerLabels),
		CompatWarnings:        pick(c.CompatWarnings, o.CompatWarnings),
		Opaque:                pick(c.Opaque, o.Opaque),
		ImageSoftwareSections: pick(c.ImageSoftwareSections, o.ImageSoftwareSections),
		SkipDirs:              pick(c.SkipDirs, o.SkipDirs),
	}
	if o.Platform != (Platform{}) {
		out.Platform = o.Platform
	}
	if o.ImageAlias != "" {
		out.ImageAlias = o.ImageAlias
	}
	return out, out.validate()
}

// pick returns over if it was given, else base.
func pick[T any](base, over []T) []T {
	if over != nil {
		return over
	}
	return base
}

func overlay(base, over map[string]string) map[string]string {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, over)
	return out
}

func overlayPtr[T any](base, over map[string]*T) map[string]*T {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]*T{}
	}
	for k, v := range over {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func lowerKeys[T any](m map[string]*T) map[string]*T {
	out := make(map[string]*T, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

var (
	// Names that end up on a command line (apt-get install <name>) or in a
	// path must not be able to smuggle in options or separators.
	packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)
	identName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
	probeArg    = regexp.MustCompile(`^-{0,2}[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
	commandName = regexp.MustCompile(`^[A-Za-z0-9_.+][A-Za-z0-9_.+-]*$`)
	placeholder = regexp.MustCompile(`\{[^{}]*\}`)
)

var messageVars = []string{"{platform}", "{distro}", "{codename}", "{image-codename}", "{arg}"}

func (c *Catalog) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	c.Actions = lowerKeys(c.Actions)
	c.commands = map[string]Requirement{}
	checkName := func(what, name string, re *regexp.Regexp) {
		if !re.MatchString(name) {
			fail("%s name %q has characters that aren't allowed", what, name)
		}
	}
	claim := func(cmd string, r Requirement) {
		if !commandName.MatchString(cmd) {
			fail("command %q has characters that aren't allowed", cmd)
			return
		}
		if prev, ok := c.commands[cmd]; ok {
			fail("command %q is claimed by %s %q and %s %q", cmd, prev.Kind, prev.Name, r.Kind, r.Name)
			return
		}
		c.commands[cmd] = r
	}

	if c.Schema == 0 {
		c.Schema = SchemaVersion
	}
	for _, name := range slices.Sorted(maps.Keys(c.OperatingSystems)) {
		if c.OperatingSystems[name] == nil {
			delete(c.OperatingSystems, name)
		}
	}
	for i := range c.RunnerLabels {
		r := &c.RunnerLabels[i]
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			fail("runner-labels[%d]: %v", i, err)
			continue
		}
		r.re = re
		if c.OperatingSystems[r.OS] == nil {
			fail("runner-labels[%d]: unknown operating system %q", i, r.OS)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Images)) {
		img := c.Images[name]
		if img == nil {
			delete(c.Images, name)
			continue
		}
		checkName("image", name, identName)
		if img.Label == "" || img.Toolset == "" {
			fail("image %q needs a label and a toolset", name)
		}
		if strings.ContainsAny(img.Toolset, "/\\") || strings.Contains(img.Toolset, "..") {
			fail("image %q: toolset %q must be a file name", name, img.Toolset)
		}
		for _, other := range slices.Sorted(maps.Keys(c.Images)) {
			if o := c.Images[other]; other < name && o != nil && strings.EqualFold(o.Label, img.Label) {
				fail("images %q and %q have the same label %q", other, name, img.Label)
			}
		}
	}

	for _, d := range c.SkipDirs {
		// A bare directory name, matched wherever it appears below the root;
		// a path or ".." would make the walk skip or reach something else.
		if d == "" || d == "." || d == ".." || strings.ContainsAny(d, "/\\") {
			fail("skip-dirs: %q must be a directory name, without a path", d)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Toolchains)) {
		tc := c.Toolchains[name]
		if tc == nil {
			delete(c.Toolchains, name) // `name: null` in a full catalog
			continue
		}
		checkName("toolchain", name, identName)
		if tc.Installer != "" {
			checkName("installer", tc.Installer, identName)
		}
		if tc.Toolcache != "" {
			checkName("toolcache directory", tc.Toolcache, identName)
		}
		for _, cmd := range tc.Commands {
			claim(cmd, Requirement{Kind: Toolchain, Name: name})
		}
		for _, f := range tc.Lockfiles {
			if strings.ContainsAny(f, "/\\") || f == ".." || f == "." || f == "" {
				fail("toolchain %q: lockfile %q must be a file name in the repo root", name, f)
			}
		}
		for _, f := range tc.Manifests {
			if strings.ContainsAny(f, "/\\") || f == ".." {
				fail("toolchain %q: manifest %q must be a file name in the repo root", name, f)
			}
		}
		if tc.WorkspaceKind != "" {
			checkName("workspace kind", tc.WorkspaceKind, identName)
		}
		if img := tc.Image; img != nil {
			d := img.Default
			n := 0
			for _, set := range []bool{d.Tool != "", d.TableMarker != "", d.Toolset != ""} {
				if set {
					n++
				}
			}
			switch {
			case n != 1:
				fail("toolchain %q: image.default needs exactly one of tool, table-marker or toolset", name)
			case d.Toolset == "" && d.Section == "":
				fail("toolchain %q: image.default needs a section", name)
			case d.Toolset != "" && img.Cached == "":
				fail("toolchain %q: image.default.toolset needs image.cached to resolve the pattern", name)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Toolchains)) {
		t := c.Toolchains[name]
		if t == nil || t.Probe == nil {
			continue
		}
		p := t.Probe
		if !slices.Contains(orName(t.Commands, name), p.Command) {
			fail("toolchain %q: probe.command %q is not one of its commands", name, p.Command)
		}
		for _, a := range p.Args {
			if !probeArg.MatchString(a) {
				fail("toolchain %q: probe.args %q is not a plain word", name, a)
			}
		}
		re, err := regexp.Compile(p.Pattern)
		switch {
		case err != nil:
			fail("toolchain %q: probe.pattern: %v", name, err)
		case re.NumSubexp() != 1:
			fail("toolchain %q: probe.pattern needs exactly one capture group, the version", name)
		default:
			p.re = re
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Tools)) {
		t := c.Tools[name]
		if t == nil {
			delete(c.Tools, name)
			continue
		}
		checkName("tool", name, identName)
		if t.Requires != "" && c.Toolchains[t.Requires] == nil {
			fail("tool %q requires unknown toolchain %q", name, t.Requires)
		}
		for _, cmd := range orName(t.Commands, name) {
			claim(cmd, Requirement{Kind: Tool, Name: name, OnImage: strings.ToLower(t.Image)})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Packages)) {
		p := c.Packages[name]
		if p == nil {
			delete(c.Packages, name)
			continue
		}
		checkName("package", name, packageName)
		if p.Debian != "" {
			checkName("package", p.Debian, packageName)
		}
		for _, cmd := range orName(p.Commands, name) {
			claim(cmd, Requirement{Kind: Package, Name: name, OnImage: name})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Wrappers)) {
		w := c.Wrappers[name]
		if w == nil {
			delete(c.Wrappers, name)
			continue
		}
		if w.SkipArgs < 0 || w.SkipArgs > 8 {
			fail("wrapper %q: skip-args %d is out of range", name, w.SkipArgs)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Installers)) {
		in := c.Installers[name]
		if in == nil {
			delete(c.Installers, name)
		} else if len(in.Verbs) == 0 {
			fail("installer %q needs verbs", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Actions)) {
		a := c.Actions[name]
		if a == nil {
			delete(c.Actions, name)
			continue
		}
		roles := 0
		for _, set := range []bool{a.Setup != "", a.Provides != "", a.NeedsDocker, a.Checkout} {
			if set {
				roles++
			}
		}
		switch {
		case roles != 1:
			fail("action %q needs exactly one of setup, provides, needs-docker or checkout", name)
		case a.Setup != "" && c.Toolchains[a.Setup] == nil:
			fail("action %q sets up unknown toolchain %q", name, a.Setup)
		case a.Setup != "" && a.VersionInput == "":
			fail("action %q needs a version-input", name)
		case a.Setup == "" && (a.VersionInput != "" || a.FileInput != "" || len(a.DefaultFiles) > 0 || a.VersionFromRef || a.RefIgnore != ""):
			fail("action %q: version settings only apply to setup actions", name)
		case a.RefIgnore != "" && !a.VersionFromRef:
			fail("action %q: ref-ignore needs version-from-ref", name)
		}
		for _, f := range a.DefaultFiles {
			if strings.Contains(f, "..") || strings.HasPrefix(f, "/") {
				fail("action %q: default file %q must stay inside the repo", name, f)
			}
		}
		if a.RefIgnore != "" {
			re, err := regexp.Compile(a.RefIgnore)
			if err != nil {
				fail("action %q: ref-ignore: %v", name, err)
			}
			a.refIgnore = re
		}
		if a.Provides != "" {
			checkName("provided tool", a.Provides, identName)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.VersionFiles)) {
		vf := c.VersionFiles[name]
		if vf == nil {
			delete(c.VersionFiles, name)
			continue
		}
		switch vf.Format {
		case FormatJSON:
			if len(vf.Fields) == 0 {
				fail("version file %q: format json needs fields", name)
			}
		case FormatLine, FormatGoMod, FormatToolVersions:
			if len(vf.Fields) > 0 {
				fail("version file %q: fields only apply to format json", name)
			}
		default:
			fail("version file %q: unknown format %q (want line, json, go.mod or tool-versions)", name, vf.Format)
		}
	}
	for i, w := range c.CompatWarnings {
		if len(w.Commands) == 0 || w.Message == "" {
			fail("compat-warnings[%d] needs commands and a message", i)
		}
		for _, p := range placeholder.FindAllString(w.Message, -1) {
			if !slices.Contains(messageVars, p) {
				fail("compat-warnings[%d]: unknown placeholder %s (want one of %s)", i, p, strings.Join(messageVars, " "))
			}
		}
		if strings.Contains(w.Message, "{arg}") && w.ArgPrefix == "" {
			fail("compat-warnings[%d]: {arg} needs arg-prefix", i)
		}
	}
	for using, major := range c.Runtimes {
		if _, err := strconv.Atoi(major); err != nil || !strings.HasPrefix(using, "node") {
			fail("runtimes: %s: %q is not a Node.js major version", using, major)
		}
	}
	if c.ImageAlias != "" && c.ImageForLabel(c.ImageAlias) != "" {
		fail("image-alias %q is also an image's own label", c.ImageAlias)
	}
	return errors.Join(errs...)
}

func orName(cmds []string, name string) []string {
	if len(cmds) == 0 {
		return []string{name}
	}
	return cmds
}

// Command returns what running cmd needs, if the catalog knows cmd.
func (c *Catalog) Command(cmd string) (Requirement, bool) {
	r, ok := c.commands[cmd]
	return r, ok
}

// ToolcacheName is the directory a toolchain lives in under the tool cache.
func (c *Catalog) ToolcacheName(toolchain string) string {
	if tc := c.Toolchains[toolchain]; tc != nil && tc.Toolcache != "" {
		return tc.Toolcache
	}
	return toolchain
}

// DebianName is the Debian package that provides an apt package (named as on
// Ubuntu), which is usually the same.
func (c *Catalog) DebianName(pkg string) string {
	if p := c.Packages[pkg]; p != nil && p.Debian != "" {
		return p.Debian
	}
	return pkg
}

// CommandsOf lists the commands a tool or package provides, which tells
// whether it is already installed.
func (c *Catalog) CommandsOf(kind Kind, name string) []string {
	switch kind {
	case Tool:
		if t := c.Tools[name]; t != nil {
			return orName(t.Commands, name)
		}
	case Package:
		if p := c.Packages[name]; p != nil {
			return orName(p.Commands, name)
		}
	case Toolchain:
		if t := c.Toolchains[name]; t != nil {
			return t.Commands
		}
	}
	return nil
}

// Wrapper returns how to see through a wrapper command such as sudo, or nil.
func (c *Catalog) Wrapper(cmd string) *WrapperDef { return c.Wrappers[cmd] }

func (c *Catalog) IsOpaque(cmd string) bool { return slices.Contains(c.Opaque, cmd) }

// Installs reports whether cmd args... installs packages for the job
// itself (apt-get install foo), and returns the package names.
func (c *Catalog) Installs(cmd string, args []string) []string {
	in := c.Installers[cmd]
	if in == nil {
		return nil
	}
	if !slices.ContainsFunc(args, func(a string) bool { return slices.Contains(in.Verbs, a) }) {
		return nil
	}
	var pkgs []string
	for _, a := range args {
		if a != "" && !slices.Contains(in.Verbs, a) && !strings.HasPrefix(a, "-") {
			pkgs = append(pkgs, a)
		}
	}
	return pkgs
}

// Action returns the catalog entry for an action name such as
// "actions/setup-node" (case-insensitive, without @ref), or nil.
func (c *Catalog) Action(name string) *ActionDef {
	return c.Actions[strings.ToLower(name)]
}

// IsVersionRef reports whether an action's @ref names a version (for actions
// with version-from-ref), as opposed to a major tag or branch.
func (a *ActionDef) IsVersionRef(ref string) bool {
	return a.VersionFromRef && ref != "" && (a.refIgnore == nil || !a.refIgnore.MatchString(ref))
}

// Title is a toolchain's display name, e.g. "Node.js".
func (c *Catalog) Title(toolchain string) string {
	if tc := c.Toolchains[toolchain]; tc != nil && tc.Title != "" {
		return tc.Title
	}
	return toolchain
}

// Requires returns the toolchain a tool runs on, or "".
func (c *Catalog) Requires(tool string) string {
	if t := c.Tools[tool]; t != nil {
		return t.Requires
	}
	return ""
}

// VersionFile says how to read a version file, by its base name.
func (c *Catalog) VersionFile(base string) VersionFile {
	if vf := c.VersionFiles[base]; vf != nil {
		return *vf
	}
	return VersionFile{Format: FormatLine}
}

// NodeMajor is the Node.js major version a JavaScript action's runs.using
// runs on.
func (c *Catalog) NodeMajor(using string) (string, error) {
	if major, ok := c.Runtimes[using]; ok {
		return major, nil
	}
	n, ok := strings.CutPrefix(using, "node")
	if _, err := strconv.Atoi(n); !ok || err != nil {
		return "", fmt.Errorf("unknown JavaScript runtime %q", using)
	}
	return n, nil
}

// ToolchainNames returns the toolchains in name order.
func (c *Catalog) ToolchainNames() []string {
	return slices.Sorted(maps.Keys(c.Toolchains))
}

// OSOfLabel returns the operating system a runs-on label asks for, or "" if
// it names none (custom self-hosted labels).
func (c *Catalog) OSOfLabel(label string) string {
	label = strings.ToLower(label)
	for _, r := range c.RunnerLabels {
		if r.re != nil && r.re.MatchString(label) {
			return r.OS
		}
	}
	return ""
}

// OSAvailable reports whether Raccoon CI can run jobs on os.
func (c *Catalog) OSAvailable(os string) bool {
	d := c.OperatingSystems[os]
	return d != nil && d.Available
}

// OSUnavailable explains what to do about a job that needs os, when it is
// not available.
func (c *Catalog) OSUnavailable(os string) string {
	if d := c.OperatingSystems[os]; d != nil && d.Unavailable != "" {
		return d.Unavailable
	}
	return "no " + os + " runner available"
}

// ImageNames returns the tracked runner images in name order.
func (c *Catalog) ImageNames() []string {
	return slices.Sorted(maps.Keys(c.Images))
}

// ImageForLabel returns the image a runs-on label such as ubuntu-24.04
// names, or "".
func (c *Catalog) ImageForLabel(label string) string {
	for name, img := range c.Images {
		if strings.EqualFold(img.Label, label) {
			return name
		}
	}
	return ""
}

// CompatMessage renders w's message for a call with the given argument, as
// run for the given GitHub image ("" if unknown).
func (c *Catalog) CompatMessage(w CompatWarning, image, arg string) string {
	codename := "Ubuntu"
	if img := c.Images[image]; img != nil && img.Codename != "" {
		codename = img.Codename
	}
	return strings.NewReplacer(
		"{platform}", c.Platform.String(),
		"{distro}", c.Platform.Distro,
		"{codename}", c.Platform.Codename,
		"{image-codename}", codename,
		"{arg}", arg,
	).Replace(w.Message)
}
