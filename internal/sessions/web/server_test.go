package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/live"
	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
)

// stubPage has the bundle's two markers and two nonce-bearing tags.
const stubPage = `<html><!--SESSION_VIEWER_CONFIG--><style nonce="__CSP_NONCE__"></style><script nonce="__CSP_NONCE__">1</script></html>`

// newTestServer serves a two-thread fakeSession on a real loopback port, so
// the Host and Origin guards see the address they expect.
func newTestServer(t *testing.T) (*server, *fakeSession, *httptest.Server) {
	t.Helper()
	fake := &fakeSession{
		updates: make(chan live.ThreadUpdate, 16),
		threads: []live.Thread{
			parseThread(t, `"id":"sthr_p","parent_thread_id":null,"status":"idle","agent":{"type":"agent","name":"coder"}`),
			parseThread(t, `"id":"sthr_c","parent_thread_id":"sthr_p","status":"terminated","archived_at":"2026-01-01T15:00:00Z","agent":{"type":"advisor","model":"claude-x"}`),
		},
		logs: []live.ThreadLog{
			{Events: []live.Event{sessiontest.Ev(t, "user.message", 100, sessiontest.Text("hi"))}},
			{ID: "sthr_c", Events: []live.Event{sessiontest.Ev(t, "agent.message", 300, sessiontest.Text("sub"))}},
		},
	}
	fake.session.ID, fake.session.Title, fake.session.Status, fake.session.Agent.Name = "sesn_1", "T", "idle", "coder"

	ts := httptest.NewUnstartedServer(nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ts.Listener = listener
	srv := newServer(fake, []byte(stubPage), listener.Addr().String(), "https://console/x")
	ts.Config.Handler = srv
	ts.Start()
	t.Cleanup(ts.Close)
	go srv.hub.run()
	return srv, fake, ts
}

func TestRejectsForeignHostAndOrigin(t *testing.T) {
	srv, _, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/?boot="+srv.newBootCode(), nil)
	req.Host = "evil.example:80"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "rebinding host")

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/send", strings.NewReader(`{"type":"user.interrupt"}`))
	req.Header.Set("Authorization", "Bearer "+srv.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "http://evil.example")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "foreign origin")

	_, port, _ := net.SplitHostPort(srv.host)
	req, _ = http.NewRequest(http.MethodGet, "http://localhost:"+port+"/?boot="+srv.newBootCode(), nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "localhost alias allowed")
	assert.Contains(t, readBody(t, resp), srv.token)
}

func TestIndexHandsOutTokenOnlyForUnspentBootCode(t *testing.T) {
	srv, _, ts := newTestServer(t)

	// No boot code (a reload, or a stale link): the resume shell, never the token.
	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, body, srv.token)
	assert.Contains(t, body, `fetch("/reboot"`)
	assert.NotContains(t, body, nonceMarker)

	bootCode := srv.newBootCode()
	resp, err = http.Get(ts.URL + "/?boot=" + bootCode)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readBody(t, resp)
	assert.NotContains(t, body, nonceMarker)
	assert.NotContains(t, body, configMarker)
	assert.Contains(t, body, `id="session-viewer-config"`)
	assert.Contains(t, body, `"token":"`+srv.token+`"`)
	assert.Contains(t, body, `sessionStorage.setItem("session-viewer-token"`, "token is parked per-origin and the URL scrubbed")
	assert.Contains(t, body, `history.replaceState(null,"","/")`)
	cspHeader := resp.Header.Get("Content-Security-Policy")
	assert.Contains(t, cspHeader, "script-src 'nonce-")
	assert.NotContains(t, cspHeader, "unsafe-inline")
	nonce := strings.SplitN(cspHeader, "script-src 'nonce-", 2)[1]
	nonce = nonce[:strings.Index(nonce, "'")]
	assert.Equal(t, 3, strings.Count(body, `nonce="`+nonce+`"`), "style, bundle script, and the stash script")
	assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	resp, err = http.Get(ts.URL + "/?boot=" + bootCode)
	require.NoError(t, err)
	assert.NotContains(t, readBody(t, resp), srv.token, "a spent boot code must not yield the token again")
}

