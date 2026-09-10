package timeline

import (
	"slices"
	"time"
)

// idleThreshold is the shortest idle→running gap that earns a NodeIdle.
const idleThreshold = 30 * time.Second

// Fold is the reducer that builds and refines the nodes: Upsert files an
// event and Reset refolds a whole log.
// Queued user nodes (sent, not yet ingested) wait at the tail and everything
// else files ahead of them, where the server will order it once it ingests
// them. The accepting turn is always the last node before that tail; only
// queued nodes ever shift, and each owns just its own id, so indices held in
// `owner` stay cheap to keep valid.
type Fold struct {
	nodes     []Node
	queued    int  // trailing queued user nodes
	dirty     int  // lowest node index changed since TakeDirty
	accepting bool // the node at end()-1 is the turn taking agent blocks, even past its span's end

	owner          map[string]int     // event id → node index; -1: consumed, renders nowhere
	orphans        map[string][]Event // results/confirmations by the use id they beat onto the wire
	lastSentToPeer map[string]Event   // peer thread id → our last agent.thread_message_sent to it
	idleOpener     *Event             // the status_idle that opened the current idle window
	lastStatus     *Event
	usage          Usage
}

// NewFold returns an empty fold.
func NewFold() *Fold {
	return &Fold{
		owner:          map[string]int{},
		orphans:        map[string][]Event{},
		lastSentToPeer: map[string]Event{},
	}
}

// Upsert files an event. An unseen id is appended, so those must arrive in
// display order; a known id refines in place (a streaming agent.message
// filling in, then its final) and only the owning node is touched.
func (f *Fold) Upsert(ev Event) {
	idx, seen := f.owner[ev.ID]
	switch {
	case !seen:
		f.push(ev)
	case idx < 0:
		// Consumed: it renders nowhere, so there is nothing to refresh.
	case idx >= f.end() && !ev.ProcessedAt.IsZero():
		// A queued node got ingested: file it where processed nodes go.
		f.removeNode(idx)
		f.queued--
		f.push(ev)
	case f.nodes[idx].Event.ID == ev.ID:
		f.nodes[idx].Event = ev
		f.touch(idx)
	default:
		refreshBlocks(&f.nodes[idx], ev)
		f.touch(idx)
	}
}

// Reset discards the fold and refolds events from scratch.
func (f *Fold) Reset(events []Event) {
	*f = *NewFold()
	for _, ev := range events {
		f.push(ev)
	}
}

// Nodes aliases internal storage: valid until the next Upsert/Reset.
func (f *Fold) Nodes() []Node { return f.nodes }

// TakeDirty reports the lowest index whose node changed (or was popped) since
// the last call, so a renderer can reuse what it drew for the nodes before it.
func (f *Fold) TakeDirty() int {
	lowest := f.dirty
	f.dirty = len(f.nodes)
	return lowest
}

var stateByStatusType = map[string]string{
	"session.status_running":     "running",
	"session.status_idle":        "idle",
	"session.status_rescheduled": "rescheduling",
	"session.status_terminated":  "terminated",
	"session.deleted":            "deleted",
}

// Status digests the latest status event seen; the zero Status before any.
func (f *Fold) Status() Status {
	if f.lastStatus == nil {
		return Status{}
	}
	ev := *f.lastStatus
	status := Status{State: stateByStatusType[ev.Type]}
	if ev.Type != "session.status_idle" {
		return status
	}
	status.StopReason = ev.StopReason.Type
	for _, id := range ev.StopReason.EventIDs {
		if call := f.findCall(id); call != nil && call.Lifecycle() == AwaitingApproval {
			status.Pending = append(status.Pending, *call)
		}
	}
	return status
}

// TotalUsage sums every request_end seen.
func (f *Fold) TotalUsage() Usage { return f.usage }

