// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"time"
)

// Member is one file inside an archive.
type Member struct {
	// Name is the path INSIDE the archive.
	Name string
	// Source is the path on disk.
	Source string
	// Mode is the recorded unix mode.
	Mode int64
}

// epoch is the fixed timestamp stamped into every archive entry. Determinism
// is the exit criterion for T0 ("byte-for-byte where the build is
// deterministic"), and a real mtime makes two identical inputs produce two
// different archives — which would make a checksum comparison meaningless.
var epoch = time.Unix(0, 0).UTC()

// WriteArchive writes members into dest, selecting the writer by format.
// Members are sorted by in-archive name so the byte stream does not depend on
// filesystem iteration order.
func WriteArchive(dest, format string, members []Member) error {
	sorted := append([]Member(nil), members...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	switch format {
	case "tar.gz", "tgz":
		return writeTarGz(f, sorted)
	case "zip":
		return writeZip(f, sorted)
	case "binary":
		if len(sorted) != 1 {
			return fmt.Errorf("release: format binary needs exactly 1 member, got %d", len(sorted))
		}
		src, err := os.Open(sorted[0].Source)
		if err != nil {
			return err
		}
		defer src.Close()
		if _, err := io.Copy(f, src); err != nil {
			return err
		}
		return os.Chmod(dest, os.FileMode(sorted[0].Mode))
	default:
		return fmt.Errorf("%w: archive format %q (supported: tar.gz, tgz, zip, binary)", ErrUnsupportedStage, format)
	}
}

func writeTarGz(w io.Writer, members []Member) error {
	// Header{} with no Name/ModTime: gzip's own header carries a filename and
	// mtime by default, which would defeat determinism.
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.ModTime = epoch
	tw := tar.NewWriter(gz)
	for _, m := range members {
		info, err := os.Stat(m.Source)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     m.Name,
			Mode:     m.Mode,
			Size:     info.Size(),
			ModTime:  epoch,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if err := copyInto(tw, m.Source); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(w io.Writer, members []Member) error {
	zw := zip.NewWriter(w)
	for _, m := range members {
		hdr := &zip.FileHeader{
			Name:     path.Clean(m.Name),
			Method:   zip.Deflate,
			Modified: epoch,
		}
		hdr.SetMode(os.FileMode(m.Mode))
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if err := copyInto(fw, m.Source); err != nil {
			return err
		}
	}
	return zw.Close()
}

func copyInto(w io.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
