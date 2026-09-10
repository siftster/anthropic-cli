package timeline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anthropics/anthropic-cli/internal/sessions/sessiontest"
)

func use(t *testing.T, typ, name, input, extra string) Event {
	return ev(t, fmt.Sprintf(`{"id":"u1","type":%q,%s}`, typ, sessiontest.Tool(name, input, extra)))
}

func result(t *testing.T, text string, isError bool) *Event {
	e := ev(t, fmt.Sprintf(`{"id":"r1","type":"agent.tool_result","tool_use_id":"u1","is_error":%v,%s}`, isError, sessiontest.Text(text)))
	return &e
}

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fmt.Sprintf("%d|%s", l.Kind, l.Text)
	}
	return out
}

func TestToolPreview(t *testing.T) {
	long := strings.Repeat("é", 150)
	cases := map[string]string{
		`{"command":"ls","description":"List   the\n dir"}`: "List the dir",
		`{"path":"src/","pattern":"foo"}`:                   "src/",
		`{"url":42,"pattern":"*.go"}`:                       "*.go",
		`{"n":1}`:                                           "",
		fmt.Sprintf(`{"query":%q}`, long):                   strings.Repeat("é", 100) + "…",
	}
	for input, want := range cases {
		require.Equal(t, want, ToolPreview(use(t, "agent.tool_use", "x", input, "")), input)
	}
}

func TestToolDisplayNameAndKind(t *testing.T) {
	require.Equal(t, "bash", ToolDisplayName(use(t, "agent.tool_use", "agent_bash", `{}`, "")))
	require.Equal(t, "issues/search", ToolDisplayName(use(t, "agent.mcp_tool_use", "search", `{}`, `"mcp_server_name":"issues"`)))
	require.Equal(t, "search", ToolDisplayName(use(t, "agent.mcp_tool_use", "mcp_search", `{}`, "")))
	for name, kind := range map[string]string{
		"bash": "bash", "computer_bash": "bash", "read": "read", "write": "write", "str_replace_editor": "edit", "agent_multi_edit": "edit",
		"grep": "grep", "glob": "glob", "web_search": "web_search", "web_fetch": "web_fetch", "repl": "", "constructor": "",
	} {
		require.Equal(t, kind, ToolKind(name), name)
	}
}

func TestToolBodyBash(t *testing.T) {
	conf := ev(t, `{"id":"c1","type":"user.tool_confirmation","tool_use_id":"u1","result":"allow"}`)
	call := ToolCall{
		Use:          use(t, "agent.tool_use", "bash", `{"command":"git push","description":"Push"}`, ""),
		Confirmation: &conf,
		Result:       result(t, "line1\nline2\n", false),
	}
	require.Equal(t, []Line{{LineAllow, "Allowed"}, {LineCmd, "$ git push"}, {LineOut, "line1"}, {LineOut, "line2"}}, ToolBody(call))

	call.Confirmation, call.Result = nil, result(t, "denied: exit 1", true)
	require.Equal(t, []Line{{LineCmd, "$ git push"}, {LineErr, "denied: exit 1"}}, ToolBody(call))
}

func TestToolBodyEditDiff(t *testing.T) {
	single := ToolCall{
		Use:    use(t, "agent.tool_use", "str_replace", `{"path":"retry.py","old_str":"class R:\n    a = 1\n    end","new_str":"class R:\n    a = 2\n    b = 3\n    end"}`, ""),
		Result: result(t, "Edited", false),
	}
	require.Equal(t, []string{
		"0|retry.py", "2| class R:", "4|-    a = 1", "3|+    a = 2", "3|+    b = 3", "2|     end",
	}, texts(ToolBody(single)), "context kept, ack result hidden")

	multi := ToolCall{
		Use:    use(t, "agent.tool_use", "multi_edit", `{"file_path":"f.go","edits":[{"old_string":"x","new_string":"y"},{"old_string":"p","new_string":""}]}`, ""),
		Result: result(t, "old_string not found", true),
	}
	require.Equal(t, []string{"0|f.go", "4|-x", "3|+y", "0|@@", "4|-p", "5|old_string not found"}, texts(ToolBody(multi)))
}

