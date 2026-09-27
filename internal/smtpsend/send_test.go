package smtpsend

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/mailparse"
)

func testSender() *Sender {
	return New(config.SMTP{
		Host:     "smtp.example.com",
		Port:     587,
		TLS:      config.TLSStartTLS,
		FromName: "Me",
		FromAddr: "me@example.com",
	})
}

func TestComposeSetsThreadingHeaders(t *testing.T) {
	s := testSender()
	raw, err := s.Compose(Reply{
		To:               []mailparse.Address{{Name: "Ann", Address: "ann@example.com"}},
		Cc:               []mailparse.Address{{Address: "cc@example.com"}},
		Subject:          "Re: the thing",
		Body:             "Sounds good.",
		InReplyTo:        "<abc@example.com>",
		References:       []string{"<root@example.com>", "<abc@example.com>"},
		Quote:            "original line one\n\nline two",
		QuoteAttribution: "On 31 Aug 2026 10:00, Ann <ann@example.com> wrote:",
	}, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	parsed, err := mailparse.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Subject != "Re: the thing" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	if got := mailparse.FirstAddress(parsed.From).Address; got != "me@example.com" {
		t.Errorf("from = %q", got)
	}
	if got := mailparse.AddressStrings(parsed.To); len(got) != 1 || got[0] != "ann@example.com" {
		t.Errorf("to = %v", got)
	}
	if got := mailparse.AddressStrings(parsed.Cc); len(got) != 1 || got[0] != "cc@example.com" {
		t.Errorf("cc = %v", got)
	}
	if parsed.InReplyTo != "abc@example.com" {
		t.Errorf("in-reply-to = %q", parsed.InReplyTo)
	}
	// References must carry the whole chain, without duplicating In-Reply-To.
	if len(parsed.References) != 2 {
		t.Errorf("references = %v, want the two-message chain", parsed.References)
	}
	if parsed.MessageID == "" {
		t.Error("reply has no Message-ID")
	}
	if !strings.Contains(parsed.Text, "Sounds good.") {
		t.Errorf("body missing the reply text: %q", parsed.Text)
	}
	if !strings.Contains(parsed.Text, "> original line one") {
		t.Errorf("body missing the quoted original: %q", parsed.Text)
	}
}

func TestComposeRejectsMissingRecipient(t *testing.T) {
	s := testSender()
	if _, err := s.Compose(Reply{Subject: "x", Body: "y"}, time.Now()); err == nil {
		t.Fatal("Compose with no recipient should fail")
	}
}

func TestComposeEncodesNonASCII(t *testing.T) {
	s := testSender()
	raw, err := s.Compose(Reply{
		To:      []mailparse.Address{{Address: "ann@example.com"}},
		Subject: "Grüße und Auszüge",
		Body:    "Schöne Grüße — ✓",
	}, time.Now())
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	for i, b := range raw {
		if b > 0x7f {
			t.Fatalf("raw message has a raw 8-bit byte at offset %d; headers and body must be encoded", i)
		}
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Subject != "Grüße und Auszüge" {
		t.Errorf("subject round trip = %q", parsed.Subject)
	}
	if !strings.Contains(parsed.Text, "Schöne Grüße — ✓") {
		t.Errorf("body round trip = %q", parsed.Text)
	}
}

func TestRecipientsIncludesCc(t *testing.T) {
	got := Recipients(Reply{
		To: []mailparse.Address{{Address: "a@example.com"}},
		Cc: []mailparse.Address{{Address: "b@example.com"}, {Address: "c@example.com"}},
	})
	if len(got) != 3 {
		t.Fatalf("Recipients = %v, want 3 envelope recipients", got)
	}
}

func TestComposeWithAttachments(t *testing.T) {
	s := testSender()
	pdf := []byte("%PDF-1.4 not really a pdf")
	raw, err := s.Compose(Reply{
		To:      []mailparse.Address{{Address: "ann@example.com"}},
		Subject: "Files",
		Body:    "Two files attached.",
		Attachments: []Attachment{
			{Filename: "report.pdf", ContentType: "application/pdf", Data: pdf},
			{Filename: "notes €.txt", Data: []byte("plain notes")},
		},
	}, time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(parsed.Text, "Two files attached.") {
		t.Errorf("text = %q, want the body", parsed.Text)
	}
	if len(parsed.Attachments) != 2 {
		t.Fatalf("attachments = %+v, want 2", parsed.Attachments)
	}
	if parsed.Attachments[0].Filename != "report.pdf" || parsed.Attachments[1].Filename != "notes €.txt" {
		t.Errorf("filenames = %q, %q", parsed.Attachments[0].Filename, parsed.Attachments[1].Filename)
	}
	name, ctype, data, err := mailparse.AttachmentBody(raw, parsed.Attachments[0].Part)
	if err != nil {
		t.Fatalf("AttachmentBody: %v", err)
	}
	if name != "report.pdf" || !strings.HasPrefix(ctype, "application/pdf") || !bytes.Equal(data, pdf) {
		t.Errorf("attachment = %q %q %q, want the PDF back byte for byte", name, ctype, data)
	}
}
