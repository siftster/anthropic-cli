package live

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamingPreviewIsReplacedByProcessedEvent(t *testing.T) {
	api := newFakeAPI(t)
	s := newStream()
	api.streams <- s
	c := openConn(t, api)
	assert.Equal(t, KindConn, nextUpdate(t, c).Kind)

	s.ch <- startFrame("m1")
	u := nextUpdate(t, c)
	assert.Equal(t, "m1", u.Event.ID)
	assert.Equal(t, "agent.message", u.Event.Type)
	assert.True(t, u.Event.ProcessedAt.IsZero())

	s.ch <- startFrame("m1") // replayed start must not reset accumulated text
	for _, step := range []struct{ delta, want string }{{"Hel", "Hel"}, {"lo", "Hello"}, {" world", "Hello world"}} {
		s.ch <- deltaFrame("m1", step.delta)
		u = nextUpdate(t, c)
		assert.Equal(t, "m1", u.Event.ID)
		assert.True(t, u.Event.ProcessedAt.IsZero())
		assert.Equal(t, step.want, textOf(u.Event))
	}
	assert.Contains(t, u.Event.JSON.Content.Raw(), "Hello world", "previews must carry raw JSON like wire events")

	s.ch <- event("m1", "agent.message", 5).RawJSON()
	u = nextUpdate(t, c)
	assert.False(t, u.Event.ProcessedAt.IsZero())
	assert.Equal(t, "m1", textOf(u.Event))
	require.Len(t, c.Snapshot(), 1)

	// A preview whose model request ends without the event is dropped.
	s.ch <- startFrame("m2")
	s.ch <- deltaFrame("m2", "partial")
	assert.Equal(t, "m2", nextUpdate(t, c).Event.ID)
	assert.Equal(t, "partial", textOf(nextUpdate(t, c).Event))
	s.ch <- event("end1", "span.model_request_end", 6).RawJSON()
	assert.Equal(t, Update{Kind: KindEvent, Reordered: true}, nextUpdate(t, c), "m2 dropped")
	u = nextUpdate(t, c)
	assert.Equal(t, "end1", u.Event.ID)
	assert.False(t, u.Reordered)
	assert.Equal(t, []string{"m1", "end1"}, ids(c.Snapshot()))
}

func TestPreviewThrottleCoalescesDeltas(t *testing.T) {
	api := newFakeAPI(t)
	s := newStream()
	api.streams <- s
	tune(t, &previewInterval, 100*time.Millisecond)
	c := openConn(t, api)
	nextUpdate(t, c)

	s.ch <- startFrame("m1")
	for _, part := range []string{"a", "b", "c", "d", "e"} {
		s.ch <- deltaFrame("m1", part)
	}
	assert.Equal(t, "", textOf(nextUpdate(t, c).Event))
	upserts := 0
	for u := nextUpdate(t, c); ; u = nextUpdate(t, c) {
		upserts++
		if textOf(u.Event) == "abcde" {
			break
		}
	}
	assert.LessOrEqual(t, upserts, 2, "five deltas inside one interval should coalesce")
	expectNoUpdate(t, c)
}
