package timeline

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EventText is an event's readable text: joined text blocks for messages and
// tool results, the message for errors, explanation/description for outcomes.
// "" if none.
func EventText(ev Event) string { return Sanitize(rawEventText(ev)) }

func rawEventText(ev Event) string {
	switch ev.Type {
	case "user.message", "agent.message", "system.message", "agent.thread_message_sent", "agent.thread_message_received",
		"agent.tool_result", "agent.mcp_tool_result", "user.tool_result", "user.custom_tool_result":
		return blocksText(gjson.Parse(ev.JSON.Content.Raw()))
	case "session.error":
		return ev.Error.Message
	case "span.outcome_evaluation_end":
		return ev.Explanation
	case "user.define_outcome":
		return ev.Description
	case "user.tool_confirmation":
		line, _ := confirmationLine(ev)
		return line.Text
	case "agent.thinking", "user.interrupt", "agent.thread_context_compacted", "session.deleted",
		"session.status_idle", "session.status_running", "session.status_rescheduled", "session.status_terminated",
		"span.model_request_start", "span.model_request_end", "span.outcome_evaluation_start", "span.outcome_evaluation_ongoing":
		return ""
	}
	// Fail open for types with no specific rendering: the non-envelope fields, compact.
	rest := ev.RawJSON()
	for _, envelopeKey := range []string{"id", "type", "processed_at", "session_thread_id"} {
		rest, _ = sjson.Delete(rest, envelopeKey)
	}
	if rest = gjson.Get(rest, "@ugly").Raw; rest == "{}" {
		return ""
	}
	return rest
}

// blocksText joins the text parts of a content value as loosely as it
// arrives: a bare string, text blocks, or wrappers nesting more `content`
// (search results). Blocks with nothing readable (image, document) are skipped.
func blocksText(content gjson.Result) string {
	switch {
	case content.Type == gjson.String:
		return content.String()
	case content.IsArray():
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if text := blocksText(block); text != "" {
				parts = append(parts, text)
			}
			return true
		})
		return strings.Join(parts, "\n")
	case content.IsObject():
		if text := content.Get("text"); text.Type == gjson.String {
			return text.String()
		}
		return blocksText(content.Get("content"))
	}
	return ""
}
