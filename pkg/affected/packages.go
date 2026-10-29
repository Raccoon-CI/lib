// Package affected works out which jobs of a workflow the changed files
// concern, so a change to the backend does not run the frontend's jobs.
//
// It is deliberately cautious: a job is only left out when the evidence says
// its inputs did not change. Where a job's inputs are known comes from
// (a) the working directories its steps run in, (b) the dependency graph of
// the repository's packages, so a package is also affected by changes to what
// it depends on, and (c) an optional .raccoonci/affected.yml that says it
// outright. Anything else falls back to "affected".
package affected

import (
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"raccoonci/internal/trigger"
)

// Package is a unit of the repository that other units can depend on: a Go
// module, an npm/pnpm/yarn workspace package or a Cargo crate.
type Package struct {
	Dir  string   // slash-separated, relative to the repository root; "." for the root
	Name string   // the name others refer to it by
	Kind string   // go, npm or cargo
	Deps []string // Dirs of the workspace packages it depends on
}

const (
	maxManifest = 4 << 20
	maxDirs     = 5000
)

// skipDirs are never searched for packages.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "target": true, "dist": true, "build": true}

// Packages finds the workspace packages of the repository and their local
// dependencies. A repository with no workspace has none (a lone Go module or
// npm package at the root is not listed: it would be the whole repository).
func Packages(repo fs.FS) []Package {
	var all []Package
	all = append(all, goPackages(repo)...)
	all = append(all, npmPackages(repo)...)
	all = append(all, cargoPackages(repo)...)
	for i := range all {
		slices.Sort(all[i].Deps)
		all[i].Deps = slices.Compact(all[i].Deps)
	}
	slices.SortFunc(all, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })
	return all
}

func read(repo fs.FS, name string) ([]byte, bool) {
	f, err := repo.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		b = append(b, buf[:n]...)
		if len(b) > maxManifest {
			return nil, false
		}
		if err != nil {
			break
		}
	}
	return b, true
}

// dirs lists the directories matching the workspace patterns (GitHub-style
// globs, with ! to exclude, as pnpm and npm write them).
func dirs(repo fs.FS, patterns []string) []string {
	var out []string
	n := 0
	fs.WalkDir(repo, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if skipDirs[d.Name()] && p != "." {
			return fs.SkipDir
		}
		if n++; n > maxDirs {
			return fs.SkipAll
		}
		if p == "." {
			return nil
		}
		if ok, err := trigger.MatchList(patterns, p); err == nil && ok {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ---- Go ----

func goPackages(repo fs.FS) []Package {
	var uses []string
	if b, ok := read(repo, "go.work"); ok {
		uses = goWorkUses(string(b))
	}
	// Modules in the workspace, or the root module with local replaces.
	if len(uses) == 0 {
		return nil
	}
	type mod struct {
		dir, path string
		reqs      []string
		replaces  map[string]string // module path -> relative dir
	}
	var mods []*mod
	byPath := map[string]*mod{}
	for _, u := range uses {
		u = path.Clean(u)
		b, ok := read(repo, path.Join(u, "go.mod"))
		if !ok {
			continue
		}
		m := &mod{dir: u, replaces: map[string]string{}}
		m.path, m.reqs = parseGoMod(string(b), m.replaces)
		mods = append(mods, m)
		byPath[m.path] = m
	}
	var out []Package
	for _, m := range mods {
		p := Package{Dir: m.dir, Name: m.path, Kind: "go"}
		for _, r := range m.reqs {
			if dep, ok := byPath[r]; ok && dep != m {
				p.Deps = append(p.Deps, dep.dir)
			}
		}
		for _, rel := range m.replaces {
			if d := path.Join(m.dir, rel); isLocal(rel) && slices.ContainsFunc(mods, func(o *mod) bool { return o.dir == d }) {
				p.Deps = append(p.Deps, d)
			}
		}
		out = append(out, p)
	}
	return out
}

func isLocal(p string) bool { return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") }

func goWorkUses(src string) []string {
	var out []string
	block := ""
	for _, line := range strings.Split(src, "\n") {
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case block == "use" && line == ")":
			block = ""
		case block == "use":
			out = append(out, strings.Trim(line, `"`))
		case line == "use (":
			block = "use"
		case strings.HasPrefix(line, "use "):
			out = append(out, strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "use ")), `"`))
		}
	}
	return out
}

