package live

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
)

// Thread is one execution thread of a multi-agent session as threads.list
// reports it; the primary thread has an empty ParentThreadID.
type Thread = anthropic.BetaManagedAgentsSessionThread

// ThreadID names one event log of a multi-agent session: "" is the primary
// thread (the session's own stream), anything else a child thread's id. It is
// not an event's session_thread_id: events cross-posted to the session
// stream carry a child's id yet belong to thread "".
type ThreadID = string

// ThreadLog is one thread's events in display order.
type ThreadLog struct {
	ID     ThreadID
	Events []Event
}

// ThreadUpdate is an Update tagged with the thread it applies to; on Reordered,
// rebuild that thread from Events(ThreadID). KindSession and KindThreads only
// ever come with ThreadID "".
type ThreadUpdate struct {
	ThreadID ThreadID
	Update
	// KindThreads: the complete thread list, primary included.
	Threads []Thread
}

// Tunables; tests shorten them.
var (
	// maxThreadStreams caps concurrently tailed child threads; further live
	// children are polled instead.
	maxThreadStreams = 8
	// listDebounce lets a burst of session.thread_* events cost one threads.list.
	listDebounce = 250 * time.Millisecond
)

// Threads follows a session and every thread it spawns: the primary Conn plus
// one child Conn per thread that threads.list reports, discovered whenever
// the primary stream shows thread activity. Consumers see one channel of
// ThreadUpdates and a Snapshot of every thread's log; writes go to the primary.
type Threads struct {
	primary *Conn

	ctx          context.Context
	cancel       context.CancelFunc
	updates      chan ThreadUpdate
	done         chan struct{}
	listRequests chan struct{}
	// wg counts the lister and every child forwarder; run closes updates
	// only after all of them are gone.
	wg sync.WaitGroup

	mu           sync.Mutex
	threads      []Thread
	children     map[ThreadID]*childThread
	streamsInUse int
}

type childThread struct {
	conn      *Conn
	createdAt time.Time
	// holdsStream: this child counts against maxThreadStreams until its conn stops.
	holdsStream bool
}

// OpenThreads fails fast like Open; thread discovery then runs in the
// background for as long as the primary Conn lives.
func OpenThreads(ctx context.Context, client anthropic.Client, sessionID string) (*Threads, error) {
	ctx, cancel := context.WithCancel(ctx)
	primary, err := Open(ctx, client, sessionID)
	if err != nil {
		cancel()
		return nil, err
	}
	t := &Threads{
		primary:      primary,
		ctx:          ctx,
		cancel:       cancel,
		updates:      make(chan ThreadUpdate, 256),
		done:         make(chan struct{}),
		listRequests: make(chan struct{}, 1),
		children:     map[ThreadID]*childThread{},
	}
	t.wg.Add(1)
	go t.lister()
	go t.run()
	return t, nil
}

// Updates closes once the primary Conn has stopped and every child with it.
func (t *Threads) Updates() <-chan ThreadUpdate { return t.updates }

// Session returns the primary Conn's session.
func (t *Threads) Session() *anthropic.BetaManagedAgentsSession { return t.primary.Session() }

// SendMessage posts a user.message to the primary thread.
func (t *Threads) SendMessage(ctx context.Context, text string) error {
	return t.primary.SendMessage(ctx, text)
}

// Interrupt posts a user.interrupt to the primary thread.
func (t *Threads) Interrupt(ctx context.Context) error { return t.primary.Interrupt(ctx) }

// Confirm answers a pending tool call through the primary thread.
func (t *Threads) Confirm(ctx context.Context, toolUseID string, allow bool, denyMessage string) error {
	return t.primary.Confirm(ctx, toolUseID, allow, denyMessage)
}

// Snapshot returns the last thread list (empty until the session shows any
// thread activity) and every thread's events: primary first, then children by
// creation time. Children that have stopped keep their events.
func (t *Threads) Snapshot() (threads []Thread, logs []ThreadLog) {
	t.mu.Lock()
	threads = slices.Clone(t.threads)
	childIDs := make([]ThreadID, 0, len(t.children))
	for id := range t.children {
		childIDs = append(childIDs, id)
	}
	slices.SortFunc(childIDs, func(a, b ThreadID) int {
		return cmp.Or(t.children[a].createdAt.Compare(t.children[b].createdAt), cmp.Compare(a, b))
	})
	childConns := make([]*Conn, len(childIDs))
	for i, id := range childIDs {
		childConns[i] = t.children[id].conn
	}
	t.mu.Unlock()

	logs = make([]ThreadLog, 0, 1+len(childIDs))
	logs = append(logs, ThreadLog{Events: t.primary.Snapshot()})
	for i, id := range childIDs {
		logs = append(logs, ThreadLog{ID: id, Events: childConns[i].Snapshot()})
	}
	return threads, logs
}

// Events returns one thread's events in display order: the rebuild source for
// a Reordered ThreadUpdate. nil for an id threads.list has not reported.
func (t *Threads) Events(id ThreadID) []Event {
	if id == "" {
		return t.primary.Snapshot()
	}
	t.mu.Lock()
	child := t.children[id]
	t.mu.Unlock()
	if child == nil {
		return nil
	}
	return child.conn.Snapshot()
}

// Close stops every Conn and waits until Updates is closed.
func (t *Threads) Close() {
	t.cancel()
	<-t.done
}

