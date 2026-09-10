package live

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInitialListErrorSurfacesThenRetries(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1))
	api.listStatus = []int{500}
	api.fallback = newStream
	c := openConn(t, api)

	u := nextUpdate(t, c)
	assert.Equal(t, KindConn, u.Kind)
	assert.False(t, u.Connected)
	assert.False(t, u.Backfilled)
	assert.Equal(t, 500, statusOf(u.Err))
	assert.Equal(t, "e1", nextUpdate(t, c).Event.ID)
	assert.Equal(t, Update{Kind: KindConn, Connected: true, Backfilled: true}, nextUpdate(t, c))
}

func TestReconnectCatchesUpNewestFirst(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1))
	api.pageSize = 1
	s1, s2 := newStream(), newStream()
	api.streams <- s1
	api.streams <- s2
	c := openConn(t, api)
	nextUpdate(t, c) // e1
	nextUpdate(t, c) // conn

	s1.ch <- event("e2", "agent.message", 2).RawJSON()
	assert.Equal(t, "e2", nextUpdate(t, c).Event.ID)

	api.mu.Lock()
	api.history = append(api.history, event("e2", "agent.message", 2), event("e3", "agent.message", 3), event("e4", "agent.message", 4))
	api.mu.Unlock()
	s1.end()

	u := nextUpdate(t, c)
	assert.Equal(t, KindConn, u.Kind)
	assert.False(t, u.Connected)
	assert.ErrorIs(t, u.Err, errStreamEnded)

	assert.Equal(t, "e3", nextUpdate(t, c).Event.ID)
	assert.Equal(t, "e4", nextUpdate(t, c).Event.ID)
	assert.Equal(t, Update{Kind: KindConn, Connected: true}, nextUpdate(t, c))

	api.mu.Lock()
	defer api.mu.Unlock()
	assert.Equal(t, []string{"stream", "list:asc", "stream", "list:desc", "list:desc", "list:desc"}, api.calls,
		"desc paging (one event a page here) stops at the first seen id instead of reading all four")
	assert.Equal(t, []string{"e1", "e2", "e3", "e4"}, ids(c.Snapshot()))
}

func TestReconnectKeepsMessageCompletedWhileDisconnected(t *testing.T) {
	api := newFakeAPI(t)
	s1, s2 := newStream(), newStream()
	api.streams <- s1
	api.streams <- s2
	c := openConn(t, api)
	nextUpdate(t, c)

	s1.ch <- startFrame("m1")
	s1.ch <- deltaFrame("m1", "hi")
	nextUpdate(t, c)
	nextUpdate(t, c)
	api.mu.Lock()
	api.history = []Event{event("m1", "agent.message", 1)}
	api.mu.Unlock()
	s1.end()

	u := nextUpdate(t, c)
	assert.False(t, u.Connected)
	assert.True(t, u.Reordered, "unfinished preview is swept on disconnect")
	u = nextUpdate(t, c)
	assert.Equal(t, "m1", u.Event.ID)
	assert.False(t, u.Event.ProcessedAt.IsZero())
	assert.True(t, nextUpdate(t, c).Connected)

	s2.ch <- event("end1", "span.model_request_end", 2).RawJSON()
	u = nextUpdate(t, c)
	assert.Equal(t, "end1", u.Event.ID)
	assert.False(t, u.Reordered)
	assert.Equal(t, []string{"m1", "end1"}, ids(c.Snapshot()))
}

func TestBackoffGrowsWhileStreamNeverYields(t *testing.T) {
	api := newFakeAPI(t)
	api.fallback = endedStream
	base := 10 * time.Millisecond
	tune(t, &backoffBase, base)
	c := openConn(t, api)
	var drops []time.Time
	for len(drops) < 5 {
		if u := nextUpdate(t, c); u.Kind == KindConn && !u.Connected {
			drops = append(drops, time.Now())
		}
	}
	// Delays run base, 2×, 4×, 8× (±20%); a reset-on-connect bug keeps them at base.
	assert.GreaterOrEqual(t, drops[4].Sub(drops[3]), 4*base)
}

func TestFatalErrorClosesUpdates(t *testing.T) {
	api := newFakeAPI(t)
	api.streamStatus = map[int]int{1: 404}
	c := openConn(t, api)

	u := nextUpdate(t, c)
	assert.Equal(t, KindConn, u.Kind)
	assert.Equal(t, 404, statusOf(u.Err), "a non-2xx over HTTP must surface as *anthropic.Error")
	_, ok := <-c.Updates()
	assert.False(t, ok, "Updates closes after a fatal error")
}
