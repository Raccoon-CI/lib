// Package runnerimages tracks what GitHub's hosted Ubuntu runner images have
// installed, so jobs that silently rely on preinstalled tools get the same
// versions on Raccoon CI's machines (which run Debian, not Ubuntu).
//
// The data comes from actions/runner-images: each image release publishes a
// software report (internal.<image>.json), and the repo's toolsets say which
// cached version is the default where the report doesn't.
package runnerimages

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
)

// Defaults is what one runner image release provides.
type Defaults struct {
	Image   string `json:"image"`   // e.g. ubuntu24
	Version string `json:"version"` // e.g. 20261023.334

	// Tools maps a toolchain (node, python, go, ... as the catalog names
	// them) to the version a job gets without any setup-* action.
	Tools map[string]string `json:"tools"`

	// Cached lists the toolcache versions per toolchain. On GitHub, setup-*
	// actions resolve these without downloading.
	Cached map[string][]string `json:"cached"`

	// Packages are the apt packages installed on the image.
	Packages []string `json:"packages"`

	// Software holds the lower-cased names of other preinstalled tools as
	// the report spells them (jq, kubectl, github cli, docker client, clang,
	// powershell, ...).
	Software []string `json:"software"`
}

// reportNode is one node of a runner-images software report.
type reportNode struct {
	NodeType string
	Title    string
	ToolName string
	Version  string
	Versions []string
	Rows     []string
	Children nodeList
}

// nodeList accepts both a list and a single node: the reports are written by
// PowerShell, which serializes one-element arrays as plain objects.
type nodeList []reportNode

func (l *nodeList) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, (*[]reportNode)(l))
	}
	var n reportNode
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*l = nodeList{n}
	return nil
}

func (n *reportNode) section(title string) *reportNode {
	if n.NodeType == "HeaderNode" && n.Title == title {
		return n
	}
	for i := range n.Children {
		if s := n.Children[i].section(title); s != nil {
			return s
		}
	}
	return nil
}

func (n *reportNode) tool(name string) *reportNode {
	for i := range n.Children {
		if c := &n.Children[i]; c.ToolName == name {
			return c
		}
	}
	return nil
}

func (n *reportNode) table() []string {
	for _, c := range n.Children {
		if c.NodeType == "TableNode" {
			return c.Rows
		}
	}
	return nil
}

// toolset is the part of images/ubuntu/toolsets/toolset-XXXX.json we read.
type toolset struct {
	Toolcache []struct {
		Name    string `json:"name"`
		Default string `json:"default"`
	} `json:"toolcache"`
}

// ParseReport extracts Defaults from a software report and the toolset of the
// same image release. The catalog says where each toolchain's default and
// cached versions are; cat defaults to catalog.Default().
func ParseReport(cat *catalog.Catalog, image, version string, report, toolsetJSON []byte) (*Defaults, error) {
	if cat == nil {
		cat = catalog.Default()
	}
	var root reportNode
	if err := json.Unmarshal(report, &root); err != nil {
		return nil, fmt.Errorf("software report: %w", err)
	}
	var ts toolset
	if err := json.Unmarshal(toolsetJSON, &ts); err != nil {
		return nil, fmt.Errorf("toolset: %w", err)
	}

	d := &Defaults{Image: image, Version: version, Tools: map[string]string{}, Cached: map[string][]string{}}
	cached := root.section("Cached Tools")
	for _, name := range cat.ToolchainNames() {
		src := cat.Toolchains[name].Image
		if src == nil {
			continue
		}
		if cached != nil && src.Cached != "" {
			if t := cached.tool(src.Cached); t != nil {
				d.Cached[name] = t.Versions
			}
		}
		if v := defaultVersion(&root, src, &ts, d.Cached[name]); v != "" {
			d.Tools[name] = v
		} else if src.Required {
			return nil, fmt.Errorf("software report has no default %s version; its format may have changed", cat.Title(name))
		}
	}

	if apt := root.section("Installed apt packages"); apt != nil {
		for _, row := range apt.table() { // "jq|1.7.1-3build1"
			name, _, _ := strings.Cut(row, "|")
			d.Packages = append(d.Packages, name)
		}
	}
	for _, title := range cat.ImageSoftwareSections {
		if s := root.section(title); s != nil {
			for _, c := range s.Children {
				if c.ToolName != "" {
					d.Software = append(d.Software, strings.ToLower(c.ToolName))
				}
			}
		}
	}
	slices.Sort(d.Packages)
	slices.Sort(d.Software)
	d.Software = slices.Compact(d.Software)
	return d, nil
}

// defaultVersion finds a toolchain's default version where the catalog says
// it is: a tool entry, a table row with a marker, or a toolset pattern.
func defaultVersion(root *reportNode, src *catalog.ImageSource, ts *toolset, cached []string) string {
	def := src.Default
	switch {
	case def.Toolset != "":
		// A toolcache pattern such as "1.24.*", resolved to the newest
		// matching cached version (they are listed oldest first).
		version := ""
		for _, tc := range ts.Toolcache {
			if tc.Name != def.Toolset || tc.Default == "" {
				continue
			}
			prefix := strings.TrimSuffix(tc.Default, "*")
			for _, v := range cached {
				if strings.HasPrefix(v, prefix) {
					version = v
				}
			}
		}
		return version
	case def.TableMarker != "":
		if s := root.section(def.Section); s != nil {
			for _, row := range s.table() { // "17.0.20+1 (default)|JAVA_HOME_17_X64"
				v, _, _ := strings.Cut(row, "|")
				if v, ok := strings.CutSuffix(v, " "+def.TableMarker); ok {
					return v
				}
			}
		}
	default:
		if s := root.section(def.Section); s != nil {
			if t := s.tool(def.Tool); t != nil {
				return t.Version
			}
		}
	}
	return ""
}