// parseGoMod reads the module path, the required module paths, and the
// replace directives to local directories (into replaces).
func parseGoMod(src string, replaces map[string]string) (module string, reqs []string) {
	block := ""
	for _, line := range strings.Split(src, "\n") {
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == ")" {
			block = ""
			continue
		}
		if strings.HasSuffix(line, "(") {
			block = strings.TrimSpace(strings.TrimSuffix(line, "("))
			continue
		}
		fields := strings.Fields(line)
		kind := block
		if kind == "" {
			kind, fields = fields[0], fields[1:]
		}
		switch kind {
		case "module":
			if len(fields) > 0 {
				module = strings.Trim(fields[0], `"`)
			}
		case "require":
			if len(fields) > 0 {
				reqs = append(reqs, strings.Trim(fields[0], `"`))
			}
		case "replace":
			if i := slices.Index(fields, "=>"); i > 0 && i+1 < len(fields) {
				replaces[strings.Trim(fields[0], `"`)] = strings.Trim(fields[i+1], `"`)
			}
		}
	}
	return module, reqs
}

// ---- npm, pnpm, yarn ----

func npmPackages(repo fs.FS) []Package {
	var patterns []string
	if b, ok := read(repo, "package.json"); ok {
		var root struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(b, &root) == nil && len(root.Workspaces) > 0 {
			var list []string
			var obj struct {
				Packages []string `json:"packages"`
			}
			if json.Unmarshal(root.Workspaces, &list) == nil {
				patterns = list
			} else if json.Unmarshal(root.Workspaces, &obj) == nil {
				patterns = obj.Packages
			}
		}
	}
	if b, ok := read(repo, "pnpm-workspace.yaml"); ok {
		var ws struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(b, &ws) == nil {
			patterns = append(patterns, ws.Packages...)
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	for i, p := range patterns {
		patterns[i] = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
	}
	type pkg struct {
		dir, name string
		deps      []string // dependency names
	}
	var pkgs []*pkg
	byName := map[string]*pkg{}
	for _, d := range dirs(repo, patterns) {
		b, ok := read(repo, path.Join(d, "package.json"))
		if !ok {
			continue
		}
		var m struct {
			Name                                                                  string
			Dependencies, DevDependencies, PeerDependencies, OptionalDependencies map[string]string
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		p := &pkg{dir: d, name: m.Name}
		for _, deps := range []map[string]string{m.Dependencies, m.DevDependencies, m.PeerDependencies, m.OptionalDependencies} {
			for n := range deps {
				p.deps = append(p.deps, n)
			}
		}
		pkgs = append(pkgs, p)
		if m.Name != "" {
			byName[m.Name] = p
		}
	}
	var out []Package
	for _, p := range pkgs {
		o := Package{Dir: p.dir, Name: p.name, Kind: "npm"}
		for _, n := range p.deps {
			if dep, ok := byName[n]; ok && dep != p {
				o.Deps = append(o.Deps, dep.dir)
			}
		}
		out = append(out, o)
	}
	return out
}

// ---- Cargo ----

func cargoPackages(repo fs.FS) []Package {
	b, ok := read(repo, "Cargo.toml")
	if !ok {
		return nil
	}
	root := parseTOML(string(b))
	members := root.array("workspace", "members")
	if len(members) == 0 {
		return nil
	}
	exclude := root.array("workspace", "exclude")
	patterns := slices.Clone(members)
	for _, e := range exclude {
		patterns = append(patterns, "!"+e)
	}
	var out []Package
	byDir := map[string]bool{}
	for _, d := range dirs(repo, patterns) {
		if _, ok := read(repo, path.Join(d, "Cargo.toml")); ok {
			byDir[d] = true
		}
	}
	docs := map[string]tomlDoc{}
	nameDir := map[string]string{}
	for d := range byDir {
		src, _ := read(repo, path.Join(d, "Cargo.toml"))
		docs[d] = parseTOML(string(src))
		if n := docs[d].str("package", "name"); n != "" {
			nameDir[n] = d
		}
	}
	for d := range byDir {
		t := docs[d]
		p := Package{Dir: d, Name: t.str("package", "name"), Kind: "cargo"}
		for _, table := range []string{"dependencies", "dev-dependencies", "build-dependencies"} {
			for _, rel := range t.paths(table) {
				if dep := path.Join(d, rel); byDir[dep] {
					p.Deps = append(p.Deps, dep)
				}
			}
			// A dependency named like a member is taken to be that member, as
			// a [patch] or the workspace usually makes it. Over-approximating
			// only means more gets run.
			for _, n := range t.depNames(table) {
				if dep, ok := nameDir[n]; ok && dep != d {
					p.Deps = append(p.Deps, dep)
				}
			}
		}
		slices.Sort(p.Deps)
		p.Deps = slices.Compact(p.Deps)
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })
	return out
}

// tomlDoc is the little of TOML Cargo.toml needs: tables, strings, arrays of
// strings, inline tables with a path key. Anything fancier is skipped.
type tomlDoc struct {
	tables map[string]map[string]string // table -> key -> raw value
}

func parseTOML(src string) tomlDoc {
	doc := tomlDoc{tables: map[string]map[string]string{}}
	table := ""
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(strings.Trim(line, "[]"), " ")
			if doc.tables[table] == nil {
				doc.tables[table] = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.Trim(strings.TrimSpace(k), `"`), strings.TrimSpace(v)
		// An array or inline table may continue on the next lines.
		for depth(v) > 0 && i+1 < len(lines) {
			i++
			v += " " + strings.TrimSpace(stripComment(lines[i]))
		}
		if doc.tables[table] == nil {
			doc.tables[table] = map[string]string{}
		}
		doc.tables[table][k] = v
	}
	return doc
}

func depth(s string) int {
	d, inStr := 0, false
	for _, c := range s {
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		}
	}
	return d
}

func stripComment(s string) string {
	inStr := false
	for i, c := range s {
		if c == '"' {
			inStr = !inStr
		}
		if c == '#' && !inStr {
			return s[:i]
		}
	}
	return s
}

func quoted(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i+1:], '"')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+1+j])
		s = s[i+j+2:]
	}
}

