package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

func TestComposerSendsInterruptsAndTogglesDetail(t *testing.T) {
	m, fs := newTestModel(t)

	require.Nil(t, press(m, tea.KeyEnter), "empty composer sends nothing")
	typeText(m, "  hello ")
	run(m, press(m, tea.KeyEnter))
	require.Equal(t, []string{"hello"}, fs.sent)
	require.Empty(t, m.composer.Value())

	typeText(m, "draft")
	press(m, tea.KeyEscape)
	require.Empty(t, m.composer.Value(), "Esc clears while idle")
	require.Zero(t, fs.interrupts)

	push(m, mk(t, "session.status_running", 100, ""))
	run(m, press(m, tea.KeyEscape))
	require.Equal(t, 1, fs.interrupts)

	press(m, tea.KeyCtrlO)
	require.True(t, m.verbose)

	fs.fail = errors.New("boom")
	typeText(m, "retry me")
	run(m, press(m, tea.KeyEnter))
	require.Equal(t, "retry me", m.composer.Value(), "failed send restores the draft")
	require.Contains(t, m.View(), "boom")
}

func TestComposerGrowthAndCtrlD(t *testing.T) {
	m, _ := newTestModel(t)
	vh := m.viewport.Height
	typeText(m, "line one")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	typeText(m, "line two")
	press(m, tea.KeyCtrlJ)
	require.Equal(t, "line one\nline two\n", m.composer.Value())
	require.Equal(t, 3, m.composer.Height())
	require.Equal(t, vh-2, m.viewport.Height)
	require.Contains(t, m.View(), " > line one")

	require.Nil(t, press(m, tea.KeyCtrlD), "Ctrl-D edits while there is text")
	press(m, tea.KeyEscape)
	require.Equal(t, 1, m.composer.Height())
	_, isQuit := press(m, tea.KeyCtrlD)().(tea.QuitMsg)
	require.True(t, isQuit)
}

func TestPasteResizesViewport(t *testing.T) {
	m, _ := newTestModel(t)
	vh := m.viewport.Height
	typeText(m, "a\nb\nc\nd\ne\nf\ng")
	require.Equal(t, composerMaxRows, m.composer.Height())
	require.Equal(t, vh-(composerMaxRows-1), m.viewport.Height)
	require.Equal(t, m.height, lipgloss.Height(m.View()))
}

func pendingApproval(t *testing.T, n int) []timeline.Event {
	evs := []timeline.Event{mk(t, "span.model_request_start", 100, "")}
	ids := ""
	for i := range n {
		evs = append(evs, mk(t, "agent.tool_use", 200+i, tool("bash", fmt.Sprintf(`{"command":"cmd%d"}`, i), `"evaluated_permission":"ask"`)))
		ids += fmt.Sprintf(`,"e%d"`, 200+i)
	}
	return append(evs,
		mk(t, "span.model_request_end", 300, `"model_request_start_id":"e100"`),
		mk(t, "session.status_idle", 400, `"stop_reason":{"type":"requires_action","event_ids":[`+ids[1:]+`]}`))
}

func TestApprovalAllowAndFailure(t *testing.T) {
	m, fs := newTestModel(t)
	push(m, pendingApproval(t, 1)...)
	require.Equal(t, modeApproval, m.mode)
	require.Contains(t, m.View(), "Allow tool call?")
	require.Contains(t, m.View(), "! awaiting approval")

	typeText(m, "x")
	require.Empty(t, m.composer.Value(), "composer is inert while confirming")
	press(m, tea.KeyEscape)
	require.Equal(t, modeApproval, m.mode, "Esc is a no-op in the list")

	elapse(fs, time.Second)
	fs.fail = errors.New("502")
	run(m, typeText(m, "Y"))
	require.Equal(t, []string{"e200 allow=true "}, fs.confirms)
	require.Equal(t, modeApproval, m.mode, "failed verdict leaves the call pending")
	require.Contains(t, m.View(), "502")

	fs.fail = nil
	run(m, typeText(m, "y"))
	require.Equal(t, modeCompose, m.mode, "the queued verdict advances the box before the echo")
	require.NotContains(t, m.View(), "! awaiting approval")
	push(m, mk(t, "user.tool_confirmation", 500, `"tool_use_id":"e200","result":"allow"`))
	require.Equal(t, modeCompose, m.mode)
}

func TestApprovalDenyWithReasonAndCounter(t *testing.T) {
	m, fs := newTestModel(t)
	push(m, pendingApproval(t, 2)...)
	require.Contains(t, m.View(), "1 of 2")

	elapse(fs, time.Second)
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	run(m, press(m, tea.KeyEnter))
	require.Equal(t, modeDenyReason, m.mode)
	press(m, tea.KeyEscape)
	require.Equal(t, modeApproval, m.mode)

	elapse(fs, time.Second)
	typeText(m, "3")
	typeText(m, "nope")
	run(m, press(m, tea.KeyEnter))
	require.Equal(t, []string{"e200 allow=false nope"}, fs.confirms)
	require.Equal(t, modeApproval, m.mode, "second call still pending")
	require.Contains(t, m.View(), "1 of 1")
	require.Contains(t, m.View(), "cmd1")
}

