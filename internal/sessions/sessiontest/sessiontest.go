// Package sessiontest builds session events from terse JSON for the
// sessions test suites. Import it from _test files only.
//
// Fixtures name events by their time: Ev(t, typ, 5200, …) has id "e5200" and
// is processed 5.2s after Epoch, so a tool result can point at its call by
// that number.
package sessiontest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Event is the wire event union the sessions packages consume.
type Event = anthropic.BetaManagedAgentsSessionEventUnion

// Epoch is time zero for fixture timestamps.
var Epoch = time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC)

// Parse decodes one wire event, failing the test on bad JSON.
func Parse(t *testing.T, js string) Event {
	t.Helper()
	var e Event
	if err := json.Unmarshal([]byte(js), &e); err != nil {
		t.Fatalf("sessiontest.Parse: %v\n%s", err, js)
	}
	return e
}

// Ev is a processed event of type typ with id "e<ms>", processed at Epoch+ms.
// extra is more JSON fields, without braces.
func Ev(t *testing.T, typ string, ms int, extra string) Event {
	t.Helper()
	at := Epoch.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)
	return Parse(t, fmt.Sprintf(`{"id":"e%d","type":%q,"processed_at":%q%s}`, ms, typ, at, comma(extra)))
}

// Pending is an event the server has not processed: a queued user event or a streaming preview.
func Pending(t *testing.T, typ, id, extra string) Event {
	t.Helper()
	return Parse(t, fmt.Sprintf(`{"id":%q,"type":%q,"processed_at":null%s}`, id, typ, comma(extra)))
}

// comma prefixes non-empty extra fields with the comma that joins them on.
func comma(extra string) string {
	if extra == "" {
		return ""
	}
	return "," + extra
}

// Text is a content field holding one text block.
func Text(s string) string { return fmt.Sprintf(`"content":[{"type":"text","text":%q}]`, s) }

// JS quotes s as a JSON string, escaping control bytes the way the wire does
// (%q would emit Go escapes JSON rejects).
func JS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Tool is the fields of an agent.tool_use calling name with input (raw JSON).
func Tool(name, input, extra string) string {
	return fmt.Sprintf(`"name":%q,"input":%s%s`, name, input, comma(extra))
}

// Result is the fields of an agent.tool_result answering the call made at useMS.
func Result(useMS int, body string, isErr bool) string {
	return fmt.Sprintf(`"tool_use_id":"e%d","is_error":%v,%s`, useMS, isErr, Text(body))
}

// Canonical is the reference session fixture: a user request, then three model
// requests (thinking, text, tool calls) ending on a call awaiting approval.
func Canonical(t *testing.T) []Event {
	return []Event{
		Ev(t, "user.message", 100, Text("tests/test_retry.py::test_backoff_jitter is flaky on CI — can you figure out why and fix it?")),
		Ev(t, "session.status_running", 200, ""),
		Ev(t, "span.model_request_start", 1000, ""),
		Ev(t, "agent.thinking", 5000, ""),
		Ev(t, "agent.message", 5100, Text("I'll start by reading the test and the retry implementation it exercises.")),
		Ev(t, "agent.tool_use", 5200, Tool("grep", `{"pattern":"test_backoff_jitter"}`, "")),
		Ev(t, "agent.tool_use", 5300, Tool("read", `{"file_path":"tests/test_retry.py"}`, "")),
		Ev(t, "agent.tool_use", 5400, Tool("read", `{"file_path":"src/httpclient/retry.py"}`, "")),
		Ev(t, "span.model_request_end", 5500, requestEnd(1000, 3000, 300)),
		Ev(t, "agent.tool_result", 5600, Result(5200, "tests/test_retry.py:41", false)),
		Ev(t, "agent.tool_result", 5700, Result(5300, "…", false)),
		Ev(t, "agent.tool_result", 5800, Result(5400, "…", false)),
		Ev(t, "span.model_request_start", 6000, ""),
		Ev(t, "agent.thinking", 13000, ""),
		Ev(t, "agent.message", 13100, Text("The test seeds **random**, but `retry.py` derives jitter from `time.monotonic()`, so the asserted sleep sequence depends on wall-clock. I'll inject an RNG and seed it from the test instead.")),
		Ev(t, "agent.tool_use", 13200, Tool("edit", `{"file_path":"src/httpclient/retry.py","old_str":"a","new_str":"b"}`, "")),
		Ev(t, "agent.tool_use", 13300, Tool("edit", `{"file_path":"tests/test_retry.py","old_str":"a","new_str":"b"}`, "")),
		Ev(t, "agent.tool_use", 13400, Tool("bash", `{"command":"for i in $(seq 50); do pytest -q tests/test_retry.py::test_backoff_jitter || exit 1; done","description":"Run the retry test 50× to check for flakes"}`, "")),
		Ev(t, "span.model_request_end", 13500, requestEnd(6000, 3400, 400)),
		Ev(t, "agent.tool_result", 13600, Result(13200, "ok", false)),
		Ev(t, "agent.tool_result", 13700, Result(13300, "ok", false)),
		Ev(t, "agent.tool_result", 26200, Result(13400, "1 passed in 0.24s", false)),
		Ev(t, "span.model_request_start", 27000, ""),
		Ev(t, "agent.message", 28000, Text("All 50 runs pass locally. I'd like to push the branch so CI can confirm on Linux runners too.")),
		Ev(t, "agent.tool_use", 28100, Tool("bash", `{"command":"git push -u origin fix/retry-jitter"}`, `"evaluated_permission":"ask"`)),
		Ev(t, "span.model_request_end", 28200, requestEnd(27000, 3400, 400)),
		Ev(t, "session.status_idle", 28300, `"stop_reason":{"type":"requires_action","event_ids":["e28100"]}`),
	}
}

// requestEnd is the fields of a span.model_request_end closing the request started at startMS.
func requestEnd(startMS, inputTokens, outputTokens int) string {
	return fmt.Sprintf(`"model_request_start_id":"e%d","model_usage":{"input_tokens":%d,"output_tokens":%d}`, startMS, inputTokens, outputTokens)
}