func (d tomlDoc) array(table, key string) []string { return quoted(d.tables[table][key]) }

func (d tomlDoc) str(table, key string) string {
	if q := quoted(d.tables[table][key]); len(q) > 0 {
		return q[0]
	}
	return ""
}

// depNames lists the dependencies declared in a table, inline or as
// [table.name].
func (d tomlDoc) depNames(table string) []string {
	var out []string
	for k := range d.tables[table] {
		out = append(out, k)
	}
	for name := range d.tables {
		if rest, ok := strings.CutPrefix(name, table+"."); ok && !strings.Contains(rest, ".") {
			out = append(out, strings.Trim(rest, `"`))
		}
	}
	return out
}

// paths returns the path = "..." values of the dependencies in a table, in
// both the inline form (foo = { path = "../foo" }) and the table form
// ([dependencies.foo] path = "../foo").
func (d tomlDoc) paths(table string) []string {
	var out []string
	for _, v := range d.tables[table] {
		if i := strings.Index(v, "path"); i >= 0 {
			if q := quoted(v[i:]); len(q) > 0 {
				out = append(out, q[0])
			}
		}
	}
	for name, t := range d.tables {
		if strings.HasPrefix(name, table+".") {
			if v, ok := t["path"]; ok {
				out = append(out, quoted(v)...)
			}
		}
	}
	return out
}
