package runnerimages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures are the real files of release ubuntu24/20261023.334.
func TestParseReport(t *testing.T) {
	d, err := ParseReport(nil, "ubuntu24", "20261023.334", readTestdata(t, "internal.ubuntu24.json"), readTestdata(t, "toolset-2404.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"node":   "22.23.3",
		"python": "3.12.3", // the system python3, not a toolcache one
		"go":     "1.24.13",
		"java":   "17.0.20+1",
		"ruby":   "3.2.3",
		"rust":   "1.99.0",
	}
	for tool, v := range want {
		if d.Tools[tool] != v {
			t.Errorf("default %s = %q, want %q", tool, d.Tools[tool], v)
		}
	}
	if got := d.Cached["python"]; !slices.Contains(got, "3.13.16") {
		t.Errorf("cached python = %v, want it to include 3.13.16", got)
	}
	for _, pkg := range []string{"gcc", "make", "curl"} {
		if !slices.Contains(d.Packages, pkg) {
			t.Errorf("apt packages lack %q", pkg)
		}
	}
	for _, sw := range []string{"jq", "yq", "kubectl", "yarn"} {
		if !slices.Contains(d.Software, sw) {
			t.Errorf("software lacks %q", sw)
		}
	}
}

func TestParseReportRejectsChangedFormat(t *testing.T) {
	_, err := ParseReport(nil, "ubuntu24", "1", []byte(`{"NodeType":"HeaderNode","Title":"Ubuntu 24.04","Children":[]}`), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "format may have changed") {
		t.Fatalf("got %v", err)
	}
}

// Where a toolchain's default lives in the report is catalog data too.
func TestParseReportFollowsCatalog(t *testing.T) {
	cat, err := catalog.Default().Overlay([]byte(`
toolchains:
  julia:
    title: Julia
    image:
      default: {section: Language and Runtime, tool: Julia}
`))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseReport(cat, "ubuntu24", "20261023.334", readTestdata(t, "internal.ubuntu24.json"), readTestdata(t, "toolset-2404.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Tools["julia"] != "1.13.1" {
		t.Errorf("no default from the catalog's new entry: %v", d.Tools)
	}
}

// fakeGitHub serves the files Fetcher reads, recording which paths were hit
// and whether a token was sent where.
type fakeGitHub struct {
	badge    string // body of the ubuntu24 badge; "" means 404
	releases string // body of the releases API
	hits     []string
	tokenOn  []string
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.hits = append(g.hits, r.URL.Path)
	if r.Header.Get("Authorization") != "" {
		g.tokenOn = append(g.tokenOn, r.URL.Path)
	}
	switch {
	case r.URL.Path == "/badge/ubuntu24.json" && g.badge != "":
		w.Write([]byte(g.badge))
	case r.URL.Path == "/api/releases":
		w.Write([]byte(g.releases))
	case r.URL.Path == "/download/ubuntu24/20261023.334/internal.ubuntu24.json":
		w.Write(readFile("testdata/internal.ubuntu24.json"))
	case r.URL.Path == "/raw/ubuntu24/20261023.334/images/ubuntu/toolsets/toolset-2404.json":
		w.Write(readFile("testdata/toolset-2404.json"))
	default:
		http.NotFound(w, r)
	}
}

func readFile(name string) []byte {
	b, _ := os.ReadFile(name)
	return b
}

func newTestFetcher(t *testing.T, g *fakeGitHub) *Fetcher {
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return &Fetcher{
		Client:       srv.Client(),
		Token:        "secret",
		BadgeBase:    srv.URL + "/badge",
		DownloadBase: srv.URL + "/download",
		RawBase:      srv.URL + "/raw",
		APIBase:      srv.URL + "/api",
	}
}

const releasesJSON = `[
	{"tag_name": "ubuntu24/20261026.354", "prerelease": true},
	{"tag_name": "ubuntu22/20261023.322", "prerelease": false},
	{"tag_name": "ubuntu24/20261023.334", "prerelease": false}
]`

func TestLatestUsesBadgeWithoutAPI(t *testing.T) {
	g := &fakeGitHub{badge: `{"label": "20261023.334", "message": "Deploying to 20261026.354"}`}
	d, err := newTestFetcher(t, g).Latest(context.Background(), "ubuntu24")
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "20261023.334" {
		t.Errorf("version = %s", d.Version)
	}
	for _, p := range g.hits {
		if strings.HasPrefix(p, "/api/") {
			t.Errorf("primary path called the API: %s", p)
		}
	}
	if len(g.tokenOn) > 0 {
		t.Errorf("token sent outside the API: %v", g.tokenOn)
	}
}

func TestLatestFallsBackToAPI(t *testing.T) {
	for name, badge := range map[string]string{
		"badge gone":          "",
		"badge format change": `{"label": "Ubuntu 24.04", "message": "ok"}`,
		"badge names missing": `{"label": "20261099.1"}`, // its report 404s
	} {
		t.Run(name, func(t *testing.T) {
			g := &fakeGitHub{badge: badge, releases: releasesJSON}
			d, err := newTestFetcher(t, g).Latest(context.Background(), "ubuntu24")
			if err != nil {
				t.Fatal(err)
			}
			if d.Version != "20261023.334" {
				t.Errorf("version = %s, want the newest non-prerelease 20261023.334", d.Version)
			}
			if !slices.Equal(g.tokenOn, []string{"/api/releases"}) {
				t.Errorf("token sent on %v, want only the API", g.tokenOn)
			}
		})
	}
}

func TestParseLatestAlias(t *testing.T) {
	readme := "| Ubuntu 26.04 | x64 | `ubuntu-26.04` | [ubuntu-26.04] |\n" +
		"| Ubuntu 24.04 | x64 | `ubuntu-latest` or `ubuntu-24.04` | [ubuntu-24.04] |\n"
	cat := catalog.Default()
	if got, err := parseLatestAlias(cat, readme); err != nil || got != "ubuntu24" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := parseLatestAlias(cat, "no table here"); err == nil {
		t.Error("expected an error without the table")
	}

	// The labels come from the catalog: a new image needs no code, and
	// the alias row can name any image the catalog tracks.
	next, err := cat.Overlay([]byte("images:\n  ubuntu28: {label: ubuntu-28.04, codename: x, toolset: toolset-2804.json}\n"))
	if err != nil {
		t.Fatal(err)
	}
	row := "| Ubuntu 28.04 | `ubuntu-latest` or `ubuntu-28.04` |\n"
	if got, err := parseLatestAlias(next, row); err != nil || got != "ubuntu28" {
		t.Errorf("new image: got %q, %v", got, err)
	}
	if _, err := parseLatestAlias(cat, row); err == nil {
		t.Error("an image the catalog doesn't track must not be returned")
	}
}

func TestEmbeddedSnapshot(t *testing.T) {
	s, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if d := s.ForLabel(nil, "ubuntu-latest"); d == nil || d.Tools["node"] == "" {
		t.Fatalf("ubuntu-latest has no defaults: %+v", d)
	}
	if s.ForLabel(nil, "Ubuntu-22.04") == nil {
		t.Error("ubuntu-22.04 missing")
	}
	if s.ForLabel(nil, "self-hosted") != nil {
		t.Error("non-GitHub label matched")
	}
	// Every image the catalog tracks has an embedded snapshot.
	for _, image := range catalog.Default().ImageNames() {
		if s.Images[image] == nil {
			t.Errorf("no embedded snapshot for %s; run make runner-images", image)
		}
	}
}
