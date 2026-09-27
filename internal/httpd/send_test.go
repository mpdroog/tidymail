package httpd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/imapsync"
	"github.com/mpdroog/tidymail/internal/mailparse"
	"github.com/mpdroog/tidymail/internal/smtpsend"
)

// fakeSMTP is a minimal SMTP server that accepts one message per session and
// records its envelope and data.
type fakeSMTP struct {
	mu   sync.Mutex
	from string
	rcpt []string
	data string
}

func startFakeSMTP(t *testing.T) (server *fakeSMTP, port int) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := ln.Close(); closeErr != nil && !strings.Contains(closeErr.Error(), "closed") {
			t.Errorf("close listener: %v", closeErr)
		}
	})
	f := &fakeSMTP{}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go f.serve(t, conn)
		}
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return f, port
}

func (f *fakeSMTP) serve(t *testing.T, conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Errorf("close smtp conn: %v", err)
		}
	}()
	r := bufio.NewReader(conn)
	say := func(line string) bool {
		_, err := io.WriteString(conn, line+"\r\n")
		return err == nil
	}
	if !say("220 fake ESMTP") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.Trim(strings.TrimSpace(line)[len("MAIL FROM:"):], "<> ")
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.Trim(strings.TrimSpace(line)[len("RCPT TO:"):], "<> "))
			f.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				dl, dataErr := r.ReadString('\n')
				if dataErr != nil {
					return
				}
				if dl == ".\r\n" {
					break
				}
				b.WriteString(dl)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSendNewMessage(t *testing.T) {
	srv, st := newTestServer(t)
	smtpSrv, port := startFakeSMTP(t)

	smtpCfg := config.SMTP{Host: "127.0.0.1", Port: port, TLS: config.TLSNone, FromName: "Me", FromAddr: "me@work.example"}
	// No IMAP server: copying to Sent fails, which must be a warning only.
	imapCfg := config.IMAP{Host: "127.0.0.1", Port: 1, TLS: config.TLSNone, Username: "me", Password: "x", Inbox: "INBOX"}
	srv.cfg.Accounts = append(srv.cfg.Accounts, config.Account{Name: "work", SMTP: smtpCfg, IMAP: imapCfg})
	srv.accounts = []Account{{Name: "work", Sender: smtpsend.New(smtpCfg), Syncer: imapsync.New("work", imapCfg, st, log.New(io.Discard, "", 0))}}

	rec := do(t, srv, http.MethodPost, "/api/send",
		`{"account":"work","to":["Ann <Ann@Example.com>"],"cc":["bob@example.com"],"subject":"Hello","body":"First line.\nSecond line."}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Sent folder") {
		t.Errorf("warnings = %v, want only the failed copy to Sent", res.Warnings)
	}

	smtpSrv.mu.Lock()
	from, rcpt, data := smtpSrv.from, smtpSrv.rcpt, smtpSrv.data
	smtpSrv.mu.Unlock()
	if from != "me@work.example" {
		t.Errorf("MAIL FROM = %q, want the chosen account's address", from)
	}
	if strings.Join(rcpt, ",") != "ann@example.com,bob@example.com" {
		t.Errorf("RCPT TO = %v", rcpt)
	}
	for _, want := range []string{"Subject: Hello", "Second line.", "ann@example.com"} {
		if !strings.Contains(data, want) {
			t.Errorf("message lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, "In-Reply-To") {
		t.Error("a new message must not carry reply headers")
	}
	whitelisted := false
	for _, c := range st.Contacts() {
		if c == "ann@example.com" {
			whitelisted = true
		}
	}
	if !whitelisted {
		t.Error("recipients should be whitelisted after sending")
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	srv, _ := newTestServer(t)
	for name, body := range map[string]string{
		"no recipient":    `{"to":[],"body":"hi"}`,
		"typo in to":      `{"to":["ann@example.com","bob at example"],"body":"hi"}`,
		"typo in cc":      `{"to":["ann@example.com"],"cc":["nope"],"body":"hi"}`,
		"empty body":      `{"to":["ann@example.com"],"body":"  "}`,
		"unknown account": `{"account":"gone","to":["ann@example.com"],"body":"hi"}`,
	} {
		rec := do(t, srv, http.MethodPost, "/api/send", body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, srv, http.MethodPost, "/api/send", `{"to":["ann@example.com"],"body":"hi"}`, false); rec.Code != http.StatusForbidden {
		t.Errorf("send without CSRF token = %d, want 403", rec.Code)
	}
}

// upload posts a multipart send request: the JSON fields plus files.
func upload(t *testing.T, srv *Server, jsonField string, files map[string][]byte, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("json", jsonField); err != nil {
		t.Fatalf("write json field: %v", err)
	}
	for k, v := range extra {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	for name, data := range files {
		fw, err := mw.CreateFormFile("attachment", name)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/send", &body)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.csrf)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSendWithAttachments(t *testing.T) {
	srv, st := newTestServer(t)
	smtpSrv, port := startFakeSMTP(t)
	smtpCfg := config.SMTP{Host: "127.0.0.1", Port: port, TLS: config.TLSNone, FromAddr: "me@work.example"}
	imapCfg := config.IMAP{Host: "127.0.0.1", Port: 1, TLS: config.TLSNone, Username: "me", Password: "x", Inbox: "INBOX"}
	srv.cfg.Accounts = []config.Account{{Name: "work", SMTP: smtpCfg, IMAP: imapCfg}}
	srv.accounts = []Account{{Name: "work", Sender: smtpsend.New(smtpCfg), Syncer: imapsync.New("work", imapCfg, st, log.New(io.Discard, "", 0))}}

	// Large enough to be more than the 1 MiB a plain request may carry.
	big := bytes.Repeat([]byte("0123456789abcdef"), 2<<16)
	rec := upload(t, srv, `{"account":"work","to":["ann@example.com"],"subject":"Files","body":"See attached."}`,
		map[string][]byte{"offerte.pdf": big, "foto.jpg": []byte("\xff\xd8\xff not a real jpeg")}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("send with files = %d: %s", rec.Code, rec.Body.String())
	}

	smtpSrv.mu.Lock()
	data := smtpSrv.data
	smtpSrv.mu.Unlock()
	parsed, err := mailparse.Parse([]byte(strings.ReplaceAll(data, "\r\n..", "\r\n.")))
	if err != nil {
		t.Fatalf("parse sent message: %v", err)
	}
	names := map[string]int64{}
	for _, a := range parsed.Attachments {
		names[a.Filename] = a.Size
	}
	if names["offerte.pdf"] != int64(len(big)) || names["foto.jpg"] == 0 {
		t.Errorf("attachments sent = %v, want offerte.pdf (%d bytes) and foto.jpg", names, len(big))
	}
	if !strings.Contains(data, "application/pdf") || !strings.Contains(data, "image/jpeg") {
		t.Error("attachments should carry a content type from their file name")
	}
}

func TestSendRejectsBadUploads(t *testing.T) {
	srv, _ := newTestServer(t)
	good := `{"to":["ann@example.com"],"body":"hi"}`
	tooBig := bytes.Repeat([]byte("x"), maxAttachments+1)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"too large":     upload(t, srv, good, map[string][]byte{"huge.bin": tooBig}, nil),
		"unknown field": upload(t, srv, good, nil, map[string]string{"sneaky": "1"}),
	} {
		if rec.Code == http.StatusOK {
			t.Errorf("%s: accepted, want a rejection", name)
		}
	}
}
