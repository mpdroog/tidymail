// Package httpd serves the tidymail web UI and its JSON API.
//
// The browser side runs under a strict Content-Security-Policy: no inline
// script, no inline style, no external origins at all. That is only possible
// because mail bodies are stored and rendered as plain text and inserted into
// the DOM as text nodes, never as markup.
package httpd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/imapsync"
	"github.com/mpdroog/tidymail/internal/smtpsend"
	"github.com/mpdroog/tidymail/internal/store"
)

//go:embed all:assets
var assets embed.FS

// contentSecurityPolicy locks the page down to same-origin script and style
// files. Everything the UI needs is served from this binary.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self'; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"require-trusted-types-for 'script'"

// downloadPolicy is used for attachment responses: the bytes are opaque and
// must never be treated as an active document.
const downloadPolicy = "default-src 'none'; sandbox"

// maxRequestBody caps a JSON request. Replies are text, so this is generous.
const maxRequestBody = 1 << 20 // 1 MiB

// Server wires the store, the mail transports and the HTTP handlers together.
type Server struct {
	cfg    *config.Config
	st     *store.Store
	syncer *imapsync.Syncer
	sender *smtpsend.Sender
	log    *log.Logger

	csrf string
	mux  *http.ServeMux

	// mirrorKick wakes RunMirror after a change that may have moved mail.
	mirrorKick chan struct{}
	// movingAll is set while a whole-box move runs in the background.
	movingAll atomic.Bool
}

// New builds the HTTP server.
func New(cfg *config.Config, st *store.Store, syncer *imapsync.Syncer, sender *smtpsend.Sender, logger *log.Logger) (*Server, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, st: st, syncer: syncer, sender: sender, log: logger, csrf: token, mux: http.NewServeMux(), mirrorKick: make(chan struct{}, 1)}
	if err := s.routes(); err != nil {
		return nil, err
	}
	return s, nil
}

// randomToken returns a 256-bit URL-safe token.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) routes() error {
	static, err := fs.Sub(assets, "assets")
	if err != nil {
		return fmt.Errorf("open embedded assets: %w", err)
	}
	// index.html is served from memory: http.FileServer would redirect
	// /index.html back to "/" and never return the page itself.
	index, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return fmt.Errorf("read embedded index.html: %w", err)
	}
	started := time.Now()

	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", started, bytes.NewReader(index))
	})
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /api/messages", s.handleList)
	s.mux.HandleFunc("GET /api/messages/{id}", s.handleMessage)
	s.mux.HandleFunc("GET /api/messages/{id}/attachments/{part}", s.handleAttachment)
	s.mux.HandleFunc("GET /api/messages/{id}/raw", s.handleRaw)

	s.mux.HandleFunc("POST /api/messages/{id}/reply", s.handleReply)
	s.mux.HandleFunc("POST /api/messages/{id}/ignore", s.handleIgnore)
	s.mux.HandleFunc("POST /api/messages/{id}/prio", s.handlePrio)
	s.mux.HandleFunc("POST /api/messages/{id}/delay", s.handleDelay)
	s.mux.HandleFunc("POST /api/messages/{id}/move", s.handleMove)
	s.mux.HandleFunc("POST /api/messages/{id}/read", s.handleRead)
	s.mux.HandleFunc("POST /api/messages/{id}/delete", s.handleDelete)

	s.mux.HandleFunc("GET /api/boxes", s.handleBoxes)
	s.mux.HandleFunc("POST /api/boxes", s.handleCreateBox)
	s.mux.HandleFunc("DELETE /api/boxes/{slug}", s.handleDeleteBox)
	s.mux.HandleFunc("POST /api/boxes/{slug}/move-all", s.handleMoveAll)

	s.mux.HandleFunc("GET /api/rules", s.handleRules)
	s.mux.HandleFunc("POST /api/rules", s.handleAddRule)
	s.mux.HandleFunc("POST /api/rules/delete", s.handleDeleteRule)

	s.mux.HandleFunc("GET /api/contacts", s.handleContacts)
	s.mux.HandleFunc("POST /api/reindex", s.handleReindex)
	return nil
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.guard(s.kickAfterChanges(s.mux)))
}

// ListenAndServe runs the server until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          s.log,
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.log.Printf("http: tidymail on http://%s (mail.d: %s)", ln.Addr(), s.st.Dir())

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("serve: %w", err)
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errc
	}
}

// securityHeaders applies the CSP and the supporting hardening headers to
// every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		h.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// guard enforces the request size limit and, for anything that changes state,
// same-origin plus a matching CSRF token. A cross-site page cannot set the
// custom header without a preflight, and no CORS headers are ever sent.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			s.fail(w, r, http.StatusForbidden, fmt.Errorf("cross-site request from %q rejected", site))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(origin, r) {
			s.fail(w, r, http.StatusForbidden, fmt.Errorf("origin %q is not this server", origin))
			return
		}
		got := r.Header.Get("X-CSRF-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
			s.fail(w, r, http.StatusForbidden, errors.New("missing or invalid CSRF token; reload the page"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether an Origin header names this server.
func (s *Server) sameOrigin(origin string, r *http.Request) bool {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return trimmed == r.Host
}

// writeJSON sends a JSON body. A write failure is logged: the status line is
// already on the wire, so there is nothing left to tell the client.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Printf("http: %s %s: encode response: %v", r.Method, r.URL.Path, err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.log.Printf("http: %s %s: write response: %v", r.Method, r.URL.Path, err)
	}
}

// errorBody is the JSON shape of every failure.
type errorBody struct {
	Error string `json:"error"`
}

// fail logs the real error and returns a status-appropriate message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.log.Printf("http: %s %s: %v", r.Method, r.URL.Path, err)
	s.writeJSON(w, r, status, errorBody{Error: err.Error()})
}

// failFor maps a store error to the right HTTP status.
func (s *Server) failFor(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, err)
	case errors.Is(err, store.ErrUnknownBox), errors.Is(err, store.ErrInvalidRule),
		errors.Is(err, store.ErrBadDelay), errors.Is(err, store.ErrBoxExists),
		errors.Is(err, store.ErrSystemBox):
		s.fail(w, r, http.StatusBadRequest, err)
	default:
		s.fail(w, r, http.StatusInternalServerError, err)
	}
}

// decodeJSON reads a JSON request body, rejecting unknown fields so a typo in
// the UI surfaces immediately instead of being silently ignored.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}
	return nil
}
