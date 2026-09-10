package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

func TestDurationsAndTokenCountsFormatCompactly(t *testing.T) {
	require.Equal(t, "0.3s", fmtDuration(300*time.Millisecond))
	require.Equal(t, "12.8s", fmtDuration(12800*time.Millisecond))
	require.Equal(t, "2m03s", fmtDuration(123*time.Second))
	require.Equal(t, "1h02m", fmtDuration(62*time.Minute))
	at := time.Unix(0, 0)
	idle := nodeRows(timeline.Node{Kind: timeline.NodeIdle, IdleFrom: at, IdleTo: at.Add(62 * time.Minute)}, false, renderOpts{Width: 40})
	require.Equal(t, " ╌╌ Session idle · 1h 02m ╌╌╌╌╌╌╌╌╌╌╌", ansi.Strip(idle[1]))
	require.Equal(t, "987", fmtTokens(987))
	require.Equal(t, "1.2k", fmtTokens(1234))
	require.Equal(t, "48.1k", fmtTokens(48100))
	require.Equal(t, "1.3M", fmtTokens(1_260_000))
}

// A hyphenated word near the limit must move whole to the next line rather
// than leave one letter behind.
func TestWrapKeepsHyphenatedWordsWhole(t *testing.T) {
	text := "edge cases (None input, empty string, numbers, already-hyphenated text, purely non-ASCII text)"
	for w := 20; w <= len(text); w++ {
		for _, l := range wrapLines(text, w) {
			if len(strings.TrimSpace(l)) == 1 {
				t.Fatalf("width %d orphaned a letter: %q", w, wrapLines(text, w))
			}
		}
	}
}
