package timeline

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEventTextShapes(t *testing.T) {
	require.Equal(t, "legacy", EventText(ev(t, `{"id":"a","type":"user.tool_result","tool_use_id":"u","content":"legacy"}`)))
	nested := `{"id":"b","type":"agent.tool_result","tool_use_id":"u","content":[{"type":"image"},{"type":"search_result","content":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}]}]}`
	require.Equal(t, "s1\ns2", EventText(ev(t, nested)))
	require.Equal(t, "", EventText(ev(t, `{"id":"c","type":"agent.thinking"}`)))
	require.Equal(t, `{"summary":"compacted 40k"}`, EventText(ev(t, `{"id":"d","type":"agent.future_kind","processed_at":null,"session_thread_id":"s","summary":"compacted 40k"}`)))
	require.Equal(t, "", EventText(ev(t, `{"id":"d", "type":"agent.future_kind"}`)))
}
