package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// MatchField says which header a rule looks at.
type MatchField string

// Supported rule match fields.
const (
	MatchFrom MatchField = "from"
	MatchTo   MatchField = "to"
	MatchAny  MatchField = "any"
)

// Rule routes incoming mail to a box based on an address or a domain.
// Address is either a full address ("ci@example.com") or a domain suffix
// written with a leading "@" ("@example.com").
type Rule struct {
	Field   MatchField `toml:"match" json:"match"`
	Address string     `toml:"address" json:"address"`
	Box     string     `toml:"box" json:"box"`
}

type ruleFile struct {
	Rule []Rule `toml:"rule"`
}

// ErrInvalidRule is returned for a rule that cannot be applied.
var ErrInvalidRule = errors.New("invalid rule")

// normalizeAddress lowercases an address and strips surrounding whitespace and
// angle brackets so comparisons are stable.
func normalizeAddress(addr string) string {
	a := strings.TrimSpace(addr)
	a = strings.TrimPrefix(a, "<")
	a = strings.TrimSuffix(a, ">")
	return strings.ToLower(strings.TrimSpace(a))
}

func (r Rule) key() string { return string(r.Field) + "\x00" + r.Address }

// matches reports whether the rule applies to a message with the given
// sender and recipients.
func (r Rule) matches(from string, recipients []string) bool {
	switch r.Field {
	case MatchFrom:
		return addressMatches(r.Address, from)
	case MatchTo:
		return anyAddressMatches(r.Address, recipients)
	case MatchAny:
		return addressMatches(r.Address, from) || anyAddressMatches(r.Address, recipients)
	default:
		return false
	}
}

func anyAddressMatches(pattern string, list []string) bool {
	for _, a := range list {
		if addressMatches(pattern, a) {
			return true
		}
	}
	return false
}

// addressMatches compares a normalized address against a rule pattern. A
// pattern starting with "@" matches the whole domain.
func addressMatches(pattern, addr string) bool {
	if pattern == "" || addr == "" {
		return false
	}
	if strings.HasPrefix(pattern, "@") {
		return strings.HasSuffix(addr, pattern)
	}
	return pattern == addr
}

// loadRules reads rules.toml, tolerating a missing file.
func (s *Store) loadRules() error {
	name := s.abs(rulesFile)
	raw, err := s.root.ReadFile(rulesFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.rules = nil
			return nil
		}
		return fmt.Errorf("read %s: %w", name, err)
	}
	var rf ruleFile
	md, err := toml.Decode(string(raw), &rf)
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return fmt.Errorf("parse %s: unknown key %s", name, u[0].String())
	}
	seen := make(map[string]struct{}, len(rf.Rule))
	out := make([]Rule, 0, len(rf.Rule))
	for _, r := range rf.Rule {
		r.Address = normalizeAddress(r.Address)
		switch r.Field {
		case MatchFrom, MatchTo, MatchAny:
		case "":
			r.Field = MatchFrom
		default:
			return fmt.Errorf("parse %s: %w: match %q", name, ErrInvalidRule, r.Field)
		}
		if r.Address == "" || r.Box == "" {
			return fmt.Errorf("parse %s: %w: address and box are required", name, ErrInvalidRule)
		}
		if _, dup := seen[r.key()]; dup {
			continue
		}
		seen[r.key()] = struct{}{}
		out = append(out, r)
	}
	s.rules = out
	return nil
}

// saveRulesLocked writes rules.toml. The caller must hold s.mu.
func (s *Store) saveRulesLocked() error {
	sorted := make([]Rule, len(s.rules))
	copy(sorted, s.rules)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Box != sorted[j].Box {
			return sorted[i].Box < sorted[j].Box
		}
		if sorted[i].Address != sorted[j].Address {
			return sorted[i].Address < sorted[j].Address
		}
		return sorted[i].Field < sorted[j].Field
	})
	var buf bytes.Buffer
	buf.WriteString("# tidymail routing rules: incoming mail matching a rule lands in that box.\n")
	buf.WriteString("# address may be a full address or a domain such as \"@example.com\".\n\n")
	if err := toml.NewEncoder(&buf).Encode(ruleFile{Rule: sorted}); err != nil {
		return fmt.Errorf("encode %s: %w", rulesFile, err)
	}
	return s.writeFileAtomic(rulesFile, buf.Bytes())
}