func TestToolBodyReadWriteAndUnknown(t *testing.T) {
	read := ToolCall{Use: use(t, "agent.tool_use", "read", `{"file_path":"a.txt"}`, ""), Result: result(t, "A\nB", false)}
	require.Equal(t, []Line{{LineMeta, "a.txt"}, {LineOut, "A"}, {LineOut, "B"}}, ToolBody(read))

	view := ToolCall{Use: use(t, "agent.tool_use", "str_replace_editor", `{"command":"view","path":"a.txt"}`, ""), Result: result(t, "A", false)}
	require.Equal(t, []Line{{LineMeta, "a.txt"}, {LineOut, "A"}}, ToolBody(view), "text_editor view routes as a read")

	write := ToolCall{Use: use(t, "agent.tool_use", "write", `{"file_path":"n.txt","content":"one\ntwo"}`, ""), Result: result(t, "Wrote", false)}
	require.Equal(t, []Line{{LineMeta, "n.txt"}, {LineOut, "one"}, {LineOut, "two"}}, ToolBody(write))

	grep := ToolCall{Use: use(t, "agent.tool_use", "grep", `{"pattern":"jitter","path":"tests/"}`, "")}
	require.Equal(t, []Line{{LineCmd, "jitter  in tests/"}}, ToolBody(grep))

	fetch := ToolCall{Use: use(t, "agent.tool_use", "web_fetch", `{"url":"https://x.dev","prompt":"status?"}`, "")}
	require.Equal(t, []Line{{LineCmd, "https://x.dev"}, {LineCmd, "↳ status?"}}, ToolBody(fetch))

	unknown := ToolCall{Use: use(t, "agent.mcp_tool_use", "search", `{"q":"flaky"}`, ""), Result: result(t, "3 hits", false)}
	require.Equal(t, []Line{{LineOut, "{"}, {LineOut, `  "q": "flaky"`}, {LineOut, "}"}, {LineOut, "3 hits"}}, ToolBody(unknown))

	repl := ToolCall{Use: use(t, "agent.tool_use", "repl", `{"script":"a = 1\nprint(a)"}`, "")}
	require.Equal(t, []Line{{LineCmd, "a = 1"}, {LineCmd, "print(a)"}}, ToolBody(repl))

	img := ev(t, `{"id":"r1","type":"agent.tool_result","tool_use_id":"u1","content":[{"type":"image","source":{"type":"base64"}}]}`)
	shot := ToolCall{Use: use(t, "agent.tool_use", "glob", `{}`, ""), Result: &img}
	require.Equal(t, []Line{{LineCmd, "(none)"}, {LineNote, "(non-text content)"}}, ToolBody(shot))
}

func TestConfirmationNotes(t *testing.T) {
	note := func(js string) string { return EventText(ev(t, `{"id":"c","type":"user.tool_confirmation",`+js+`}`)) }
	require.Equal(t, "Allowed", note(`"result":"allow"`))
	require.Equal(t, "Denied", note(`"result":"deny"`))
	require.Equal(t, "Denied — not on main", note(`"result":"deny","deny_message":"not on main"`))

	deny := ev(t, `{"id":"c","type":"user.tool_confirmation","tool_use_id":"u1","result":"deny","deny_message":"no"}`)
	require.Equal(t, Line{LineDeny, "Denied — no"}, ToolBody(ToolCall{Use: use(t, "agent.tool_use", "glob", `{}`, ""), Confirmation: &deny})[0])
}

func TestTruncateDense(t *testing.T) {
	mk := func(n, width int) []Line {
		out := make([]Line, n)
		for i := range out {
			out[i] = Line{LineOut, strings.Repeat("x", width)}
		}
		return out
	}
	require.Len(t, TruncateDense(mk(12, 10)), 12, "at the cap: untouched")

	cut := TruncateDense(mk(104, 10))
	require.Len(t, cut, 13)
	require.Equal(t, Line{LineNote, "… 92 more lines · 12 of 104 shown"}, cut[12])

	byChars := TruncateDense(mk(10, 900))
	require.Len(t, []rune(byChars[2].Text), denseMaxChars-2*901, "line crossing the char cap is cut to the remaining budget")
	require.Equal(t, Line{LineNote, "… 7 more lines · 3 of 10 shown"}, byChars[3])

	huge := TruncateDense([]Line{{LineOut, strings.Repeat("é", 100_000)}})
	require.Len(t, huge, 2)
	require.Equal(t, strings.Repeat("é", denseMaxChars), huge[0].Text)
	require.Equal(t, LineNote, huge[1].Kind)

	require.Empty(t, TruncateDense(nil))
}
