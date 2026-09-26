package httpd

import (
	"context"
	"net/http"
	"time"

	"github.com/mpdroog/tidymail/internal/imapsync"
)

// mirrorBatch is how many box moves go to the server per IMAP connection.
const mirrorBatch = 200

// mirrorInterval is how often pending moves are retried without a change in
// the UI, e.g. after the server was unreachable or delayed mail came back.
const mirrorInterval = time.Minute

// RunMirror repeats box moves in IMAP folders until ctx is cancelled. The work
// list lives in the state files (server_box), so moves made while serve was
// down, or while the server was unreachable, are picked up later.
func (s *Server) RunMirror(ctx context.Context) {
	if len(s.accounts) == 0 {
		return
	}
	tick := time.NewTicker(mirrorInterval)
	defer tick.Stop()
	for {
		s.mirrorPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.mirrorKick:
		}
	}
}

// mirrorPending works through every account's pending moves. An account whose
// server fails is retried on the next run; the others carry on.
func (s *Server) mirrorPending(ctx context.Context) {
	boxNames := make(map[string]string)
	for _, b := range s.st.Boxes() {
		boxNames[b.Slug] = b.Name
	}
	for i, acct := range s.accounts {
		if !acct.Mirror {
			continue // boxes are local only; the server is never touched
		}
		s.mirrorAccount(ctx, acct, s.owned(i), boxNames)
	}
}

// mirrorAccount works through one account's pending moves until none are left
// or its server fails.
func (s *Server) mirrorAccount(ctx context.Context, acct Account, owned []string, boxNames map[string]string) {
	for ctx.Err() == nil {
		pending, err := s.st.PendingMoves(ctx, mirrorBatch, owned...)
		if err != nil {
			s.log.Printf("imap%s: list pending box moves: %v", accountTag(acct.Name), err)
			return
		}
		if len(pending) == 0 {
			return
		}
		moves := make([]imapsync.ServerMove, len(pending))
		target := make(map[string]string, len(pending))
		for i, p := range pending {
			moves[i] = imapsync.ServerMove{ID: p.ID, MessageID: p.MessageID, From: p.ServerBox, To: p.Box}
			target[p.ID] = p.Box
		}

		done, mirrorErr := acct.Syncer.MirrorMoves(ctx, moves, boxNames)
		for _, id := range done {
			if setErr := s.st.SetServerBox(ctx, id, target[id]); setErr != nil {
				s.log.Printf("imap%s: record server move of %s: %v", accountTag(acct.Name), id, setErr)
			}
		}
		if len(done) > 0 {
			s.log.Printf("imap%s: repeated %d box move(s) on the server", accountTag(acct.Name), len(done))
		}
		if mirrorErr != nil {
			if ctx.Err() == nil {
				s.log.Printf("imap%s: box moves: %v (retrying in %s)", accountTag(acct.Name), mirrorErr, mirrorInterval)
			}
			return
		}
		if len(done) == 0 {
			return
		}
	}
}

// accountTag names an account in a log line; the unnamed account needs none.
func accountTag(name string) string {
	if name == "" {
		return ""
	}
	return " [" + name + "]"
}

// kickMirror asks RunMirror to look for pending moves now.
func (s *Server) kickMirror() {
	select {
	case s.mirrorKick <- struct{}{}:
	default: // a run is already due
	}
}

// kickAfterChanges starts mirroring after any request that may have moved mail.
func (s *Server) kickAfterChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.kickMirror()
		}
	})
}
