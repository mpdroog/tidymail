package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mpdroog/tidymail/internal/mailparse"
)

// idLen is how many hex characters of the message digest form its id. 128 bits
// is far beyond any realistic collision risk for a personal mailbox.
const idLen = 32

// Mail is the immutable half of a stored message: everything that comes from
// the raw file and can never change once written.
type Mail struct {
	EML         string                 `json:"eml"`
	TXT         string                 `json:"txt"`
	MessageID   string                 `json:"message_id"`
	InReplyTo   string                 `json:"in_reply_to,omitempty"`
	References  []string               `json:"references,omitempty"`
	From        mailparse.Address      `json:"from"`
	ReplyTo     []mailparse.Address    `json:"reply_to,omitempty"`
	To          []mailparse.Address    `json:"to,omitempty"`
	Cc          []mailparse.Address    `json:"cc,omitempty"`
	Subject     string                 `json:"subject"`
	Date        time.Time              `json:"date"`
	Size        int64                  `json:"size"`
	Attachments []mailparse.Attachment `json:"attachments,omitempty"`
	Warnings    []string               `json:"warnings,omitempty"`
}

// Message is a stored mail plus its mutable placement. It is exactly what a
// state/<id>.json file contains.
type Message struct {
	ID string `json:"id"`
	// Account is the name of the account the message was fetched through.
	// Empty means the first account (mail stored before accounts had names).
	Account string `json:"account,omitempty"`
	Box     string `json:"box"`
	PrevBox string `json:"prev_box,omitempty"`
	// ServerBox is the box whose IMAP folder holds the server copy. When it
	// differs from Box, serve still has to move the message on the server.
	// Empty means unknown (state written before mirroring existed).
	ServerBox    string     `json:"server_box,omitempty"`
	DelayedUntil *time.Time `json:"delayed_until,omitempty"`
	Unread       bool       `json:"unread"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Mail         Mail       `json:"mail"`
}

// ErrNotFound is returned when no message has the requested id.
var ErrNotFound = errors.New("message not found")

// messageID derives the content-addressed id of a raw message.
func messageID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:idLen]
}

// statePath returns the mail.d-relative state/<shard>/<id>.json path.
func statePath(id string) string {
	return rel(stateDir, id[:2], id+".json")
}

// mailPaths returns the eml and txt paths for a message, relative to mail.d.
func mailPaths(id string, date time.Time) (eml, txt string) {
	dir := path.Join(mailDir, date.UTC().Format("2006"), date.UTC().Format("01"))
	return path.Join(dir, id+".eml"), path.Join(dir, id+".txt")
}

// validID guards against a caller turning a user-supplied id into a path
// outside mail.d.
func validID(id string) error {
	if len(id) != idLen {
		return fmt.Errorf("%w: id %q has wrong length", ErrNotFound, id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: id %q is not hexadecimal", ErrNotFound, id)
		}
	}
	return nil
}

// readState loads a message state file.
func (s *Store) readState(id string) (*Message, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	p := statePath(id)
	raw, err := s.root.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("read %s: %w", s.abs(p), err)
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.abs(p), err)
	}
	if m.ID != id {
		return nil, fmt.Errorf("parse %s: id mismatch (%q)", s.abs(p), m.ID)
	}
	return &m, nil
}

// writeState persists a message state file and refreshes the index row.
func (s *Store) writeState(ctx context.Context, m *Message) error {
	m.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state for %s: %w", m.ID, err)
	}
	raw = append(raw, '\n')
	if writeErr := s.writeFileAtomic(statePath(m.ID), raw); writeErr != nil {
		return writeErr
	}
	body, err := s.readText(m)
	if err != nil {
		// The index is a convenience; a missing text file must not stop a
		// move from being recorded, but it must be visible.
		s.log.Printf("store: body for %s unavailable, indexing headers only: %v", m.ID, err)
		body = ""
	}
	indexStart := time.Now()
	err = s.idx.Upsert(ctx, m, body)
	s.indexNanos.Add(int64(time.Since(indexStart)))
	return err
}

// readText returns the extracted plain-text body of a message.
func (s *Store) readText(m *Message) (string, error) {
	b, err := s.readFile(m.Mail.TXT)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Raw returns the raw RFC 5322 bytes of a stored message.
func (s *Store) Raw(id string) ([]byte, error) {
	m, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	return s.readFile(m.Mail.EML)
}

// Get returns a single message.
func (s *Store) Get(id string) (*Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readState(id)
}

// Ingest stores a raw message fetched through account and files it into a
// box. It reports whether the message was new; re-ingesting identical bytes is
// a no-op, which is what makes a failed server-side archive safe to retry.
func (s *Store) Ingest(ctx context.Context, account string, raw []byte, received time.Time) (*Message, bool, error) {
	id := messageID(raw)
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.readState(id)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	parseStart := time.Now()
	parsed, err := mailparse.Parse(raw)
	s.parseNanos.Add(int64(time.Since(parseStart)))
	if err != nil {
		return nil, false, fmt.Errorf("parse message %s: %w", id, err)
	}
	date := parsed.Date
	if date.IsZero() {
		date = received.UTC()
	}
	if date.IsZero() {
		date = time.Now().UTC()
	}

	eml, txt := mailPaths(id, date)
	if err := s.writeFileAtomic(eml, raw); err != nil {
		return nil, false, err
	}
	if err := s.writeFileAtomic(txt, []byte(parsed.Text)); err != nil {
		return nil, false, err
	}

	from := mailparse.FirstAddress(parsed.From)
	recipients := append(mailparse.AddressStrings(parsed.To), mailparse.AddressStrings(parsed.Cc)...)

	m := &Message{
		ID:      id,
		Account: account,
		Box:     s.routeLocked(from.Address, recipients),
		// As fetched: the folder sync found it in. A rule filing it elsewhere
		// makes serve move it there on the server too.
		ServerBox: BoxUnprocessed,
		Unread:    true,
		Mail: Mail{
			EML:         eml,
			TXT:         txt,
			MessageID:   parsed.MessageID,
			InReplyTo:   parsed.InReplyTo,
			References:  parsed.References,
			From:        from,
			ReplyTo:     parsed.ReplyTo,
			To:          parsed.To,
			Cc:          parsed.Cc,
			Subject:     parsed.Subject,
			Date:        date.UTC(),
			Size:        int64(len(raw)),
			Attachments: parsed.Attachments,
			Warnings:    parsed.Warnings,
		},
	}
	if err := s.writeState(ctx, m); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// SetBox moves a message to another box, remembering where it came from.
func (s *Store) SetBox(ctx context.Context, id, box string) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasBoxLocked(box) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownBox, box)
	}
	if err := s.setBoxLocked(ctx, id, box, "", 0); err != nil {
		return nil, err
	}
	return s.readState(id)
}

// setBoxLocked performs the move. When until is non-zero the message is parked
// in the Delayed box and prevBox records where it returns to.
func (s *Store) setBoxLocked(ctx context.Context, id, box, prevBox string, until int64) error {
	m, err := s.readState(id)
	if err != nil {
		return err
	}
	if prevBox == "" {
		prevBox = m.Box
	}
	if m.ServerBox == "" {
		// Older state: assume the server copy is where the box says.
		m.ServerBox = m.Box
	}
	m.PrevBox = prevBox
	m.Box = box
	if until > 0 {
		t := time.Unix(until, 0).UTC()
		m.DelayedUntil = &t
	} else {
		m.DelayedUntil = nil
	}
	return s.writeState(ctx, m)
}

// ErrBadDelay is returned for a delay outside the supported range.
var ErrBadDelay = errors.New("delay out of range")

// maxDelayDays keeps a typo from parking mail for a century.
const maxDelayDays = 3650

// Delay parks a message in the Delayed box for the given number of days. It
// returns to the box it is in now once the delay expires.
func (s *Store) Delay(ctx context.Context, id string, days int) (*Message, error) {
	if days < 1 || days > maxDelayDays {
		return nil, fmt.Errorf("%w: %d days, want 1..%d", ErrBadDelay, days, maxDelayDays)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readState(id)
	if err != nil {
		return nil, err
	}
	// Re-delaying an already delayed message keeps its original destination.
	prev := m.Box
	if m.Box == BoxDelayed && m.PrevBox != "" {
		prev = m.PrevBox
	}
	if !s.hasBoxLocked(prev) {
		prev = BoxUnprocessed
	}
	until := time.Now().UTC().AddDate(0, 0, days).Truncate(time.Second)
	if err := s.setBoxLocked(ctx, id, BoxDelayed, prev, until.Unix()); err != nil {
		return nil, err
	}
	return s.readState(id)
}

// ReleaseDue moves every delayed message whose time has come back to the box it
// came from, and returns how many moved.
func (s *Store) ReleaseDue(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.idx.DueDelayed(ctx, now.UTC().Unix())
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range ids {
		m, err := s.readState(id)
		if err != nil {
			s.log.Printf("store: release %s: %v", id, err)
			continue
		}
		target := m.PrevBox
		if target == "" || !s.hasBoxLocked(target) || target == BoxDelayed {
			target = BoxUnprocessed
		}
		if err := s.setBoxLocked(ctx, id, target, m.Box, 0); err != nil {
			s.log.Printf("store: release %s: %v", id, err)
			continue
		}
		moved++
	}
	return moved, nil
}

// MarkRead sets or clears the unread flag.
func (s *Store) MarkRead(ctx context.Context, id string, read bool) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readState(id)
	if err != nil {
		return nil, err
	}
	if m.Unread == !read {
		return m, nil
	}
	m.Unread = !read
	if err := s.writeState(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Ignore blacklists the sender and moves the message to Spam. When
// applyExisting is set, every other message from that sender moves too.
func (s *Store) Ignore(ctx context.Context, id string, applyExisting bool) (moved int, err error) {
	s.mu.RLock()
	m, err := s.readState(id)
	s.mu.RUnlock()
	if err != nil {
		return 0, err
	}
	sender := m.Mail.From.Address
	if sender == "" {
		return 0, fmt.Errorf("%w: message %s has no sender to blacklist", ErrInvalidRule, id)
	}
	if err := s.AddRule(Rule{Field: MatchFrom, Address: sender, Box: BoxSpam}); err != nil {
		return 0, err
	}
	if err := s.RemoveContact(sender); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []string{id}
	if applyExisting {
		fromSender, lookupErr := s.idx.IDsFromSender(ctx, sender)
		if lookupErr != nil {
			return 0, lookupErr
		}
		ids = fromSender
	}
	for _, mid := range ids {
		cur, readErr := s.readState(mid)
		if readErr != nil {
			s.log.Printf("store: ignore %s: %v", mid, readErr)
			continue
		}
		if cur.Box == BoxSpam {
			continue
		}
		if moveErr := s.setBoxLocked(ctx, mid, BoxSpam, cur.Box, 0); moveErr != nil {
			return moved, moveErr
		}
		moved++
	}
	return moved, nil
}

// ReplyTargets returns the addresses a reply to this message should go to:
// Reply-To when present, otherwise From.
func (m *Message) ReplyTargets() []mailparse.Address {
	if len(m.Mail.ReplyTo) > 0 {
		return m.Mail.ReplyTo
	}
	if m.Mail.From.Address != "" {
		return []mailparse.Address{m.Mail.From}
	}
	return nil
}

// ReplySubject prefixes the subject with Re: unless it already has one.
func (m *Message) ReplySubject() string {
	subj := strings.TrimSpace(m.Mail.Subject)
	if subj == "" {
		return "Re:"
	}
	if strings.HasPrefix(strings.ToLower(subj), "re:") {
		return subj
	}
	return "Re: " + subj
}
