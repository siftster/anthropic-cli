package timeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
)

var (
	epoch   = sessiontest.Epoch
	ev      = sessiontest.Parse
	js      = sessiontest.JS
	mk      = sessiontest.Ev
	pending = sessiontest.Pending
)

func fold(evs ...Event) *Fold {
	f := NewFold()
	f.Reset(evs)
	return f
}

// kinds is the rendered column's shape, minus verbose-only machinery.
func kinds(f *Fold) []string {
	var out []string
	for _, n := range f.Nodes() {
		if n.Kind != NodeSilent {
			out = append(out, string(n.Kind))
		}
	}
	return out
}

func turns(f *Fold) []Node {
	var out []Node
	for _, n := range f.Nodes() {
		if n.Kind == NodeTurn {
			out = append(out, n)
		}
	}
	return out
}

func blockKinds(n Node) []string {
	var out []string
	for _, b := range n.Blocks {
		out = append(out, string(b.Kind))
	}
	return out
}

func useIDs(calls []ToolCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Use.ID)
	}
	return out
}

const usageJSON = `"model_request_start_id":"e100","model_usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":1}`

func TestTurnGroupingAndToolBatching(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.thinking", 200, ""),
		mk(t, "agent.message", 300, sessiontest.Text("hi")),
		mk(t, "agent.tool_use", 400, sessiontest.Tool("grep", `{"pattern":"x"}`, "")),
		mk(t, "agent.tool_use", 500, sessiontest.Tool("read", `{"file_path":"a.py"}`, "")),
		mk(t, "agent.message", 600, sessiontest.Text("then")),
		mk(t, "agent.tool_use", 700, sessiontest.Tool("bash", `{"command":"ls"}`, "")),
		mk(t, "span.model_request_end", 800, usageJSON),
	)
	require.Equal(t, []string{"turn"}, kinds(f))
	turn := f.Nodes()[0]
	require.Equal(t, []string{"thinking", "text", "tools", "text", "tools", "request_end"}, blockKinds(turn))
	require.Len(t, turn.Blocks[2].Calls, 2)
	require.Len(t, turn.Blocks[4].Calls, 1)
	require.False(t, turn.Open)
	require.Equal(t, epoch.Add(100*time.Millisecond), turn.Event.ProcessedAt)
	require.Equal(t, &Usage{Input: 111, Output: 5}, UsageOf(turn.Blocks[5].Event))
}

func TestResultsPairByIDIncludingOrphans(t *testing.T) {
	f := fold(
		mk(t, "agent.tool_result", 50, `"tool_use_id":"e300",`+sessiontest.Text("early")),
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.tool_use", 200, sessiontest.Tool("bash", `{}`, "")),
		mk(t, "agent.tool_use", 300, sessiontest.Tool("bash", `{}`, "")),
		mk(t, "agent.mcp_tool_use", 400, sessiontest.Tool("q", `{}`, `"mcp_server_name":"db"`)),
		mk(t, "agent.custom_tool_use", 500, sessiontest.Tool("mine", `{}`, "")),
		mk(t, "agent.mcp_tool_result", 600, `"mcp_tool_use_id":"e400","is_error":true`),
		mk(t, "user.custom_tool_result", 700, `"custom_tool_use_id":"e500"`),
		mk(t, "agent.tool_result", 800, `"tool_use_id":"e200"`),
	)
	require.Equal(t, []string{"turn"}, kinds(f))
	calls := f.Nodes()[0].Blocks[0].Calls
	require.Len(t, calls, 4)
	require.Equal(t, "e800", calls[0].Result.ID)
	require.Equal(t, "e50", calls[1].Result.ID, "orphan result adopted when its use arrives")
	require.Equal(t, Failed, calls[2].Lifecycle())
	require.Equal(t, Completed, calls[3].Lifecycle())
}

func TestLifecycle(t *testing.T) {
	cases := []struct {
		name string
		use  string
		rest []Event
		want Lifecycle
	}{
		{"plain running", `"name":"bash"`, nil, Running},
		{"ask unanswered", `"name":"bash","evaluated_permission":"ask"`, nil, AwaitingApproval},
		{"ask allowed", `"name":"bash","evaluated_permission":"ask"`,
			[]Event{mk(t, "user.tool_confirmation", 300, `"tool_use_id":"e200","result":"allow"`)}, Running},
		{"ask denied by user", `"name":"bash","evaluated_permission":"ask"`,
			[]Event{mk(t, "user.tool_confirmation", 300, `"tool_use_id":"e200","result":"deny","deny_message":"no"`)}, Denied},
		{"denied by policy beats result", `"name":"bash","evaluated_permission":"deny"`,
			[]Event{mk(t, "agent.tool_result", 300, `"tool_use_id":"e200"`)}, Denied},
		{"completed", `"name":"bash"`, []Event{mk(t, "agent.tool_result", 300, `"tool_use_id":"e200"`)}, Completed},
		{"failed", `"name":"bash"`, []Event{mk(t, "agent.tool_result", 300, `"tool_use_id":"e200","is_error":true`)}, Failed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := append([]Event{mk(t, "span.model_request_start", 100, ""), mk(t, "agent.tool_use", 200, tc.use)}, tc.rest...)
			require.Equal(t, tc.want, fold(evs...).Nodes()[0].Blocks[0].Calls[0].Lifecycle())
		})
	}
}

