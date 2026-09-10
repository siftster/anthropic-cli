package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

func TestStatusHintAndFooterLines(t *testing.T) {
	f := timeline.NewFold()
	f.Reset(sessiontest.Canonical(t))
	status := f.Status()
	require.Len(t, status.Pending, 1)
	require.Equal(t, "e28100", status.Pending[0].Use.ID)

	golden(t, `
 Fix flaky retry test · coder · ! awaiting approval                                            live
`, statusLine("Fix flaky retry test", "coder", status, "live", nil, "⠙", 100))

	u := f.TotalUsage()
	golden(t, `
 Fix flaky retry test · coder · ○ idle                                           live · ↑9.8k ↓1.1k
`, statusLine("Fix flaky retry test", "coder", timeline.Status{State: "idle"}, "live", &u, "⠙", 100))

	golden(t, `
   Enter send · Alt-Enter newline · Esc interrupt · PgUp/PgDn scroll · Ctrl-O detail  Ctrl-C detach
`, hintLine(modeCompose, false, true, "", 100))
	require.NotContains(t, hintLine(modeCompose, false, false, "", 80), "PgUp", "scroll hint only at width >= 100")
	require.Contains(t, hintLine(modeCompose, true, false, "", 100), "Ctrl-O detail: on")
	url := "https://platform.claude.com/workspaces/default/sessions/sesn_011CZkZAtmR3yMPDzynEDxu7"
	golden(t, `
   sesn_011CZkZAtmR3yMPDzynEDxu7  …tform.claude.com/workspaces/default/sessions/sesn_011CZkZAtmR3yMPDzynEDxu7
`, metaLine("sesn_011CZkZAtmR3yMPDzynEDxu7", url, 110))
	require.Equal(t, "   sesn_x", trimLines(metaLine("sesn_x", url, 20)), "too narrow: id only")
	require.Contains(t, metaLine("sesn_x", url, 200), "  "+url, "fits whole")
	require.Contains(t, hintLine(modeCompose, false, false, "boom", 100), "boom")
}
