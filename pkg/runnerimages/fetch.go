package runnerimages

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// Fetcher finds the image release GitHub has fully deployed and downloads its
// data. The primary path never touches api.github.com, so it needs no token
// and doesn't compete for the 60 requests/hour that unauthenticated API
// callers on a shared IP get. The API is only a fallback.
type Fetcher struct {
	Client *http.Client
	// Token, if set, authenticates the API fallback (5000 requests/hour).
	Token string
	// Catalog says what to read from the reports; nil means catalog.Default().
	Catalog *catalog.Catalog

	// Base URLs, overridable in tests.
	BadgeBase    string // deployment badges: <base>/<image>.json
	DownloadBase string // release assets: <base>/<tag>/<asset>
	RawBase      string // repo files: <base>/<ref>/<path>
	APIBase      string // releases API
}

func NewFetcher(token string) *Fetcher {
	return &Fetcher{
		Client:       http.DefaultClient,
		Token:        token,
		BadgeBase:    "https://gist.githubusercontent.com/hosted-runners-images-bot/79267492faab096d04cdd25ce7014cec/raw",
		DownloadBase: "https://github.com/actions/runner-images/releases/download",
		RawBase:      "https://raw.githubusercontent.com/actions/runner-images",
		APIBase:      "https://api.github.com/repos/actions/runner-images",
	}
}

// Latest returns the Defaults of the release of image that is currently
// deployed to all of GitHub's hosted runners (not one still rolling out).
func (f *Fetcher) Latest(ctx context.Context, image string) (*Defaults, error) {
	version, err := f.deployedFromBadge(ctx, image)
	if err == nil {
		d, ferr := f.fetch(ctx, image, version)
		if ferr == nil {
			return d, nil
		}
		err = ferr
	}
	// The badge is a README decoration with no format guarantee, so fall
	// back to the official API when it or the version it names doesn't work.
	version, apiErr := f.deployedFromAPI(ctx, image)
	if apiErr != nil {
		return nil, fmt.Errorf("%s: badge: %v; API fallback: %w", image, err, apiErr)
	}
	return f.fetch(ctx, image, version)
}

var versionPattern = regexp.MustCompile(`^\d{8}\.\d+$`)

// deployedFromBadge reads the README's status badge, whose label is the
// deployed version ("20261023.334") while its message tracks a rollout in
// progress ("Deploying to 20261026.354").
func (f *Fetcher) deployedFromBadge(ctx context.Context, image string) (string, error) {
	body, err := f.get(ctx, f.BadgeBase+"/"+image+".json", false)
	if err != nil {
		return "", err
	}
	var badge struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(body, &badge); err != nil {
		return "", fmt.Errorf("badge: %w", err)
	}
	if !versionPattern.MatchString(badge.Label) {
		return "", fmt.Errorf("badge label %q is not an image version", badge.Label)
	}
	return badge.Label, nil
}

// deployedFromAPI finds the newest release of image that is not a
// prerelease; runner-images keeps releases as prereleases while rolling out.
func (f *Fetcher) deployedFromAPI(ctx context.Context, image string) (string, error) {
	body, err := f.get(ctx, f.APIBase+"/releases?per_page=100", true)
	if err != nil {
		return "", err
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf("releases: %w", err)
	}
	for _, r := range releases { // newest first
		if v, ok := strings.CutPrefix(r.TagName, image+"/"); ok && !r.Draft && !r.Prerelease {
			return v, nil
		}
	}
	return "", fmt.Errorf("no deployed %s release among the latest %d", image, len(releases))
}

func (f *Fetcher) cat() *catalog.Catalog {
	if f.Catalog == nil {
		return catalog.Default()
	}
	return f.Catalog
}

func (f *Fetcher) fetch(ctx context.Context, image, version string) (*Defaults, error) {
	def := f.cat().Images[image]
	if def == nil {
		return nil, fmt.Errorf("the catalog doesn't know image %q", image)
	}
	tag := image + "/" + version
	report, err := f.get(ctx, fmt.Sprintf("%s/%s/internal.%s.json", f.DownloadBase, tag, image), false)
	if err != nil {
		return nil, err
	}
	ts, err := f.get(ctx, fmt.Sprintf("%s/%s/images/ubuntu/toolsets/%s", f.RawBase, tag, def.Toolset), false)
	if err != nil {
		return nil, err
	}
	return ParseReport(f.Catalog, image, version, report, ts)
}

// LatestAlias returns the image the catalog's moving label (ubuntu-latest)
// currently points to, read from the table in runner-images' README.
func (f *Fetcher) LatestAlias(ctx context.Context) (string, error) {
	body, err := f.get(ctx, f.RawBase+"/main/README.md", false)
	if err != nil {
		return "", err
	}
	return parseLatestAlias(f.cat(), string(body))
}

var backticked = regexp.MustCompile("`([^`]+)`")

// parseLatestAlias finds the README table row that names the alias label
// next to an image's own label.
func parseLatestAlias(cat *catalog.Catalog, readme string) (string, error) {
	for _, line := range strings.Split(readme, "\n") {
		names := backticked.FindAllStringSubmatch(line, -1)
		if !slices.ContainsFunc(names, func(m []string) bool { return strings.EqualFold(m[1], cat.ImageAlias) }) {
			continue
		}
		for _, m := range names {
			if image := cat.ImageForLabel(m[1]); image != "" {
				return image, nil
			}
		}
	}
	return "", fmt.Errorf("README no longer says which image %s is", cat.ImageAlias)
}

// maxResponse caps downloads; the largest file we read is ~100 KB.
const maxResponse = 10 << 20

func (f *Fetcher) get(ctx context.Context, url string, api bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		if f.Token != "" {
			req.Header.Set("Authorization", "Bearer "+f.Token)
		}
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponse))
}
