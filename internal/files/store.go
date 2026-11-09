// Package files stores uploaded files on a private disk by id; the database holds
// their metadata and where they are attached (REQUIREMENTS 5.12).
package files

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// Store keeps file bytes under Dir/<first two hex chars>/<uuid>. Files are written
// once and never changed, so backups can copy them incrementally.
type Store struct{ Dir string }

func (s Store) path(id uuid.UUID) string {
	h := id.String()
	return filepath.Join(s.Dir, h[:2], h)
}

// Save copies r (at most max bytes) to a new file and returns its size and SHA-256.
// The file appears under its final name only when complete.
func (s Store) Save(id uuid.UUID, r io.Reader, max int64) (int64, []byte, error) {
	dst := s.path(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return 0, nil, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, nil, err
	}
	if n > max {
		return n, nil, ErrTooLarge
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, nil, err
	}
	return n, h.Sum(nil), nil
}

// Open returns the file's bytes.
func (s Store) Open(id uuid.UUID) (*os.File, error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("open stored file %s: %w", id, err)
	}
	return f, nil
}

// Remove deletes bytes of a file whose database record was never committed.
func (s Store) Remove(id uuid.UUID) { _ = os.Remove(s.path(id)) }

// ErrTooLarge reports an upload over the limit.
var ErrTooLarge = errors.New("file too large")