func TestTurnCloseRulesAndMarkers(t *testing.T) {
	msg := sessiontest.Text("x")
	f := fold(
		mk(t, "user.message", 50, msg),
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, msg),
		mk(t, "user.interrupt", 300, ""),
		mk(t, "agent.message", 400, msg), // an agent event trailing a boundary opens a span-less turn
		mk(t, "span.model_request_start", 500, ""),
		mk(t, "agent.message", 600, msg),
		mk(t, "user.message", 700, msg),
		mk(t, "span.model_request_start", 800, ""), // no blocks before the next boundary: dropped
		mk(t, "session.status_rescheduled", 900, ""),
		mk(t, "user.define_outcome", 1000, `"description":"ship it"`),
		mk(t, "agent.thread_context_compacted", 1100, ""),
		mk(t, "session.status_terminated", 1200, ""),
	)
	require.Equal(t, []string{"user", "turn", "interrupted", "turn", "turn", "user", "rescheduled", "outcome_defined", "unknown", "terminated"}, kinds(f))
	require.True(t, turns(f)[0].Open, "interrupted before its span ended")
	require.Empty(t, turns(f)[1].Event.ID, "span-less turn")
	require.Equal(t, "terminated", f.Status().State)
	require.Equal(t, "ship it", EventText(f.Nodes()[7].Event))
}

// A span end is filed under the turn its start opened, even after a boundary
// closed that turn; an end naming a start we never saw is dropped by a span-ful
// turn and adopted only by a span-less one (its start fell outside the replay).
func TestSpanEndFindsItsOwnTurn(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, sessiontest.Text("x")),
		mk(t, "user.interrupt", 300, ""),
		mk(t, "span.model_request_start", 400, ""),
		mk(t, "agent.message", 500, sessiontest.Text("y")),
		mk(t, "span.model_request_end", 600, usageJSON), // names e100
		mk(t, "span.model_request_end", 700, `"model_request_start_id":"e999"`),
	)
	ts := turns(f)
	require.Equal(t, []string{"text", "request_end"}, blockKinds(ts[0]))
	require.False(t, ts[0].Open)
	require.Equal(t, []string{"text"}, blockKinds(ts[1]))
	require.True(t, ts[1].Open, "an end for an unknown start is not adopted by a span-ful turn")
	require.Equal(t, Usage{Input: 111, Output: 5}, f.TotalUsage())

	g := fold(mk(t, "agent.message", 200, sessiontest.Text("x")), mk(t, "span.model_request_end", 300, `"model_request_start_id":"e1"`))
	require.False(t, turns(g)[0].Open, "a span-less turn adopts an end whose start is absent")
}

func TestThreadsSentAndReceived(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.thread_message_sent", 200, `"to_session_thread_id":"th_r","to_agent_name":"reviewer",`+sessiontest.Text("please review")),
		mk(t, "span.model_request_end", 300, `"model_request_start_id":"e100"`),
		mk(t, "agent.thread_message_received", 400, `"from_session_thread_id":"th_r","from_agent_name":"reviewer",`+sessiontest.Text("LGTM")),
	)
	require.Equal(t, []string{"turn", "thread_received"}, kinds(f))
	require.Equal(t, []string{"thread_sent", "request_end"}, blockKinds(f.Nodes()[0]))
	recv := f.Nodes()[1]
	require.NotNil(t, recv.Brief)
	require.Equal(t, "please review", EventText(*recv.Brief))
	require.Equal(t, "LGTM", EventText(recv.Event))
}

func TestOutcomeAndErrors(t *testing.T) {
	f := fold(
		mk(t, "session.error", 50, `"error":{"type":"unknown_error","message":"boom"}`),
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "session.error", 200, `"error":{"type":"model_overloaded_error","message":"529"}`),
		mk(t, "span.outcome_evaluation_start", 300, `"outcome_id":"o1","iteration":1`),
		mk(t, "span.outcome_evaluation_ongoing", 400, `"outcome_id":"o1","iteration":1`),
		mk(t, "span.outcome_evaluation_end", 500, `"outcome_evaluation_start_id":"e300","result":"satisfied","explanation":"all good"`),
	)
	require.Equal(t, []string{"error", "turn", "outcome"}, kinds(f))
	require.Equal(t, "boom", EventText(f.Nodes()[0].Event))
	require.Equal(t, []string{"error"}, blockKinds(f.Nodes()[1]))
	out := f.Nodes()[2]
	require.Equal(t, "e300", out.Event.ID)
	require.Equal(t, "satisfied", out.OutcomeEnd.Result)
	require.Equal(t, "all good", EventText(*out.OutcomeEnd))
}