func TestEventsStreamSnapshotThenLiveFrames(t *testing.T) {
	srv, fake, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/events")
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?token="+srv.token, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "right token but not from the page (no Sec-Fetch-Site)")

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?token="+srv.token, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	stream := newFrameScanner(resp)

	name, data := readFrame(t, stream)
	assert.Equal(t, "snapshot", name)
	var snapshot struct {
		Session struct {
			ID    string
			Title string
			Agent struct{ Name string }
		}
		Threads []json.RawMessage
		Logs    []struct {
			ThreadID *string          `json:"thread_id"`
			Events   []map[string]any `json:"events"`
		} `json:"lanes"`
	}
	require.NoError(t, json.Unmarshal([]byte(data), &snapshot))
	assert.Equal(t, "sesn_1", snapshot.Session.ID)
	assert.Equal(t, "coder", snapshot.Session.Agent.Name)
	require.Len(t, snapshot.Threads, 2)
	assert.JSONEq(t, `{"id":"sthr_p","parent_thread_id":null,"status":"idle","archived_at":null,"created_at":"2026-01-01T14:00:00Z","agent":{"type":"agent","name":"coder"}}`, string(snapshot.Threads[0]))
	assert.JSONEq(t, `{"id":"sthr_c","parent_thread_id":"sthr_p","status":"terminated","archived_at":"2026-01-01T15:00:00Z","created_at":"2026-01-01T14:00:00Z","agent":{"type":"advisor","model":"claude-x"}}`, string(snapshot.Threads[1]))
	require.Len(t, snapshot.Logs, 2)
	assert.Nil(t, snapshot.Logs[0].ThreadID, "primary thread is thread_id null and comes first")
	require.Len(t, snapshot.Logs[0].Events, 1)
	assert.Equal(t, "user.message", snapshot.Logs[0].Events[0]["type"])
	require.NotNil(t, snapshot.Logs[1].ThreadID)
	assert.Equal(t, "sthr_c", *snapshot.Logs[1].ThreadID)
	assert.Equal(t, "e300", snapshot.Logs[1].Events[0]["id"])

	name, data = readFrame(t, stream)
	assert.Equal(t, "conn", name)
	assert.JSONEq(t, `{"connected":false}`, data, "nothing reported yet")

	fake.updates <- live.ThreadUpdate{Update: live.Update{Kind: live.KindConn, Connected: true}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "conn", name)
	assert.JSONEq(t, `{"connected":true}`, data)

	event := sessiontest.Ev(t, "agent.message", 200, sessiontest.Text("yo"))
	fake.updates <- live.ThreadUpdate{Update: live.Update{Kind: live.KindEvent, Event: event}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "upsert", name)
	assert.NotContains(t, data, "\n")
	assert.JSONEq(t, `{"thread_id":null,"event":`+event.RawJSON()+`}`, data)

	fake.updates <- live.ThreadUpdate{ThreadID: "sthr_c", Update: live.Update{Kind: live.KindEvent, Event: event}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "upsert", name)
	assert.Contains(t, data, `"thread_id":"sthr_c"`)

	fake.updates <- live.ThreadUpdate{ThreadID: "sthr_c", Update: live.Update{Kind: live.KindEvent, Reordered: true}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "reset", name)
	assert.JSONEq(t, `{"thread_id":"sthr_c","events":[`+fake.logs[1].Events[0].RawJSON()+`]}`, data, "a reset carries only the thread that moved")

	fake.updates <- live.ThreadUpdate{Update: live.Update{Kind: live.KindEvent, Reordered: true}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "reset", name)
	assert.Contains(t, data, `"thread_id":null`)

	fake.updates <- live.ThreadUpdate{Update: live.Update{Kind: live.KindThreads}, Threads: fake.threads[:1]}
	name, data = readFrame(t, stream)
	assert.Equal(t, "threads", name)
	assert.JSONEq(t, `{"threads":[`+string(snapshot.Threads[0])+`]}`, data)

	fake.updates <- live.ThreadUpdate{ThreadID: "sthr_c", Update: live.Update{Kind: live.KindConn, Connected: false}}
	fake.session.Title = "T2"
	fake.updates <- live.ThreadUpdate{Update: live.Update{Kind: live.KindSession, Session: &fake.session}}
	name, data = readFrame(t, stream)
	assert.Equal(t, "session", name, "a child thread's conn state is not the page's")
	assert.Contains(t, data, `"title":"T2"`)

	close(fake.updates)
	name, data = readFrame(t, stream)
	assert.Equal(t, "conn", name)
	assert.Contains(t, data, `"connected":false`)
	assert.Contains(t, data, `"final":true`, "the drop the server will not retry is marked so the page stops reconnecting")

	resp, err = http.DefaultClient.Do(req.Clone(ctx))
	require.NoError(t, err)
	stream = newFrameScanner(resp)
	readFrame(t, stream) // snapshot
	_, later := readFrame(t, stream)
	assert.Equal(t, data, later, "a page that connects later still hears the last word")
}

