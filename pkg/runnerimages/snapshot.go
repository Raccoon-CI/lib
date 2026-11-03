package runnerimages

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// The snapshot is the fallback for a first start or no network. Refresh it
// with `make runner-images`. Once the server exists it will keep fresher data
// in its database, polled from GitHub.
//
//go:embed snapshot/*.json
var snapshotFS embed.FS

// Snapshot holds Defaults per image, plus where moving labels such as
// ubuntu-latest currently point.
type Snapshot struct {
	Aliases map[string]string // label -> image, e.g. ubuntu-latest -> ubuntu24
	Images  map[string]*Defaults
}

const aliasFile = "aliases.json"

// Embedded returns the snapshot compiled into the binary.
func Embedded() (*Snapshot, error) {
	s := &Snapshot{Aliases: map[string]string{}, Images: map[string]*Defaults{}}
	entries, err := snapshotFS.ReadDir("snapshot")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := snapshotFS.ReadFile("snapshot/" + e.Name())
		if err != nil {
			return nil, err
		}
		if e.Name() == aliasFile {
			if err := json.Unmarshal(b, &s.Aliases); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			continue
		}
		var d Defaults
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		s.Images[d.Image] = &d
	}
	return s, nil
}

// WriteDir writes s as snapshot files, one per image plus the aliases.
func (s *Snapshot) WriteDir(dir string) error {
	write := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
	}
	for image, d := range s.Images {
		if err := write(image+".json", d); err != nil {
			return err
		}
	}
	return write(aliasFile, s.Aliases)
}

// maxWireSnapshot caps a snapshot read from a controller (the real one is
// about 10 KB).
const maxWireSnapshot = 1 << 20

type wireSnapshot struct {
	Aliases map[string]string    `json:"aliases"`
	Images  map[string]*Defaults `json:"images"`
}

// JSON is the snapshot as the server hands it to a machine with a job, so that
// the machine plans with the same preinstalled-tools data as the server.
func (s *Snapshot) JSON() ([]byte, error) {
	return json.Marshal(wireSnapshot{Aliases: s.Aliases, Images: s.Images})
}

// ParseSnapshot reads what JSON wrote. It refuses what could not have come
// from a real snapshot (no images, an image under another name, too much), and
// drops an alias that points at an image it does not hold.
func ParseSnapshot(b []byte) (*Snapshot, error) {
	if len(b) > maxWireSnapshot {
		return nil, fmt.Errorf("runnerimages: the snapshot is %d bytes, more than the %d allowed", len(b), maxWireSnapshot)
	}
	var w wireSnapshot
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("runnerimages: %w", err)
	}
	if len(w.Images) == 0 || len(w.Images) > 64 {
		return nil, fmt.Errorf("runnerimages: a snapshot holds 1 to 64 images, this one %d", len(w.Images))
	}
	s := &Snapshot{Aliases: map[string]string{}, Images: map[string]*Defaults{}}
	for name, d := range w.Images {
		if d == nil || d.Image != name {
			return nil, fmt.Errorf("runnerimages: the image %q is stored under another name", name)
		}
		s.Images[name] = d
	}
	for label, image := range w.Aliases {
		if s.Images[image] != nil {
			s.Aliases[label] = image
		}
	}
	return s, nil
}

// ForLabel returns the Defaults for a runs-on label such as ubuntu-latest or
// ubuntu-22.04, or nil if the label isn't a GitHub-hosted image we track.
// Which labels name which images is the catalog's knowledge (nil means
// catalog.Default()); where a moving label points is the snapshot's.
func (s *Snapshot) ForLabel(cat *catalog.Catalog, label string) *Defaults {
	if cat == nil {
		cat = catalog.Default()
	}
	label = strings.ToLower(label)
	if image, ok := s.Aliases[label]; ok {
		return s.Images[image]
	}
	if image := cat.ImageForLabel(label); image != "" {
		return s.Images[image]
	}
	return nil
}
