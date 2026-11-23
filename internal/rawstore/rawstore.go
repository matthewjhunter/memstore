// Package rawstore keeps raw session transcripts as files: one compressed
// file per session, under a directory per user.
//
// It stores bytes and knows nothing of what is in them. The caller hands it
// the compressed transcript exactly as it should rest on disk, so the day
// those bytes are encrypted before they arrive, nothing here changes.
//
// A transcript holds tool output -- file contents, and whatever credentials
// passed through a session -- so files are 0600 in 0700 directories, and the
// only client-chosen part of a path, the session id, must be a UUID.
package rawstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/google/uuid"
)

// ErrInvalidSessionID is returned for a session id that is not a UUID in
// canonical form.
var ErrInvalidSessionID = errors.New("rawstore: session id is not a UUID")

const suffix = ".jsonl.gz"

// Store is a directory of raw transcripts.
type Store struct {
	root *os.Root
}

// Open returns the store rooted at dir, creating the directory if needed.
// Every later operation is confined to it by the kernel (os.Root), not by
// path cleaning.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("rawstore: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("rawstore: %w", err)
	}
	return &Store{root: root}, nil
}

// Close releases the directory handle.
func (s *Store) Close() error { return s.root.Close() }

// rel returns the path of a session's file relative to the store, after
// checking both parts of it.
func rel(userID int64, sessionID string) (dir, file string, err error) {
	if userID <= 0 {
		return "", "", fmt.Errorf("rawstore: invalid user id %d", userID)
	}
	// uuid.Parse accepts several spellings (braces, urn:, no hyphens,
	// upper case). Only the canonical one is a file name here.
	if !ValidSessionID(sessionID) {
		return "", "", ErrInvalidSessionID
	}
	dir = strconv.FormatInt(userID, 10)
	return dir, filepath.Join(dir, sessionID+suffix), nil
}

// ValidSessionID reports whether sessionID is a UUID in canonical form, the
// only kind of id the store will name a file after.
func ValidSessionID(sessionID string) bool {
	id, err := uuid.Parse(sessionID)
	return err == nil && id.String() == sessionID
}

// Put stores data as the session's transcript, replacing any earlier one, and
// returns the file's path relative to the store. The write goes to a
// temporary file in the same directory and is renamed into place, so a reader
// sees the old file or the new one and never part of either.
func (s *Store) Put(userID int64, sessionID string, data []byte) (string, error) {
	dir, file, err := rel(userID, sessionID)
	if err != nil {
		return "", err
	}
	if err := s.root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("rawstore: %w", err)
	}
	tmp := file + ".tmp-" + uuid.NewString()
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("rawstore: %w", err)
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = s.root.Rename(tmp, file)
	}
	if werr != nil {
		_ = s.root.Remove(tmp)
		return "", fmt.Errorf("rawstore: %w", werr)
	}
	return file, nil
}

// Get opens a session's stored transcript. The bytes are as Put received
// them. The error wraps os.ErrNotExist when there is no such file.
func (s *Store) Get(userID int64, sessionID string) (io.ReadCloser, error) {
	_, file, err := rel(userID, sessionID)
	if err != nil {
		return nil, err
	}
	return s.root.Open(file)
}

// Remove deletes a session's stored transcript. Removing one that is not
// there is not an error.
func (s *Store) Remove(userID int64, sessionID string) error {
	_, file, err := rel(userID, sessionID)
	if err != nil {
		return err
	}
	if err := s.root.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rawstore: %w", err)
	}
	return nil
}
