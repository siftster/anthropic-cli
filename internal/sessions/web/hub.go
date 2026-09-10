package web

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/anthropics/anthropic-cli/internal/sessions/live"
)

// hub is the sole reader of the session's Updates and fans frames out to
// every connected page. A page that falls too far behind is dropped; its
// EventSource reconnects and starts over from a snapshot.
type hub struct {
	session Session

	mu          sync.Mutex
	subscribers map[chan frame]struct{}
	lastConn    frame // replayed to pages that connect later
}

// frame is one named SSE message with a single-line JSON payload.
type frame struct {
	name string
	data []byte
}

// subscriberBuffer is how many frames a page may fall behind before it is dropped.
const subscriberBuffer = 256

func newHub(session Session) *hub {
	return &hub{
		session:     session,
		subscribers: map[chan frame]struct{}{},
		lastConn:    connFrame(false, nil),
	}
}

// run turns each update into a frame until Updates closes.
func (h *hub) run() {
	var lastErr error
	for update := range h.session.Updates() {
		// Reordered can ride on any kind. The reset carries the thread's
		// whole log, including this update's event.
		if update.Reordered {
			h.broadcast(h.resetFrame(update.ThreadID))
		}
		switch update.Kind {
		case live.KindEvent:
			if !update.Reordered {
				h.broadcast(upsertFrame(update.ThreadID, update.Event))
			}
		case live.KindSession:
			h.broadcast(frame{"session", sessionInfo(update.Session)})
		case live.KindConn:
			// A child thread's link comes and goes with it; the page's
			// banner is about the session's.
			if update.ThreadID == "" {
				lastErr = update.Err
				h.setConn(connFrame(update.Connected, update.Err))
			}
		case live.KindThreads:
			h.broadcast(threadsFrame(update.Threads))
		}
	}
	// After Updates closes nothing more will arrive; say why, and mark it
	// final so the page stops reconnecting.
	msg := "session stream closed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	h.setConn(frame{"conn", mustJSON(map[string]any{"connected": false, "error": msg, "final": true})})
}

func (h *hub) subscribe() chan frame {
	ch := make(chan frame, subscriberBuffer)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// unsubscribe is safe after broadcast has already dropped ch.
func (h *hub) unsubscribe(ch chan frame) {
	h.mu.Lock()
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *hub) broadcast(f frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- f:
		default:
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

// setConn records f for pages yet to connect before broadcasting it, so a
// page that subscribes and then reads lastConnFrame can miss neither.
func (h *hub) setConn(f frame) {
	h.mu.Lock()
	h.lastConn = f
	h.mu.Unlock()
	h.broadcast(f)
}

// lastConnFrame is the primary thread's most recent connection state.
func (h *hub) lastConnFrame() frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastConn
}

// snapshotFrame is everything a newly connected page starts from.
func (h *hub) snapshotFrame() frame {
	threads, logs := h.session.Snapshot()
	wireLogs := make([]wireThreadLog, len(logs))
	for i, threadLog := range logs {
		wireLogs[i] = wireThreadLog{wireThreadID(threadLog.ID), rawEvents(threadLog.Events)}
	}
	return frame{"snapshot", mustJSON(map[string]any{
		"session": json.RawMessage(sessionInfo(h.session.Session())),
		"threads": threadInfos(threads),
		// The viewer protocol calls per-thread event logs "lanes".
		"lanes": wireLogs,
	})}
}

// resetFrame replaces one thread's log on the page.
func (h *hub) resetFrame(id live.ThreadID) frame {
	return frame{"reset", mustJSON(wireThreadLog{wireThreadID(id), rawEvents(h.session.Events(id))})}
}

// upsertFrame adds an event to a thread's log, or replaces the one with its id.
func upsertFrame(id live.ThreadID, event live.Event) frame {
	return frame{"upsert", mustJSON(struct {
		ThreadID *string         `json:"thread_id"`
		Event    json.RawMessage `json:"event"`
	}{wireThreadID(id), json.RawMessage(event.RawJSON())})}
}

func threadsFrame(threads []live.Thread) frame {
	return frame{"threads", mustJSON(map[string]any{"threads": threadInfos(threads)})}
}

func connFrame(connected bool, err error) frame {
	fields := map[string]any{"connected": connected}
	if err != nil {
		fields["error"] = err.Error()
	}
	return frame{"conn", mustJSON(fields)}
}

// wireThreadLog is one thread's event log on the wire.
type wireThreadLog struct {
	ThreadID *string           `json:"thread_id"`
	Events   []json.RawMessage `json:"events"`
}

// wireThreadID is null for the primary thread.
func wireThreadID(id live.ThreadID) *string {
	if id == "" {
		return nil
	}
	return &id
}

// threadInfo is a thread as the viewer expects it; pointers keep absent values null.
type threadInfo struct {
	ID             string      `json:"id"`
	ParentThreadID *string     `json:"parent_thread_id"`
	Status         string      `json:"status"`
	ArchivedAt     *time.Time  `json:"archived_at"`
	CreatedAt      time.Time   `json:"created_at"`
	Agent          threadAgent `json:"agent"`
}

// threadAgent is the roster union narrowed to what labels need: a named
// agent, or the advisor (no name, just its model).
type threadAgent struct {
	Type  string  `json:"type"`
	Name  *string `json:"name,omitempty"`
	Model *string `json:"model,omitempty"`
}

func threadInfos(threads []live.Thread) []threadInfo {
	infos := make([]threadInfo, len(threads))
	for i, thread := range threads {
		info := threadInfo{ID: thread.ID, Status: string(thread.Status), CreatedAt: thread.CreatedAt}
		if thread.ParentThreadID != "" {
			info.ParentThreadID = &thread.ParentThreadID
		}
		if !thread.ArchivedAt.IsZero() {
			info.ArchivedAt = &thread.ArchivedAt
		}
		if thread.Agent.Type == "advisor" {
			model := thread.Agent.Model.OfString
			if model == "" {
				model = string(thread.Agent.Model.ID)
			}
			info.Agent = threadAgent{Type: "advisor", Model: &model}
		} else {
			info.Agent = threadAgent{Type: "agent", Name: &thread.Agent.Name}
		}
		infos[i] = info
	}
	return infos
}

// sessionInfo is the fields the viewer's header reads, taken from the
// live-updated session rather than its original wire JSON.
func sessionInfo(session *anthropic.BetaManagedAgentsSession) []byte {
	return mustJSON(map[string]any{
		"id": session.ID, "title": session.Title, "status": session.Status,
		"agent": map[string]any{"name": session.Agent.Name},
	})
}

// rawEvents forwards each event's wire JSON untouched.
func rawEvents(events []live.Event) []json.RawMessage {
	raw := make([]json.RawMessage, len(events))
	for i, event := range events {
		raw[i] = json.RawMessage(event.RawJSON())
	}
	return raw
}

// mustJSON's output is always one line (encoding/json compacts embedded
// RawMessages too), which is what an SSE data: field needs.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
