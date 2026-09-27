package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store is the mail.d directory: message files, per-message state, boxes,
// rules, contacts and the derived SQLite index. All exported methods are safe
// for concurrent use.
type Store struct {
	dir  string
	log  *log.Logger
	idx  *index
	root *os.Root

	mu       sync.RWMutex
	boxes    []Box
	rules    []Rule
	contacts map[string]struct{}
	purged   map[string]struct{} // ids removed by EmptyTrash, never stored again

	// Cumulative cost of the expensive steps, reported through Timings.
	parseNanos atomic.Int64
	fileNanos  atomic.Int64
	fileWrites atomic.Int64
	indexNanos atomic.Int64
}

// Open prepares a mail.d directory for use, creating the skeleton on first run
// and rebuilding the search index when it is missing or stale.
func Open(ctx context.Context, dir string, logger *log.Logger) (s *Store, err error) {
	if logger == nil {
		return nil, fmt.Errorf("open store %s: logger is required", dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	s = &Store{dir: abs, log: logger, contacts: map[string]struct{}{}}
	if layoutErr := s.ensureLayout(); layoutErr != nil {
		return nil, layoutErr
	}
	if boxErr := s.loadBoxes(); boxErr != nil {
		return nil, boxErr
	}
	if ruleErr := s.loadRules(); ruleErr != nil {
		return nil, ruleErr
	}
	if contactErr := s.loadContacts(); contactErr != nil {
		return nil, contactErr
	}
	if purgedErr := s.loadPurged(); purgedErr != nil {
		return nil, purgedErr
	}

	idx, err := openIndex(ctx, s.abs(indexFile))
	if err != nil {
		return nil, err
	}
	s.idx = idx
	defer func() {
		if err != nil {
			if cerr := s.idx.Close(); cerr != nil {
				err = fmt.Errorf("%w (and closing index: %w)", err, cerr)
			}
		}
	}()

	empty, err := s.idx.Empty(ctx)
	if err != nil {
		return nil, err
	}
	if empty {
		var n int
		if n, err = s.Reindex(ctx); err != nil {
			return nil, err
		}
		if n > 0 {
			s.log.Printf("store: built search index for %d message(s)", n)
		}
	}
	return s, nil
}

// Dir returns the absolute mail.d path.
func (s *Store) Dir() string { return s.dir }

// Close releases the index and the mail.d root.
func (s *Store) Close() error {
	err := s.idx.Close()
	if rerr := s.root.Close(); rerr != nil && err == nil {
		err = fmt.Errorf("close %s: %w", s.dir, rerr)
	}
	return err
}

// Reindex rebuilds the SQLite index from the state files and returns how many
// messages were indexed. It is safe to run at any time: the index holds no
// truth of its own.
func (s *Store) Reindex(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.idx.Reset(ctx); err != nil {
		return 0, err
	}

	n := 0
	err := fs.WalkDir(s.root.FS(), stateDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", s.abs(p), walkErr)
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		raw, readErr := s.root.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", s.abs(p), readErr)
		}
		var m Message
		if jsonErr := json.Unmarshal(raw, &m); jsonErr != nil {
			return fmt.Errorf("parse %s: %w", s.abs(p), jsonErr)
		}
		if m.ID == "" {
			return fmt.Errorf("parse %s: missing id", s.abs(p))
		}
		if !s.hasBoxLocked(m.Box) {
			s.log.Printf("store: %s references unknown box %q, filing as %s", m.ID, m.Box, BoxUnprocessed)
			m.Box = BoxUnprocessed
		}
		body, textErr := s.readText(&m)
		if textErr != nil {
			s.log.Printf("store: %s: %v", m.ID, textErr)
			body = ""
		}
		if upErr := s.idx.Upsert(ctx, &m, body); upErr != nil {
			return upErr
		}
		n++
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, nil
}

// List returns a page of messages plus the total number of matches.
func (s *Store) List(ctx context.Context, q Query) ([]ListItem, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if q.Box != "" && !s.hasBoxLocked(q.Box) {
		return nil, 0, fmt.Errorf("%w: %s", ErrUnknownBox, q.Box)
	}
	return s.idx.List(ctx, q)
}

// Counts returns the message tally for every box, including empty ones.
func (s *Store) Counts(ctx context.Context) (map[string]BoxCount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts, err := s.idx.Counts(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range s.boxes {
		if _, ok := counts[b.Slug]; !ok {
			counts[b.Slug] = BoxCount{Box: b.Slug}
		}
	}
	return counts, nil
}

// Cursor is the IMAP sync position, kept in mail.d so a second machine does
// not re-download everything.
type Cursor struct {
	UIDValidity uint32    `json:"uid_validity"`
	InboxUID    uint32    `json:"inbox_uid"`
	SentUID     uint32    `json:"sent_uid"`
	LastSync    time.Time `json:"last_sync"`
}

// cursorFile is the sync position file of an account: sync.json for the
// unnamed single account, sync-<name>.json for named ones.
func cursorFile(account string) string {
	if account == "" {
		return syncFile
	}
	return "sync-" + account + ".json"
}

// LoadCursor reads an account's sync position, returning a zero value when it
// does not exist yet.
func (s *Store) LoadCursor(account string) (Cursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadCursorLocked(account)
}

func (s *Store) loadCursorLocked(account string) (Cursor, error) {
	var st Cursor
	name := cursorFile(account)
	raw, err := s.root.ReadFile(name)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, fmt.Errorf("read %s: %w", s.abs(name), err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", s.abs(name), err)
	}
	return st, nil
}

// SaveCursor persists an account's IMAP sync position.
func (s *Store) SaveCursor(account string, st Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := cursorFile(account)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return s.writeFileAtomic(name, append(raw, '\n'))
}

// SaveOutbox records a message we sent, so replies are part of the git-synced
// archive too.
func (s *Store) SaveOutbox(raw []byte, sentAt time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := fmt.Sprintf("%s-%s.eml", sentAt.UTC().Format("20060102T150405Z"), messageID(raw)[:8])
	relPath := path.Join(outboxDir, name)
	if err := s.writeFileAtomic(relPath, raw); err != nil {
		return "", err
	}
	return relPath, nil
}

// Stats summarises the archive for the status endpoint.
type Stats struct {
	Messages    int       `json:"messages"`
	Boxes       int       `json:"boxes"`
	Rules       int       `json:"rules"`
	Contacts    int       `json:"contacts"`
	LastSync    time.Time `json:"last_sync"`
	Dir         string    `json:"dir"`
	PendingIMAP int       `json:"pending_imap"` // box moves not yet made on the server
}

// Stats returns a summary of what the store holds. LastSync is the most recent
// sync of any of accounts; PendingIMAP counts only the mirrored accounts,
// since the others never move mail on the server.
func (s *Store) Stats(ctx context.Context, accounts, mirrored []string) (Stats, error) {
	contacts := s.Contacts()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var lastSync time.Time
	for _, account := range accounts {
		st, err := s.loadCursorLocked(account)
		if err != nil {
			return Stats{}, err
		}
		if st.LastSync.After(lastSync) {
			lastSync = st.LastSync
		}
	}
	ids, err := s.idx.AllIDs(ctx)
	if err != nil {
		return Stats{}, err
	}
	pending, err := s.idx.CountPendingMoves(ctx, mirrored)
	if err != nil {
		return Stats{}, err
	}
	return Stats{
		Messages:    len(ids),
		Boxes:       len(s.boxes),
		Rules:       len(s.rules),
		Contacts:    len(contacts),
		LastSync:    lastSync,
		Dir:         s.dir,
		PendingIMAP: pending,
	}, nil
}

// Timings is the time the store has spent in its expensive steps since Open.
// Take a snapshot before some work and Sub it afterwards to see where that
// work went.
type Timings struct {
	Parse      time.Duration // MIME parsing and HTML-to-text of new mail
	Files      time.Duration // atomic file writes, fsync included
	FileWrites int64         // number of atomic file writes
	Index      time.Duration // SQLite index and full-text updates
}

// Sub returns what was spent between an earlier snapshot and t.
func (t Timings) Sub(earlier Timings) Timings {
	return Timings{
		Parse:      t.Parse - earlier.Parse,
		Files:      t.Files - earlier.Files,
		FileWrites: t.FileWrites - earlier.FileWrites,
		Index:      t.Index - earlier.Index,
	}
}

// Timings returns the cumulative timings so far.
func (s *Store) Timings() Timings {
	return Timings{
		Parse:      time.Duration(s.parseNanos.Load()),
		Files:      time.Duration(s.fileNanos.Load()),
		FileWrites: s.fileWrites.Load(),
		Index:      time.Duration(s.indexNanos.Load()),
	}
}

// timeFileWrite starts timing one file write; call the result when done.
func (s *Store) timeFileWrite() func() {
	start := time.Now()
	return func() {
		s.fileNanos.Add(int64(time.Since(start)))
		s.fileWrites.Add(1)
	}
}

// KnownAddresses returns the whitelist as a sorted slice, for the UI.
func (s *Store) KnownAddresses() []string {
	list := s.Contacts()
	sort.Strings(list)
	return list
}
