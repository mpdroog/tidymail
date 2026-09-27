// Package smtpsend composes and delivers replies over SMTP submission.
package smtpsend

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"
	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/mailparse"
)

// dialTimeout bounds how long a submission attempt waits for the server.
const dialTimeout = 30 * time.Second

// Sender delivers messages for one account.
type Sender struct {
	cfg config.SMTP
}

// New returns a Sender for the given account.
func New(cfg config.SMTP) *Sender { return &Sender{cfg: cfg} }

// Reply is a message to send in response to a stored one.
type Reply struct {
	To         []mailparse.Address
	Cc         []mailparse.Address
	Subject    string
	Body       string
	InReplyTo  string
	References []string
	// Quote is the original message text, appended below the reply in the
	// usual "> " form. It may be empty.
	Quote string
	// QuoteAttribution is the "On <date>, <who> wrote:" line.
	QuoteAttribution string
	// Attachments are sent as files next to the text.
	Attachments []Attachment
}

// Attachment is a file sent with a message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ErrNoRecipient is returned when a reply has nobody to go to.
var ErrNoRecipient = errors.New("reply has no recipient")

// Compose renders the reply as a raw RFC 5322 message. It is separate from
// Send so the result can be archived and inspected even if delivery fails.
func (s *Sender) Compose(r Reply, now time.Time) ([]byte, error) {
	if len(r.To) == 0 {
		return nil, ErrNoRecipient
	}
	if strings.TrimSpace(s.cfg.FromAddr) == "" {
		return nil, fmt.Errorf("%w: smtp.from_addr is empty", ErrNoRecipient)
	}

	var h mail.Header
	h.SetAddressList("From", []*mail.Address{{Name: s.cfg.FromName, Address: s.cfg.FromAddr}})
	h.SetAddressList("To", toMailAddresses(r.To))
	if len(r.Cc) > 0 {
		h.SetAddressList("Cc", toMailAddresses(r.Cc))
	}
	h.SetSubject(r.Subject)
	h.SetDate(now)
	if r.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{trimAngles(r.InReplyTo)})
	}
	if refs := msgIDList(r.References, r.InReplyTo); len(refs) > 0 {
		h.SetMsgIDList("References", refs)
	}
	if err := h.GenerateMessageID(); err != nil {
		return nil, fmt.Errorf("generate message id: %w", err)
	}
	if len(r.Attachments) > 0 {
		return composeMixed(h, buildBody(r), r.Attachments)
	}
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})

	var buf bytes.Buffer
	w, err := mail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("create message writer: %w", err)
	}
	body := buildBody(r)
	if _, err := io.WriteString(w, body); err != nil {
		if cerr := w.Close(); cerr != nil {
			return nil, fmt.Errorf("write body: %w (and close: %w)", err, cerr)
		}
		return nil, fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close message writer: %w", err)
	}
	return buf.Bytes(), nil
}

// composeMixed renders a multipart/mixed message: the text, then one part per
// attachment.
func composeMixed(h mail.Header, body string, atts []Attachment) ([]byte, error) {
	var buf bytes.Buffer
	mw, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("create message writer: %w", err)
	}
	var th mail.InlineHeader
	th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	tw, err := mw.CreateSingleInline(th)
	if err != nil {
		return nil, fmt.Errorf("create text part: %w", err)
	}
	if err := writeAndClose(tw, []byte(body)); err != nil {
		return nil, fmt.Errorf("write text part: %w", err)
	}
	for _, a := range atts {
		var ah mail.AttachmentHeader
		ctype := a.ContentType
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		ah.SetContentType(ctype, nil)
		ah.SetFilename(a.Filename)
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return nil, fmt.Errorf("create attachment %q: %w", a.Filename, err)
		}
		if err := writeAndClose(aw, a.Data); err != nil {
			return nil, fmt.Errorf("write attachment %q: %w", a.Filename, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close message writer: %w", err)
	}
	return buf.Bytes(), nil
}

// writeAndClose writes one part and closes it, reporting both failures.
func writeAndClose(w io.WriteCloser, data []byte) error {
	if _, err := w.Write(data); err != nil {
		if cerr := w.Close(); cerr != nil {
			return fmt.Errorf("%w (and close: %w)", err, cerr)
		}
		return err
	}
	return w.Close()
}

