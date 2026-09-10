package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// mode is what the strip between the two rules is doing.
type mode int

const (
	modeCompose    mode = iota // the composer takes keys
	modeApproval               // a tool call awaits allow or deny
	modeDenyReason             // the composer takes the reason for a deny
	modeClosed                 // the session has ended or the stream has failed
)

const (
	composerMaxRows = 6
	// approvalKeyGuard is how long a fresh (or re-aimed) approval prompt
	// ignores verdict keys, so typing meant for the composer or for the
	// previous call cannot answer it.
	approvalKeyGuard = 600 * time.Millisecond
)

// handleKey serves the keys that work in every mode, then defers to the mode.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+o":
		m.verbose = !m.verbose
		m.render()
		return nil
	case "pgup":
		m.viewport.HalfPageUp()
		return m.scrolled(nil)
	case "pgdown":
		m.viewport.HalfPageDown()
		return m.scrolled(nil)
	case "end":
		m.viewport.GotoBottom()
		return m.scrolled(nil)
	}

	switch m.mode {
	case modeClosed:
		return nil
	case modeApproval:
		return m.handleApprovalKey(msg)
	case modeDenyReason:
		return m.handleDenyReasonKey(msg)
	default:
		return m.handleComposeKey(msg)
	}
}

// scrolled re-arms follow only when the user lands back at the bottom.
func (m *model) scrolled(cmd tea.Cmd) tea.Cmd {
	m.follow = m.viewport.AtBottom()
	return cmd
}

func (m *model) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	option := -1
	switch strings.ToLower(msg.String()) {
	case "up":
		m.selected = (m.selected + len(approvalOptions) - 1) % len(approvalOptions)
	case "down":
		m.selected = (m.selected + 1) % len(approvalOptions)
	case "1", "y":
		option = optionAllow
	case "2", "n":
		option = optionDeny
	case "3", "m":
		option = optionDenyWithReason
	case "enter":
		option = m.selected
	}
	if option < 0 || m.now().Sub(m.approvalShownAt) < approvalKeyGuard {
		return nil
	}
	return m.chooseOption(option)
}

func (m *model) handleDenyReasonKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		return m.sendVerdict(m.approvalFor, false, strings.TrimSpace(m.composer.Value()))
	case "esc":
		m.clearComposer("")
		m.showApproval()
		return nil
	}
	return m.forwardToComposer(msg)
}

func (m *model) handleComposeKey(msg tea.KeyMsg) tea.Cmd {
	switch key := msg.String(); {
	case key == "enter":
		text := strings.TrimSpace(m.composer.Value())
		if text == "" {
			return nil
		}
		m.clearComposer("")
		m.follow = true
		send := func(ctx context.Context) error { return m.session.SendMessage(ctx, text) }
		return m.runSender(send, sendFailedMsg{draft: text})
	case key == "esc" && m.status.State == "running":
		return m.runSender(m.session.Interrupt, sendFailedMsg{})
	case key == "esc":
		m.clearComposer("")
		return nil
	case key == "ctrl+d" && m.composer.Value() == "":
		return tea.Quit
	}
	return m.forwardToComposer(msg)
}

func (m *model) chooseOption(option int) tea.Cmd {
	switch option {
	case optionAllow, optionDeny:
		return m.sendVerdict(m.approvalFor, option == optionAllow, "")
	}
	if !m.isPending(m.approvalFor) {
		m.showApproval()
		return resolvedElsewhere
	}
	m.mode = modeDenyReason
	m.clearComposer("reason for denying…")
	return nil
}

// sendVerdict sends the answer; live shows it as queued at once so the box
// advances before the echo. A call resolved meanwhile (elsewhere, or by the
// agent moving on) is not answered twice.
func (m *model) sendVerdict(toolUseID string, allow bool, reason string) tea.Cmd {
	stillPending := m.isPending(toolUseID)
	m.selected = 0
	m.clearComposer("")
	m.showApproval()
	if !stillPending {
		return resolvedElsewhere
	}
	confirm := func(ctx context.Context) error { return m.session.Confirm(ctx, toolUseID, allow, reason) }
	return m.runSender(confirm, sendFailedMsg{})
}

var errResolved = errors.New("approval resolved elsewhere")

func resolvedElsewhere() tea.Msg { return sendFailedMsg{err: errResolved} }

