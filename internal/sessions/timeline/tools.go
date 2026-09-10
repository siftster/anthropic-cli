package timeline

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// previewMaxRunes caps a one-line tool preview.
const previewMaxRunes = 100

// LineKind says how a renderer should style a Line of a tool body.
type LineKind int

const (
	LineMeta LineKind = iota // dim header: a path, or "@@" between edits
	LineCmd                  // the call's one-line input: "$ cmd", "pattern  in path", url
	LineOut                  // payload: result text, written content, diff context, input JSON
	LineAdd
	LineDel
	LineErr   // result text of a failed call
	LineNote  // "(non-text content)", "… N more lines"
	LineAllow // the user's verdict: "Allowed"
	LineDeny  // "Denied — msg"
)

// Line is one styled line of a tool body.
type Line struct {
	Kind LineKind
	Text string
}

// ToolKind maps a tool name to the built-in whose body renderer applies; "" is unknown.
func ToolKind(name string) string {
	switch base := baseName(name); base {
	case "bash", "read", "write", "glob", "grep", "web_search", "web_fetch":
		return base
	case "edit", "text_editor", "str_replace", "str_replace_editor", "multi_edit":
		return "edit"
	}
	return ""
}

// baseName strips the one runtime prefix built-ins arrive under.
func baseName(name string) string {
	for _, prefix := range []string{"agent_", "mcp_", "computer_"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return rest
		}
	}
	return name
}

// ToolDisplayName is the name to show for a tool use, qualified by its MCP
// server when it has one.
func ToolDisplayName(ev Event) string {
	name := baseName(ev.Name)
	if ev.Type == "agent.mcp_tool_use" && ev.MCPServerName != "" {
		name = ev.MCPServerName + "/" + name
	}
	return Sanitize(name)
}

var (
	previewKeys = []string{"description", "command", "script", "file_path", "path", "query", "url", "pattern"}
	payloadKeys = []string{"command", "script", "code", "file_path", "path", "url", "query", "pattern", "description"}
)

// ToolPreview is the call's one identifying argument on a single line: the
// first string-valued key in a fixed precedence, whitespace collapsed, capped
// at previewMaxRunes.
func ToolPreview(ev Event) string { return preview(ev, previewKeys) }

// ToolPayloadPreview prefers what will actually execute over the model's
// own description of it, for places where the user is asked to approve it.
func ToolPayloadPreview(ev Event) string { return preview(ev, payloadKeys) }

func preview(ev Event, keys []string) string {
	input := gjson.Parse(ev.JSON.Input.Raw())
	for _, key := range keys {
		value := input.Get(key)
		if value.Type != gjson.String {
			continue
		}
		oneLine := strings.Join(strings.Fields(Sanitize(value.String())), " ")
		if runes := []rune(oneLine); len(runes) > previewMaxRunes {
			return string(runes[:previewMaxRunes]) + "…"
		}
		return oneLine
	}
	return ""
}

// ToolBody is the expanded view of a call: the user's verdict if any, the
// input in the tool's own idiom, then the result. Write and edit results are
// one-line acks, so they only surface on error.
func ToolBody(call ToolCall) []Line {
	lines := toolBody(call)
	for i := range lines {
		lines[i].Text = Sanitize(lines[i].Text)
	}
	return lines
}

func toolBody(call ToolCall) []Line {
	var out []Line
	if confirmation := call.Confirmation; confirmation != nil {
		if line, ok := confirmationLine(*confirmation); ok {
			out = append(out, line)
		}
	}
	input := gjson.Parse(call.Use.JSON.Input.Raw())
	path := firstString(input, "file_path", "path")
	resultOnlyOnError := false

	switch kind := ToolKind(call.Use.Name); kind {
	case "bash":
		out = appendText(out, LineCmd, "$ "+firstString(input, "command"))
	case "glob":
		out = append(out, Line{LineCmd, cmp.Or(firstString(input, "pattern"), "(none)")})
	case "grep":
		search := cmp.Or(firstString(input, "pattern"), "(none)")
		if path != "" {
			search += "  in " + path
		}
		out = append(out, Line{LineCmd, search})
	case "web_search":
		out = append(out, Line{LineCmd, `"` + cmp.Or(firstString(input, "query"), "(none)") + `"`})
	case "web_fetch":
		out = append(out, Line{LineCmd, cmp.Or(firstString(input, "url"), "(none)")})
		if prompt := firstString(input, "prompt"); prompt != "" {
			out = append(out, Line{LineCmd, "↳ " + prompt})
		}
	case "read", "write", "edit":
		// text_editor is command-discriminated (view / create / str_replace),
		// so route by the payload actually present rather than the name.
		out = append(out, Line{LineMeta, cmp.Or(path, "(no path)")})
		fileText := firstString(input, "content", "file_text")
		diff := editDiff(input)
		switch {
		case kind == "read":
		case kind == "write" || fileText != "":
			out = appendText(out, LineOut, cmp.Or(fileText, "(empty)"))
			resultOnlyOnError = true
		case diff != nil:
			out = append(out, diff...)
			resultOnlyOnError = true
		}
	default:
		// repl / code_execution runtimes: the payload is source, shown like a command.
		if code := firstString(input, "script", "code"); code != "" {
			out = appendText(out, LineCmd, code)
			break
		}
		var pretty bytes.Buffer
		if input.IsObject() && json.Indent(&pretty, []byte(input.Raw), "", "  ") == nil {
			out = appendText(out, LineOut, pretty.String())
		}
	}

	if call.Result == nil || (resultOnlyOnError && !call.Result.IsError) {
		return out
	}
	text := rawEventText(*call.Result)
	if raw := strings.TrimSpace(call.Result.JSON.Content.Raw()); text == "" && raw != "" && raw != "[]" && raw != "null" {
		return append(out, Line{LineNote, "(non-text content)"})
	}
	resultKind := LineOut
	if call.Result.IsError {
		resultKind = LineErr
	}
	return appendText(out, resultKind, text)
}

