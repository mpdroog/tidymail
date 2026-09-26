package smtpsend

import (
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