func TestDenyReasonTargetResolvedElsewhere(t *testing.T) {
	m, fs := newTestModel(t)
	push(m, pendingApproval(t, 2)...)
	elapse(fs, time.Second)
	typeText(m, "m")
	typeText(m, "because")
	require.Equal(t, "e200", m.approvalFor)

	// Someone else answers e200 while the reason is being typed.
	push(m, mk(t, "user.tool_confirmation", 500, `"tool_use_id":"e200","result":"allow"`))
	require.Equal(t, modeApproval, m.mode)
	require.Empty(t, m.composer.Value(), "stale reason must not leak into the composer")
	require.Contains(t, m.View(), "cmd1")
}

func TestApprovalKeyGuardAndDraft(t *testing.T) {
	m, fs := newTestModel(t)
	typeText(m, "hello")
	push(m, pendingApproval(t, 1)...)
	require.Equal(t, modeApproval, m.mode)
	require.Empty(t, m.composer.Value())

	require.Nil(t, typeText(m, "y"), "a key already in flight when the prompt appeared answers nothing")
	elapse(fs, 300*time.Millisecond)
	require.Nil(t, press(m, tea.KeyEnter))
	press(m, tea.KeyDown)
	require.Equal(t, 1, m.selected, "selection still moves inside the guard")
	press(m, tea.KeyUp)
	require.Empty(t, fs.confirms)

	elapse(fs, 400*time.Millisecond)
	run(m, typeText(m, "y"))
	require.Equal(t, []string{"e200 allow=true "}, fs.confirms)
	require.Equal(t, modeCompose, m.mode)
	require.Equal(t, "hello", m.composer.Value(), "the draft survives the prompt")

	// A send that failed while the prompt was up comes back to the composer too.
	m, fs = newTestModel(t)
	push(m, pendingApproval(t, 1)...)
	m.Update(sendFailedMsg{err: errors.New("502"), draft: "unsent"})
	elapse(fs, time.Second)
	run(m, typeText(m, "n"))
	require.Equal(t, "unsent", m.composer.Value())
}

func TestVerdictTargetsTheDisplayedCall(t *testing.T) {
	m, fs := newTestModel(t)
	push(m, pendingApproval(t, 2)...)
	require.Equal(t, "e200", m.approvalFor)
	elapse(fs, time.Second)

	// e200 is answered elsewhere: the prompt re-aims at e201 and re-arms its guard.
	push(m, mk(t, "user.tool_confirmation", 500, `"tool_use_id":"e200","result":"allow"`))
	require.Equal(t, "e201", m.approvalFor)
	require.Contains(t, m.View(), "cmd1")
	require.Nil(t, typeText(m, "y"), "a key meant for e200 cannot land on e201")
	elapse(fs, time.Second)
	run(m, typeText(m, "y"))
	require.Equal(t, []string{"e201 allow=true "}, fs.confirms)
}

func TestApprovalBox(t *testing.T) {
	f := timeline.NewFold()
	f.Reset(sessiontest.Canonical(t))
	status := f.Status()
	require.Len(t, status.Pending, 1)

	golden(t, `
 Allow tool call?  $ bash  git push -u origin fix/retry-jitter                             1 of 1
 ❯ 1. Yes                                   y
   2. No                                    n
   3. No, and tell the agent why…           m
`, trimLines(approvalBox(status.Pending[0], 1, 0, 100)))
}

func TestApprovalShowsPayloadInFull(t *testing.T) {
	evs := askBash(t, 100, `{"description":"Tidy up","command":"rm -rf build"}`)
	got := renderEvents(t, false, evs...)
	require.Contains(t, got, "▾ $ bash  rm -rf build ")
	require.NotContains(t, got, "Tidy up")
	f := timeline.NewFold()
	f.Reset(evs)
	require.Contains(t, approvalBox(f.Status().Pending[0], 1, 0, 100), "$ bash  rm -rf build ")

	var script []string
	for i := range 40 {
		script = append(script, fmt.Sprintf("step %02d", i))
	}
	evs = askBash(t, 100, `{"command":`+js(strings.Join(script, "\n"))+`}`)
	require.Equal(t, 40, strings.Count(renderEvents(t, false, evs...), "│ │ "), "nothing is cut from a call awaiting a verdict")

	done := append(evs, mk(t, "user.tool_confirmation", 500, `"tool_use_id":"e200","result":"allow"`), mk(t, "agent.tool_result", 600, result(200, "ok", false)))
	got = renderEvents(t, true, done...)
	require.Equal(t, 13, strings.Count(got, "│ │ "), "dense caps apply once it has run")
	require.Contains(t, got, "more lines · 12 of 42 shown")
}
