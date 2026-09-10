package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func init() {
	previewInterval, backoffBase = time.Millisecond, time.Millisecond
	listDebounce, pollInterval = time.Millisecond, 10*time.Millisecond
}

// event builds a wire event. processedSec 0 leaves it pending; message types
// get their id as text content.
func event(id, typ string, processedSec int) Event {
	fields := map[string]any{"id": id, "type": typ}
	if processedSec > 0 {
		fields["processed_at"] = fmt.Sprintf("2026-08-06T12:00:%02d.000123Z", processedSec)
	}
	if typ == "user.message" || typ == "agent.message" {
		fields["content"] = []map[string]any{{"type": "text", "text": id}}
	}
	raw, _ := json.Marshal(fields)
	var ev Event
	_ = json.Unmarshal(raw, &ev)
	return ev
}

func frameJSON(fields map[string]any) string {
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func startFrame(id string) string {
	return frameJSON(map[string]any{"type": "event_start", "event": map[string]any{"id": id, "type": "agent.message"}})
}

func deltaFrame(id, text string) string {
	return frameJSON(map[string]any{"type": "event_delta", "event_id": id,
		"delta": map[string]any{"type": "content_delta", "index": 0, "content": map[string]any{"type": "text", "text": text}}})
}

func statusOf(err error) int {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// tune sets a package tunable for one test; call it before openConn so the
// Conn is closed before the value is restored.
func tune[T any](t *testing.T, tunable *T, value T) {
	old := *tunable
	*tunable = value
	t.Cleanup(func() { *tunable = old })
}

func openConn(t *testing.T, api *fakeAPI) *Conn {
	t.Helper()
	c, err := Open(context.Background(), api.client, "sesn_1")
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func nextUpdate(t *testing.T, c *Conn) Update {
	t.Helper()
	select {
	case u, ok := <-c.Updates():
		require.True(t, ok, "updates closed")
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for update")
		return Update{}
	}
}

func expectNoUpdate(t *testing.T, c *Conn) {
	t.Helper()
	select {
	case u := <-c.Updates():
		t.Fatalf("unexpected update: %+v", u)
	case <-time.After(50 * time.Millisecond):
	}
}

// ids lists event ids in order, marking pending ones with a trailing "?".
func ids(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.ID
		if ev.ProcessedAt.IsZero() {
			out[i] += "?"
		}
	}
	return out
}

func textOf(ev Event) string {
	var text string
	for _, block := range gjson.Parse(ev.JSON.Content.Raw()).Array() {
		text += block.Get("text").String()
	}
	return text
}

func TestBackfillThenStreamDedupes(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1), event("e2", "agent.message", 2))
	api.pageSize, api.gate = 1, make(chan struct{})
	s := newStream()
	s.ch <- event("e2", "agent.message", 2).RawJSON()
	s.ch <- event("e3", "agent.tool_use", 3).RawJSON()
	api.streams <- s
	c := openConn(t, api)

	u := nextUpdate(t, c)
	assert.Equal(t, "e1", u.Event.ID)
	expectNoUpdate(t, c) // e3 is buffered behind the gated second page, not delivered early
	api.mu.Lock()
	assert.Equal(t, []string{"stream", "list:asc"}, api.calls[:2], "must subscribe before listing so nothing falls in the gap")
	api.mu.Unlock()
	close(api.gate)

	assert.Equal(t, "e2", nextUpdate(t, c).Event.ID)
	assert.Equal(t, Update{Kind: KindConn, Connected: true, Backfilled: true}, nextUpdate(t, c))
	u = nextUpdate(t, c)
	assert.Equal(t, "e3", u.Event.ID)
	assert.False(t, u.Reordered)
	expectNoUpdate(t, c)
	assert.Equal(t, []string{"e1", "e2", "e3"}, ids(c.Snapshot()))
}

func TestSessionEventsRefreshSession(t *testing.T) {
	api := newFakeAPI(t)
	s := newStream()
	api.streams <- s
	c := openConn(t, api)
	nextUpdate(t, c)

	s.ch <- event("st1", "session.status_running", 1).RawJSON()
	assert.Equal(t, KindEvent, nextUpdate(t, c).Kind)
	u := nextUpdate(t, c)
	assert.Equal(t, KindSession, u.Kind)
	assert.Equal(t, anthropic.BetaManagedAgentsSessionStatusRunning, u.Session.Status)

	s.ch <- frameJSON(map[string]any{"id": "up1", "type": "session.updated", "processed_at": "2026-08-06T12:00:02Z", "title": "New title"})
	nextUpdate(t, c)
	assert.Equal(t, "New title", nextUpdate(t, c).Session.Title)
	assert.Equal(t, "New title", c.Session().Title)
}