func TestIdleWindowThreshold(t *testing.T) {
	for _, gap := range []int{29_000, 30_000} {
		f := fold(
			mk(t, "session.status_idle", 1000, `"stop_reason":{"type":"end_turn"}`),
			mk(t, "session.status_running", 1000+gap, ""),
		)
		want := []string(nil)
		if gap >= 30_000 {
			want = []string{"idle"}
		}
		require.Equal(t, want, kinds(f), "gap %dms", gap)
		for _, n := range f.Nodes() {
			if n.Kind == NodeIdle {
				require.Equal(t, epoch.Add(time.Second), n.IdleFrom)
				require.Equal(t, epoch.Add(31*time.Second), n.IdleTo)
			}
		}
	}
}

func TestMachineryNeverSplitsATurn(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, sessiontest.Text("x")),
		mk(t, "session.usage", 250, ""),
		mk(t, "span.model_request_end", 300, `"model_request_start_id":"e100"`),
		mk(t, "session.updated", 400, `"title":"t"`),
		mk(t, "session.thread_status_idle", 450, `"session_thread_id":"th"`),
		mk(t, "agent.tool_use", 500, sessiontest.Tool("bash", `{}`, "")),
		mk(t, "session.status_idle", 600, `"stop_reason":{"type":"end_turn"}`),
	)
	require.Equal(t, []string{"turn"}, kinds(f))
	require.Equal(t, []string{"text", "request_end", "tools"}, blockKinds(f.Nodes()[0]))
	require.Equal(t, NodeSilent, f.Nodes()[1].Kind, "a status change past the span end still surfaces for verbose")
}

func TestQueuedAndStreaming(t *testing.T) {
	f := fold(mk(t, "span.model_request_start", 100, ""), pending(t, "agent.thinking", "th", ""))
	f.Upsert(mk(t, "user.message", 200, sessiontest.Text("q")))
	f.Upsert(mk(t, "agent.thread_message_sent", 300, `"to_session_thread_id":"th_r",`+sessiontest.Text("hi")))
	f.Upsert(pending(t, "user.message", "u1", sessiontest.Text("next")))
	require.Equal(t, []string{"turn", "user", "turn", "user"}, kinds(f), "thread_message_sent outside a turn opens a span-less one")
	require.True(t, f.Nodes()[0].Blocks[0].Streaming())
	require.False(t, f.Nodes()[0].Queued(), "Queued is user-only")
	require.False(t, f.Nodes()[1].Queued())
	require.False(t, f.Nodes()[2].Blocks[0].Streaming())
	require.True(t, f.Nodes()[3].Queued())
}

// Nodes processed while a sent message is still queued file ahead of it, and
// the message takes its place once ingested: the fold a Reset would produce.
func TestQueuedUserTailStaysLast(t *testing.T) {
	evs := []Event{
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, sessiontest.Text("working")),
		mk(t, "agent.tool_use", 300, sessiontest.Tool("bash", `{"command":"ls"}`, "")),
		mk(t, "span.model_request_end", 400, usageJSON),
		mk(t, "agent.tool_result", 500, `"tool_use_id":"e300"`),
		mk(t, "user.message", 600, sessiontest.Text("one")),
	}
	u1, u2 := pending(t, "user.message", "e600", sessiontest.Text("one")), pending(t, "user.interrupt", "u2", "")

	f := fold(evs[:2]...)
	f.Upsert(u1)
	f.Upsert(u2)
	f.TakeDirty()
	for _, e := range evs[2:5] {
		f.Upsert(e)
	}
	require.Equal(t, 0, f.TakeDirty(), "an insert ahead of the queued tail redraws from there")
	require.Equal(t, []string{"turn", "user", "interrupted"}, kinds(f))
	require.Equal(t, []string{"text", "tools", "request_end"}, blockKinds(f.Nodes()[0]), "the queued tail does not close the turn")
	require.Equal(t, Completed, f.Nodes()[0].Blocks[1].Calls[0].Lifecycle())
	require.True(t, f.Nodes()[1].Queued())

	f.Upsert(evs[5])
	require.Equal(t, []string{"turn", "user", "interrupted"}, kinds(f))
	require.False(t, f.Nodes()[1].Queued())

	re := NewFold()
	re.Reset(append(evs, u2))
	require.Equal(t, re.Nodes(), f.Nodes())
	require.Equal(t, re.TotalUsage(), f.TotalUsage())
}

