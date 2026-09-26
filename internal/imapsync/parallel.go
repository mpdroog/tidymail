package imapsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// fetched is one downloaded batch.
type fetched struct {
	msgs     []*imapclient.FetchMessageBuffer
	download time.Duration
	err      error
}

// downloader fetches batches over several connections at once, since the
// download is what limits a sync. Batches are handed out strictly in order, so
// the cursor never moves past a batch that is still downloading.
type downloader struct {
	conns   int
	results []chan fetched // one per batch, buffered so workers never block
	tokens  chan struct{}  // bounds how many downloaded batches wait in memory
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	firstErr error
}

// startDownloads opens up to imap.connections read-only connections on mailbox
// and starts fetching batches.
func (s *Syncer) startDownloads(ctx context.Context, mailbox string, batches [][]imap.UID, opts *imap.FetchOptions) (*downloader, error) {
	n := min(max(s.cfg.Connections, 1), len(batches))

	var clients []*imapclient.Client
	for range n {
		c, err := s.openFetcher(mailbox)
		if err != nil {
			if len(clients) == 0 {
				return nil, err
			}
			// Servers cap connections per account; fewer still works.
			s.log.Printf("sync: extra connection failed, continuing with %d: %v", len(clients), err)
			break
		}
		clients = append(clients, c)
	}

	ctx, cancel := context.WithCancel(ctx)
	d := &downloader{
		conns:   len(clients),
		results: make([]chan fetched, len(batches)),
		tokens:  make(chan struct{}, 2*len(clients)),
		ctx:     ctx,
		cancel:  cancel,
	}
	for i := range d.results {
		d.results[i] = make(chan fetched, 1)
	}

	// Batches go out in order, each only once a token is free; next returns
	// the token. Every batch sent out therefore gets a result.
	jobs := make(chan int)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(jobs)
		for i := range batches {
			select {
			case d.tokens <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	for _, c := range clients {
		// IMAP commands take no context: closing the connection is what
		// interrupts a fetch in flight.
		stop := context.AfterFunc(ctx, func() { s.closeConn(c) })
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() {
				if stop() {
					if err := logoutClient(c); err != nil {
						s.log.Printf("imap: fetch connection: %v", err)
					}
				}
			}()
			for i := range jobs {
				start := time.Now()
				msgs, err := c.Fetch(imap.UIDSetNum(batches[i]...), opts).Collect()
				if err != nil {
					err = fmt.Errorf("imap fetch from %s: %w", mailbox, err)
					d.fail(err)
				}
				d.results[i] <- fetched{msgs: msgs, download: time.Since(start), err: err}
			}
		}()
	}
	return d, nil
}

// next waits for batch i, which must be asked for in order.
func (d *downloader) next(i int) fetched {
	select {
	case r := <-d.results[i]:
		<-d.tokens
		return r
	default:
	}
	select {
	case r := <-d.results[i]:
		<-d.tokens
		return r
	case <-d.ctx.Done():
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.firstErr != nil {
			return fetched{err: d.firstErr}
		}
		return fetched{err: fmt.Errorf("fetch inbox: %w", d.ctx.Err())}
	}
}

// fail records the first download error and stops the other downloads.
func (d *downloader) fail(err error) {
	d.mu.Lock()
	if d.firstErr == nil {
		d.firstErr = err
	}
	d.mu.Unlock()
	d.cancel()
}

// close waits for the workers. After a complete run they log out cleanly;
// otherwise their connections are closed first so a hung fetch cannot block.
func (d *downloader) close(completed bool) {
	if completed {
		d.wg.Wait()
		d.cancel()
		return
	}
	d.cancel()
	d.wg.Wait()
}

// openFetcher opens an extra read-only connection on mailbox for downloading.
func (s *Syncer) openFetcher(mailbox string) (*imapclient.Client, error) {
	c, err := s.dial()
	if err != nil {
		return nil, err
	}
	if err = c.Login(s.cfg.Username, s.cfg.Password).Wait(); err != nil {
		err = fmt.Errorf("imap login as %s: %w", s.cfg.Username, err)
	} else if _, err = c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		err = fmt.Errorf("imap examine %s: %w", mailbox, err)
	}
	if err != nil {
		if lerr := logoutClient(c); lerr != nil {
			s.log.Printf("imap: fetch connection: %v", lerr)
		}
		return nil, err
	}
	return c, nil
}

// closeConn closes a connection to interrupt whatever it is doing.
func (s *Syncer) closeConn(c *imapclient.Client) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Printf("imap: close on cancel: %v", err)
	}
}
