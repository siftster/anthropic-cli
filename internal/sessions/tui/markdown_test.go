package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

func TestMarkdownRendersTheFormsAgentsUse(t *testing.T) {
	require.Equal(t, []string{"Title", "a bold b and code see x (http://u)", "  fenced  raw", "- item_one here"},
		markdown("# Title\na **bold** *b* and `code` see [x](http://u)\n```\nfenced  raw\n```\n- item_one here", 80))
}

func TestMarkdownLeavesLookalikesAlone(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	styled := func(s string) bool { return strings.Contains(s, "\x1b[") }
	line := func(s string) string { return strings.Join(markdown(s, 80), "\n") }

	require.False(t, styled(line("def __init__(self): snake_case_name _not italic_")))
	require.Contains(t, line("def __init__(self)"), "__init__")
	require.False(t, styled(line("unclosed **bold and a*b*c")))
	require.Contains(t, line("unclosed **bold"), "**bold")
	both := line("*a* *b*")
	require.Equal(t, 2, strings.Count(both, "\x1b[4"), both)
	require.NotContains(t, both, "*")
	fence := markdown("```go\nx := 1\nstill code", 80)
	require.Equal(t, []string{"  x := 1", "  still code"}, []string{ansi.Strip(fence[0]), ansi.Strip(fence[1])})
}
