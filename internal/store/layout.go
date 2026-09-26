// Package store owns everything under mail.d: the immutable message files, the
// mutable per-message state, the box/rule definitions and the SQLite search
// index built from all of it.
//
// Layout:
//
//	mail.d/
//	  config.toml            operator settings (not touched by this package)
//	  boxes.toml             box definitions
//	  rules.toml             address -> box routing rules
//	  contacts.txt           addresses we have written to (the whitelist)
//	  sync.json              IMAP cursor state
//	  mail/YYYY/MM/<id>.eml  raw RFC 5322 message, immutable
//	  mail/YYYY/MM/<id>.txt  extracted plain text, immutable
//	  state/<ab>/<id>.json   which box a message is in, mutable
//	  outbox/<id>.eml        replies we sent
//	  index.db               SQLite index, derived and disposable
//
// Everything except index.db is line-oriented text so the directory can be
// committed to git and synced between machines.
//
// All file access goes through an os.Root anchored at mail.d, so a path read
// out of a state file can never reach outside the directory, symlinks
// included.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// File and directory names inside mail.d.
const (
	boxesFile   = "boxes.toml"
	rulesFile   = "rules.toml"
	contactFile = "contacts.txt"
	syncFile    = "sync.json"
	indexFile   = "index.db"
	gitIgnore   = ".gitignore"

	mailDir   = "mail"
	stateDir  = "state"
	outboxDir = "outbox"
	tmpDir    = "tmp"
)

// dirPerm and filePerm keep mail.d readable by its owner only; it holds
// private correspondence.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// gitIgnoreBody excludes the derived index and scratch files from git so the
// directory can be synced without binary merge conflicts.
const gitIgnoreBody = `# Derived from the text files next to it; rebuilt with: tidymail reindex
index.db
index.db-wal
index.db-shm
tmp/
`

// rel joins path elements into a slash-separated path relative to mail.d, the
// form every os.Root call expects.
func rel(elem ...string) string { return path.Join(elem...) }

// abs returns the on-disk path of a mail.d-relative path, for error messages
// and for the few APIs that cannot take a root.
func (s *Store) abs(relPath string) string {
	return filepath.Join(s.dir, filepath.FromSlash(relPath))
}

// ensureLayout creates the directory skeleton and opens the root. It is safe
// to call on an existing directory.
func (s *Store) ensureLayout() error {
	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", s.dir, err)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.dir, err)
	}
	s.root = root

	for _, d := range []string{mailDir, stateDir, outboxDir, tmpDir} {
		if err := s.root.MkdirAll(d, dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", s.abs(d), err)
		}
	}
	if _, err := s.root.Stat(gitIgnore); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", s.abs(gitIgnore), err)
		}
		if err := s.root.WriteFile(gitIgnore, []byte(gitIgnoreBody), filePerm); err != nil {
			return fmt.Errorf("write %s: %w", s.abs(gitIgnore), err)
		}
	}
	return nil
}

// readFile reads a mail.d-relative file.
func (s *Store) readFile(relPath string) ([]byte, error) {
	b, err := s.root.ReadFile(relPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.abs(relPath), err)
	}
	return b, nil
}

// writeFileAtomic writes data to a mail.d-relative path via a temporary file
// and a rename, so a crash can never leave a half-written state file behind.
func (s *Store) writeFileAtomic(relPath string, data []byte) (err error) {
	defer s.timeFileWrite()()
	if dir := path.Dir(relPath); dir != "." {
		if mkErr := s.root.MkdirAll(dir, dirPerm); mkErr != nil {
			return fmt.Errorf("create %s: %w", s.abs(dir), mkErr)
		}
	}

	suffix := make([]byte, 8)
	if _, err = rand.Read(suffix); err != nil {
		return fmt.Errorf("name temp file: %w", err)
	}
	tmp := rel(tmpDir, "w-"+hex.EncodeToString(suffix))

	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", s.abs(tmp), err)
	}
	defer func() {
		if err == nil {
			return
		}
		if rmErr := s.root.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			err = fmt.Errorf("%w (and removing %s: %w)", err, s.abs(tmp), rmErr)
		}
	}()

	if _, err = f.Write(data); err != nil {
		err = fmt.Errorf("write %s: %w", s.abs(tmp), err)
		return closeAlso(f, err)
	}
	if err = f.Sync(); err != nil {
		err = fmt.Errorf("sync %s: %w", s.abs(tmp), err)
		return closeAlso(f, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", s.abs(tmp), err)
	}
	if err = s.root.Rename(tmp, relPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", s.abs(tmp), s.abs(relPath), err)
	}
	return nil
}

// closeAlso closes f, keeping the original error but reporting a close failure
// when there was none.
func closeAlso(f *os.File, err error) error {
	if cerr := f.Close(); cerr != nil && err == nil {
		return fmt.Errorf("close %s: %w", f.Name(), cerr)
	}
	return err
}