// Rules returns a copy of the routing rules.
func (s *Store) Rules() []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// AddRule stores a routing rule, replacing any rule with the same field and
// address.
func (s *Store) AddRule(r Rule) error {
	r.Address = normalizeAddress(r.Address)
	if r.Address == "" {
		return fmt.Errorf("%w: address is required", ErrInvalidRule)
	}
	switch r.Field {
	case MatchFrom, MatchTo, MatchAny:
	default:
		return fmt.Errorf("%w: match %q: want from, to or any", ErrInvalidRule, r.Field)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasBoxLocked(r.Box) {
		return fmt.Errorf("%w: %s", ErrUnknownBox, r.Box)
	}
	replaced := false
	for i := range s.rules {
		if s.rules[i].key() == r.key() {
			s.rules[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		s.rules = append(s.rules, r)
	}
	return s.saveRulesLocked()
}

// DeleteRule removes the rule with the given field and address.
func (s *Store) DeleteRule(field MatchField, address string) error {
	want := Rule{Field: field, Address: normalizeAddress(address)}.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.rules[:0]
	found := false
	for _, r := range s.rules {
		if r.key() == want {
			found = true
			continue
		}
		out = append(out, r)
	}
	s.rules = out
	if !found {
		return fmt.Errorf("%w: no rule for %s %s", ErrInvalidRule, field, address)
	}
	return s.saveRulesLocked()
}

// dropRulesForBoxLocked removes every rule pointing at a box. Caller holds s.mu.
func (s *Store) dropRulesForBoxLocked(box string) error {
	out := s.rules[:0]
	changed := false
	for _, r := range s.rules {
		if r.Box == box {
			changed = true
			continue
		}
		out = append(out, r)
	}
	s.rules = out
	if !changed {
		return nil
	}
	return s.saveRulesLocked()
}

// routeLocked decides which box a newly ingested message belongs in. Rules win
// over the whitelist; without either it is unprocessed.
func (s *Store) routeLocked(from string, recipients []string) string {
	for _, r := range s.rules {
		if r.matches(from, recipients) && s.hasBoxLocked(r.Box) {
			return r.Box
		}
	}
	if _, ok := s.contacts[from]; ok {
		return BoxWhitelisted
	}
	return BoxUnprocessed
}

// loadContacts reads contacts.txt, the set of addresses we have written to.
func (s *Store) loadContacts() error {
	name := s.abs(contactFile)
	f, err := s.root.Open(contactFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.contacts = make(map[string]struct{})
			return nil
		}
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			s.log.Printf("store: close %s: %v", name, cerr)
		}
	}()

	s.contacts = make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s.contacts[normalizeAddress(line)] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return nil
}

// saveContactsLocked writes contacts.txt sorted, one address per line, so git
// diffs stay small. Caller holds s.mu.
func (s *Store) saveContactsLocked() error {
	list := make([]string, 0, len(s.contacts))
	for a := range s.contacts {
		list = append(list, a)
	}
	sort.Strings(list)
	var buf bytes.Buffer
	buf.WriteString("# Addresses tidymail has seen you write to. Mail from these lands in Whitelisted.\n")
	for _, a := range list {
		buf.WriteString(a)
		buf.WriteByte('\n')
	}
	return s.writeFileAtomic(contactFile, buf.Bytes())
}

// AddContacts whitelists addresses and reports how many were new.
func (s *Store) AddContacts(addrs ...string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// sync and serve are separate processes that both edit contacts.txt, so
	// merge into what is on disk now rather than into a stale copy.
	if err := s.loadContacts(); err != nil {
		return 0, err
	}
	added := 0
	for _, a := range addrs {
		n := normalizeAddress(a)
		if n == "" || !strings.Contains(n, "@") {
			continue
		}
		if _, ok := s.contacts[n]; ok {
			continue
		}
		s.contacts[n] = struct{}{}
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if err := s.saveContactsLocked(); err != nil {
		return 0, err
	}
	return added, nil
}

// RemoveContact drops an address from the whitelist.
func (s *Store) RemoveContact(addr string) error {
	n := normalizeAddress(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadContacts(); err != nil {
		return err
	}
	if _, ok := s.contacts[n]; !ok {
		return nil
	}
	delete(s.contacts, n)
	return s.saveContactsLocked()
}

// Contacts returns the whitelist as it is on disk now, sorted.
func (s *Store) Contacts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadContacts(); err != nil {
		s.log.Printf("store: reload contacts: %v", err)
	}
	out := make([]string, 0, len(s.contacts))
	for a := range s.contacts {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ApplyRule files existing messages that match a rule into its box, and
// reports how many moved. Newly created rules only affect incoming mail unless
// this is called.
func (s *Store) ApplyRule(ctx context.Context, r Rule) (int, error) {
	r.Address = normalizeAddress(r.Address)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasBoxLocked(r.Box) {
		return 0, fmt.Errorf("%w: %s", ErrUnknownBox, r.Box)
	}
	ids, err := s.idx.AllIDs(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range ids {
		m, err := s.readState(id)
		if err != nil {
			s.log.Printf("store: apply rule to %s: %v", id, err)
			continue
		}
		if m.Box == r.Box {
			continue
		}
		recipients := make([]string, 0, len(m.Mail.To)+len(m.Mail.Cc))
		for _, a := range m.Mail.To {
			recipients = append(recipients, a.Address)
		}
		for _, a := range m.Mail.Cc {
			recipients = append(recipients, a.Address)
		}
		if !r.matches(m.Mail.From.Address, recipients) {
			continue
		}
		if err := s.setBoxLocked(ctx, id, r.Box, m.Box, 0); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}