func TestSendMapsEachTypeToASessionCall(t *testing.T) {
	srv, fake, ts := newTestServer(t)
	post := func(body, auth, contentType string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/send", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp.StatusCode
	}
	bearer := "Bearer " + srv.token
	assert.Equal(t, http.StatusForbidden, post(`{"type":"user.interrupt"}`, "", "application/json"))
	assert.Equal(t, http.StatusForbidden, post(`{"type":"user.interrupt"}`, "Bearer nope", "application/json"))
	assert.Equal(t, http.StatusUnsupportedMediaType, post(`{"type":"user.interrupt"}`, bearer, "text/plain"))
	assert.Equal(t, http.StatusBadRequest, post(`{"type":"user.message","text":"  "}`, bearer, "application/json"))
	assert.Equal(t, http.StatusBadRequest, post(`{"type":"user.tool_confirmation","tool_use_id":"t1","result":"maybe"}`, bearer, "application/json"))
	assert.Equal(t, http.StatusBadRequest, post(`{"type":"nope"}`, bearer, "application/json"))

	assert.Equal(t, http.StatusOK, post(`{"type":"user.message","text":"hello"}`, bearer, "application/json"))
	assert.Equal(t, http.StatusOK, post(`{"type":"user.interrupt"}`, bearer, "application/json; charset=utf-8"))
	assert.Equal(t, http.StatusOK, post(`{"type":"user.tool_confirmation","tool_use_id":"t1","result":"deny","deny_message":"no"}`, bearer, "application/json"))
	assert.Equal(t, []string{"msg:hello", "interrupt", "confirm:t1:deny:no"}, fake.calls)
}

func TestRebootTradesTokenForFreshBootCode(t *testing.T) {
	srv, _, ts := newTestServer(t)
	post := func(auth, fetchSite string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/reboot", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	assert.Equal(t, http.StatusForbidden, post("", "same-origin").StatusCode)
	assert.Equal(t, http.StatusForbidden, post("Bearer nope", "same-origin").StatusCode)
	assert.Equal(t, http.StatusForbidden, post("Bearer "+srv.token, "").StatusCode, "no Sec-Fetch-Site: not the page")
	assert.Equal(t, http.StatusForbidden, post("Bearer "+srv.token, "same-site").StatusCode, "another localhost port")

	resp := post("Bearer "+srv.token, "same-origin")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct{ Boot string }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.Boot)
	page, err := http.Get(ts.URL + "/?boot=" + out.Boot)
	require.NoError(t, err)
	assert.Contains(t, readBody(t, page), `"token":"`+srv.token+`"`)
}

// fakeSession serves fixed threads and logs, takes updates from the test, and
// records each call the server makes on the user's behalf.
type fakeSession struct {
	updates chan live.ThreadUpdate
	threads []live.Thread
	logs    []live.ThreadLog
	session anthropic.BetaManagedAgentsSession
	calls   []string
}

func (f *fakeSession) Updates() <-chan live.ThreadUpdate            { return f.updates }
func (f *fakeSession) Snapshot() ([]live.Thread, []live.ThreadLog)  { return f.threads, f.logs }
func (f *fakeSession) Session() *anthropic.BetaManagedAgentsSession { return &f.session }

func (f *fakeSession) Events(id live.ThreadID) []live.Event {
	for _, threadLog := range f.logs {
		if threadLog.ID == id {
			return threadLog.Events
		}
	}
	return nil
}

func (f *fakeSession) SendMessage(_ context.Context, text string) error {
	f.calls = append(f.calls, "msg:"+text)
	return nil
}

func (f *fakeSession) Interrupt(context.Context) error {
	f.calls = append(f.calls, "interrupt")
	return nil
}

func (f *fakeSession) Confirm(_ context.Context, toolUseID string, allow bool, denyMessage string) error {
	result := "deny"
	if allow {
		result = "allow"
	}
	f.calls = append(f.calls, "confirm:"+toolUseID+":"+result+":"+denyMessage)
	return nil
}

// parseThread decodes a thread from its JSON fields, created at sessiontest.Epoch.
func parseThread(t *testing.T, fields string) live.Thread {
	t.Helper()
	var thread live.Thread
	require.NoError(t, json.Unmarshal([]byte(`{"created_at":"2026-01-01T14:00:00Z",`+fields+`}`), &thread))
	return thread
}

func newFrameScanner(resp *http.Response) *bufio.Scanner {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	return scanner
}

// readFrame returns the next SSE message's event name and data line.
func readFrame(t *testing.T, stream *bufio.Scanner) (name, data string) {
	t.Helper()
	for stream.Scan() {
		line := stream.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "":
			return name, data
		}
	}
	t.Fatalf("stream ended: %v", stream.Err())
	return "", ""
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
