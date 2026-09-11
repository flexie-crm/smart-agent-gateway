// Package filestore keeps the bytes somebody uploaded.
//
// It is deliberately the dullest thing in the codebase: a folder, one file per
// attachment, named after that attachment's own public id. Object storage comes
// later and replaces this implementation without moving the seam, because the
// seam is already "give me these bytes for this id" rather than "here is a
// path".
//
// # The rule this package exists to enforce
//
// A NAME A PERSON TYPED NEVER REACHES A PATH. The file's name is data: it is
// kept in a column so it can be shown back, and it is not used to decide where
// anything is written. The bytes go under the attachment's public id, which
// this product minted out of 128 bits of randomness and which cannot contain a
// separator, a dot-dot, a null byte, or anything else somebody thought of.
//
// That is the whole defence, and it is a structural one: there is no sanitising
// step to get wrong, because there is no untrusted input on the path at all.
package filestore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store writes and reads attachment bytes under one root directory.
type Store struct{ root string }

// ErrNotFound is what a missing file looks like, so a caller does not have to
// know that this is a filesystem.
var ErrNotFound = errors.New("filestore: no such file")

// New prepares a store rooted at dir, creating it if it is not there. It fails
// at startup rather than at the first upload: a deployment whose upload
// directory cannot be written to should say so while somebody is watching.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("filestore: no directory configured")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: create %q: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Root is where the bytes live. For logs and for tests.
func (s *Store) Root() string { return s.root }

// Write stores the bytes of one attachment and returns how many there were.
//
// It stops at limit and reports an error rather than writing a truncated file
// somebody would later read as if it were whole: a half-written PDF is worse
// than a refused one. The partial file is removed on any failure, so a failed
// upload leaves nothing behind to account for.
func (s *Store) Write(workspaceID int64, id string, r io.Reader, limit int64) (int64, error) {
	path, err := s.path(workspaceID, id)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, fmt.Errorf("filestore: create folder: %w", err)
	}

	//nolint:gosec // path comes from s.path, which refuses anything but a token
	// we minted: no separator, no dot, so it cannot leave the root.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("filestore: create file: %w", err)
	}
	// One extra byte, so a file exactly at the limit is kept and one over it is
	// caught rather than silently trimmed to the limit.
	written, err := io.Copy(f, io.LimitReader(r, limit+1))
	closeErr := f.Close()
	switch {
	case err != nil:
		_ = os.Remove(path)
		return 0, fmt.Errorf("filestore: write: %w", err)
	case closeErr != nil:
		_ = os.Remove(path)
		return 0, fmt.Errorf("filestore: close: %w", closeErr)
	case written > limit:
		_ = os.Remove(path)
		return 0, fmt.Errorf("filestore: the file is larger than %d bytes", limit)
	}
	return written, nil
}

// Open reads an attachment back.
func (s *Store) Open(workspaceID int64, id string) (*os.File, error) {
	path, err := s.path(workspaceID, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // the path is built from an id we minted
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("filestore: open: %w", err)
	}
	return f, nil
}

// Remove deletes an attachment's bytes. A file that is already gone is not an
// error: the caller wanted it gone.
func (s *Store) Remove(workspaceID int64, id string) error {
	path, err := s.path(workspaceID, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("filestore: remove: %w", err)
	}
	return nil
}

// path is where one attachment's bytes live, and the only place a path is built
// in this package.
//
// The id is checked rather than trusted, even though this product mints it.
// Everything else here rests on that id being a plain token, and a check that
// can only ever pass is cheap; the day somebody passes a value through from
// somewhere else, this refuses instead of writing outside the root.
func (s *Store) path(workspaceID int64, id string) (string, error) {
	if !safeID(id) {
		return "", fmt.Errorf("filestore: %q is not a usable file id", id)
	}
	return filepath.Join(s.root, strconv.FormatInt(workspaceID, 10), id), nil
}

// safeID accepts what NewAttachmentUID produces and nothing else: letters,
// digits, underscore and the two characters base64url adds. No separator, no
// dot, so no traversal and no hidden file.
func safeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
