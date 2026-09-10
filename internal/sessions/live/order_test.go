package live

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreviewsSortBeforeQueuedUserEvents(t *testing.T) {
	api := newFakeAPI(t, event("u0", "user.message", 1))
	s := newStream()
	api.streams <- s
	api.echoIDs = []string{"u1"}
	c := openConn(t, api)
	nextUpdate(t, c) // u0
	nextUpdate(t, c) // conn

	s.ch <- startFrame("m1")
	nextUpdate(t, c)
	require.NoError(t, c.SendMessage(context.Background(), "hi"))
	u := nextUpdate(t, c)
	assert.Equal(t, "u1", u.Event.ID)
	assert.False(t, u.Reordered)
	assert.Equal(t, []string{"u0", "m1?", "u1?"}, ids(c.Snapshot()))

	// A second streaming preview sorts before the queued user message, which
	// the contract already implies.
	s.ch <- startFrame("m2")
	u = nextUpdate(t, c)
	assert.False(t, u.Reordered)
	assert.Equal(t, []string{"u0", "m1?", "m2?", "u1?"}, ids(c.Snapshot()))

	// Once processed, u1 joins the processed group ahead of the previews.
	s.ch <- event("u1", "user.message", 2).RawJSON()
	u = nextUpdate(t, c)
	assert.True(t, u.Reordered)
	assert.Equal(t, []string{"u0", "u1", "m1?", "m2?"}, ids(c.Snapshot()))
}

// While a sent message waits to be ingested, everything the agent does lands
// ahead of it; that is the contract's normal case, not a reorder per event.
func TestQueuedEchoDoesNotReorderPerEvent(t *testing.T) {
	api := newFakeAPI(t, event("u0", "user.message", 1))
	s := newStream()
	api.streams <- s
	api.echoIDs = []string{"u1"}
	c := openConn(t, api)
	nextUpdate(t, c) // u0
	nextUpdate(t, c) // conn

	var list []Event
	list = applyUpdate(c, list, Update{Kind: KindEvent, Event: event("u0", "user.message", 1)})
	require.NoError(t, c.SendMessage(context.Background(), "hi"))
	u := nextUpdate(t, c)
	assert.Equal(t, "u1", u.Event.ID)
	list = applyUpdate(c, list, u)

	for i := range 5 {
		s.ch <- event(fmt.Sprintf("a%d", i), "agent.tool_use", 2+i).RawJSON()
		u = nextUpdate(t, c)
		assert.False(t, u.Reordered, u.Event.ID)
		list = applyUpdate(c, list, u)
	}
	assert.Equal(t, []string{"u0", "a0", "a1", "a2", "a3", "a4", "u1?"}, ids(c.Snapshot()))
	assert.Equal(t, ids(c.Snapshot()), ids(list))

	s.ch <- event("u1", "user.message", 9).RawJSON()
	list = applyUpdate(c, list, nextUpdate(t, c))
	assert.Equal(t, []string{"u0", "a0", "a1", "a2", "a3", "a4", "u1"}, ids(c.Snapshot()))
	assert.Equal(t, ids(c.Snapshot()), ids(list))
	expectNoUpdate(t, c)
}
