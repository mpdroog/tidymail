package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// System box slugs. Every message is in exactly one box at any time.
const (
	BoxUnprocessed = "unprocessed"
	BoxWhitelisted = "whitelisted"
	BoxPriority    = "priority"
	BoxDelayed     = "delayed"
	BoxProcessed   = "processed"
	BoxSpam        = "spam"
	BoxTrash       = "trash"
)

// Box is a folder in the tidymail UI.
type Box struct {
	Slug   string `toml:"slug" json:"slug"`
	Name   string `toml:"name" json:"name"`
	System bool   `toml:"system" json:"system"`
	Order  int    `toml:"order" json:"order"`
}

type boxFile struct {
	Box []Box `toml:"box"`
}

// defaultBoxes are created on first run and may not be deleted.
func defaultBoxes() []Box {
	return []Box{
		{Slug: BoxUnprocessed, Name: "Unprocessed", System: true, Order: 10},
		{Slug: BoxWhitelisted, Name: "Whitelisted", System: true, Order: 20},
		{Slug: BoxPriority, Name: "Priority", System: true, Order: 30},
		{Slug: BoxDelayed, Name: "Delayed", System: true, Order: 40},
		{Slug: BoxProcessed, Name: "Processed", System: true, Order: 50},
		{Slug: BoxSpam, Name: "Spam", System: true, Order: 60},
		{Slug: BoxTrash, Name: "Trash", System: true, Order: 70},
	}
}

// ErrUnknownBox is returned when a slug does not name an existing box.
var ErrUnknownBox = errors.New("unknown box")

// ErrBoxExists is returned when creating a box whose slug is taken.
var ErrBoxExists = errors.New("box already exists")

// ErrSystemBox is returned when trying to delete a built-in box.
var ErrSystemBox = errors.New("system boxes cannot be removed")

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Slugify turns a human box name into a slug usable in URLs and file names.
func Slugify(name string) string {
	var b strings.Builder
	prevDash := true // suppress a leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// loadBoxes reads boxes.toml, creating it with the defaults when missing, and
// makes sure every system box is present.
func (s *Store) loadBoxes() error {
	name := s.abs(boxesFile)
	var bf boxFile

	raw, err := s.root.ReadFile(boxesFile)
	switch {
	case err == nil:
		md, decErr := toml.Decode(string(raw), &bf)
		if decErr != nil {
			return fmt.Errorf("parse %s: %w", name, decErr)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return fmt.Errorf("parse %s: unknown key %s", name, u[0].String())
		}
	case os.IsNotExist(err):
		bf.Box = nil
	default:
		return fmt.Errorf("read %s: %w", name, err)
	}

	byslug := make(map[string]Box, len(bf.Box))
	for _, b := range bf.Box {
		if !slugRe.MatchString(b.Slug) {
			return fmt.Errorf("parse %s: invalid box slug %q", name, b.Slug)
		}
		if _, dup := byslug[b.Slug]; dup {
			return fmt.Errorf("parse %s: duplicate box slug %q", name, b.Slug)
		}
		if b.Name == "" {
			b.Name = b.Slug
		}
		byslug[b.Slug] = b
	}
	// System boxes are mandatory: re-add any that were removed by hand.
	changed := len(bf.Box) != len(byslug)
	for _, def := range defaultBoxes() {
		cur, ok := byslug[def.Slug]
		if !ok {
			byslug[def.Slug] = def
			changed = true
			continue
		}
		if !cur.System {
			cur.System = true
			byslug[def.Slug] = cur
			changed = true
		}
	}

	s.boxes = sortBoxes(byslug)
	if changed || os.IsNotExist(err) {
		return s.saveBoxesLocked()
	}
	return nil
}

func sortBoxes(m map[string]Box) []Box {
	out := make([]Box, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// saveBoxesLocked writes boxes.toml. The caller must hold s.mu.
func (s *Store) saveBoxesLocked() error {
	var buf bytes.Buffer
	buf.WriteString("# tidymail boxes. Managed by the app; safe to edit by hand.\n")
	buf.WriteString("# System boxes cannot be removed.\n\n")
	if err := toml.NewEncoder(&buf).Encode(boxFile{Box: s.boxes}); err != nil {
		return fmt.Errorf("encode %s: %w", boxesFile, err)
	}
	return s.writeFileAtomic(boxesFile, buf.Bytes())
}

// Boxes returns all boxes in display order.
func (s *Store) Boxes() []Box {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Box, len(s.boxes))
	copy(out, s.boxes)
	return out
}

// HasBox reports whether slug names an existing box.
func (s *Store) HasBox(slug string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasBoxLocked(slug)
}

func (s *Store) hasBoxLocked(slug string) bool {
	for _, b := range s.boxes {
		if b.Slug == slug {
			return true
		}
	}
	return false
}

// CreateBox adds a custom box and returns it. Names that collide with an
// existing slug are rejected.
func (s *Store) CreateBox(name string) (Box, error) {
	slug := Slugify(name)
	if slug == "" {
		return Box{}, fmt.Errorf("box name %q has no usable characters", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasBoxLocked(slug) {
		return Box{}, fmt.Errorf("%w: %s", ErrBoxExists, slug)
	}
	order := 100
	for _, b := range s.boxes {
		if b.Order >= order {
			order = b.Order + 10
		}
	}
	box := Box{Slug: slug, Name: strings.TrimSpace(name), System: false, Order: order}
	s.boxes = append(s.boxes, box)
	s.boxes = sortBoxes(boxMap(s.boxes))
	if err := s.saveBoxesLocked(); err != nil {
		return Box{}, err
	}
	return box, nil
}

// DeleteBox removes a custom box. Messages in it move to Unprocessed and any
// rule pointing at it is dropped.
func (s *Store) DeleteBox(ctx context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, b := range s.boxes {
		if b.Slug != slug {
			continue
		}
		if b.System {
			return fmt.Errorf("%w: %s", ErrSystemBox, slug)
		}
		idx = i
	}
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrUnknownBox, slug)
	}

	ids, err := s.idx.IDsInBox(ctx, slug)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.setBoxLocked(ctx, id, BoxUnprocessed, "", 0); err != nil {
			return err
		}
	}
	if err := s.dropRulesForBoxLocked(slug); err != nil {
		return err
	}
	s.boxes = append(s.boxes[:idx], s.boxes[idx+1:]...)
	return s.saveBoxesLocked()
}

func boxMap(in []Box) map[string]Box {
	m := make(map[string]Box, len(in))
	for _, b := range in {
		m[b.Slug] = b
	}
	return m
}