// confirmationLine is the user's verdict on a tool_confirmation; false when
// it carries neither allow nor deny.
func confirmationLine(ev Event) (Line, bool) {
	switch ev.Result {
	case "allow":
		return Line{LineAllow, "Allowed"}, true
	case "deny":
		if ev.DenyMessage != "" {
			return Line{LineDeny, "Denied — " + ev.DenyMessage}, true
		}
		return Line{LineDeny, "Denied"}, true
	}
	return Line{}, false
}

// firstString is the first of keys holding a non-empty string.
func firstString(input gjson.Result, keys ...string) string {
	for _, key := range keys {
		if value := input.Get(key); value.Type == gjson.String && value.String() != "" {
			return value.String()
		}
	}
	return ""
}

// editDiff renders old_str/new_str (either spelling, or multi_edit's edits[])
// as -/+ lines, keeping shared leading/trailing lines as context. nil when
// the input carries no edit payload.
func editDiff(input gjson.Result) []Line {
	edits := []gjson.Result{input}
	if list := input.Get("edits"); list.IsArray() {
		edits = list.Array()
	}
	var out []Line
	for _, edit := range edits {
		oldText, newText := firstString(edit, "old_str", "old_string"), firstString(edit, "new_str", "new_string")
		if oldText == "" && newText == "" {
			continue
		}
		if out != nil {
			out = append(out, Line{LineMeta, "@@"})
		}
		out = append(out, hunk(textLines(oldText), textLines(newText))...)
	}
	return out
}

// hunk diffs two texts by trimming their common head and tail: what remains
// in between is one deletion followed by one addition.
func hunk(oldLines, newLines []string) []Line {
	head := 0
	for head < len(oldLines) && head < len(newLines) && oldLines[head] == newLines[head] {
		head++
	}
	tail := 0
	for tail < len(oldLines)-head && tail < len(newLines)-head &&
		oldLines[len(oldLines)-1-tail] == newLines[len(newLines)-1-tail] {
		tail++
	}
	var out []Line
	out = appendPrefixed(out, LineOut, " ", oldLines[:head])
	out = appendPrefixed(out, LineDel, "-", oldLines[head:len(oldLines)-tail])
	out = appendPrefixed(out, LineAdd, "+", newLines[head:len(newLines)-tail])
	return appendPrefixed(out, LineOut, " ", oldLines[len(oldLines)-tail:])
}

// appendText appends text to out as one Line of kind per line.
func appendText(out []Line, kind LineKind, text string) []Line {
	return appendPrefixed(out, kind, "", textLines(text))
}

func appendPrefixed(out []Line, kind LineKind, prefix string, texts []string) []Line {
	for _, text := range texts {
		out = append(out, Line{kind, prefix + text})
	}
	return out
}

// textLines splits without the phantom empty line a trailing newline or an
// empty payload would otherwise contribute.
func textLines(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// Dense truncation caps for tool bodies shown inline in the transcript, matching the web viewer's.
const (
	denseMaxLines = 12
	denseMaxChars = 2000
)

// TruncateDense keeps the head of a body within the dense caps and appends a
// note saying what it dropped. denseMaxChars counts runes, newlines included;
// the line that crosses it is cut to the remaining budget on a rune boundary.
func TruncateDense(lines []Line) []Line {
	var kept []Line
	chars, cut := 0, false
	for _, line := range lines {
		if len(kept) == denseMaxLines || chars >= denseMaxChars {
			cut = true
			break
		}
		runes := utf8.RuneCountInString(line.Text)
		if chars+runes > denseMaxChars {
			line.Text, cut = string([]rune(line.Text)[:denseMaxChars-chars]), true
		}
		chars += runes + 1
		kept = append(kept, line)
	}
	if !cut {
		return lines
	}
	return append(kept, Line{LineNote, fmt.Sprintf("… %d more lines · %d of %d shown", len(lines)-len(kept), len(kept), len(lines))})
}
