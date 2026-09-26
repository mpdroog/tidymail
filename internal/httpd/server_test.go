package httpd

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/store"
)

// newTestServer builds a Server backed by a temporary store. The IMAP and SMTP
// transports are nil: no test here reaches an endpoint that uses them.
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	st, err := store.Open(t.Context(), t.TempDir(), logger)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store.Close: %v", cerr)
		}
	})

	cfg := &config.Config{Listen: "127.0.0.1:0"}
	cfg.Accounts = []config.Account{{SMTP: config.SMTP{FromAddr: "me@example.com"}}}
	srv, err := New(cfg, st, nil, logger)
	if err != nil {
		t.Fatalf("httpd.New: %v", err)
	}
	return srv, st
}

func seed(t *testing.T, st *store.Store, from, subject, body string) *store.Message {
	t.Helper()
	raw := []byte("From: " + from + "\r\nTo: me@example.com\r\nSubject: " + subject +
		"\r\nDate: Mon, 31 Aug 2026 10:00:00 +0000\r\nMessage-ID: <" + subject + "@test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
	m, _, err := st.Ingest(t.Context(), "", raw, time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return m
}

// do issues a request through the full middleware stack.
func do(t *testing.T, srv *Server, method, path, body string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	req.Host = "127.0.0.1:8080"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken {
		req.Header.Set("X-CSRF-Token", srv.csrf)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/", "/static/app.js", "/static/app.css", "/api/state"} {
		rec := do(t, srv, http.MethodGet, path, "", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'none'", "script-src 'self'", "style-src 'self'",
			"frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: CSP %q is missing %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("GET %s: CSP must not relax script or style: %q", path, csp)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

// The CSP forbids inline script and style, so the shipped assets must not
// contain any. This test is what keeps a future edit honest.
func TestNoInlineScriptOrStyleInAssets(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv, http.MethodGet, "/", "", false)
	page := rec.Body.String()

	for _, bad := range []string{"<script>", "<style", "javascript:", " onclick=", " onload=", " onerror=", " style="} {
		if strings.Contains(strings.ToLower(page), bad) {
			t.Errorf("index.html contains %q, which the CSP blocks", bad)
		}
	}
	if !strings.Contains(page, `src="/static/app.js"`) || !strings.Contains(page, `href="/static/app.css"`) {
		t.Error("index.html should load its script and style from separate files")
	}

	js := do(t, srv, http.MethodGet, "/static/app.js", "", false).Body.String()
	if strings.Contains(js, ".innerHTML") || strings.Contains(js, "outerHTML") {
		t.Error("app.js must not assign markup; mail content is inserted as text nodes only")
	}
	if strings.Contains(js, "eval(") {
		t.Error("app.js must not use eval")
	}
}

func TestMutationsRequireCSRFToken(t *testing.T) {
	srv, st := newTestServer(t)
	m := seed(t, st, "ann@example.com", "hello", "body")

	if rec := do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/prio", "{}", false); rec.Code != http.StatusForbidden {
		t.Errorf("POST without token = %d, want 403", rec.Code)
	}
	if rec := do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/prio", "{}", true); rec.Code != http.StatusOK {
		t.Errorf("POST with token = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

func TestCrossSiteRequestIsRejected(t *testing.T) {
	srv, st := newTestServer(t)
	m := seed(t, st, "ann@example.com", "hello", "body")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/messages/"+m.ID+"/prio", strings.NewReader("{}"))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-CSRF-Token", srv.csrf)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}

	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/messages/"+m.ID+"/prio", strings.NewReader("{}"))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-CSRF-Token", srv.csrf)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign-origin POST = %d, want 403", rec.Code)
	}
}

func TestBoxFlow(t *testing.T) {
	srv, st := newTestServer(t)
	m := seed(t, st, "ann@example.com", "hello", "the body text")

	// A fresh message starts in Unprocessed.
	var list listView
	decode(t, do(t, srv, http.MethodGet, "/api/messages?box=unprocessed", "", false), &list)
	if list.Total != 1 || list.Items[0].ID != m.ID {
		t.Fatalf("unprocessed listing = %+v", list)
	}

	// Prio moves it.
	do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/prio", "{}", true)
	decode(t, do(t, srv, http.MethodGet, "/api/messages?box=priority", "", false), &list)
	if list.Total != 1 {
		t.Fatalf("priority listing = %+v", list)
	}

	// Delay parks it and remembers where it came from.
	var res actionResult
	decode(t, do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/delay", `{"days":7}`, true), &res)
	if res.Message.Box != store.BoxDelayed || res.Message.PrevBox != store.BoxPriority {
		t.Fatalf("after delay: box=%s prev=%s", res.Message.Box, res.Message.PrevBox)
	}
	if res.Message.DelayedUntil == nil {
		t.Fatal("delayed_until not set")
	}

	// A custom box can be created and used.
	rec := do(t, srv, http.MethodPost, "/api/boxes", `{"name":"Error mails"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create box = %d (%s)", rec.Code, rec.Body.String())
	}
	decode(t, do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/move", `{"box":"error-mails"}`, true), &res)
	if res.Message.Box != "error-mails" {
		t.Fatalf("move: box = %s", res.Message.Box)
	}
}

func TestBadRequestsAreRejected(t *testing.T) {
	srv, st := newTestServer(t)
	m := seed(t, st, "ann@example.com", "hello", "body")

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"unknown box", "/api/messages/" + m.ID + "/move", `{"box":"nope"}`, http.StatusBadRequest},
		{"zero delay", "/api/messages/" + m.ID + "/delay", `{"days":0}`, http.StatusBadRequest},
		{"absurd delay", "/api/messages/" + m.ID + "/delay", `{"days":99999}`, http.StatusBadRequest},
		{"unknown field", "/api/messages/" + m.ID + "/delay", `{"dayz":3}`, http.StatusBadRequest},
		{"missing message", "/api/messages/" + strings.Repeat("a", 32) + "/prio", `{}`, http.StatusNotFound},
		{"malformed id", "/api/messages/zzz/prio", `{}`, http.StatusNotFound},
	} {
		if rec := do(t, srv, http.MethodPost, tc.path, tc.body, true); rec.Code != tc.want {
			t.Errorf("%s: got %d (%s), want %d", tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
		}
	}
}

func TestAttachmentIsServedAsInertDownload(t *testing.T) {
	srv, st := newTestServer(t)
	raw := []byte("From: ann@example.com\r\nTo: me@example.com\r\nSubject: files\r\n" +
		"Date: Mon, 31 Aug 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--b\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"evil.html\"\r\n\r\n" +
		"<script>alert(1)</script>\r\n--b--\r\n")
	m, _, err := st.Ingest(t.Context(), "", raw, time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(m.Mail.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(m.Mail.Attachments))
	}

	rec := do(t, srv, http.MethodGet, "/api/messages/"+m.ID+"/attachments/"+strconv.Itoa(m.Mail.Attachments[0].Part), "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("attachment = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream for an HTML attachment", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("attachment CSP = %q, want a sandbox", csp)
	}
}

func TestSearchAcrossBoxes(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, "ann@example.com", "invoice", "quarterly numbers")
	seed(t, st, "bob@example.com", "lunch", "sandwich plans")

	var list listView
	decode(t, do(t, srv, http.MethodGet, "/api/messages?q=quarterly", "", false), &list)
	if list.Total != 1 || list.Items[0].Subject != "invoice" {
		t.Fatalf("search = %+v", list)
	}
	decode(t, do(t, srv, http.MethodGet, "/api/messages?q=from%3Abob%40example.com", "", false), &list)
	if list.Total != 1 || list.Items[0].Subject != "lunch" {
		t.Fatalf("from: search = %+v", list)
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// Panels in the UI are toggled with the `hidden` attribute. An author rule
// setting `display` on the same element wins over the browser's
// `[hidden] { display: none }` -- they have equal specificity -- which pins the
// panel open. That bug shipped once: the modal overlay covered the whole app.
// Guard the override rather than trusting each future style edit.
func TestArchiveAllEmptiesUnprocessed(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, "ann@example.com", "one", "body")
	seed(t, st, "bob@example.com", "two", "body")

	rec := do(t, srv, http.MethodPost, "/api/boxes/unprocessed/move-all", `{"box":"processed"}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("move-all = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, http.MethodPost, "/api/boxes/unprocessed/move-all", `{"box":"nope"}`, true); rec.Code < 400 {
		t.Errorf("move-all to an unknown box = %d, want an error", rec.Code)
	}

	// The move runs in the background.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, total, err := st.List(t.Context(), store.Query{Box: store.BoxUnprocessed})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total == 0 && !srv.movingAll.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unprocessed still holds %d after 5s", total)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pending, err := st.PendingMoves(t.Context(), 10, "")
	if err != nil {
		t.Fatalf("PendingMoves: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("pending server moves = %d, want 2 (to the Archive folder)", len(pending))
	}
}

func TestHiddenPanelsStayHidden(t *testing.T) {
	srv, _ := newTestServer(t)
	page := do(t, srv, http.MethodGet, "/", "", false).Body.String()
	css := do(t, srv, http.MethodGet, "/static/app.css", "", false).Body.String()

	override := regexp.MustCompile(`\[hidden\][^{]*\{[^}]*display\s*:\s*none\s*!important`)
	hasOverride := override.MatchString(css)

	// Which classes are on an element carrying the hidden attribute?
	tags := regexp.MustCompile(`<[a-z]+[^>]*\shidden\s*>`)
	classAttr := regexp.MustCompile(`class="([^"]+)"`)
	var hiddenClasses []string
	for _, tag := range tags.FindAllString(page, -1) {
		if m := classAttr.FindStringSubmatch(tag); m != nil {
			hiddenClasses = append(hiddenClasses, strings.Fields(m[1])...)
		}
	}
	if len(hiddenClasses) == 0 {
		t.Fatal("no elements with the hidden attribute found; this test needs updating")
	}

	for _, class := range hiddenClasses {
		rule := regexp.MustCompile(`(?s)\.` + regexp.QuoteMeta(class) + `\s*\{(.*?)\}`)
		for _, m := range rule.FindAllStringSubmatch(css, -1) {
			if !strings.Contains(m[1], "display:") {
				continue
			}
			if !hasOverride {
				t.Errorf(".%s sets display and would override the hidden attribute; "+
					"app.css needs a `[hidden] { display: none !important; }` rule", class)
			}
		}
	}
	if !hasOverride {
		t.Error("app.css is missing the [hidden] display override")
	}
}

func TestMessageShowsItsAccountsReplyAddress(t *testing.T) {
	srv, st := newTestServer(t)
	srv.cfg.Accounts = append(srv.cfg.Accounts, config.Account{Name: "work", SMTP: config.SMTP{FromAddr: "me@work.example"}})
	raw := []byte("From: bob@example.com\r\nTo: me@work.example\r\nSubject: for work\r\n" +
		"Date: Mon, 31 Aug 2026 10:00:00 +0000\r\nMessage-ID: <work@test>\r\n\r\nbody\r\n")
	m, _, err := st.Ingest(t.Context(), "work", raw, time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	rec := do(t, srv, http.MethodGet, "/api/messages/"+m.ID, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET message = %d: %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Account   string `json:"account"`
		ReplyFrom string `json:"reply_from"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Account != "work" || view.ReplyFrom != "me@work.example" {
		t.Errorf("account %q, reply_from %q; want work, me@work.example", view.Account, view.ReplyFrom)
	}
}

func TestReplyThroughUnconfiguredAccountIsRefused(t *testing.T) {
	srv, st := newTestServer(t)
	raw := []byte("From: bob@example.com\r\nTo: me@old.example\r\nSubject: old account\r\n" +
		"Date: Mon, 31 Aug 2026 10:00:00 +0000\r\nMessage-ID: <old@test>\r\n\r\nbody\r\n")
	m, _, err := st.Ingest(t.Context(), "removed", raw, time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	rec := do(t, srv, http.MethodPost, "/api/messages/"+m.ID+"/reply", `{"body":"hi"}`, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("reply = %d %s, want 409 naming the missing account", rec.Code, rec.Body.String())
	}
}

func TestUnmirroredAccountNeverTouchesTheServer(t *testing.T) {
	srv, st := newTestServer(t)
	// No syncer: reaching the server for this account would panic.
	srv.accounts = []Account{{Mirror: false}}
	m := seed(t, st, "ann@example.com", "local only", "body")
	if _, err := st.SetBox(t.Context(), m.ID, store.BoxPriority); err != nil {
		t.Fatalf("SetBox: %v", err)
	}

	srv.mirrorPending(t.Context())

	got, err := st.Get(m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ServerBox != store.BoxUnprocessed {
		t.Errorf("server box = %q, want it left where sync found it", got.ServerBox)
	}
	rec := do(t, srv, http.MethodGet, "/api/state", "", false)
	var state struct {
		Stats store.Stats `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Stats.PendingIMAP != 0 {
		t.Errorf("pending = %d, want 0: an unmirrored account has nothing to move", state.Stats.PendingIMAP)
	}
}
