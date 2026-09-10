package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// Colour roles mirror the web viewer's; everything it draws as a muted caption is dim.
var (
	colUser  = lipgloss.Color("#d97a9c")
	colAgent = lipgloss.Color("#e08a5e")
	colPeer  = lipgloss.Color("#7fb0c8")
	colSpan  = lipgloss.Color("#a99bd6")
	colErr   = lipgloss.Color("#e5605a")
	colOK    = lipgloss.Color("#7cc49a")
	colWarn  = lipgloss.Color("#e2b451")
	colDim   = lipgloss.AdaptiveColor{Light: "246", Dark: "243"}
	colBarBg = lipgloss.AdaptiveColor{Light: "254", Dark: "236"}

	stPlain = lipgloss.NewStyle()
	stBold  = lipgloss.NewStyle().Bold(true)
	// No italics anywhere: terminals without an italic face fall back to
	// reverse video, which reads as a highlight.
	stEm       = lipgloss.NewStyle().Underline(true)
	stDim      = lipgloss.NewStyle().Foreground(colDim)
	stUser     = lipgloss.NewStyle().Foreground(colUser)
	stAgent    = lipgloss.NewStyle().Foreground(colAgent)
	stPeer     = lipgloss.NewStyle().Foreground(colPeer)
	stSpan     = lipgloss.NewStyle().Foreground(colSpan)
	stErr      = lipgloss.NewStyle().Foreground(colErr)
	stOK       = lipgloss.NewStyle().Foreground(colOK)
	stWarn     = lipgloss.NewStyle().Foreground(colWarn)
	stWarnBold = stWarn.Bold(true)

	// Badges carry their own padding in the text (" Error ") so they stay
	// legible when colour is off.
	stBadgeErr = lipgloss.NewStyle().Background(colErr).Foreground(lipgloss.Color("0"))
	stBadgeDim = stDim.Reverse(true)
)

// toolGlyph is the symbol that marks a tool row, chosen by timeline.ToolKind.
func toolGlyph(toolName string) string {
	if glyph, ok := toolGlyphs[timeline.ToolKind(toolName)]; ok {
		return glyph
	}
	return "⚙"
}

var toolGlyphs = map[string]string{
	"bash": "$", "read": "≡", "write": "✎", "edit": "✎", "grep": "⌕", "glob": "⁂", "web_search": "⊕", "web_fetch": "⊕",
}
