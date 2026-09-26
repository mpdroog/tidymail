package mailparse

import (
	"strings"
	"testing"
)

func TestParsePlainText(t *testing.T) {
	raw := []byte("From: Ann Example <Ann@Example.COM>\r\n" +
		"To: me@example.com, Bob <bob@example.com>\r\n" +
		"Cc: cc@example.com\r\n" +
		"Subject: =?utf-8?B?R3LDvMOfZQ==?=\r\n" +
		"Date: Mon, 31 Aug 2026 10:00:00 +0200\r\n" +
		"Message-ID: <one@example.com>\r\n" +
		"In-Reply-To: <zero@example.com>\r\n" +
		"References: <root@example.com> <zero@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nhello there\r\n")

	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Subject != "Grüße" {
		t.Errorf("subject = %q, want the decoded RFC 2047 word", m.Subject)
	}
	if got := FirstAddress(m.From).Address; got != "ann@example.com" {
		t.Errorf("from = %q, want it lowercased", got)
	}
	if got := AddressStrings(m.To); len(got) != 2 {
		t.Errorf("to = %v, want both recipients", got)
	}
	if m.InReplyTo != "zero@example.com" || len(m.References) != 2 {
		t.Errorf("threading headers: in-reply-to=%q refs=%v", m.InReplyTo, m.References)
	}
	if m.Date.IsZero() || m.Date.Hour() != 8 {
		t.Errorf("date = %v, want it normalised to UTC", m.Date)
	}
	if strings.TrimSpace(m.Text) != "hello there" {
		t.Errorf("body = %q", m.Text)
	}
}

func TestParsePrefersPlainOverHTML(t *testing.T) {
	raw := []byte("From: a@b.com\r\nSubject: alt\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nthe plain part\r\n" +
		"--x\r\nContent-Type: text/html\r\n\r\n<p>the html part</p>\r\n--x--\r\n")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(m.Text, "the plain part") || strings.Contains(m.Text, "html part") {
		t.Errorf("body = %q, want the text/plain alternative", m.Text)
	}
}

func TestLatin1BodyIsDecoded(t *testing.T) {
	raw := []byte("From: a@b.com\r\nSubject: latin\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n\r\nGr\xfc\xdfe\r\n")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(m.Text, "Grüße") {
		t.Errorf("body = %q, want the latin-1 bytes decoded to UTF-8", m.Text)
	}
}

func TestAttachmentsAreListedAndFetchable(t *testing.T) {
	raw := []byte("From: a@b.com\r\nSubject: files\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\n\r\n" +
		"%PDF-1.4 fake\r\n--b--\r\n")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want one", m.Attachments)
	}
	att := m.Attachments[0]
	if att.Filename != "report.pdf" || att.MIMEType != "application/pdf" {
		t.Errorf("attachment = %+v", att)
	}

	name, mimeType, data, err := AttachmentBody(raw, att.Part)
	if err != nil {
		t.Fatalf("AttachmentBody: %v", err)
	}
	if name != "report.pdf" || mimeType != "application/pdf" || !strings.Contains(string(data), "%PDF") {
		t.Errorf("fetched %q (%s): %q", name, mimeType, data)
	}
	if _, _, _, err := AttachmentBody(raw, 99); err == nil {
		t.Error("fetching a part that does not exist should fail")
	}
}

func TestSafeFilenameCannotEscape(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd": "_.._etc_passwd",
		"/etc/shadow":      "_etc_shadow",
		"..":               "attachment.bin",
		"":                 "attachment.bin",
		"ok.txt":           "ok.txt",
		"a\x00b.txt":       "a_b.txt",
	} {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLToTextDropsActiveContent(t *testing.T) {
	html := `<html><head><style>b{color:red}</style><script>alert(1)</script></head>
	<body><h1>Title</h1><p>Some <b>bold</b> text.</p>
	<a href="https://example.com/x">click</a><br><img alt="a picture" src="x">
	<ul><li>one</li><li>two</li></ul></body></html>`
	got, err := HTMLToText(strings.NewReader(html))
	if err != nil {
		t.Fatalf("HTMLToText: %v", err)
	}
	for _, bad := range []string{"alert(1)", "color:red", "<b>", "<script"} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q: %q", bad, got)
		}
	}
	for _, want := range []string{"Title", "Some bold text.", "<https://example.com/x>", "[image: a picture]", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
}

func TestParseEmptyInput(t *testing.T) {
	if _, err := Parse([]byte("   \r\n")); err == nil {
		t.Fatal("parsing empty input should fail")
	}
}

// Malformed MIME seen in the wild must degrade to warnings: the raw message is
// still worth keeping.
func TestMalformedMIMEIsAWarningNotAFailure(t *testing.T) {
	head := "From: a@b.com\r\nSubject: broken\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n"
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"preamble line longer than the peek buffer": {
			body: strings.Repeat("A", 5000) + "\r\n--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n",
			want: "AAAA",
		},
		"part without a header block": {
			body: "--b\r\nThis is a MIME encoded message.\r\n\r\nhello\r\n--b--\r\n",
			want: "This is a MIME encoded message.",
		},
		"body labelled base64 that is not": {
			body: "--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
				"<html>hello</html>\r\n--b--\r\n",
			want: "hello",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := Parse([]byte(head + tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(m.Warnings) == 0 {
				t.Error("the defect should be reported as a warning")
			}
			if !strings.Contains(m.Text, tc.want) {
				t.Errorf("body = %q, want it to contain %q", m.Text, tc.want)
			}
		})
	}
}

func TestBrokenDateIsAWarningNotAFailure(t *testing.T) {
	raw := []byte("From: a@b.com\r\nSubject: s\r\nDate: not a date\r\n\r\nbody\r\n")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.Date.IsZero() {
		t.Errorf("date = %v, want the zero time", m.Date)
	}
	if len(m.Warnings) == 0 {
		t.Error("a bad Date header should be reported as a warning, not swallowed")
	}
}
