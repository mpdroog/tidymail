package httpd

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mpdroog/tidymail/internal/store"
)

// maxBulkIDs caps an explicit id list; larger selections go through
// handleMoveMatching, which never ships ids over the wire.
const maxBulkIDs = 5000

// selection is what the UI has ticked: explicit ids, or every message a list
// query matches ("select all"), which never ships ids over the wire.
type selection struct {
	IDs    []string `json:"ids"`
	All    bool     `json:"all"`
	In     string   `json:"in"` // with All: box to select from; empty searches every box
	Query  string   `json:"q"`
	Unread bool     `json:"unread"`
}

// selectionIDs resolves a selection to message ids.
func (s *Server) selectionIDs(r *http.Request, sel selection) ([]string, error) {
	if !sel.All {
		if len(sel.IDs) == 0 || len(sel.IDs) > maxBulkIDs {
			return nil, fmt.Errorf("%w: select between 1 and %d messages, got %d", errBadSelection, maxBulkIDs, len(sel.IDs))
		}
		return sel.IDs, nil
	}
	if sel.In == "" && sel.Query == "" {
		// Every message in every box is never what "select all" meant.
		return nil, fmt.Errorf("%w: pick a box or a search first", errBadSelection)
	}
	return s.st.MatchingIDs(r.Context(), store.Query{Box: sel.In, Text: sel.Query, UnreadOnly: sel.Unread})
}

// errBadSelection marks a selection the UI should never send.
var errBadSelection = errors.New("bad selection")

// bulkResult reports what a bulk action did. Skipped counts messages the
// action did not apply to, such as ones without a sender to ignore.
type bulkResult struct {
	Done    int `json:"moved"`
	Skipped int `json:"skipped,omitempty"`
}

// handleBulk applies one action to every selected message: move (also Prio,
// Delete), delay, ignore, or read/unread.
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		selection
		Action string `json:"action"`
		Box    string `json:"box"`  // move
		Days   int    `json:"days"` // delay
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	ids, err := s.selectionIDs(r, req.selection)
	if errors.Is(err, errBadSelection) {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	var res bulkResult
	switch req.Action {
	case "move":
		res.Done, err = s.st.MoveMany(r.Context(), ids, req.Box)
	case "delay":
		res, err = s.eachMessage(r, ids, func(id string) error {
			_, delayErr := s.st.Delay(r.Context(), id, req.Days)
			return delayErr
		})
	case "ignore":
		res, err = s.eachMessage(r, ids, func(id string) error {
			_, ignoreErr := s.st.Ignore(r.Context(), id, false)
			return ignoreErr
		})
	case "read", "unread":
		read := req.Action == "read"
		res, err = s.eachMessage(r, ids, func(id string) error {
			_, markErr := s.st.MarkRead(r.Context(), id, read)
			return markErr
		})
	default:
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("unknown action %q (want move, delay, ignore, read or unread)", req.Action))
		return
	}
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, res)
}

// eachMessage runs one action per message. A message that vanished meanwhile
// or cannot take the action (no sender to ignore) is logged and skipped; any
// other error stops the run, which is reported with what was done so far.
func (s *Server) eachMessage(r *http.Request, ids []string, act func(id string) error) (bulkResult, error) {
	var res bulkResult
	for _, id := range ids {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return res, fmt.Errorf("bulk action stopped after %d message(s): %w", res.Done, ctxErr)
		}
		err := act(id)
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidRule):
			s.log.Printf("http: bulk action on %s: %v, skipping", id, err)
			res.Skipped++
		case err != nil:
			return res, fmt.Errorf("bulk action stopped after %d message(s): %w", res.Done, err)
		default:
			res.Done++
		}
	}
	return res, nil
}

// handleDeleteSender moves every message from the sender of one message to
// Trash.
func (s *Server) handleDeleteSender(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Get(r.PathValue("id"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	sender := m.Mail.From.Address
	if sender == "" {
		s.fail(w, r, http.StatusBadRequest, errors.New("this message has no sender address"))
		return
	}
	ids, err := s.st.IDsFromSender(r.Context(), sender)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	moved, err := s.st.MoveMany(r.Context(), ids, store.BoxTrash)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Moved: moved})
}

// handleEmptyBox removes everything in Trash from this machine for good. Only
// Trash can be emptied: every other box is a place mail is kept.
func (s *Server) handleEmptyBox(w http.ResponseWriter, r *http.Request) {
	if slug := r.PathValue("slug"); slug != store.BoxTrash {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("only Trash can be emptied, not %q", slug))
		return
	}
	removed, err := s.st.EmptyTrash(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.log.Printf("http: emptied Trash, %d message(s) removed from mail.d", removed)
	s.writeJSON(w, r, http.StatusOK, actionResult{Moved: removed})
}

// handlePurge removes ticked messages in Trash from this machine for good;
// ids outside Trash are skipped.
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > maxBulkIDs {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("select between 1 and %d messages, got %d", maxBulkIDs, len(req.IDs)))
		return
	}
	removed, err := s.st.PurgeFromTrash(r.Context(), req.IDs)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.log.Printf("http: deleted %d message(s) from Trash for good", removed)
	s.writeJSON(w, r, http.StatusOK, actionResult{Moved: removed})
}
