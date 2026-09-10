// Package timeline folds an ordered stream of session events into the nodes
// the connect TUI renders. It mirrors the fold of the web session viewer that
// --web serves, so both views agree on turns, tool previews, bodies and
// truncation caps. Pure: no I/O, no styling.
//
// Files:
//   - timeline.go: the fold's output types (Node, Block, ToolCall, Status).
//   - fold.go: Fold, the reducer that builds and refines those nodes.
//   - tools.go: one-line previews and expanded bodies for tool calls.
//   - eventtext.go: the readable text of any event.
//   - sanitize.go: stripping terminal control sequences from event text.
package timeline

import (
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Event is one session event as the API returns it.
type Event = anthropic.BetaManagedAgentsSessionEventUnion

// NodeKind says what a Node represents and which of its fields are set.
type NodeKind string

const (
	NodeTurn           NodeKind = "turn"
	NodeUser           NodeKind = "user"
	NodeThreadReceived NodeKind = "thread_received"
	NodeOutcome        NodeKind = "outcome"
	NodeError          NodeKind = "error"
	NodeInterrupted    NodeKind = "interrupted"
	NodeRescheduled    NodeKind = "rescheduled"
	NodeTerminated     NodeKind = "terminated"
	NodeOutcomeDefined NodeKind = "outcome_defined"
	NodeIdle           NodeKind = "idle"    // a status_idle→status_running gap >= idleThreshold
	NodeSilent         NodeKind = "silent"  // status machinery between turns; verbose-only
	NodeUnknown        NodeKind = "unknown" // system.message, thread_context_compacted, anything unrecognised
)

// Node is one row group of the transcript, in render order.
type Node struct {
	Kind             NodeKind
	Event            Event   // the node's event; NodeTurn / NodeOutcome: the span start, if seen
	Blocks           []Block // NodeTurn
	Open             bool    // NodeTurn: no span.model_request_end yet
	Brief            *Event  // NodeThreadReceived: our last thread_message_sent to that peer
	OutcomeEnd       *Event  // NodeOutcome: the verdict (Result, Explanation); nil while grading
	IdleFrom, IdleTo time.Time
}

// Queued reports a user message sent but not yet ingested by the server.
func (n Node) Queued() bool { return n.Kind == NodeUser && n.Event.ProcessedAt.IsZero() }

// BlockKind says what a Block holds.
type BlockKind string

const (
	BlockThinking   BlockKind = "thinking"
	BlockText       BlockKind = "text"
	BlockTools      BlockKind = "tools"
	BlockThreadSent BlockKind = "thread_sent"
	BlockError      BlockKind = "error"
	BlockRequestEnd BlockKind = "request_end"
)

// Block is one thing a model request produced, in arrival order. Consecutive
// tool uses share one BlockTools; anything else between them starts another.
type Block struct {
	Kind  BlockKind
	Event Event      // every kind but BlockTools
	Calls []ToolCall // BlockTools
}

// Streaming reports an agent block whose event has not settled (no processed_at).
func (b Block) Streaming() bool { return b.Kind != BlockTools && b.Event.ProcessedAt.IsZero() }

// Lifecycle is where a ToolCall stands, derived from its parts.
type Lifecycle string

const (
	Running          Lifecycle = "running"
	AwaitingApproval Lifecycle = "awaiting_approval"
	Denied           Lifecycle = "denied"
	Failed           Lifecycle = "failed"
	Completed        Lifecycle = "completed"
)

// ToolCall is a tool use with the confirmation and result that name it by id.
type ToolCall struct {
	Use          Event
	Confirmation *Event
	Result       *Event
}

// Lifecycle derives from a call's parts: denial (policy or user) beats a
// stray result; else the result decides; else an unanswered ask gate is
// blocked, not running.
func (c ToolCall) Lifecycle() Lifecycle {
	permission := c.Use.EvaluatedPermission
	switch {
	case permission == "deny" || (c.Confirmation != nil && c.Confirmation.Result == "deny"):
		return Denied
	case c.Result != nil && c.Result.IsError:
		return Failed
	case c.Result != nil:
		return Completed
	case permission == "ask" && c.Confirmation == nil:
		return AwaitingApproval
	default:
		return Running
	}
}

// Status is the latest session.status_* (or session.deleted) event, digested.
// StopReason "requires_action" with no Pending means the session waits on a
// custom tool result (a worker's job), not on an approval we can give.
type Status struct {
	State      string     // running | idle | rescheduling | terminated | deleted | ""
	StopReason string     // status_idle only: end_turn | requires_action | retries_exhausted
	Pending    []ToolCall // requires_action calls still AwaitingApproval, oldest first
}

// Usage is one model request's token counts. Input is everything sent (the ↑
// figure): uncached input plus cache reads plus cache writes.
type Usage struct {
	Input, Output int64
}

// UsageOf is a span.model_request_end's token counts; nil when it carried none.
func UsageOf(ev Event) *Usage {
	if !ev.JSON.ModelUsage.Valid() {
		return nil
	}
	tokens := ev.ModelUsage
	return &Usage{
		Input:  tokens.InputTokens + tokens.CacheReadInputTokens + tokens.CacheCreationInputTokens,
		Output: tokens.OutputTokens,
	}
}
