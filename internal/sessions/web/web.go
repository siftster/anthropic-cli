// Package web serves the single-file session viewer on loopback and bridges
// it to internal/sessions/live: the browser gets whole events over SSE and
// writes back through POST /send, while credentials and transport stay in
// this process.
//
// web.go is the entry point (Serve), server.go the HTTP routes and their
// guards, and hub.go the fan-out of live updates to connected pages.
// viewer/README.md says where the bundle and its wire protocol come from.
package web

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/anthropics/anthropic-cli/internal/sessions/live"
)

//go:embed viewer/viewer.html.gz
var viewerGzip []byte

// Session is what the server needs from an attached session; *live.Threads is one.
type Session interface {
	Updates() <-chan live.ThreadUpdate
	Snapshot() (threads []live.Thread, logs []live.ThreadLog)
	Events(id live.ThreadID) []live.Event
	Session() *anthropic.BetaManagedAgentsSession
	SendMessage(ctx context.Context, text string) error
	Interrupt(ctx context.Context) error
	Confirm(ctx context.Context, toolUseID string, allow bool, denyMessage string) error
}

var _ Session = (*live.Threads)(nil)

// Options configures Serve.
type Options struct {
	// ConsoleURL is the session's page in the web Console, for the header link.
	ConsoleURL string
	// OpenURL is called with the page's address once the server is listening;
	// nil just prints it via Logf.
	OpenURL func(string) error
	// Logf reports the viewer URL and browser-open failures to the user; required.
	Logf func(format string, args ...any)
}

// Serve serves the viewer for session on a loopback port until ctx is done.
func Serve(ctx context.Context, session Session, opts Options) error {
	page, err := loadViewer()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := newServer(session, page, listener.Addr().String(), opts.ConsoleURL)
	go srv.hub.run()

	// The URL carries a one-time boot code, not the session token: it lands
	// in the browser opener's argv and in shell/browser history, so it must
	// be worthless once used.
	url := fmt.Sprintf("http://%s/?boot=%s", listener.Addr(), srv.newBootCode())
	opts.Logf("Session viewer: %s", url)
	if opts.OpenURL != nil {
		if err := opts.OpenURL(url); err != nil {
			opts.Logf("could not open a browser (%v); open the URL above", err)
		}
	}

	httpServer := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadViewer inflates the embedded viewer page.
func loadViewer() ([]byte, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(viewerGzip))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(unzipped)
}
