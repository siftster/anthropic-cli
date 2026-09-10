package live

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// thread builds a threads.list entry; parent "" makes it the primary thread.
func thread(id, parent, status string, createdSec int) Thread {
	fields := map[string]any{
		"id": id, "status": status, "parent_thread_id": nil,
		"created_at": fmt.Sprintf("2026-08-06T12:00:%02dZ", createdSec),
		"agent":      map[string]any{"type": "agent", "name": id},
	}
	if parent != "" {
		fields["parent_thread_id"] = parent
	}
	raw, _ := json.Marshal(fields)
	var listed Thread
	_ = json.Unmarshal(raw, &listed)
	return listed
}

func openThreads(t *testing.T, api *fakeAPI) *Threads {
	t.Helper()
	ts, err := OpenThreads(context.Background(), api.client, "sesn_1")
	require.NoError(t, err)
	t.Cleanup(ts.Close)
	return ts
}

func nextThreadUpdate(t *testing.T, ts *Threads) ThreadUpdate {
	t.Helper()
	select {
	case u, ok := <-ts.Updates():
		require.True(t, ok, "updates closed")
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for thread update")
		return ThreadUpdate{}
	}
}

func expectNoThreadUpdate(t *testing.T, ts *Threads) {
	t.Helper()
	select {
	case u := <-ts.Updates():
		t.Fatalf("unexpected update: %+v", u)
	case <-time.After(50 * time.Millisecond):
	}
}

func logIDs(logs []ThreadLog) []string {
	out := make([]string, len(logs))
	for i, log := range logs {
		out[i] = log.ID
	}
	return out
}

func TestThreadsSingleAgentNeverListsThreads(t *testing.T) {
	api := newFakeAPI(t, event("e1", "user.message", 1))
	api.streams <- newStream()
	ts := openThreads(t, api)

	u := nextThreadUpdate(t, ts)
	assert.Equal(t, "", u.ThreadID)
	assert.Equal(t, "e1", u.Event.ID)
	assert.Equal(t, ThreadUpdate{Update: Update{Kind: KindConn, Connected: true, Backfilled: true}}, nextThreadUpdate(t, ts))
	expectNoThreadUpdate(t, ts)

	assert.Zero(t, api.count("threads"))
	threads, logs := ts.Snapshot()
	assert.Empty(t, threads)
	require.Equal(t, []string{""}, logIDs(logs))
	assert.Equal(t, []string{"e1"}, ids(logs[0].Events))
}

func TestThreadsThreadCreatedOpensChildAndFinishesIt(t *testing.T) {
	api := newFakeAPI(t)
	s, sc := newStream(), newStream()
	api.streams <- s
	api.threads = []Thread{thread("sthr_p", "", "running", 1), thread("sthr_c", "sthr_p", "running", 2)}
	api.threadHistory = map[string][]Event{"sthr_c": {event("c1", "user.message", 2), event("c2", "agent.message", 3)}}
	api.threadStreams = map[string]*stream{"sthr_c": sc}
	ts := openThreads(t, api)
	assert.True(t, nextThreadUpdate(t, ts).Backfilled)

	s.ch <- event("t1", "session.thread_created", 1).RawJSON()
	u := nextThreadUpdate(t, ts)
	assert.Equal(t, "", u.ThreadID)
	assert.Equal(t, "t1", u.Event.ID)
	u = nextThreadUpdate(t, ts)
	assert.Equal(t, KindThreads, u.Kind)
	require.Len(t, u.Threads, 2)
	assert.Equal(t, "sthr_c", u.Threads[1].ID)

	for _, want := range []string{"c1", "c2"} {
		u = nextThreadUpdate(t, ts)
		assert.Equal(t, "sthr_c", u.ThreadID)
		assert.Equal(t, want, u.Event.ID)
	}
	assert.Equal(t, ThreadUpdate{ThreadID: "sthr_c", Update: Update{Kind: KindConn, Connected: true, Backfilled: true}}, nextThreadUpdate(t, ts))
	sc.ch <- event("c3", "agent.tool_use", 4).RawJSON()
	u = nextThreadUpdate(t, ts)
	assert.Equal(t, "sthr_c", u.ThreadID)
	assert.Equal(t, "c3", u.Event.ID)

	threads, logs := ts.Snapshot()
	assert.Len(t, threads, 2)
	require.Equal(t, []string{"", "sthr_c"}, logIDs(logs))
	assert.Equal(t, []string{"t1"}, ids(logs[0].Events))
	assert.Equal(t, []string{"c1", "c2", "c3"}, ids(logs[1].Events))
	assert.Equal(t, 1, api.count("threads"))
	assert.Equal(t, 1, api.count("tstream:sthr_c"))
	assert.Zero(t, api.count("tstream:sthr_p"), "the primary thread is thread \"\", never a child")

	// The child terminates: one last catch-up, then its stream is dropped.
	api.mu.Lock()
	api.threads[1] = thread("sthr_c", "sthr_p", "terminated", 2)
	api.threadHistory["sthr_c"] = append(api.threadHistory["sthr_c"], event("c3", "agent.tool_use", 4), event("c4", "agent.message", 5))
	api.mu.Unlock()
	s.ch <- event("t2", "session.thread_status_terminated", 6).RawJSON()
	assert.Equal(t, "t2", nextThreadUpdate(t, ts).Event.ID)
	u = nextThreadUpdate(t, ts)
	assert.Equal(t, KindThreads, u.Kind)
	assert.Equal(t, anthropic.BetaManagedAgentsSessionThreadStatusTerminated, u.Threads[1].Status)
	u = nextThreadUpdate(t, ts)
	assert.Equal(t, "sthr_c", u.ThreadID)
	assert.Equal(t, "c4", u.Event.ID)
	expectNoThreadUpdate(t, ts)
	sc.ch <- event("c5", "agent.message", 7).RawJSON()
	expectNoThreadUpdate(t, ts)
	assert.Equal(t, 2, api.count("tlist:sthr_c"))
	_, logs = ts.Snapshot()
	assert.Equal(t, []string{"c1", "c2", "c3", "c4"}, ids(logs[1].Events), "a finished thread keeps its events")
}