// run forwards the primary Conn's updates and asks for a thread listing
// whenever they show thread activity.
func (t *Threads) run() {
	defer close(t.done)
	backfilled, threadSeenInBackfill := false, false
	for u := range t.primary.Updates() {
		t.emit(ThreadUpdate{Update: u})
		switch {
		case u.Kind == KindEvent && strings.HasPrefix(u.Event.Type, "session.thread_"):
			// During backfill a long history would ask once per event; ask
			// once when it completes instead.
			if backfilled {
				t.requestList()
			} else {
				threadSeenInBackfill = true
			}
		case u.Kind == KindConn && u.Backfilled:
			backfilled = true
			if threadSeenInBackfill {
				t.requestList()
			}
		}
	}
	t.cancel()
	t.wg.Wait()
	close(t.updates)
}

func (t *Threads) requestList() {
	select {
	case t.listRequests <- struct{}{}:
	default:
	}
}

// lister is the only goroutine that lists threads and opens children, so
// reconcile needs no ordering beyond mu and wg.Add never races wg.Wait.
func (t *Threads) lister() {
	defer t.wg.Done()
	for t.awaitListRequest() {
		threads, err := t.listThreads()
		if t.ctx.Err() != nil {
			return
		}
		if err != nil {
			// The primary Conn reports connectivity; just try again shortly.
			time.AfterFunc(pollInterval, t.requestList)
			continue
		}
		t.reconcile(threads)
	}
}

// awaitListRequest waits for a listing request, then absorbs any others that
// arrive within listDebounce. It reports false once Threads is closing.
func (t *Threads) awaitListRequest() bool {
	select {
	case <-t.listRequests:
	case <-t.ctx.Done():
		return false
	}
	debounce := time.NewTimer(listDebounce)
	defer debounce.Stop()
	for {
		select {
		case <-t.listRequests:
		case <-debounce.C:
			return true
		case <-t.ctx.Done():
			return false
		}
	}
}

func (t *Threads) listThreads() ([]Thread, error) {
	var threads []Thread
	it := t.primary.client.Beta.Sessions.Threads.ListAutoPaging(t.ctx, t.primary.sessionID, anthropic.BetaSessionThreadListParams{})
	for it.Next() {
		threads = append(threads, it.Current())
	}
	return threads, it.Err()
}

// reconcile publishes the new list and opens a Conn for each child thread not
// seen before: tailed while it is live and a stream slot is free, polled when
// none is, read once when it has already finished. A known child that has
// since finished gets one last catch-up and stops.
func (t *Threads) reconcile(threads []Thread) {
	t.mu.Lock()
	t.threads = threads
	t.mu.Unlock()
	t.emit(ThreadUpdate{Update: Update{Kind: KindThreads}, Threads: threads})

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, listed := range threads {
		if listed.ParentThreadID == "" {
			continue
		}
		finished := listed.Status == anthropic.BetaManagedAgentsSessionThreadStatusTerminated || !listed.ArchivedAt.IsZero()
		if known, ok := t.children[listed.ID]; ok {
			if finished {
				known.conn.finish()
			}
			continue
		}
		mode := modeStream
		switch {
		case finished:
			mode = modeOnce
		case t.streamsInUse >= maxThreadStreams:
			mode = modePoll
		default:
			t.streamsInUse++
		}
		child := &childThread{
			conn:        openThread(t.ctx, t.primary.client, t.primary.sessionID, listed.ID, mode),
			createdAt:   listed.CreatedAt,
			holdsStream: mode == modeStream,
		}
		t.children[listed.ID] = child
		t.wg.Add(1)
		go t.forward(listed.ID, child)
	}
}

// threadMode is how a child Conn follows its thread.
type threadMode int

const (
	modeStream threadMode = iota // tail the thread's stream
	modePoll                     // re-list every pollInterval
	modeOnce                     // backfill once and stop
)

// openThread follows one thread of sessionID. It makes no request up front;
// failures surface as KindConn updates like any reconnect. Session() stays
// zero; Threads never writes through a child.
func openThread(ctx context.Context, client anthropic.Client, sessionID string, threadID ThreadID, mode threadMode) *Conn {
	events := client.Beta.Sessions.Threads.Events
	src := source{
		// The endpoint has no order parameter; it pages oldest-first.
		list: func(ctx context.Context, _ bool) *pagination.PageCursorAutoPager[Event] {
			return events.ListAutoPaging(ctx, threadID, anthropic.BetaSessionThreadEventListParams{SessionID: sessionID})
		},
		once: mode == modeOnce,
	}
	if mode == modeStream {
		src.stream = func(ctx context.Context) (subscription, error) {
			sse := events.StreamEvents(ctx, threadID, anthropic.BetaSessionThreadEventStreamParams{
				SessionID:   sessionID,
				EventDeltas: messageDeltas,
				Betas:       []anthropic.AnthropicBeta{deltasBeta},
			})
			// The thread stream's frames are the session stream's shapes
			// under a different Go type, so they round-trip through JSON.
			return subscribe(ctx, sse, func(threadFrame anthropic.BetaManagedAgentsStreamSessionThreadEventsUnion) streamEvent {
				var frame streamEvent
				_ = json.Unmarshal([]byte(threadFrame.RawJSON()), &frame)
				return frame
			})
		}
	}
	c := newConn(ctx, client, sessionID, src)
	go c.run()
	return c
}

// forward relays a child's updates until its Conn stops, then frees its
// stream slot.
func (t *Threads) forward(id ThreadID, child *childThread) {
	defer t.wg.Done()
	for u := range child.conn.Updates() {
		t.emit(ThreadUpdate{ThreadID: id, Update: u})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if child.holdsStream {
		child.holdsStream = false
		t.streamsInUse--
	}
}

func (t *Threads) emit(u ThreadUpdate) {
	select {
	case t.updates <- u:
	case <-t.ctx.Done():
	}
}