// runSender calls send off the event loop. A failure comes back as onErr with
// its err set.
func (m *model) runSender(send func(context.Context) error, onErr sendFailedMsg) tea.Cmd {
	return func() tea.Msg {
		if onErr.err = send(m.ctx); onErr.err != nil {
			return onErr
		}
		return nil
	}
}

// showApproval hands the strip to the approval prompt, aimed at the head of
// Pending. The target is latched so a keypress answers the call that was on
// screen, never whatever Pending holds by then; a prompt that is new or newly
// aimed restarts the key guard. Composer text is set aside until showComposer.
func (m *model) showApproval() {
	head := ""
	if len(m.status.Pending) > 0 {
		head = m.status.Pending[0].Use.ID
	}
	if m.mode == modeCompose {
		m.draft = m.composer.Value()
		m.clearComposer("")
	}
	if m.mode != modeApproval || head != m.approvalFor {
		m.approvalShownAt = m.now()
	}
	m.mode, m.approvalFor = modeApproval, head
}

// showComposer gives the strip back to the composer, with the draft that
// showApproval set aside.
func (m *model) showComposer() {
	m.mode = modeCompose
	m.clearComposer("")
	m.composer.SetValue(m.draft)
	m.draft = ""
	m.fitComposer()
}

func (m *model) isPending(toolUseID string) bool {
	for _, call := range m.status.Pending {
		if call.Use.ID == toolUseID {
			return true
		}
	}
	return false
}

const (
	optionAllow = iota
	optionDeny
	optionDenyWithReason
)

// approvalOptions are the choices under an approval prompt, indexed by the
// option constants; each also answers to its number and its letter.
var approvalOptions = [...]struct{ label, key string }{
	optionAllow:          {"Yes", "y"},
	optionDeny:           {"No", "n"},
	optionDenyWithReason: {"No, and tell the agent why…", "m"},
}

// approvalBox replaces the composer while calls await a verdict: a header
// naming the first of pendingCount calls and a numbered list with the
// selected option highlighted.
func approvalBox(call timeline.ToolCall, pendingCount, selected int, width int) string {
	head := " " + stWarnBold.Render("Allow tool call?") + "  " + stDim.Render(toolGlyph(call.Use.Name)) + " " +
		stBold.Render(timeline.ToolDisplayName(call.Use)) + "  "
	counter := stDim.Render(fmt.Sprintf("1 of %d", pendingCount))
	head += clip(timeline.ToolPayloadPreview(call.Use), width-6-lipgloss.Width(head)-lipgloss.Width(counter))
	rows := []string{spread(head, counter, width-3)}
	for i, option := range approvalOptions {
		label := fmt.Sprintf("%d. %s", i+1, option.label)
		line := "   " + label
		if i == selected {
			line = " " + stOK.Render("❯ "+label)
		}
		rows = append(rows, spread(line, stDim.Render(option.key), 45))
	}
	return strings.Join(rows, "\n")
}

// forwardToComposer passes msg to the textarea. Keys are withheld while it is
// not on screen; cursor blink ticks always pass so the blink chain survives a
// mode switch.
func (m *model) forwardToComposer(msg tea.Msg) tea.Cmd {
	if _, isKey := msg.(tea.KeyMsg); isKey && (m.mode == modeApproval || m.mode == modeClosed) {
		return nil
	}
	// Head-room first: if the edit adds a row while the box is exactly full,
	// textarea scrolls its first line away and growing afterwards won't bring it back.
	m.composer.SetHeight(composerMaxRows)
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.fitComposer()
	return cmd
}

func (m *model) clearComposer(placeholder string) {
	m.composer.Reset()
	m.composer.Placeholder = placeholder
	m.fitComposer()
	m.layout()
}

// fitComposer sizes the composer to its content, 1–composerMaxRows, and hands
// the viewport whatever that frees or takes. The textarea's own height is no
// guide to what the viewport was sized against: forwardToComposer inflates it
// first.
func (m *model) fitComposer() {
	rows := 0
	for _, line := range strings.Split(m.composer.Value(), "\n") {
		rows += 1 + max(0, lipgloss.Width(line)-1)/max(m.composer.Width(), 1)
	}
	rows = min(max(rows, 1), composerMaxRows)
	m.composer.SetHeight(rows)
	if rows != m.composerRows {
		m.composerRows = rows
		m.layout()
	}
}