func TestThreadsListsOnceAfterBackfillAndDebouncesBursts(t *testing.T) {
	tune(t, &listDebounce, 30*time.Millisecond)
	api := newFakeAPI(t, event("t1", "session.thread_created", 1), event("t2", "session.thread_status_running", 2))
	s := newStream()
	api.streams <- s
	api.threads = []Thread{thread("sthr_p", "", "running", 1)}
	ts := openThreads(t, api)

	nextThreadUpdate(t, ts) // t1
	nextThreadUpdate(t, ts) // t2
	assert.Zero(t, api.count("threads"), "no listing per event while backfilling")
	assert.True(t, nextThreadUpdate(t, ts).Backfilled)
	assert.Equal(t, KindThreads, nextThreadUpdate(t, ts).Kind)
	assert.Equal(t, 1, api.count("threads"))

	for i := range 3 {
		s.ch <- event(fmt.Sprintf("b%d", i), "session.thread_status_idle", 3+i).RawJSON()
	}
	kinds := map[UpdateKind]int{}
	for range 4 {
		kinds[nextThreadUpdate(t, ts).Kind]++
	}
	assert.Equal(t, map[UpdateKind]int{KindEvent: 3, KindThreads: 1}, kinds)
	time.Sleep(100 * time.Millisecond)
	for len(ts.Updates()) > 0 {
		nextThreadUpdate(t, ts)
	}
	lists := api.count("threads") - 1
	assert.GreaterOrEqual(t, lists, 1)
	assert.LessOrEqual(t, lists, 2, "a burst of thread events must not list once per event")
}

func TestThreadsListRetriesAfterError(t *testing.T) {
	api := newFakeAPI(t, event("t1", "session.thread_created", 1))
	api.streams <- newStream()
	api.threadsStatus = []int{503}
	api.threads = []Thread{thread("sthr_p", "", "running", 1)}
	ts := openThreads(t, api)
	nextThreadUpdate(t, ts) // t1
	nextThreadUpdate(t, ts) // conn
	assert.Equal(t, KindThreads, nextThreadUpdate(t, ts).Kind, "a failed listing is retried without another thread event")
	assert.Equal(t, 2, api.count("threads"))
	expectNoThreadUpdate(t, ts)
}

