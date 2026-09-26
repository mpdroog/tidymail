package mailparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	"github.com/emersion/go-message/mail"
)

// maxBodyBytes caps how much of a single body part is turned into text, so a
// pathological message cannot exhaust memory.
const maxBodyBytes = 8 << 20 // 8 MiB

// Address is one parsed mail address.
type Address struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// String renders the address for display.
func (a Address) String() string {
	if a.Name == "" {
		return a.Address
	}
	return a.Name + " <" + a.Address + ">"
}

// Attachment describes a non-inline part. Part is its ordinal in the message,
// used to fetch the bytes again later without keeping them in memory.
type Attachment struct {
	Part     int    `json:"part"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// Message is everything tidymail needs from a raw mail.
type Message struct {
	MessageID   string
	InReplyTo   string
	References  []string
	From        []Address
	ReplyTo     []Address
	To          []Address
	Cc          []Address
	Subject     string
	Date        time.Time
	Text        string
	Attachments []Attachment
	// Warnings collects recoverable defects (a bad Date, an undecodable
	// part). They are surfaced rather than dropped.
	Warnings []string
}

// ErrEmpty is returned when the input contains no message at all.
var ErrEmpty = errors.New("empty message")

// Parse reads a raw RFC 5322 message.
func Parse(raw []byte) (*Message, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrEmpty
	}
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, fmt.Errorf("read message: %w", err)
	}
	out := &Message{}
	if err != nil {
		out.Warnings = append(out.Warnings, "unknown charset: "+err.Error())
	}

	mr := mail.NewReader(ent)
	defer func() {
		// Closing a mail.Reader only releases readers; a failure here is
		// worth knowing about but must not mask a successful parse.
		if cerr := mr.Close(); cerr != nil {
			out.Warnings = append(out.Warnings, "close reader: "+cerr.Error())
		}
	}()

	out.readHeader(&mr.Header)
	out.readParts(mr, raw)
	out.Text = collapseBlankLines(out.Text)
	return out, nil
}

func (m *Message) readHeader(h *mail.Header) {
	if v, err := h.Subject(); err != nil {
		m.Warnings = append(m.Warnings, "subject: "+err.Error())
		m.Subject = strings.TrimSpace(h.Get("Subject"))
	} else {
		m.Subject = strings.TrimSpace(v)
	}

	if v, err := h.Date(); err != nil {
		m.Warnings = append(m.Warnings, "date: "+err.Error())
	} else {
		m.Date = v.UTC()
	}

	if v, err := h.MessageID(); err != nil {
		m.Warnings = append(m.Warnings, "message-id: "+err.Error())
	} else {
		m.MessageID = v
	}

	if list, err := h.MsgIDList("In-Reply-To"); err != nil {
		m.Warnings = append(m.Warnings, "in-reply-to: "+err.Error())
	} else if len(list) > 0 {
		m.InReplyTo = list[0]
	}

	if list, err := h.MsgIDList("References"); err != nil {
		m.Warnings = append(m.Warnings, "references: "+err.Error())
	} else {
		m.References = list
	}

	m.From = m.addressList(h, "From")
	m.ReplyTo = m.addressList(h, "Reply-To")
	m.To = m.addressList(h, "To")
	m.Cc = m.addressList(h, "Cc")
}

func (m *Message) addressList(h *mail.Header, key string) []Address {
	// AddressList returns (nil, nil) when the header is absent, so any
	// error here is a genuine parse failure worth reporting.
	list, err := h.AddressList(key)
	if err != nil {
		m.Warnings = append(m.Warnings, strings.ToLower(key)+": "+err.Error())
		return nil
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		if a == nil || a.Address == "" {
			continue
		}
		out = append(out, Address{Name: strings.TrimSpace(a.Name), Address: strings.ToLower(a.Address)})
	}
	return out
}

// readParts walks the MIME tree collecting the best text body and the
// attachment list. Malformed MIME never fails the parse: the raw message is
// kept regardless, so defects become warnings and, when no text could be
// extracted, the undecoded body stands in for it.
func (m *Message) readParts(mr *mail.Reader, raw []byte) {
	var plain, html strings.Builder
	part := 0
	broken := false
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				m.Warnings = append(m.Warnings, "part: "+err.Error())
				continue
			}
			// The multipart reader cannot resync after this, so keep
			// what was read so far and stop.
			m.Warnings = append(m.Warnings, "next part: "+err.Error())
			broken = true
			break
		}
		part++

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			if !m.readInline(h, p.Body, &plain, &html) {
				broken = true
			}
		case *mail.AttachmentHeader:
			m.readAttachment(h, p.Body, part)
		default:
			m.Warnings = append(m.Warnings, fmt.Sprintf("part %d: unexpected header type %T", part, p.Header))
		}
	}

	if plain.Len() > 0 {
		m.Text = plain.String()
		return
	}
	if html.Len() > 0 {
		// A body that will not render as text is a defect in the message,
		// not in tidymail: keep the rest of the message and say so.
		if text, htmlErr := HTMLToText(strings.NewReader(html.String())); htmlErr != nil {
			m.Warnings = append(m.Warnings, "html body: "+htmlErr.Error())
		} else {
			m.Text = text
		}
	}
	if m.Text == "" && broken {
		m.Warnings = append(m.Warnings, "no decodable text part, using the raw body")
		m.Text = rawBodyText(raw)
	}
}

// rawBodyText returns everything after the top-level header block, rendered
// as text when it looks like HTML.
func rawBodyText(raw []byte) string {
	body := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		body = raw[i+4:]
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		body = raw[i+2:]
	}
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	if bytes.Contains(bytes.ToLower(body), []byte("<html")) {
		if text, err := HTMLToText(bytes.NewReader(body)); err == nil {
			return text
		}
	}
	return string(body)
}

// readInline appends a text part to plain or htm. It reports false when the
// part could not be fully decoded; whatever was decoded is still kept.
func (m *Message) readInline(h *mail.InlineHeader, body io.Reader, plain, htm *strings.Builder) bool {
	ct, _, err := h.ContentType()
	if err != nil {
		m.Warnings = append(m.Warnings, "content-type: "+err.Error())
		ct = "text/plain"
	}
	ok := true
	b, err := io.ReadAll(io.LimitReader(body, maxBodyBytes))
	if err != nil {
		m.Warnings = append(m.Warnings, "read inline part: "+err.Error())
		ok = false
	}
	switch {
	case strings.EqualFold(ct, "text/html"):
		htm.Write(b)
	case strings.HasPrefix(strings.ToLower(ct), "text/"):
		if plain.Len() > 0 {
			plain.WriteString("\n\n")
		}
		plain.Write(b)
	default:
		m.Warnings = append(m.Warnings, "skipped inline part of type "+ct)
	}
	return ok
}

func (m *Message) readAttachment(h *mail.AttachmentHeader, body io.Reader, part int) {
	name, err := h.Filename()
	if err != nil {
		m.Warnings = append(m.Warnings, fmt.Sprintf("part %d filename: %v", part, err))
	}
	ct, _, err := h.ContentType()
	if err != nil {
		m.Warnings = append(m.Warnings, fmt.Sprintf("part %d content-type: %v", part, err))
		ct = "application/octet-stream"
	}
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		m.Warnings = append(m.Warnings, fmt.Sprintf("part %d size: %v", part, err))
	}
	if name == "" {
		name = fmt.Sprintf("part-%d%s", part, extensionFor(ct))
	}
	m.Attachments = append(m.Attachments, Attachment{
		Part:     part,
		Filename: SafeFilename(name),
		MIMEType: ct,
		Size:     n,
	})
}

// AttachmentBody returns the bytes of the attachment with the given part
// ordinal, re-reading the raw message.
func AttachmentBody(raw []byte, part int) (name, mimeType string, data []byte, err error) {
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return "", "", nil, fmt.Errorf("read message: %w", err)
	}
	mr := mail.NewReader(ent)
	defer func() {
		if cerr := mr.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close reader: %w", cerr)
		}
	}()

	n := 0
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			return "", "", nil, fmt.Errorf("part %d: %w", part, ErrNoSuchPart)
		}
		if perr != nil {
			if message.IsUnknownCharset(perr) || message.IsUnknownEncoding(perr) {
				continue
			}
			return "", "", nil, fmt.Errorf("next part: %w", perr)
		}
		n++
		if n != part {
			continue
		}
		ah, ok := p.Header.(*mail.AttachmentHeader)
		if !ok {
			return "", "", nil, fmt.Errorf("part %d: %w", part, ErrNoSuchPart)
		}
		data, err = io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
		if err != nil {
			return "", "", nil, fmt.Errorf("read part %d: %w", part, err)
		}
		name, err = ah.Filename()
		if err != nil {
			name = ""
		}
		mimeType, _, err = ah.ContentType()
		if err != nil {
			mimeType = "application/octet-stream"
		}
		if name == "" {
			name = fmt.Sprintf("part-%d%s", part, extensionFor(mimeType))
		}
		return SafeFilename(name), mimeType, data, nil
	}
}

// ErrNoSuchPart is returned when an attachment ordinal does not exist.
var ErrNoSuchPart = errors.New("no such attachment")

func extensionFor(mimeType string) string {
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ".bin"
	}
	return exts[0]
}

// SafeFilename strips directory separators and control characters so an
// attachment name can never escape a download directory.
func SafeFilename(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteByte('_')
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case r == '"':
			b.WriteByte('\'')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" || out == ".." {
		return "attachment.bin"
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// FirstAddress returns the first address in the list, or the zero value.
func FirstAddress(list []Address) Address {
	if len(list) == 0 {
		return Address{}
	}
	return list[0]
}

// AddressStrings flattens a list to bare addresses.
func AddressStrings(list []Address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Address != "" {
			out = append(out, a.Address)
		}
	}
	return out
}
