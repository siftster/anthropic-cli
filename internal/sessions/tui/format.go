package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// wrapLines word-wraps, then hard-wraps what has no spaces; both passes are
// ANSI-aware so styled text keeps its width. Always at least one line.
func wrapLines(s string, width int) []string {
	width = max(width, 1)
	// No hyphen breakpoints: reflow keeps the hyphen after deciding the fit,
	// leaving the line one over and the hard wrap then orphans a letter.
	ww := wordwrap.NewWriter(width)
	ww.Breakpoints = nil
	ww.KeepNewlines = true
	_, _ = ww.Write([]byte(strings.TrimRight(s, "\n")))
	_ = ww.Close()
	return strings.Split(wrap.String(ww.String(), width), "\n")
}

// spread joins left and right so right ends at column width.
func spread(left, right string, width int) string {
	if right == "" {
		return left
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", max(gap, 1)) + right
}

// clip cuts s to at most width cells, ending in "…" when anything was cut.
func clip(s string, width int) string { return truncate.StringWithTail(s, uint(max(width, 1)), "…") }

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// oneLine collapses every run of whitespace, newlines included, to one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// fmtDuration: 0.3s · 12.8s · 2m03s · 1h02m.
func fmtDuration(d time.Duration) string { return fmtDurationSep(d, "") }

// fmtDurationSep puts sep between the two units: "14m 03s", "1h 02m".
func fmtDurationSep(d time.Duration, sep string) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%s%02ds", int(d.Minutes()), sep, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%s%02dm", int(d.Hours()), sep, int(d.Minutes())%60)
}

// fmtTokens: 987 · 1.2k · 48.1k · 1.3M.
func fmtTokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

// fmtUsage shows input and output tokens: "↑1.2k ↓348".
func fmtUsage(usage timeline.Usage) string {
	return "↑" + fmtTokens(usage.Input) + " ↓" + fmtTokens(usage.Output)
}
