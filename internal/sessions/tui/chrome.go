package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// statusLine is the top bar: title · agent · status on the left, connection
// (and token usage when usage is non-nil) on the right, on the bar background.
func statusLine(title, agent string, status timeline.Status, connState string, usage *timeline.Usage, spinner string, width int) string {
	on := func(style lipgloss.Style, text string) string { return style.Background(colBarBg).Render(text) }
	var state string
	switch {
	case status.State == "running":
		state = on(stAgent, spinner+" running")
	case status.StopReason == "requires_action" && len(status.Pending) > 0:
		state = on(stWarnBold, "! awaiting approval")
	case status.StopReason == "requires_action":
		state = on(stDim, "○ waiting on tool result")
	case status.StopReason == "retries_exhausted":
		state = on(stErr, "! retries exhausted")
	case status.State == "idle":
		state = on(stDim, "○ idle")
	case status.State == "rescheduling":
		state = on(stDim, "◌ rescheduling")
	case status.State == "":
		state = on(stDim, "…")
	default:
		state = on(stPlain, "■ "+status.State)
	}
	sep := on(stDim, " · ")
	left := on(stPlain, " ") + on(stBold, clip(title, width/2)) + sep + on(stAgent, agent) + sep + state
	connStyle := stWarn
	if connState == "live" {
		connStyle = stOK
	}
	right := on(connStyle, connState)
	if usage != nil {
		right += sep + on(stSpan, fmtUsage(*usage))
	}
	right += on(stPlain, " ")
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + on(stPlain, strings.Repeat(" ", gap)) + right
}

// hintLine is the bottom row: mode-specific keys on the left (or a transient
// flash message), detach hard right so the longest key list still fits 100
// columns.
func hintLine(current mode, verbose, running bool, flash string, width int) string {
	detail, scroll := stDim.Render("Ctrl-O detail"), ""
	if verbose {
		detail = stBold.Render("Ctrl-O detail: on")
	}
	if width >= 100 {
		scroll = "PgUp/PgDn scroll · "
	}
	var left string
	switch current {
	case modeApproval:
		left = stDim.Render("↑/↓ select · Enter confirm · y allow · n deny · m deny with reason")
	case modeDenyReason:
		left = stDim.Render("Enter deny with this reason · Esc back to options")
	case modeClosed:
		left = stDim.Render("Session closed · "+scroll) + detail
	default:
		keys := "Enter send · Alt-Enter newline · "
		if running {
			keys += "Esc interrupt · "
		}
		left = stDim.Render(keys+scroll) + detail
	}
	if flash != "" {
		left = stErr.Render(flash)
	}
	return spread("   "+left, stDim.Render("Ctrl-C detach"), width-1)
}

// metaLine is the footer: session id left, Console link right. The link is
// clipped from the left so its distinctive tail survives a narrow terminal.
func metaLine(sessionID, consoleURL string, width int) string {
	left := "   " + stDim.Render(sessionID)
	room := width - 1 - lipgloss.Width(left) - 2
	if consoleURL == "" || room < 12 {
		return left
	}
	if w := lipgloss.Width(consoleURL); w > room {
		consoleURL = "…" + string([]rune(consoleURL)[w-room+1:])
	}
	return spread(left, stDim.Render(consoleURL), width-1)
}
