package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
)

// sha256Hex returns the lower-case hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// verifyAsset checks data against the pinned hash. A mismatch means nothing
// is written.
func verifyAsset(a Asset, data []byte) error {
	if got := sha256Hex(data); got != a.SHA256 {
		return fmt.Errorf("sha256 mismatch for %s: pinned %s, downloaded %s; nothing was installed",
			a.URL, a.SHA256, got)
	}
	return nil
}

// file is one file to write.
type file struct {
	Dest string
	Data []byte
	Mode fs.FileMode
}

// untarGz returns the regular files of a gzip'd tar archive that match,
// placed in dir under their base name. Mode is always 0755.
func untarGz(data []byte, match func(h *tar.Header) bool, dir string) ([]file, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var out []file
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || !match(h) {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 512<<20))
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", h.Name, err)
		}
		out = append(out, file{Dest: path.Join(dir, path.Base(h.Name)), Data: body, Mode: 0o755})
	}
	if len(out) == 0 {
		return nil, errors.New("archive has no files to install")
	}
	return out, nil
}

// matchBinDir picks the containerd tarball's bin/ entries.
func matchBinDir(h *tar.Header) bool {
	return path.Dir(path.Clean(h.Name)) == "bin"
}

// matchExecutable picks the executable files of the CNI tarball (plugins
// only; LICENSE and README are skipped).
func matchExecutable(h *tar.Header) bool {
	return h.Mode&0o111 != 0
}
