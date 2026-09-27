package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// Summary is an AI summary of one message. Messages never change, so it is
// made once and kept next to the state, in summaries/ab/<id>.json.
type Summary struct {
	TLDR    string    `json:"tldr"`
	Details string    `json:"details"`
	At      time.Time `json:"at"`
}

func summaryPath(id string) string {
	return rel(summaryDir, id[:2], id+".json")
}

// Summary returns the stored summary of message id, or nil when it has none.
func (s *Store) Summary(id string) (*Summary, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	raw, err := s.root.ReadFile(summaryPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.abs(summaryPath(id)), err)
	}
	var sum Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.abs(summaryPath(id)), err)
	}
	return &sum, nil
}

// SaveSummary stores the summary of message id.
func (s *Store) SaveSummary(id string, sum Summary) error {
	if err := validID(id); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("encode summary for %s: %w", id, err)
	}
	return s.writeFileAtomic(summaryPath(id), append(raw, '\n'))
}