// push files an event whose id the fold has not placed yet.
func (f *Fold) push(ev Event) {
	if _, seen := f.owner[ev.ID]; !seen {
		f.owner[ev.ID] = -1
	}
	switch ev.Type {
	case "span.model_request_start":
		f.openTurn(ev)
	case "span.model_request_end":
		f.endTurn(ev)
	case "agent.thinking":
		f.pushBlock(Block{Kind: BlockThinking, Event: ev})
	case "agent.message":
		f.pushBlock(Block{Kind: BlockText, Event: ev})
	case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
		f.pushCall(ev)
	case "agent.thread_message_sent":
		if ev.ToSessionThreadID != "" {
			f.lastSentToPeer[ev.ToSessionThreadID] = ev
		}
		f.pushBlock(Block{Kind: BlockThreadSent, Event: ev})
	case "session.error":
		if f.accepting {
			f.pushBlock(Block{Kind: BlockError, Event: ev})
		} else {
			f.pushNode(Node{Kind: NodeError, Event: ev})
		}
	case "agent.thread_message_received":
		f.closeTurn()
		node := Node{Kind: NodeThreadReceived, Event: ev}
		if brief, ok := f.lastSentToPeer[ev.FromSessionThreadID]; ok {
			node.Brief = &brief
		}
		f.pushNode(node)
	case "user.message":
		f.pushUser(Node{Kind: NodeUser, Event: ev})
	case "user.interrupt":
		f.pushUser(Node{Kind: NodeInterrupted, Event: ev})
	case "user.define_outcome":
		f.pushUser(Node{Kind: NodeOutcomeDefined, Event: ev})
	case "session.status_terminated":
		f.lastStatus = &ev
		f.closeTurn()
		f.pushNode(Node{Kind: NodeTerminated, Event: ev})
	case "session.status_rescheduled":
		f.lastStatus = &ev
		f.closeTurn()
		f.pushNode(Node{Kind: NodeRescheduled, Event: ev})
	case "span.outcome_evaluation_start":
		f.closeTurn()
		f.pushNode(Node{Kind: NodeOutcome, Event: ev})
	case "span.outcome_evaluation_ongoing":
		// A heartbeat: renders nothing and must not split a turn it lands inside.
	case "span.outcome_evaluation_end":
		idx, ok := f.owner[ev.OutcomeEvaluationStartID]
		if !ok || idx < 0 || f.nodes[idx].Kind != NodeOutcome {
			f.closeTurn()
			idx = f.pushNode(Node{Kind: NodeOutcome})
		}
		f.nodes[idx].OutcomeEnd = &ev
		f.owner[ev.ID] = idx
		f.touch(idx)
	case "agent.tool_result", "agent.mcp_tool_result", "user.tool_result", "user.custom_tool_result", "user.tool_confirmation":
		f.pairResult(ev)
	case "session.status_idle", "session.status_running", "session.deleted":
		f.trackStatus(ev)
		f.pushSilent(ev, ev.Type != "session.status_running")
	case "session.updated", "session.usage", "session.thread_created", "session.thread_idle", "session.thread_terminated",
		"session.thread_status_running", "session.thread_status_idle", "session.thread_status_rescheduled", "session.thread_status_terminated":
		f.pushSilent(ev, false)
	default:
		f.closeTurn()
		f.pushNode(Node{Kind: NodeUnknown, Event: ev})
	}
}

// end is where the next processed node goes: ahead of the queued tail.
func (f *Fold) end() int { return len(f.nodes) - f.queued }

func (f *Fold) touch(idx int) { f.dirty = min(f.dirty, idx) }

func (f *Fold) pushNode(node Node) int {
	idx := f.end()
	f.nodes = slices.Insert(f.nodes, idx, node)
	f.reindex(idx)
	f.touch(idx)
	return idx
}

// pushUser files a user node. A queued one waits at the tail without closing
// the turn: events processed before the server ingests it still belong ahead.
func (f *Fold) pushUser(node Node) {
	if !node.Event.ProcessedAt.IsZero() {
		f.closeTurn()
		f.pushNode(node)
		return
	}
	f.nodes = append(f.nodes, node)
	f.queued++
	f.reindex(len(f.nodes) - 1)
	f.touch(len(f.nodes) - 1)
}

func (f *Fold) removeNode(idx int) {
	if id := f.nodes[idx].Event.ID; id != "" {
		f.owner[id] = -1
	}
	f.nodes = slices.Delete(f.nodes, idx, idx+1)
	f.reindex(idx)
	f.touch(idx)
}

// reindex repoints owner at nodes from idx on. Past an insert or removal
// those are only queued user nodes, which own nothing but their own event.
func (f *Fold) reindex(from int) {
	for i := from; i < len(f.nodes); i++ {
		if id := f.nodes[i].Event.ID; id != "" {
			f.owner[id] = i
		}
	}
}

func (f *Fold) openTurn(start Event) {
	f.closeTurn()
	f.pushNode(Node{Kind: NodeTurn, Open: true, Event: start})
	f.accepting = true
}

// closeTurn stops the last turn accepting blocks. A turn that closes with
// none renders nowhere and is dropped.
func (f *Fold) closeTurn() {
	if !f.accepting {
		return
	}
	f.accepting = false
	if last := f.end() - 1; len(f.nodes[last].Blocks) == 0 {
		f.removeNode(last)
	}
}

// acceptingTurn is the turn taking agent blocks, opening a span-less one for
// an agent event that arrives outside any (truncated replay, or one trailing
// a boundary).
func (f *Fold) acceptingTurn() (*Node, int) {
	if !f.accepting {
		f.openTurn(Event{})
	}
	idx := f.end() - 1
	f.touch(idx)
	return &f.nodes[idx], idx
}

func (f *Fold) pushBlock(block Block) {
	turn, idx := f.acceptingTurn()
	turn.Blocks = append(turn.Blocks, block)
	f.owner[block.Event.ID] = idx
}

