package dispatch

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path"
)

// TarDir writes dir as a .tar.gz with everything under one top-level
// directory (the layout archive.ExtractTarGz expects). Only directories,
// regular files and symlinks are included; reads go through os.Root.
func TarDir(w io.Writer, dir string) error { return TarDirSkipping(w, dir, nil) }

// TarDirSkipping is TarDir without the paths skip approves. skip gets the
// slash-separated path relative to dir; a skipped directory is left out with
// everything below it.
func TarDirSkipping(w io.Writer, dir string, skip func(rel string, isDir bool) bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip != nil && p != "." && skip(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := path.Join("src", p)
		h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		switch {
		case info.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
			return tw.WriteHeader(h)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, target
			return tw.WriteHeader(h)
		case info.Mode().IsRegular():
			f, err := root.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			return err
		}
		return nil // sockets, devices
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}