// History older than the session Open fetched must not regress it.
func TestBackfillDoesNotRegressSession(t *testing.T) {
	api := newFakeAPI(t, event("st1", "session.status_running", 1))
	api.session = `{"id":"sesn_1","status":"idle","updated_at":"2026-08-06T12:00:05Z"}`
	api.streams <- newStream()
	c := openConn(t, api)
	assert.Equal(t, "st1", nextUpdate(t, c).Event.ID)
	assert.Equal(t, KindConn, nextUpdate(t, c).Kind, "no KindSession for an event the session already reflects")
	assert.Equal(t, anthropic.BetaManagedAgentsSessionStatusIdle, c.Session().Status)
}

func TestCloseStopsEverything(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1))
	api.streams <- newStream()
	c, err := Open(context.Background(), api.client, "sesn_1")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		for range c.Updates() {
		}
		close(done)
	}()
	c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Updates not closed after Close")
	}
	require.NoError(t, c.SendMessage(context.Background(), "after close")) // no panic on closed channel
}

// A busy session, drained by a deliberately slow consumer that follows only
// the documented Update contract, must leave that consumer's list equal to
// Snapshot().
func TestSlowContractConsumerConvergesOnSnapshot(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1), event("e2", "agent.message", 2), event("e3", "agent.tool_use", 3))
	api.pageSize = 2
	s1, s2 := newStream(), newStream()
	api.streams <- s1
	api.streams <- s2
	api.echoIDs = []string{"u1"}
	c := openConn(t, api)

	var (
		mu   sync.Mutex
		list []Event
		last = time.Now()
	)
	go func() {
		for u := range c.Updates() {
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			list, last = applyUpdate(c, list, u), time.Now()
			mu.Unlock()
		}
	}()

	s1.ch <- startFrame("m1")
	for _, d := range []string{"a", "b", "c", "d"} {
		s1.ch <- deltaFrame("m1", d)
	}
	s1.ch <- event("e4", "agent.tool_result", 4).RawJSON()
	s1.ch <- startFrame("m2")
	s1.ch <- deltaFrame("m2", "x")
	require.NoError(t, c.SendMessage(context.Background(), "hi"))
	s1.ch <- event("u1", "user.message", 5).RawJSON()
	s1.ch <- event("m1", "agent.message", 6).RawJSON()
	s1.ch <- event("end1", "span.model_request_end", 7).RawJSON() // drops m2
	s1.ch <- startFrame("m3")
	s1.ch <- deltaFrame("m3", "y")
	api.mu.Lock()
	api.history = append(api.history, event("e4", "agent.tool_result", 4), event("u1", "user.message", 5),
		event("m1", "agent.message", 6), event("end1", "span.model_request_end", 7), event("m3", "agent.message", 8), event("e9", "session.status_idle", 9))
	api.mu.Unlock()
	s1.end() // m3 preview swept; catch-up brings m3 final and e9
	s2.ch <- startFrame("m4")
	s2.ch <- deltaFrame("m4", "z")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return time.Since(last) > 200*time.Millisecond && len(c.Updates()) == 0
	}, 5*time.Second, 20*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"e1", "e2", "e3", "e4", "u1", "m1", "end1", "m3", "e9", "m4?"}, ids(c.Snapshot()))
	assert.Equal(t, ids(c.Snapshot()), ids(list))
}

// applyUpdate is a consumer that knows only the documented Update contract.
func applyUpdate(c *Conn, list []Event, u Update) []Event {
	if u.Reordered {
		return c.Snapshot()
	}
	if u.Kind != KindEvent {
		return list
	}
	for i := range list {
		if list[i].ID != u.Event.ID {
			continue
		}
		stale := u.Event.ProcessedAt.IsZero() && !list[i].ProcessedAt.IsZero()
		if !stale {
			list[i] = u.Event
		}
		return list
	}
	at := len(list)
	for !isQueued(u.Event) && at > 0 && isQueued(list[at-1]) {
		at--
	}
	return slices.Insert(list, at, u.Event)
}

func isQueued(ev Event) bool { return ev.ProcessedAt.IsZero() && strings.HasPrefix(ev.Type, "user.") }
