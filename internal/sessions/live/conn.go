// Package live is the view-agnostic transport for an attached Managed Agents
// session. It backfills history, follows the SSE stream without a gap,
// de-duplicates by event id, folds agent.message deltas into preview events,
// keeps everything in display order and reconnects with backoff. The TUI
// consumes a Conn and the local web viewer a Threads (a Conn per thread), both
// as Snapshot() plus a channel of Updates; nothing here knows about rendering.
//
// An event is pending until the server has processed it. A pending event is
// either a queued user event (sent, not yet processed) or a streaming preview
// (an agent.message still receiving deltas).
//
// conn.go holds the Conn, its consumer contract and the sources it follows,
// follow.go the loop that keeps it attached, order.go the display order,
// preview.go the streaming previews, send.go the writes, and threads.go the
// multi-thread fan-out.
package live

import (
	"context"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// Event is the List endpoint's union: every variant's fields flattened, with
// ProcessedAt.IsZero() marking a pending event.
type Event = anthropic.BetaManagedAgentsSessionEventUnion

// UpdateKind says which fields of an Update carry the change.
type UpdateKind int

const (
	// KindEvent: Update.Event was inserted or replaced.
	KindEvent UpdateKind = iota
	// KindSession: Update.Session is the refreshed session.
	KindSession
	// KindConn: stream connectivity changed.
	KindConn
	// KindThreads comes only from Threads: the thread list in ThreadUpdate.Threads changed.
	KindThreads
)

// Update is one change to what Snapshot()/Session() return, or to connectivity.
//
// Consumer contract: every KindEvent is an upsert by Event.ID — replace the
// event you hold with that id; an id unknown to you goes last, except that
// queued user events always stay behind everything else.
// When Reordered is set (on any Kind), discard your list and rebuild from
// Snapshot(). A consumer that lags and rebuilds may briefly re-apply older
// updates; letting a pending event never overwrite a processed one avoids the
// only visible artefact, and the list always converges at quiescence.
type Update struct {
	Kind UpdateKind
	// KindEvent: a whole event, never a raw stream frame. A pending
	// agent.message is a streaming preview refined by later updates with the
	// same id and finally replaced by the processed event. Empty when the
	// update only signals Reordered.
	Event Event
	// Reordered: the ordered list changed in a way the contract cannot
	// express (an insert ahead of a processed event or a streaming preview,
	// a known id moving, or dropped previews).
	Reordered bool
	// KindSession: the refreshed session after a session.updated or status event.
	Session *anthropic.BetaManagedAgentsSession
	// KindConn: stream connectivity. Err says why it dropped (errors.As it to
	// *anthropic.Error for API failures; 401/403/404 are fatal and are
	// followed by Updates closing). Backfilled is set once, on the update that
	// follows delivery of the full initial history.
	Connected  bool
	Err        error
	Backfilled bool
}

// Conn follows one event log, the session's own or one thread's, and
// publishes it as Snapshot() plus Updates().
type Conn struct {
	client    anthropic.Client
	sessionID string
	src       source

	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	updates chan Update

	// finishing asks the run loop for one last catch-up and a quiet exit.
	finishing  chan struct{}
	finishOnce sync.Once

	// emitMu serialises mutate-then-emit across the run loop and Send callers
	// so Updates arrive in the order the store changed, and guards closed.
	emitMu sync.Mutex
	closed bool

	// mu guards what Snapshot/Session read.
	mu      sync.Mutex
	store   *orderedEvents
	session anthropic.BetaManagedAgentsSession
	// sessionAsOf is the processed_at of the newest event folded into session.
	sessionAsOf time.Time

	// Run-loop only. previews maps a streaming agent.message id to whether it
	// has deltas not yet emitted.
	acc        anthropic.BetaManagedAgentsEventAccumulator
	previews   map[string]bool
	backfilled bool
}

// streamEvent is a stream frame: an Event, or an event_start / event_delta
// for a streaming agent.message.
type streamEvent = anthropic.BetaManagedAgentsStreamSessionEventsUnion

// source is the event log a Conn follows: the session's own, or one thread's.
type source struct {
	// list pages the log.
	list               func(ctx context.Context, newestFirst bool) *pagination.PageCursorAutoPager[Event]
	canListNewestFirst bool
	// stream tails the log. It returns once the SSE subscription (with
	// agent.message deltas) is established, so events processed after that
	// are on it. nil re-lists every pollInterval instead.
	stream func(ctx context.Context) (subscription, error)
	// once stops the Conn, still connected, after its first successful catch-up.
	once bool
	// primary marks the session's own log rather than a thread's: session.*
	// events fold into Session().
	primary bool
}

// subscription copies an established stream's frames into out until the
// stream ends or its context is done.
type subscription = func(out chan<- streamEvent) error

// subscribe fails with the stream's connect error, which the SDK reports
// before the first Next, or returns the subscription to run.
func subscribe[T any](ctx context.Context, sse *ssestream.Stream[T], toFrame func(T) streamEvent) (subscription, error) {
	if err := sse.Err(); err != nil {
		sse.Close()
		return nil, err
	}
	return func(out chan<- streamEvent) error {
		defer sse.Close()
		for sse.Next() {
			select {
			case out <- toFrame(sse.Current()):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return sse.Err()
	}, nil
}

// The server only honours event_deltas under this beta and ignores it for
// orgs that aren't enrolled, so it is safe to always send.
const deltasBeta anthropic.AnthropicBeta = "managed-agents-vnext"

var messageDeltas = []anthropic.BetaManagedAgentsDeltaType{anthropic.BetaManagedAgentsDeltaTypeAgentMessage}

// Open loads the session synchronously so callers fail fast on a bad id or
// credentials; backfill and streaming then proceed in the background. The
// Conn lives until ctx is done or Close is called — pass the command's root
// context so Ctrl-C tears it down.
func Open(ctx context.Context, client anthropic.Client, sessionID string) (*Conn, error) {
	sess, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
	if err != nil {
		return nil, err
	}
	events := client.Beta.Sessions.Events
	c := newConn(ctx, client, sessionID, source{
		list: func(ctx context.Context, newestFirst bool) *pagination.PageCursorAutoPager[Event] {
			order := anthropic.BetaSessionEventListParamsOrderAsc
			if newestFirst {
				order = anthropic.BetaSessionEventListParamsOrderDesc
			}
			return events.ListAutoPaging(ctx, sessionID, anthropic.BetaSessionEventListParams{Order: order})
		},
		canListNewestFirst: true,
		stream: func(ctx context.Context) (subscription, error) {
			sse := events.StreamEvents(ctx, sessionID, anthropic.BetaSessionEventStreamParams{
				EventDeltas: messageDeltas,
				Betas:       []anthropic.AnthropicBeta{deltasBeta},
			})
			return subscribe(ctx, sse, func(frame streamEvent) streamEvent { return frame })
		},
		primary: true,
	})
	c.session, c.sessionAsOf = *sess, sess.UpdatedAt
	go c.run()
	return c, nil
}

func newConn(ctx context.Context, client anthropic.Client, sessionID string, src source) *Conn {
	c := &Conn{
		client:    client,
		sessionID: sessionID,
		src:       src,
		done:      make(chan struct{}),
		updates:   make(chan Update, 256),
		finishing: make(chan struct{}),
		store:     newOrderedEvents(),
		previews:  map[string]bool{},
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	return c
}

// Updates delivers every change after the one Snapshot reflects. It is closed
// once the Conn has fully stopped.
func (c *Conn) Updates() <-chan Update { return c.updates }

// Snapshot returns a copy of every event held, in display order.
func (c *Conn) Snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.store.events...)
}

// Session returns a copy of the session as of the newest event folded into it.
func (c *Conn) Session() *anthropic.BetaManagedAgentsSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.session
	return &session
}

// Close stops the Conn and waits until Updates is closed.
func (c *Conn) Close() {
	c.cancel()
	<-c.done
}

// ingest stores ev and emits the resulting updates. A processed event is
// final: re-deliveries (backfill, stream and catch-up overlap by design) and
// late previews or Send echoes for its id are dropped.
func (c *Conn) ingest(ev Event) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	if c.closed {
		return
	}
	c.mu.Lock()
	if c.store.hasProcessed(ev.ID) {
		c.mu.Unlock()
		return
	}
	fresh, moved := c.store.put(ev)
	var refreshed *anthropic.BetaManagedAgentsSession
	if fresh && c.src.primary && c.applySession(ev) {
		session := c.session
		refreshed = &session
	}
	c.mu.Unlock()
	c.emit(Update{Kind: KindEvent, Event: ev, Reordered: moved})
	if refreshed != nil {
		c.emit(Update{Kind: KindSession, Session: refreshed})
	}
}