// endTurn files a span end under the turn its model_request_start_id names,
// never under whichever turn happens to be accepting.
func (f *Fold) endTurn(ev Event) {
	startID, last := ev.ModelRequestStartID, f.end()-1
	idx, seen := f.owner[startID]
	switch {
	case f.accepting && !seen && f.nodes[last].Event.ID == "" && f.nodes[last].Open:
		idx = last // only a start absent from the log lets a span-less turn adopt an end
	case seen && idx >= 0 && f.nodes[idx].Kind == NodeTurn:
	default:
		return
	}
	turn := &f.nodes[idx]
	turn.Open = false
	turn.Blocks = append(turn.Blocks, Block{Kind: BlockRequestEnd, Event: ev})
	if usage := UsageOf(ev); usage != nil {
		f.usage = Usage{f.usage.Input + usage.Input, f.usage.Output + usage.Output}
	}
	f.owner[ev.ID] = idx
	f.touch(idx)
}

func (f *Fold) pushCall(use Event) {
	call := ToolCall{Use: use}
	for _, ev := range f.orphans[use.ID] {
		call.adopt(ev)
	}
	delete(f.orphans, use.ID)

	turn, idx := f.acceptingTurn()
	if last := len(turn.Blocks) - 1; last >= 0 && turn.Blocks[last].Kind == BlockTools {
		turn.Blocks[last].Calls = append(turn.Blocks[last].Calls, call)
	} else {
		turn.Blocks = append(turn.Blocks, Block{Kind: BlockTools, Calls: []ToolCall{call}})
	}
	f.owner[use.ID] = idx
}

// pairResult pairs a result or confirmation onto its call by id; one that beat
// its use onto the wire waits as an orphan for pushCall to adopt.
func (f *Fold) pairResult(ev Event) {
	useID := pairID(ev)
	call := f.findCall(useID)
	if call == nil {
		f.orphans[useID] = append(f.orphans[useID], ev)
		return
	}
	call.adopt(ev)
	f.owner[ev.ID] = f.owner[useID]
	f.touch(f.owner[useID])
}

func (f *Fold) findCall(useID string) *ToolCall {
	idx, ok := f.owner[useID]
	if !ok || idx < 0 || f.nodes[idx].Kind != NodeTurn {
		return nil
	}
	for i := range f.nodes[idx].Blocks {
		block := &f.nodes[idx].Blocks[i]
		for j := range block.Calls {
			if block.Calls[j].Use.ID == useID {
				return &block.Calls[j]
			}
		}
	}
	return nil
}

// pairID is the tool use id a result or confirmation names; "" for other events.
func pairID(ev Event) string {
	switch ev.Type {
	case "agent.tool_result", "user.tool_result", "user.tool_confirmation":
		return ev.ToolUseID
	case "agent.mcp_tool_result":
		return ev.MCPToolUseID
	case "user.custom_tool_result":
		return ev.CustomToolUseID
	}
	return ""
}

func (c *ToolCall) adopt(ev Event) {
	if ev.Type == "user.tool_confirmation" {
		c.Confirmation = &ev
	} else {
		c.Result = &ev
	}
}

// refreshBlocks replaces the copy of ev held by whichever block or call in
// node carries its id.
func refreshBlocks(node *Node, ev Event) {
	for i := range node.Blocks {
		block := &node.Blocks[i]
		if block.Kind != BlockTools {
			if block.Event.ID == ev.ID {
				block.Event = ev
				return
			}
			continue
		}
		for j := range block.Calls {
			call := &block.Calls[j]
			switch {
			case call.Use.ID == ev.ID:
				call.Use = ev
			case call.Confirmation != nil && call.Confirmation.ID == ev.ID, call.Result != nil && call.Result.ID == ev.ID:
				call.adopt(ev)
			default:
				continue
			}
			return
		}
	}
}

// trackStatus records a status event and, when a running event ends an idle
// window of at least idleThreshold, files a NodeIdle for the gap.
func (f *Fold) trackStatus(ev Event) {
	f.lastStatus = &ev
	switch ev.Type {
	case "session.status_idle":
		if f.idleOpener == nil {
			f.idleOpener = &ev
		}
	case "session.status_running":
		opener := f.idleOpener
		f.idleOpener = nil
		if opener == nil || opener.ProcessedAt.IsZero() || ev.ProcessedAt.IsZero() {
			return
		}
		if ev.ProcessedAt.Sub(opener.ProcessedAt) >= idleThreshold {
			f.closeTurn()
			f.pushNode(Node{Kind: NodeIdle, IdleFrom: opener.ProcessedAt, IdleTo: ev.ProcessedAt})
		}
	}
}

// pushSilent keeps machinery out of the conversation: while a turn accepts
// blocks it renders nowhere — only a session status change past the span's
// end may close the turn to surface as a verbose-only node.
func (f *Fold) pushSilent(ev Event, mayClose bool) {
	if f.accepting && (!mayClose || f.nodes[f.end()-1].Open) {
		return
	}
	f.closeTurn()
	f.pushNode(Node{Kind: NodeSilent, Event: ev})
}
