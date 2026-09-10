package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stream is one SSE response: the test feeds ch and calls end to finish it.
// Frames queued before end are still delivered.
type stream struct {
	ch   chan string
	done chan struct{}
}

func newStream() *stream { return &stream{ch: make(chan string, 64), done: make(chan struct{})} }

// endedStream connects fine but finishes before yielding a frame.
func endedStream() *stream {
	s := newStream()
	s.end()
	return s
}

func (s *stream) end() { close(s.done) }

// fakeAPI serves the slice of the Sessions API that Conn and Threads use, for
// one session, over real HTTP; client is an SDK client pointed at it.
type fakeAPI struct {
	client anthropic.Client

	mu             sync.Mutex
	session        string            // GET session body
	history        []Event           // the session's log, oldest first
	pageSize       int               // events per list page; 0 serves the whole log at once
	gate           chan struct{}     // continuation pages wait for it
	listStatus     []int             // error statuses for the next list requests, consumed in order; 0 serves normally
	streams        chan *stream      // session streams, served in request order
	fallback       func() *stream    // once streams is empty; nil leaves the request pending until the client goes
	streamRequests int               // session stream requests so far
	streamStatus   map[int]int       // nth session stream request (1-based) fails with this status
	calls          []string          // one entry per request, e.g. "stream", "list:desc", "tlist:<thread>"
	sent           []json.RawMessage // every posted event
	echoIDs        []string          // ids stamped on echoed events, consumed in order
	sendStatus     int               // error status for every POST; 0 succeeds
	sendGate       chan struct{}     // each POST waits for one receive from it

	threads       []Thread
	threadsStatus []int // error statuses for the next threads.list requests, consumed in order
	threadHistory map[string][]Event
	threadStreams map[string]*stream // served on first open; later opens are left pending
}

func newFakeAPI(t *testing.T, history ...Event) *fakeAPI {
	f := &fakeAPI{session: `{"id":"sesn_1"}`, history: history, streams: make(chan *stream, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.session
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		f.serveList(w, r, "list:"+r.URL.Query().Get("order"), func() []Event { return f.history })
	})
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "stream")
		f.streamRequests++
		status := f.streamStatus[f.streamRequests]
		var s *stream
		if status == 0 {
			select {
			case s = <-f.streams:
			default:
				if f.fallback != nil {
					s = f.fallback()
				}
			}
		}
		f.mu.Unlock()
		serveStream(w, r, s, status)
	})
	mux.HandleFunc("POST /v1/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		var echo []string
		for _, e := range gjson.GetBytes(body, "events").Array() {
			f.sent = append(f.sent, json.RawMessage(e.Raw))
			id := fmt.Sprintf("sevt_%d", len(f.sent))
			if len(f.echoIDs) > 0 {
				id, f.echoIDs = f.echoIDs[0], f.echoIDs[1:]
			}
			raw, _ := sjson.Set(e.Raw, "id", id)
			echo = append(echo, raw)
		}
		status, gate := f.sendStatus, f.sendGate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			writeError(w, status)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":[`+strings.Join(echo, ",")+`]}`)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/threads", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "threads")
		data, status := rawList(f.threads), 0
		if len(f.threadsStatus) > 0 {
			status, f.threadsStatus = f.threadsStatus[0], f.threadsStatus[1:]
		}
		f.mu.Unlock()
		if status != 0 {
			writeError(w, status)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":`+data+`,"next_page":null}`)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/threads/{tid}/events", func(w http.ResponseWriter, r *http.Request) {
		tid := r.PathValue("tid")
		f.serveList(w, r, "tlist:"+tid, func() []Event { return f.threadHistory[tid] })
	})
	mux.HandleFunc("GET /v1/sessions/{id}/threads/{tid}/stream", func(w http.ResponseWriter, r *http.Request) {
		tid := r.PathValue("tid")
		f.mu.Lock()
		f.calls = append(f.calls, "tstream:"+tid)
		s := f.threadStreams[tid]
		delete(f.threadStreams, tid)
		f.mu.Unlock()
		serveStream(w, r, s, 0)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.client = anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"), option.WithMaxRetries(0))
	return f
}

// serveList pages log (reversed for order=desc) pageSize at a time behind an
// offset cursor, honouring listStatus and gate.
func (f *fakeAPI) serveList(w http.ResponseWriter, r *http.Request, call string, log func() []Event) {
	q := r.URL.Query()
	f.mu.Lock()
	f.calls = append(f.calls, call)
	events := slices.Clone(log())
	size, gate, status := f.pageSize, f.gate, 0
	if len(f.listStatus) > 0 {
		status, f.listStatus = f.listStatus[0], f.listStatus[1:]
	}
	f.mu.Unlock()
	if status != 0 {
		writeError(w, status)
		return
	}
	from, _ := strconv.Atoi(q.Get("page"))
	if from > 0 && gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if q.Get("order") == "desc" {
		slices.Reverse(events)
	}
	events = events[min(from, len(events)):]
	next := "null"
	if size > 0 && len(events) > size {
		events, next = events[:size], strconv.Quote(strconv.Itoa(from+size))
	}
	writeJSON(w, http.StatusOK, `{"data":`+rawList(events)+`,"next_page":`+next+`}`)
}

// serveStream answers status as an API error, leaves a nil stream pending
// until the client goes, and otherwise relays s as SSE until it ends.
func serveStream(w http.ResponseWriter, r *http.Request, s *stream, status int) {
	if status != 0 {
		writeError(w, status)
		return
	}
	if s == nil {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush := w.(http.Flusher).Flush
	flush()
	send := func(data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(data, "type").String(), data)
		flush()
	}
	for {
		select {
		case data := <-s.ch:
			send(data)
		case <-s.done:
			for {
				select {
				case data := <-s.ch:
					send(data)
				default:
					return
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

// count reports how many recorded calls start with prefix.
func (f *fakeAPI) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func rawList[T interface{ RawJSON() string }](items []T) string {
	raw := make([]string, len(items))
	for i, it := range items {
		raw[i] = it.RawJSON()
	}
	return "[" + strings.Join(raw, ",") + "]"
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeError(w http.ResponseWriter, status int) {
	writeJSON(w, status, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"status %d"}}`, status))
}