var statusByEvent = map[string]anthropic.BetaManagedAgentsSessionStatus{
	"session.status_running":     anthropic.BetaManagedAgentsSessionStatusRunning,
	"session.status_idle":        anthropic.BetaManagedAgentsSessionStatusIdle,
	"session.status_rescheduled": anthropic.BetaManagedAgentsSessionStatusRescheduling,
	"session.status_terminated":  anthropic.BetaManagedAgentsSessionStatusTerminated,
}

// applySession folds a session-shaping event into the cached session and
// reports whether it changed anything. Events older than what the cache
// already reflects (history replay, newest-first catch-up) are ignored.
// Caller holds mu.
func (c *Conn) applySession(ev Event) bool {
	if ev.ProcessedAt.Before(c.sessionAsOf) {
		return false
	}
	status, isStatus := statusByEvent[ev.Type]
	switch {
	case isStatus:
		c.session.Status = status
	case ev.Type == "session.updated":
		if ev.JSON.Title.Valid() {
			c.session.Title = ev.Title
		}
		if ev.JSON.Agent.Valid() {
			c.session.Agent = ev.Agent
		}
		if ev.JSON.Metadata.Valid() {
			c.session.Metadata = ev.Metadata
		}
	default:
		return false
	}
	c.sessionAsOf = ev.ProcessedAt
	return true
}

// emit never blocks past ctx, and run cancels ctx before closing the
// channel, so a writer holding emitMu always gets out of the way first.
func (c *Conn) emit(u Update) {
	select {
	case c.updates <- u:
	case <-c.ctx.Done():
	}
}

// emitReordered has consumers rebuild from Snapshot after the store changed
// shape outside an upsert.
func (c *Conn) emitReordered() {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	if c.closed {
		return
	}
	c.emit(Update{Kind: KindEvent, Reordered: true})
}

func (c *Conn) hasProcessed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store.hasProcessed(id)
}
