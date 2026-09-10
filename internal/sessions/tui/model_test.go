package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/live"
	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

var (
	mk   = sessiontest.Ev
	text = sessiontest.Text
	tool = sessiontest.Tool
	js   = sessiontest.JS
)

// fakeSession records what the model sends. Like live.Conn, a successful
// Confirm delivers the verdict as a queued event on Updates.
type fakeSession struct {
	t          *testing.T
	clock      time.Time // what the model's now() reads
	updates    chan live.Update
	sess       anthropic.BetaManagedAgentsSession
	snapshot   []live.Event
	snapshots  int
	sent       []string
	interrupts int
	confirms   []string
	fail       error
}

func (f *fakeSession) Updates() <-chan live.Update                  { return f.updates }
func (f *fakeSession) Snapshot() []live.Event                       { f.snapshots++; return f.snapshot }
func (f *fakeSession) Session() *anthropic.BetaManagedAgentsSession { return &f.sess }
func (f *fakeSession) Interrupt(context.Context) error              { f.interrupts++; return f.fail }
func (f *fakeSession) SendMessage(_ context.Context, text string) error {
	f.sent = append(f.sent, text)
	return f.fail
}
func (f *fakeSession) Confirm(_ context.Context, id string, allow bool, deny string) error {
	f.confirms = append(f.confirms, fmt.Sprintf("%s allow=%v %s", id, allow, deny))
	if f.fail != nil {
		return f.fail
	}
	result := map[bool]string{true: "allow", false: "deny"}[allow]
	f.updates <- live.Update{Kind: live.KindEvent, Event: sessiontest.Pending(f.t, "user.tool_confirmation", "local:confirm:"+id,
		fmt.Sprintf(`"tool_use_id":%q,"result":%q,"deny_message":%q`, id, result, deny))}
	return nil
}

func newTestModel(t *testing.T) (*model, *fakeSession) {
	t.Helper()
	fs := &fakeSession{t: t, updates: make(chan live.Update, 8)}
	fs.sess.Title, fs.sess.Agent.Name = "Fix flaky retry test", "coder"
	m := newModel(context.Background(), fs, Options{})
	fs.clock = sessiontest.Epoch
	m.now = func() time.Time { return fs.clock }
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	send(m, live.Update{Kind: live.KindConn, Connected: true, Backfilled: true})
	return m, fs
}

func send(m *model, us ...live.Update) tea.Cmd {
	_, cmd := m.Update(updatesMsg{batch: us})
	return cmd
}

func push(m *model, evs ...timeline.Event) {
	for _, e := range evs {
		send(m, live.Update{Kind: live.KindEvent, Event: e})
	}
}

// elapse moves the model's clock on; past approvalKeyGuard a prompt takes verdict keys.
func elapse(fs *fakeSession, d time.Duration) { fs.clock = fs.clock.Add(d) }

func press(m *model, k tea.KeyType) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: k})
	return cmd
}

func typeText(m *model, s string) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return cmd
}

// run executes a command the way the runtime would: its message is fed
// back, then whatever the session queued on Updates meanwhile is applied.
func run(m *model, cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg != nil {
		m.Update(msg)
	}
	for ch := m.session.Updates(); len(ch) > 0; {
		send(m, <-ch)
	}
	return msg
}

func TestReorderedBatchResetsOnce(t *testing.T) {
	m, fs := newTestModel(t)
	fs.snapshot = []live.Event{mk(t, "user.message", 1, text("a")), mk(t, "user.message", 2, text("b"))}
	send(m,
		live.Update{Kind: live.KindEvent, Event: fs.snapshot[0], Reordered: true},
		live.Update{Kind: live.KindEvent, Reordered: true},
		live.Update{Kind: live.KindEvent, Event: fs.snapshot[1], Reordered: true},
	)
	require.Equal(t, 1, fs.snapshots)
	require.Contains(t, m.View(), "│ b")
}

