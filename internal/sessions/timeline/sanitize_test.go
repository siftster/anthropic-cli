package timeline

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"clear\x1b[2Jscreen":                       "clearscreen",
		"\x1b[31mred\x1b[0m":                       "red",
		"\x1b]0;title\x07after":                    "after",
		"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1bPdcs\x1b\\x":                          "x",
		"lone\x1b":                                 "lone",
		"esc\x1bZpair":                             "escpair",
		"over\rwrite":                              "overwrite",
		"back\b\bspace":                            "backspace",
		"bell\x07":                                 "bell",
		"c1\xc2\x9b2Jcsi":                          "c12Jcsi",
		"del\x7f":                                  "del",
		"stray\x9bbyte":                            "straybyte",
		"keep\ttabs\nand lines":                    "keep\ttabs\nand lines",
		"emoji 🙂 and 日本語":                          "emoji 🙂 and 日本語",
		"unterminated\x1b]0;swallows the rest":     "unterminated",
	}
	for in, want := range cases {
		require.Equal(t, want, Sanitize(in), "%q", in)
	}
}

func TestSanitizedAtTheSource(t *testing.T) {
	f := fold(
		mk(t, "span.model_request_start", 100, ""),
		mk(t, "agent.message", 200, `"content":[{"type":"text","text":`+js("hi\x1b[2J there")+`}]`),
		mk(t, "agent.tool_use", 300, `"evaluated_permission":"ask","name":"agent_bash","input":{"command":`+js("echo a\r\nb")+`,"description":`+js("say\xc2\x9b hi")+`}`),
	)
	turn, call := f.Nodes()[0], f.Nodes()[0].Blocks[1].Calls[0]
	require.Equal(t, "hi there", EventText(turn.Blocks[0].Event))
	require.Equal(t, "db/q", ToolDisplayName(use(t, "agent.mcp_tool_use", "q", `{}`, `"mcp_server_name":`+js("db\x1b]0;t\x07"))))
	require.Equal(t, "say hi", ToolPreview(call.Use))
	require.Equal(t, "echo a b", ToolPayloadPreview(call.Use))
	require.Equal(t, []Line{{LineCmd, "$ echo a"}, {LineCmd, "b"}}, ToolBody(call))
	for _, l := range ToolBody(ToolCall{Use: use(t, "agent.mcp_tool_use", "q", `{"k":`+js("v\x7f\xc2\x90")+`}`, "")}) {
		require.False(t, strings.ContainsAny(l.Text, "\x7f\xc2\x90"), "%q", l.Text)
	}
}
