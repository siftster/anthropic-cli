package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

var (
	result   = sessiontest.Result
	testOpts = renderOpts{Width: 100, Agent: "coder", SpinnerFrame: "⠙", Now: sessiontest.Epoch.Add(time.Hour)}
)

// renderEvents folds evs and renders them from scratch at testOpts.
func renderEvents(t *testing.T, verbose bool, evs ...timeline.Event) string {
	t.Helper()
	fold := timeline.NewFold()
	fold.Reset(evs)
	opts := testOpts
	opts.Verbose = verbose
	out, _ := new(transcript).render(fold.Nodes(), 0, opts)
	return trimLines(out)
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

func golden(t *testing.T, want, got string) {
	t.Helper()
	want = trimLines(strings.Trim(want, "\n"))
	got = trimLines(strings.Trim(got, "\n"))
	if want != got {
		t.Fatalf("render mismatch\n--- want\n%s\n--- got\n%s", want, got)
	}
}

const canonicalGolden = `
 ● User
 │ tests/test_retry.py::test_backoff_jitter is flaky on CI — can you figure out why and fix it?

 ● coder
 │ Thought for 4s
 │ I'll start by reading the test and the retry implementation it exercises.
 │ ▸ ⌕ grep  test_backoff_jitter                                                             0.4s
 │ ▸ ≡ read  tests/test_retry.py                                                             0.4s
 │ ▸ ≡ read  src/httpclient/retry.py                                                         0.4s

 │ Thought for 7s
 │ The test seeds random, but retry.py derives jitter from time.monotonic(), so the asserted sleep
 │ sequence depends on wall-clock. I'll inject an RNG and seed it from the test instead.
 │ ▸ ✎ edit  src/httpclient/retry.py                                                         0.4s
 │ ▸ ✎ edit  tests/test_retry.py                                                             0.4s
 │ ▸ $ bash  Run the retry test 50× to check for flakes                                     12.8s

 │ All 50 runs pass locally. I'd like to push the branch so CI can confirm on Linux runners too.
 │ ▾ $ bash  git push -u origin fix/retry-jitter                              ⠙ awaiting approval
 │ │ $ git push -u origin fix/retry-jitter`

func TestCanonicalSessionMatchesGolden(t *testing.T) {
	golden(t, canonicalGolden, renderEvents(t, false, sessiontest.Canonical(t)...))
}

// TestIncrementalRenderMatchesFresh feeds the canonical fixture one event per
// redraw so every node is served from kept rows at some point, then streams into it.
func TestIncrementalRenderMatchesFresh(t *testing.T) {
	f, tr := timeline.NewFold(), new(transcript)
	var got string
	animating := false
	for _, e := range sessiontest.Canonical(t) {
		f.Upsert(e)
		got, animating = tr.render(f.Nodes(), f.TakeDirty(), testOpts)
	}
	golden(t, canonicalGolden, got)
	require.True(t, animating, "awaiting approval spins")

	f.Upsert(mk(t, "user.tool_confirmation", 29000, `"tool_use_id":"e28100","result":"allow"`))
	f.Upsert(mk(t, "agent.tool_result", 29100, result(28100, "pushed", false)))
	f.Upsert(sessiontest.Pending(t, "agent.message", "m1", text("Pushed. CI")))
	got, animating = tr.render(f.Nodes(), f.TakeDirty(), testOpts)
	require.False(t, animating)
	require.Equal(t, renderEvents(t, false, append(sessiontest.Canonical(t),
		mk(t, "user.tool_confirmation", 29000, `"tool_use_id":"e28100","result":"allow"`),
		mk(t, "agent.tool_result", 29100, result(28100, "pushed", false)),
		sessiontest.Pending(t, "agent.message", "m1", text("Pushed. CI")))...), trimLines(got))
	require.Contains(t, got, "Pushed. CI▍")

	verbose := testOpts
	verbose.Verbose = true
	got, _ = tr.render(f.Nodes(), f.TakeDirty(), verbose)
	require.Contains(t, got, "│ │ Allowed", "an option change redraws every node")
}

func TestVerboseBodiesAndUsagePill(t *testing.T) {
	got := renderEvents(t, true, sessiontest.Canonical(t)...)
	require.Contains(t, got, `
 │ ▾ ✎ edit  src/httpclient/retry.py                                                         0.4s
 │ │ src/httpclient/retry.py
 │ │ -a
 │ │ +b
`)
	require.Contains(t, got, `
 │ ▾ $ bash  Run the retry test 50× to check for flakes                                     12.8s
 │ │ $ for i in $(seq 50); do pytest -q tests/test_retry.py::test_backoff_jitter || exit 1; done
 │ │ 1 passed in 0.24s
`)
	require.Contains(t, got, "\n · session.status_running")
	require.Contains(t, got, "                          ↑3.4k ↓400\n")
	for kind, glyph := range map[string]string{"bash": "$", "computer_bash": "$", "str_replace_editor": "✎", "glob": "⁂", "web_fetch": "⊕", "repl": "⚙"} {
		require.Equal(t, glyph, toolGlyph(kind), kind)
	}
}

func TestMarkersThreadsAndErrors(t *testing.T) {
	m := 60_000
	evs := []timeline.Event{
		mk(t, "user.interrupt", 100, ""),
		mk(t, "session.status_idle", 200, `"stop_reason":{"type":"end_turn"}`),
		mk(t, "session.status_running", 200+14*m+3000, ""),
		mk(t, "user.message", 15*m, text("Skip the PR for now. Ask the reviewer agent to sanity-check the diff first.")),
		mk(t, "span.model_request_start", 15*m+100, ""),
		mk(t, "agent.thinking", 15*m+2100, ""),
		mk(t, "agent.thread_message_sent", 15*m+2200, `"to_agent_name":"reviewer","to_session_thread_id":"th_r",`+text("Please review the diff on fix/retry-jitter (retry.py, test_retry.py) for correctness.")),
		mk(t, "agent.tool_use", 15*m+2300, tool("edit", `{"file_path":"CHANGELOG.md","old_str":"x","new_str":"y"}`, "")),
		mk(t, "agent.tool_result", 15*m+2500, result(15*m+2300, "old_str not found in CHANGELOG.md\n(no changes)", true)),
		mk(t, "agent.tool_use", 15*m+2600, tool("read", `{"file_path":"CHANGELOG.md"}`, "")),
		mk(t, "agent.tool_result", 15*m+2700, result(15*m+2600, "…", false)),
		mk(t, "span.model_request_end", 15*m+2800, fmt.Sprintf(`"model_request_start_id":"e%d"`, 15*m+100)),
		mk(t, "agent.thread_message_received", 16*m, `"from_agent_name":"reviewer","from_session_thread_id":"th_r",`+text("Logic is right. One nit: clamp max_jitter to ≥ 0 in __init__ so a negative config can't produce negative sleeps. Otherwise LGTM.")),
		mk(t, "session.error", 16*m+100, `"error":{"type":"api_error","message":"Upstream model overloaded (529). Retrying with backoff."}`),
		mk(t, "session.status_rescheduled", 16*m+200, ""),
		mk(t, "span.model_request_start", 16*m+300, ""),
		mk(t, "agent.message", 16*m+400, text("Applied the clamp and updated the changelog.")),
		mk(t, "span.model_request_end", 16*m+500, fmt.Sprintf(`"model_request_start_id":"e%d"`, 16*m+300)),
		mk(t, "session.status_terminated", 17*m, ""),
	}
	golden(t, `
                                            Interrupted

 ╌╌ Session idle · 14m 03s ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌

 ● User
 │ Skip the PR for now. Ask the reviewer agent to sanity-check the diff first.

 ● coder
 │ Thought for 2s
 │ → @reviewer  Please review the diff on fix/retry-jitter (retry.py, test_retry.py) for
 │              correctness.
 │ ▸ ✎ edit  CHANGELOG.md                                                             Error  0.2s
 │  Error  old_str not found in CHANGELOG.md
 │ ▸ ≡ read  CHANGELOG.md                                                                    0.1s

 ● reviewer ▸  re: “Please review the diff on fix/retry-jitter (retry.py, test_…”
 │ Logic is right. One nit: clamp max_jitter to ≥ 0 in __init__ so a negative config can't produce
 │ negative sleeps. Otherwise LGTM.

 ┌ Error  api_error  ────────────────────────────────────────────────────────────────────────────
 │ Upstream model overloaded (529). Retrying with backoff.
 └───────────────────────────────────────────────────────────────────────────────────────────────

                                        Session rescheduled

 ● coder
 │ Applied the clamp and updated the changelog.

                                         Session terminated
`, renderEvents(t, false, evs...))
}

func askBash(t *testing.T, ms int, input string) []timeline.Event {
	return []timeline.Event{
		mk(t, "span.model_request_start", ms, ""),
		mk(t, "agent.tool_use", ms+100, `"name":"bash","evaluated_permission":"ask","input":`+input),
		mk(t, "span.model_request_end", ms+200, fmt.Sprintf(`"model_request_start_id":"e%d"`, ms)),
		mk(t, "session.status_idle", ms+300, fmt.Sprintf(`"stop_reason":{"type":"requires_action","event_ids":["e%d"]}`, ms+100)),
	}
}

func TestControlSequencesNeverReachTheTerminal(t *testing.T) {
	evs := append([]timeline.Event{
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, `"content":[{"type":"text","text":`+js("done\x1b[2J **really**")+`}]`),
		mk(t, "span.model_request_end", 300, `"model_request_start_id":"e100"`),
		mk(t, "agent.thread_message_received", 400, `"content":[],"from_agent_name":`+js("re\x1b[7mviewer")),
	}, askBash(t, 1000, `{"command":`+js("rm -rf /tmp/x\r# harmless")+`}`)...)
	f := timeline.NewFold()
	f.Reset(evs)
	got, _ := new(transcript).render(f.Nodes(), 0, testOpts)
	box := approvalBox(f.Status().Pending[0], 1, 0, 100)
	for _, s := range []string{got, box} {
		require.False(t, strings.ContainsAny(s, "\x1b\r"), "%q", s)
	}
	require.Contains(t, got, "done really")
	require.Contains(t, got, "● reviewer ▸")
	require.Contains(t, box, "rm -rf /tmp/x# harmless")
}

func TestSpeakerElisionAndStreaming(t *testing.T) {
	pendingMsg := sessiontest.Pending(t, "agent.message", "s1", text("Pushed. CI run"))
	got := renderEvents(t, false,
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, text("one")),
		mk(t, "span.model_request_end", 300, `"model_request_start_id":"e100"`),
		mk(t, "session.status_running", 350, ""),
		mk(t, "span.model_request_start", 400, ""),
		mk(t, "agent.message", 500, text("two")),
		mk(t, "span.model_request_end", 600, `"model_request_start_id":"e400"`),
		mk(t, "user.message", 700, text("hi")),
		mk(t, "span.model_request_start", 800, ""),
		pendingMsg,
	)
	golden(t, `
 ● coder
 │ one

 │ two

 ● User
 │ hi

 ● coder
 │ Pushed. CI run▍
`, got)
}
