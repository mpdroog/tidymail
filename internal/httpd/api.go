package httpd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mpdroog/tidymail/internal/mailparse"
	"github.com/mpdroog/tidymail/internal/smtpsend"
	"github.com/mpdroog/tidymail/internal/store"
)

// boxView is a box plus its message tally.
type boxView struct {
	store.Box
	Total  int `json:"total"`
	Unread int `json:"unread"`
}

// stateView is everything the UI needs to draw its chrome.
type stateView struct {
	CSRF     string       `json:"csrf"`
	Boxes    []boxView    `json:"boxes"`
	Rules    []store.Rule `json:"rules"`
	Stats    store.Stats  `json:"stats"`
	FromAddr string       `json:"from_addr"`
}

func (s *Server) boxViews(ctx context.Context) ([]boxView, error) {
	counts, err := s.st.Counts(ctx)
	if err != nil {
		return nil, err
	}
	boxes := s.st.Boxes()
	out := make([]boxView, 0, len(boxes))
	for _, b := range boxes {
		c := counts[b.Slug]
		out = append(out, boxView{Box: b, Total: c.Total, Unread: c.Unread})
	}
	return out, nil
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.boxViews(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	stats, err := s.st.Stats(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, stateView{
		CSRF:     s.csrf,
		Boxes:    boxes,
		Rules:    s.st.Rules(),
		Stats:    stats,
		FromAddr: s.cfg.SMTP.FromAddr,
	})
}

func (s *Server) handleBoxes(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.boxViews(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, boxes)
}

// listView is a page of messages.
type listView struct {
	Items  []store.ListItem `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"), 50)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("limit: %w", err))
		return
	}
	offset, err := intParam(q.Get("offset"), 0)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("offset: %w", err))
		return
	}

	query := store.Query{
		Box:        q.Get("box"),
		Text:       q.Get("q"),
		UnreadOnly: q.Get("unread") == "1",
		Limit:      limit,
		Offset:     offset,
	}
	items, total, err := s.st.List(r.Context(), query)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, listView{Items: items, Total: total, Limit: limit, Offset: offset})
}

// messageView is a single message with its body text.
type messageView struct {
	*store.Message
	Body        string              `json:"body"`
	ReplyTo     []mailparse.Address `json:"reply_targets"`
	ReplySubj   string              `json:"reply_subject"`
	Whitelisted bool                `json:"whitelisted"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Get(r.PathValue("id"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	raw, err := s.st.Raw(m.ID)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	// Opening a message marks it read; a failure here is worth logging but
	// must not hide the message itself.
	if _, err := s.st.MarkRead(r.Context(), m.ID, true); err != nil {
		s.log.Printf("http: mark %s read: %v", m.ID, err)
	}

	whitelisted := false
	for _, c := range s.st.Contacts() {
		if c == m.Mail.From.Address {
			whitelisted = true
			break
		}
	}
	s.writeJSON(w, r, http.StatusOK, messageView{
		Message:     m,
		Body:        parsed.Text,
		ReplyTo:     m.ReplyTargets(),
		ReplySubj:   m.ReplySubject(),
		Whitelisted: whitelisted,
	})
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	raw, err := s.st.Raw(r.PathValue("id"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	serveDownload(w, r, r.PathValue("id")+".eml", "message/rfc822", raw)
}

func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	part, err := strconv.Atoi(r.PathValue("part"))
	if err != nil || part < 1 {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("attachment part %q is not a positive number", r.PathValue("part")))
		return
	}
	raw, err := s.st.Raw(r.PathValue("id"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	name, mimeType, data, err := mailparse.AttachmentBody(raw, part)
	if err != nil {
		if errors.Is(err, mailparse.ErrNoSuchPart) {
			s.fail(w, r, http.StatusNotFound, err)
			return
		}
		s.failFor(w, r, err)
		return
	}

	serveDownload(w, r, name, safeContentType(mimeType), data)
}

// serveDownload hands raw message bytes to the browser as a file. Three things
// keep sender-controlled content inert: it is always an attachment, never a
// document; the sniffing opt-out is on; and active MIME types are rewritten to
// application/octet-stream by safeContentType.
func serveDownload(w http.ResponseWriter, r *http.Request, name, mimeType string, data []byte) {
	h := w.Header()
	h.Set("Content-Security-Policy", downloadPolicy)
	h.Set("Content-Type", mimeType)
	h.Set("Content-Disposition", contentDisposition(name))
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// safeContentType keeps active types from being served back as themselves.
func safeContentType(mimeType string) string {
	lower := strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(lower, "text/html"),
		strings.HasPrefix(lower, "application/xhtml"),
		strings.HasPrefix(lower, "image/svg"),
		strings.Contains(lower, "javascript"):
		return "application/octet-stream"
	case lower == "":
		return "application/octet-stream"
	default:
		return mimeType
	}
}

// contentDisposition builds an attachment header that survives non-ASCII names.
func contentDisposition(name string) string {
	safe := mailparse.SafeFilename(name)
	return mime.FormatMediaType("attachment", map[string]string{"filename": safe})
}

// --- actions on a message ---------------------------------------------------

// actionResult reports what an action did.
type actionResult struct {
	Message  *store.Message `json:"message,omitempty"`
	Moved    int            `json:"moved,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

func (s *Server) handlePrio(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.SetBox(r.Context(), r.PathValue("id"), store.BoxPriority)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m})
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Box string `json:"box"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	m, err := s.st.SetBox(r.Context(), r.PathValue("id"), req.Box)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m})
}

func (s *Server) handleDelay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	m, err := s.st.Delay(r.Context(), r.PathValue("id"), req.Days)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m})
}