func TestEndedOrFailedSessionClosesTheInput(t *testing.T) {
	m, fs := newTestModel(t)
	push(m, mk(t, "session.status_terminated", 100, ""))
	require.Equal(t, modeClosed, m.mode)
	typeText(m, "hello")
	require.Nil(t, press(m, tea.KeyEnter))
	require.Empty(t, fs.sent)
	require.Contains(t, m.View(), "■ terminated")

	m, _ = newTestModel(t)
	send(m, live.Update{Kind: live.KindConn, Connected: false, Err: errors.New("404 session not found")})
	require.Contains(t, m.View(), "reconnecting…")
	_, cmd := m.Update(updatesMsg{closed: true})
	require.Nil(t, cmd, "fatal close keeps the screen up")
	require.Equal(t, modeClosed, m.mode)
	require.Contains(t, m.View(), "Disconnected: 404 session not found")
	require.Contains(t, m.View(), "Session closed")

	m, _ = newTestModel(t)
	_, cmd = m.Update(updatesMsg{closed: true})
	_, isQuit := cmd().(tea.QuitMsg)
	require.True(t, isQuit, "clean close (ctx cancelled) just exits")
}

func TestRenderingWaitsForBackfillThenBatchesAndFollows(t *testing.T) {
	fs := &fakeSession{updates: make(chan live.Update, 300)}
	fs.sess.Agent.Name = "coder"
	m := newModel(context.Background(), fs, Options{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	base := m.renders

	var batch []live.Update
	for i := range 40 {
		batch = append(batch, live.Update{Kind: live.KindEvent, Event: mk(t, "user.message", i+1, text(fmt.Sprint("msg ", i)))})
	}
	send(m, batch[:20]...)
	require.Equal(t, base, m.renders, "nothing rendered before backfill completes")
	require.Contains(t, m.View(), "loading history… 20 events")

	send(m, append(batch[20:], live.Update{Kind: live.KindConn, Connected: true, Backfilled: true})...)
	require.Equal(t, base+1, m.renders, "one render for the whole batch")
	require.True(t, m.viewport.AtBottom())
	require.Contains(t, m.View(), "msg 39")

	for _, u := range batch[:30] {
		fs.updates <- u
	}
	got := m.waitUpdates()().(updatesMsg)
	require.Len(t, got.batch, 30, "reader drains what is buffered")

	press(m, tea.KeyPgUp)
	require.False(t, m.follow)
	push(m, mk(t, "user.message", 99, text("new")))
	require.False(t, m.viewport.AtBottom(), "scrolled up: new output does not yank the view")
	press(m, tea.KeyEnd)
	require.True(t, m.follow)

	fs.snapshot = []live.Event{mk(t, "user.message", 1, text("only"))}
	send(m, live.Update{Kind: live.KindEvent, Reordered: true})
	require.True(t, m.viewport.AtBottom())
	require.Contains(t, m.View(), "│ only")
	require.NotContains(t, m.View(), "msg 39")

	send(m, live.Update{Kind: live.KindSession, Session: &anthropic.BetaManagedAgentsSession{Title: "renamed"}})
	require.Contains(t, m.View(), "renamed")
}

func TestIdleSessionDoesNotRerenderOnTick(t *testing.T) {
	m, _ := newTestModel(t)
	push(m, mk(t, "user.message", 1, text("hi")), mk(t, "session.status_idle", 2, `"stop_reason":{"type":"end_turn"}`))
	base := m.renders
	m.Update(m.spinner.Tick())
	require.Equal(t, base, m.renders)

	push(m, mk(t, "session.status_running", 3, ""), mk(t, "span.model_request_start", 4, ""))
	base = m.renders
	m.Update(m.spinner.Tick())
	require.Equal(t, base+1, m.renders, "open turn shows Generating… spinner")
	require.Contains(t, m.View(), "Generating…")
}

func TestTinyTerminalsDoNotPanic(t *testing.T) {
	for _, size := range []tea.WindowSizeMsg{{Width: 30, Height: 8}, {Width: 0, Height: 0}} {
		fs := &fakeSession{updates: make(chan live.Update, 1)}
		m := newModel(context.Background(), fs, Options{Verbose: true})
		m.Update(size)
		send(m, live.Update{Kind: live.KindConn, Connected: true, Backfilled: true})
		push(m, sessiontest.Canonical(t)...)
		require.NotPanics(t, func() { _ = m.View() })
	}
}