func TestStatusAndPendingShrinkAsConfirmationsArrive(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.tool_use", 200, sessiontest.Tool("bash", `{"command":"git push"}`, `"evaluated_permission":"ask"`)),
		mk(t, "agent.tool_use", 300, sessiontest.Tool("bash", `{"command":"rm -rf x"}`, `"evaluated_permission":"ask"`)),
		mk(t, "span.model_request_end", 400, `"model_request_start_id":"e100"`),
		mk(t, "session.status_idle", 500, `"stop_reason":{"type":"requires_action","event_ids":["e200","e300"]}`),
	)
	s := f.Status()
	require.Equal(t, "idle", s.State)
	require.Equal(t, "requires_action", s.StopReason)
	require.Equal(t, []string{"e200", "e300"}, useIDs(s.Pending))
	require.Equal(t, "git push", ToolPreview(s.Pending[0].Use))

	f.Upsert(mk(t, "user.tool_confirmation", 600, `"tool_use_id":"e200","result":"allow"`))
	require.Equal(t, []string{"e300"}, useIDs(f.Status().Pending))
	require.Equal(t, Running, turns(f)[0].Blocks[0].Calls[0].Lifecycle(), "confirmation reaches a call in an already-closed turn")

	f.Upsert(mk(t, "user.tool_confirmation", 700, `"tool_use_id":"e300","result":"deny"`))
	f.Upsert(mk(t, "session.status_running", 800, ""))
	require.Equal(t, Status{State: "running"}, f.Status())

	f.Upsert(mk(t, "session.deleted", 900, ""))
	require.Equal(t, "deleted", f.Status().State)
}

func TestUpsertRefinesStreamingMessageInPlace(t *testing.T) {
	f := fold(mk(t, "span.model_request_start", 100, ""))
	f.Upsert(pending(t, "agent.message", "m1", sessiontest.Text("He")))
	f.Upsert(pending(t, "agent.message", "m1", sessiontest.Text("Hello wor")))
	require.Equal(t, []string{"turn"}, kinds(f))
	turn := f.Nodes()[0]
	require.Equal(t, []string{"text"}, blockKinds(turn))
	require.Equal(t, "Hello wor", EventText(turn.Blocks[0].Event))
	require.True(t, turn.Blocks[0].Streaming())

	final := mk(t, "agent.message", 200, sessiontest.Text("Hello world."))
	final.ID = "m1"
	f.Upsert(final)
	turn = f.Nodes()[0]
	require.Len(t, turn.Blocks, 1)
	require.Equal(t, "Hello world.", EventText(turn.Blocks[0].Event))
	require.False(t, turn.Blocks[0].Streaming())

	f.Upsert(pending(t, "user.message", "u1", sessiontest.Text("next"))) // unknown id ⇒ appended
	require.Equal(t, []string{"turn", "user"}, kinds(f))
	require.True(t, f.Nodes()[1].Queued())
}

func TestCanonicalOutlineResetEquivalenceAndTotalUsage(t *testing.T) {
	events := sessiontest.Canonical(t)
	f := fold(events...)
	require.Equal(t, []string{"user", "turn", "turn", "turn"}, kinds(f))
	ts := turns(f)
	require.Equal(t, []string{"thinking", "text", "tools", "request_end"}, blockKinds(ts[0]))
	require.Equal(t, []string{"thinking", "text", "tools", "request_end"}, blockKinds(ts[1]))
	require.Equal(t, []string{"text", "tools", "request_end"}, blockKinds(ts[2]))
	require.Len(t, ts[1].Blocks[2].Calls, 3)
	long := ts[1].Blocks[2].Calls[2]
	require.Equal(t, 12800*time.Millisecond, long.Result.ProcessedAt.Sub(long.Use.ProcessedAt))

	gate := f.Status().Pending
	require.Len(t, gate, 1)
	require.Equal(t, AwaitingApproval, gate[0].Lifecycle())
	require.Equal(t, "git push -u origin fix/retry-jitter", ToolPreview(gate[0].Use))
	require.Equal(t, Usage{Input: 9800, Output: 1100}, f.TotalUsage())

	// A live fold sees the streaming preview refined by Upserts; a refold sees only finals.
	final := mk(t, "agent.message", 29050, sessiontest.Text("streamed"))
	final.ID = "m1"
	f.Upsert(mk(t, "span.model_request_start", 29040, ""))
	f.Upsert(pending(t, "agent.message", "m1", sessiontest.Text("str")))
	f.Upsert(final)
	events = append(events, mk(t, "span.model_request_start", 29040, ""), final)

	re := NewFold()
	re.Upsert(mk(t, "user.message", 1, ""))
	re.Reset(events)
	require.Equal(t, f.Nodes(), re.Nodes())
	require.Equal(t, f.Status(), re.Status())
	require.Equal(t, f.TotalUsage(), re.TotalUsage())
}