func (s *Server) handleIgnore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApplyExisting bool `json:"apply_existing"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	moved, err := s.st.Ignore(r.Context(), r.PathValue("id"), req.ApplyExisting)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	m, err := s.st.Get(r.PathValue("id"))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m, Moved: moved})
}

// handleDelete files a message in Trash. Like every box move, RunMirror then
// moves it to the Trash folder on the server.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.SetBox(r.Context(), r.PathValue("id"), store.BoxTrash)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m})
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Read bool `json:"read"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	m, err := s.st.MarkRead(r.Context(), r.PathValue("id"), req.Read)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: m})
}

// handleReply composes, sends and archives a reply, then files the original in
// the Processed box.
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body  string   `json:"body"`
		Cc    []string `json:"cc"`
		Quote bool     `json:"quote"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		s.fail(w, r, http.StatusBadRequest, errors.New("reply body is empty"))
		return
	}

	id := r.PathValue("id")
	m, err := s.st.Get(id)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	targets := m.ReplyTargets()
	if len(targets) == 0 {
		s.fail(w, r, http.StatusBadRequest, smtpsend.ErrNoRecipient)
		return
	}

	reply := smtpsend.Reply{
		To:         targets,
		Cc:         parseAddresses(req.Cc),
		Subject:    m.ReplySubject(),
		Body:       req.Body,
		InReplyTo:  m.Mail.MessageID,
		References: append(append([]string{}, m.Mail.References...), m.Mail.InReplyTo),
	}
	if req.Quote {
		body, rawErr := s.st.Raw(id)
		if rawErr != nil {
			s.failFor(w, r, rawErr)
			return
		}
		parsed, parseErr := mailparse.Parse(body)
		if parseErr != nil {
			s.failFor(w, r, parseErr)
			return
		}
		reply.Quote = parsed.Text
		reply.QuoteAttribution = fmt.Sprintf("On %s, %s wrote:",
			m.Mail.Date.Format("2 Jan 2006 15:04"), m.Mail.From.String())
	}

	now := time.Now()
	raw, err := s.sender.Compose(reply, now)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	if sendErr := s.sender.Send(r.Context(), raw, smtpsend.Recipients(reply)); sendErr != nil {
		s.fail(w, r, http.StatusBadGateway, fmt.Errorf("send reply: %w", sendErr))
		return
	}

	// The reply is out. Everything after this is bookkeeping: report each
	// failure as a warning rather than pretending the send failed.
	var warnings []string
	note := func(what string, cause error) {
		s.log.Printf("http: %s: %v", what, cause)
		warnings = append(warnings, "reply sent, but "+what+" failed: "+cause.Error())
	}
	if _, outErr := s.st.SaveOutbox(raw, now); outErr != nil {
		note("archiving it in mail.d/outbox", outErr)
	}
	if appendErr := s.syncer.AppendSent(raw, now); appendErr != nil {
		note("copying it to the IMAP Sent folder", appendErr)
	}
	if _, contactErr := s.st.AddContacts(smtpsend.Recipients(reply)...); contactErr != nil {
		note("updating the whitelist", contactErr)
	}

	moved, boxErr := s.st.SetBox(r.Context(), id, store.BoxProcessed)
	if boxErr != nil {
		note("moving the message to Processed", boxErr)
		moved = m
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Message: moved, Warnings: warnings})
}

// --- boxes, rules, contacts -------------------------------------------------

func (s *Server) handleCreateBox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	box, err := s.st.CreateBox(req.Name)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, box)
}

func (s *Server) handleDeleteBox(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteBox(r.Context(), r.PathValue("slug")); err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{})
}

// handleMoveAll moves every message in a box to another box, e.g. archiving
// all of Unprocessed into Processed. That can be tens of thousands of state
// writes, far longer than a request may take, so it runs in the background and
// answers 202; the UI follows the box counts.
func (s *Server) handleMoveAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Box string `json:"box"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	from := r.PathValue("slug")
	for _, box := range []string{from, req.Box} {
		if !s.st.HasBox(box) {
			s.failFor(w, r, fmt.Errorf("%w: %s", store.ErrUnknownBox, box))
			return
		}
	}
	if !s.movingAll.CompareAndSwap(false, true) {
		s.fail(w, r, http.StatusConflict, errors.New("another whole-box move is still running"))
		return
	}

	ctx := context.WithoutCancel(r.Context())
	go func() {
		defer s.movingAll.Store(false)
		start := time.Now()
		moved, err := s.st.MoveAll(ctx, from, req.Box)
		if err != nil {
			s.log.Printf("http: move all of %s to %s stopped after %d message(s): %v", from, req.Box, moved, err)
		} else {
			s.log.Printf("http: moved %d message(s) from %s to %s in %s", moved, from, req.Box, time.Since(start).Round(time.Second))
		}
		s.kickMirror()
	}()
	s.writeJSON(w, r, http.StatusAccepted, actionResult{})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, s.st.Rules())
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Match         string `json:"match"`
		Address       string `json:"address"`
		Box           string `json:"box"`
		ApplyExisting bool   `json:"apply_existing"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	rule := store.Rule{Field: store.MatchField(req.Match), Address: req.Address, Box: req.Box}
	if err := s.st.AddRule(rule); err != nil {
		s.failFor(w, r, err)
		return
	}
	moved := 0
	if req.ApplyExisting {
		n, err := s.st.ApplyRule(r.Context(), rule)
		if err != nil {
			s.failFor(w, r, err)
			return
		}
		moved = n
	}
	s.writeJSON(w, r, http.StatusCreated, actionResult{Moved: moved})
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Match   string `json:"match"`
		Address string `json:"address"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.st.DeleteRule(store.MatchField(req.Match), req.Address); err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{})
}

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, s.st.KnownAddresses())
}

// --- maintenance ------------------------------------------------------------

func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.Reindex(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]int{"indexed": n})
}

// --- helpers ----------------------------------------------------------------

func intParam(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("%d is negative", v)
	}
	return v, nil
}

// parseAddresses turns UI-entered strings into addresses, skipping blanks.
func parseAddresses(list []string) []mailparse.Address {
	out := make([]mailparse.Address, 0, len(list))
	for _, raw := range list {
		a := strings.ToLower(strings.TrimSpace(raw))
		if a == "" || !strings.Contains(a, "@") {
			continue
		}
		out = append(out, mailparse.Address{Address: a})
	}
	return out
}
