// Package platform reads runs-on values and, with the catalog's knowledge of
// runner labels and operating systems, says which OS a job asks for and
// whether Raccoon CI can provide it.
package platform

import (
	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/value"
)

// FromRunsOn infers the OS an evaluated runs-on value asks for, from the
// catalog's runner-label rules (GitHub's hosted labels such as ubuntu-latest
// or windows-2022, and the self-hosted OS labels linux, windows, macos). It
// returns "" when no label names an OS, e.g. for custom self-hosted labels
// only. cat defaults to catalog.Default().
func FromRunsOn(cat *catalog.Catalog, runsOn any) string {
	if cat == nil {
		cat = catalog.Default()
	}
	for _, l := range Labels(runsOn) {
		if os := cat.OSOfLabel(l); os != "" {
			return os
		}
	}
	return ""
}

// Labels returns the labels of a runs-on value: a string, a list of
// strings, or a {group, labels} mapping.
func Labels(runsOn any) []string {
	switch v := runsOn.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case value.Object:
		l, _ := v.Get("labels")
		return Labels(l)
	}
	return nil
}