func TestThreadsTerminatedChildIsReadOnce(t *testing.T) {
	api := newFakeAPI(t, event("t1", "session.thread_created", 1))
	api.streams <- newStream()
	api.threads = []Thread{thread("sthr_p", "", "idle", 1), thread("sthr_c", "sthr_p", "terminated", 2)}
	api.threadHistory = map[string][]Event{"sthr_c": {event("c1", "agent.message", 2)}}
	ts := openThreads(t, api)

	nextThreadUpdate(t, ts) // t1
	nextThreadUpdate(t, ts) // conn
	assert.Equal(t, KindThreads, nextThreadUpdate(t, ts).Kind)
	u := nextThreadUpdate(t, ts)
	assert.Equal(t, "sthr_c", u.ThreadID)
	assert.Equal(t, "c1", u.Event.ID)
	assert.Equal(t, ThreadUpdate{ThreadID: "sthr_c", Update: Update{Kind: KindConn, Connected: true, Backfilled: true}}, nextThreadUpdate(t, ts))
	expectNoThreadUpdate(t, ts)

	assert.Zero(t, api.count("tstream:sthr_c"))
	assert.Equal(t, 1, api.count("tlist:sthr_c"), "a finished thread is read once, not polled")
	_, logs := ts.Snapshot()
	require.Equal(t, []string{"", "sthr_c"}, logIDs(logs))
	assert.Equal(t, []string{"c1"}, ids(logs[1].Events))
}

func TestThreadsOverStreamCapArePolled(t *testing.T) {
	tune(t, &maxThreadStreams, 1)
	api := newFakeAPI(t, event("t1", "session.thread_created", 1))
	api.streams <- newStream()
	api.threads = []Thread{
		thread("sthr_p", "", "running", 1),
		thread("sthr_c2", "sthr_p", "running", 3),
		thread("sthr_c1", "sthr_p", "running", 2),
	}
	api.threadHistory = map[string][]Event{"sthr_c1": {event("a1", "agent.message", 2)}, "sthr_c2": {event("b1", "agent.message", 3)}}
	api.threadStreams = map[string]*stream{"sthr_c2": newStream()}
	ts := openThreads(t, api)
	go func() {
		for range ts.Updates() {
		}
	}()

	require.Eventually(t, func() bool { return api.count("tlist:sthr_c1") >= 3 }, 2*time.Second, 5*time.Millisecond,
		"the child over the cap is polled")
	assert.Equal(t, 1, api.count("tstream:sthr_c2"), "list order decides who gets the stream slot")
	assert.Zero(t, api.count("tstream:sthr_c1"))
	_, logs := ts.Snapshot()
	require.Equal(t, []string{"", "sthr_c1", "sthr_c2"}, logIDs(logs), "threads go by created_at, not list order")
	assert.Equal(t, []string{"a1"}, ids(logs[1].Events), "repeated polls do not duplicate events")
}

func TestThreadsCloseStopsEverything(t *testing.T) {
	api := newFakeAPI(t, event("t1", "session.thread_created", 1))
	api.streams <- newStream()
	api.threads = []Thread{thread("sthr_p", "", "running", 1), thread("sthr_c", "sthr_p", "running", 2)}
	api.threadStreams = map[string]*stream{"sthr_c": newStream()}
	ts, err := OpenThreads(context.Background(), api.client, "sesn_1")
	require.NoError(t, err)
	for u := nextThreadUpdate(t, ts); u.ThreadID != "sthr_c"; u = nextThreadUpdate(t, ts) {
	}
	done := make(chan struct{})
	go func() {
		for range ts.Updates() {
		}
		close(done)
	}()
	ts.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Updates not closed after Close")
	}
}

func TestThreadsPrimaryFatalTearsDownChildren(t *testing.T) {
	api := newFakeAPI(t, event("t1", "session.thread_created", 1))
	s := newStream()
	api.streams <- s
	api.streamStatus = map[int]int{2: 404}
	api.threads = []Thread{thread("sthr_p", "", "running", 1), thread("sthr_c", "sthr_p", "running", 2)}
	api.threadStreams = map[string]*stream{"sthr_c": newStream()}
	ts := openThreads(t, api)
	for u := nextThreadUpdate(t, ts); u.ThreadID != "sthr_c"; u = nextThreadUpdate(t, ts) {
	}

	s.end() // the reconnect is answered 404
	var last ThreadUpdate
	timeout := time.After(2 * time.Second)
	for updatesOpen := true; updatesOpen; {
		select {
		case u, ok := <-ts.Updates():
			if !ok {
				updatesOpen = false
			} else if u.ThreadID == "" && u.Kind == KindConn {
				last = u
			}
		case <-timeout:
			t.Fatal("Updates not closed after a fatal primary error")
		}
	}
	assert.Equal(t, 404, statusOf(last.Err))
}

func TestOnceThreadConnStopsAfterCatchUp(t *testing.T) {
	api := newFakeAPI(t)
	c := openThread(context.Background(), api.client, "sesn_1", "sthr_c", modeOnce)
	defer c.Close()
	assert.Equal(t, Update{Kind: KindConn, Connected: true, Backfilled: true}, nextUpdate(t, c))
	_, ok := <-c.Updates()
	assert.False(t, ok, "a once conn stops after its catch-up")
}
