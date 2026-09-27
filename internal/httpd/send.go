package httpd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	"github.com/mpdroog/tidymail/internal/mailparse"
	"github.com/mpdroog/tidymail/internal/smtpsend"
)

// decodeCompose reads a send or reply request. Without files it is plain JSON;
// with files it is multipart/form-data holding that same JSON in a "json"
// field and every file in an "attachment" field. Parts are read one by one,
// each under a hard limit, so nothing unbounded is ever buffered.
func (s *Server) decodeCompose(r *http.Request, v any) ([]smtpsend.Attachment, error) {
	ctype, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ctype != "multipart/form-data" {
		return nil, decodeJSON(r, v)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("parse upload: %w", err)
	}

	var (
		atts    []smtpsend.Attachment
		total   int64
		gotJSON bool
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse upload: %w", err)
		}
		switch part.FormName() {
		case "json":
			if gotJSON {
				err = errors.New("parse upload: more than one json field")
				break
			}
			gotJSON = true
			var data []byte
			if data, err = readLimited(part, maxRequestBody); err != nil {
				break
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if decErr := dec.Decode(v); decErr != nil {
				err = fmt.Errorf("parse request body: %w", decErr)
			}
		case "attachment":
			var data []byte
			if data, err = readLimited(part, maxAttachments-total); err != nil {
				err = fmt.Errorf("attachments are larger than %d MB together: %w", maxAttachments>>20, err)
				break
			}
			total += int64(len(data))
			name := mailparse.SafeFilename(part.FileName())
			atts = append(atts, smtpsend.Attachment{Filename: name, ContentType: attachmentType(name, part.Header.Get("Content-Type")), Data: data})
		default:
			err = fmt.Errorf("parse upload: unexpected field %q", part.FormName())
		}
		if cerr := part.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("parse upload: %w", cerr)
		}
		if err != nil {
			return nil, err
		}
	}
	if !gotJSON {
		return nil, errors.New("parse upload: the json field is missing")
	}
	return atts, nil
}

// errTooLarge is returned by readLimited for a part over its limit.
var errTooLarge = errors.New("too large")

// readLimited reads r completely, failing once it exceeds limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// attachmentType picks a content type for an uploaded file: what the browser
// said, else what the file name suggests, else opaque bytes.
func attachmentType(name, sent string) string {
	if t, _, err := mime.ParseMediaType(sent); err == nil && t != "application/octet-stream" {
		return t
	}
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// accountView is an account the UI can send from.
type accountView struct {
	Name     string `json:"name"`
	FromName string `json:"from_name"`
	FromAddr string `json:"from_addr"`
}

// accountViews lists the configured accounts in config order.
func (s *Server) accountViews() []accountView {
	out := make([]accountView, 0, len(s.cfg.Accounts))
	for i := range s.cfg.Accounts {
		a := &s.cfg.Accounts[i]
		out = append(out, accountView{Name: a.Name, FromName: a.SMTP.FromName, FromAddr: a.SMTP.FromAddr})
	}
	return out
}

// errBadAddress marks a recipient the person has to correct.
var errBadAddress = errors.New("invalid address")

// strictAddresses parses recipients typed for a new message. Unlike a reply's
// Cc, a typo is an error: dropping it quietly would send the mail to fewer
// people than the person wrote down.
func strictAddresses(field string, list []string) ([]mailparse.Address, error) {
	out := make([]mailparse.Address, 0, len(list))
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		a, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("%w in %s: %q", errBadAddress, field, raw)
		}
		out = append(out, mailparse.Address{Name: a.Name, Address: strings.ToLower(a.Address)})
	}
	return out, nil
}

// handleSend composes and sends a new message from one account: the message
// is not a reply, so it has no thread headers and files nothing.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string   `json:"account"`
		To      []string `json:"to"`
		Cc      []string `json:"cc"`
		Subject string   `json:"subject"`
		Body    string   `json:"body"`
	}
	atts, err := s.decodeCompose(r, &req)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	to, err := strictAddresses("To", req.To)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	cc, err := strictAddresses("Cc", req.Cc)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if len(to) == 0 {
		s.fail(w, r, http.StatusBadRequest, errors.New("add at least one address in To"))
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		s.fail(w, r, http.StatusBadRequest, errors.New("message body is empty"))
		return
	}
	acct, err := s.account(req.Account)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}

	msg := smtpsend.Reply{To: to, Cc: cc, Subject: strings.TrimSpace(req.Subject), Body: req.Body, Attachments: atts}
	now := time.Now()
	raw, err := acct.Sender.Compose(msg, now)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	if sendErr := acct.Sender.Send(r.Context(), raw, smtpsend.Recipients(msg)); sendErr != nil {
		s.fail(w, r, http.StatusBadGateway, fmt.Errorf("send message: %w", sendErr))
		return
	}

	// The message is out. Everything after this is bookkeeping: report each
	// failure as a warning rather than pretending the send failed.
	var warnings []string
	note := func(what string, cause error) {
		s.log.Printf("http: %s: %v", what, cause)
		warnings = append(warnings, "message sent, but "+what+" failed: "+cause.Error())
	}
	if _, outErr := s.st.SaveOutbox(raw, now); outErr != nil {
		note("archiving it in mail.d/outbox", outErr)
	}
	if appendErr := acct.Syncer.AppendSent(raw, now); appendErr != nil {
		note("copying it to the IMAP Sent folder", appendErr)
	}
	if _, contactErr := s.st.AddContacts(smtpsend.Recipients(msg)...); contactErr != nil {
		note("updating the whitelist", contactErr)
	}
	s.writeJSON(w, r, http.StatusOK, actionResult{Warnings: warnings})
}