// buildBody joins the reply text with the quoted original.
func buildBody(r Reply) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(r.Body, "\n"))
	if strings.TrimSpace(r.Quote) == "" {
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString("\n\n")
	if r.QuoteAttribution != "" {
		b.WriteString(r.QuoteAttribution)
		b.WriteString("\n")
	}
	for _, line := range strings.Split(strings.ReplaceAll(r.Quote, "\r\n", "\n"), "\n") {
		if line == "" {
			b.WriteString(">\n")
			continue
		}
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Send delivers a raw message to the listed recipients.
func (s *Sender) Send(ctx context.Context, raw []byte, recipients []string) (err error) {
	if len(recipients) == 0 {
		return ErrNoRecipient
	}
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Quit sends QUIT and closes; if the session already failed, Close
		// is the honest fallback and both outcomes are reported.
		if qerr := client.Quit(); qerr != nil {
			if cerr := client.Close(); cerr != nil && !errors.Is(cerr, io.EOF) {
				qerr = fmt.Errorf("%w (and close: %w)", qerr, cerr)
			}
			if err == nil {
				err = fmt.Errorf("smtp quit: %w", qerr)
			}
		}
	}()

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth as %s: %w", s.cfg.Username, err)
		}
	}
	if err = client.Mail(s.cfg.FromAddr); err != nil {
		return fmt.Errorf("smtp MAIL FROM %s: %w", s.cfg.FromAddr, err)
	}
	for _, rcpt := range recipients {
		if err = client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", rcpt, err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err = wc.Write(raw); err != nil {
		if cerr := wc.Close(); cerr != nil {
			return fmt.Errorf("write message: %w (and close: %w)", err, cerr)
		}
		return fmt.Errorf("write message: %w", err)
	}
	if err = wc.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	return nil
}

// dial opens an authenticated-capable SMTP session honouring the TLS mode.
func (s *Sender) dial(ctx context.Context) (*smtp.Client, error) {
	addr := s.cfg.Addr()
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: dialTimeout}

	if s.cfg.TLS == config.TLSImplicit {
		conn, err := (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial smtp %s: %w", addr, err)
		}
		client, err := smtp.NewClient(conn, s.cfg.Host)
		if err != nil {
			if cerr := conn.Close(); cerr != nil {
				return nil, fmt.Errorf("smtp handshake: %w (and close: %w)", err, cerr)
			}
			return nil, fmt.Errorf("smtp handshake with %s: %w", addr, err)
		}
		return client, nil
	}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial smtp %s: %w", addr, err)
	}
	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		if cerr := conn.Close(); cerr != nil {
			return nil, fmt.Errorf("smtp handshake: %w (and close: %w)", err, cerr)
		}
		return nil, fmt.Errorf("smtp handshake with %s: %w", addr, err)
	}
	if s.cfg.TLS == config.TLSStartTLS {
		if err := client.StartTLS(tlsCfg); err != nil {
			if cerr := client.Close(); cerr != nil {
				return nil, fmt.Errorf("starttls: %w (and close: %w)", err, cerr)
			}
			return nil, fmt.Errorf("starttls with %s: %w", addr, err)
		}
	}
	return client, nil
}

func toMailAddresses(list []mailparse.Address) []*mail.Address {
	out := make([]*mail.Address, 0, len(list))
	for _, a := range list {
		out = append(out, &mail.Address{Name: a.Name, Address: a.Address})
	}
	return out
}

func trimAngles(id string) string {
	return strings.Trim(strings.TrimSpace(id), "<>")
}

// msgIDList builds the References header: the original chain plus the message
// being replied to, without duplicates.
func msgIDList(refs []string, inReplyTo string) []string {
	seen := make(map[string]struct{}, len(refs)+1)
	out := make([]string, 0, len(refs)+1)
	add := func(id string) {
		id = trimAngles(id)
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, r := range refs {
		add(r)
	}
	add(inReplyTo)
	return out
}

// Recipients flattens the To and Cc lists into envelope recipients.
func Recipients(r Reply) []string {
	out := make([]string, 0, len(r.To)+len(r.Cc))
	out = append(out, mailparse.AddressStrings(r.To)...)
	out = append(out, mailparse.AddressStrings(r.Cc)...)
	return out
}
