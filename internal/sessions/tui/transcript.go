package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// transcript renders nodes top to bottom, keeping each node's rows so a
// redraw only renders nodes from dirtyFrom on, plus any showing a spinner.
type transcript struct {
	drawn []drawnNode
	opts  renderOpts // Width/Verbose/Agent the kept rows were drawn at
}

type drawnNode struct {
	rows      []string
	afterTurn bool // the last visible node through this one is an agent turn: the next turn skips its label
	animating bool
}

type renderOpts struct {
	Width        int
	Verbose      bool
	Agent        string // the label heading a run of agent turns
	SpinnerFrame string
	Now          time.Time
}

// Layout columns, relative to Width: block rows are indented 3 and, like the
// hint line and token pills, right-align to Width-3; prose wraps one short of
// the edge so a full line never triggers terminal auto-wrap.
func (o renderOpts) textWidth() int { return max(o.Width-4, 10) }
func (o renderOpts) rowWidth() int  { return max(o.Width-6, 10) }

// render returns the transcript and whether any node animates. Nothing here
// depends on model state, so frames can be golden-tested.
func (t *transcript) render(nodes []timeline.Node, dirtyFrom int, opts renderOpts) (string, bool) {
	if opts.Width != t.opts.Width || opts.Verbose != t.opts.Verbose || opts.Agent != t.opts.Agent {
		dirtyFrom = 0
	}
	t.opts = opts
	t.drawn = t.drawn[:min(len(t.drawn), dirtyFrom, len(nodes))]
	var out []string
	afterTurn, animating := false, false
	for i, node := range nodes {
		switch {
		case i == len(t.drawn):
			t.drawn = append(t.drawn, drawNode(node, afterTurn, opts))
		case t.drawn[i].animating:
			t.drawn[i] = drawNode(node, afterTurn, opts)
		}
		drawn := t.drawn[i]
		out = append(out, drawn.rows...)
		afterTurn = drawn.afterTurn
		animating = animating || drawn.animating
	}
	return strings.Join(out, "\n"), animating
}

func drawNode(node timeline.Node, afterTurn bool, opts renderOpts) drawnNode {
	drawn := drawnNode{rows: nodeRows(node, afterTurn, opts), afterTurn: afterTurn, animating: animates(node)}
	if drawn.rows != nil {
		drawn.afterTurn = node.Kind == timeline.NodeTurn
	}
	return drawn
}

// animates reports whether the node draws a spinner or a running clock, so it
// must be redrawn on every tick rather than served from kept rows.
func animates(node timeline.Node) bool {
	switch node.Kind {
	case timeline.NodeOutcome:
		return node.OutcomeEnd == nil
	case timeline.NodeTurn:
		if node.Open && len(node.Blocks) == 0 {
			return true
		}
		for _, block := range node.Blocks {
			if block.Kind == timeline.BlockThinking && block.Streaming() {
				return true
			}
			for _, call := range block.Calls {
				if lifecycle := call.Lifecycle(); lifecycle == timeline.Running || lifecycle == timeline.AwaitingApproval {
					return true
				}
			}
		}
	}
	return false
}

// nodeRows draws one node. Nil means the node is not shown at all.
func nodeRows(node timeline.Node, afterTurn bool, opts renderOpts) []string {
	width, textWidth := opts.Width, opts.textWidth()
	switch node.Kind {
	case timeline.NodeTurn:
		return turnRows(node, afterTurn, opts)
	case timeline.NodeUser:
		label := " " + stUser.Render("● User")
		if node.Queued() {
			label += " " + stBadgeDim.Render(" queued ")
		}
		return append([]string{"", label}, gutter(stUser, wrapLines(timeline.EventText(node.Event), textWidth))...)
	case timeline.NodeThreadReceived:
		label := " " + stPeer.Render("● "+timeline.Sanitize(node.Event.FromAgentName)+" ▸")
		if node.Brief != nil {
			label += "  " + stDim.Render("re: “"+clip(timeline.EventText(*node.Brief), 60)+"”")
		}
		return append([]string{"", label}, gutter(stPeer, markdown(timeline.EventText(node.Event), textWidth))...)
	case timeline.NodeInterrupted:
		return []string{"", lipgloss.PlaceHorizontal(width, lipgloss.Center, stBadgeDim.Render(" Interrupted "))}
	case timeline.NodeRescheduled, timeline.NodeTerminated:
		return []string{"", lipgloss.PlaceHorizontal(width, lipgloss.Center, stDim.Render("Session "+string(node.Kind)))}
	case timeline.NodeOutcomeDefined:
		body := wrapLines("Outcome defined — "+timeline.EventText(node.Event), textWidth-2)
		if opts.Verbose && node.Event.Rubric.Content != "" {
			body = append(body, styleAll(stDim, wrapLines(timeline.Sanitize(node.Event.Rubric.Content), textWidth-2))...)
		}
		return box(stSpan, stSpan.Render("Outcome"), body, width)
	case timeline.NodeOutcome:
		body := stDim.Render(opts.SpinnerFrame + " Grading…")
		if end := node.OutcomeEnd; end != nil {
			body = "Grading → " + stBold.Render(timeline.Sanitize(end.Result)) + ": " + firstLine(timeline.EventText(*end))
		}
		return box(stSpan, stSpan.Render("Outcome"), []string{body}, width)
	case timeline.NodeError:
		head := stErr.Render("Error") + " " + stBadgeErr.Render(" "+timeline.Sanitize(node.Event.Error.Type)+" ")
		return box(stErr, head, wrapLines(timeline.EventText(node.Event), textWidth-2), width)
	case timeline.NodeIdle:
		head := "╌╌ Session idle · " + fmtDurationSep(node.IdleTo.Sub(node.IdleFrom), " ") + " "
		return []string{"", " " + stDim.Render(head+strings.Repeat("╌", max(0, width-4-lipgloss.Width(head))))}
	case timeline.NodeSilent:
		if !opts.Verbose {
			return nil
		}
		row := " " + stDim.Render("· "+node.Event.Type+"  "+node.Event.StopReason.Type)
		return []string{spread(row, stDim.Render(node.Event.ProcessedAt.Local().Format("15:04:05")), width-3)}
	default:
		return []string{" " + stDim.Render("· "+node.Event.Type+"  "+clip(oneLine(timeline.EventText(node.Event)), 100))}
	}
}

func turnRows(node timeline.Node, afterTurn bool, opts renderOpts) []string {
	var body []string
	for _, block := range node.Blocks {
		body = append(body, blockRows(node, block, opts)...)
	}
	if node.Open && len(node.Blocks) == 0 {
		body = append(body, stDim.Render(opts.SpinnerFrame+" Generating…"))
	}
	if len(body) == 0 {
		return nil
	}
	// One gutter run per model request; the label only heads a run of turns.
	out := []string{""}
	if !afterTurn {
		out = append(out, " "+stAgent.Render("● "+opts.Agent))
	}
	return append(out, gutter(stAgent, body)...)
}

func blockRows(turn timeline.Node, block timeline.Block, opts renderOpts) []string {
	textWidth := opts.textWidth()
	switch block.Kind {
	case timeline.BlockThinking:
		label := "Thought"
		if block.Streaming() {
			label = opts.SpinnerFrame + " Thinking…"
		} else if turn.Event.ID != "" {
			label = fmt.Sprintf("Thought for %ds", int(block.Event.ProcessedAt.Sub(turn.Event.ProcessedAt).Seconds()))
		}
		return []string{stDim.Render(label)}
	case timeline.BlockText:
		lines := markdown(timeline.EventText(block.Event), textWidth)
		if block.Streaming() && len(lines) > 0 {
			lines[len(lines)-1] += "▍"
		}
		return lines
	case timeline.BlockTools:
		var rows []string
		for _, call := range block.Calls {
			rows = append(rows, toolRows(call, opts)...)
		}
		return rows
	case timeline.BlockThreadSent:
		head := stPeer.Render("→ @"+timeline.Sanitize(block.Event.ToAgentName)) + "  "
		return hang(head, wrapLines(timeline.EventText(block.Event), textWidth-lipgloss.Width(head)))
	case timeline.BlockError:
		head := stBadgeErr.Render(" Error ") + " "
		return hang(head, styleAll(stErr, wrapLines(timeline.EventText(block.Event), textWidth-lipgloss.Width(head))))
	case timeline.BlockRequestEnd:
		if usage := timeline.UsageOf(block.Event); opts.Verbose && usage != nil {
			return []string{spread("", stSpan.Render(fmtUsage(*usage)), opts.rowWidth())}
		}
	}
	return nil
}

// toolRows draws a call's head row (name, preview, state) and, when expanded,
// its body.
func toolRows(call timeline.ToolCall, opts renderOpts) []string {
	rowWidth, lifecycle := opts.rowWidth(), call.Lifecycle()
	awaiting := lifecycle == timeline.AwaitingApproval
	expanded := opts.Verbose || awaiting
	started := call.Use.ProcessedAt
	took := ""
	if call.Result != nil && !started.IsZero() && !call.Result.ProcessedAt.IsZero() {
		took = stDim.Render(fmtDuration(call.Result.ProcessedAt.Sub(started)))
	}
	var state string
	switch lifecycle {
	case timeline.Running:
		state = stDim.Render(opts.SpinnerFrame)
		if !started.IsZero() && !opts.Now.IsZero() {
			state += " " + stDim.Render(fmtDuration(opts.Now.Sub(started)))
		}
	case timeline.AwaitingApproval:
		state = stWarn.Render(opts.SpinnerFrame + " awaiting approval")
	case timeline.Denied:
		state = spread(stBadgeDim.Render(" Denied "), took, 0)
	case timeline.Failed:
		state = spread(stBadgeErr.Render(" Error "), took, 0)
	default:
		state = took
	}

	// A call up for approval shows what will run, in full: the payload rather
	// than the model's description of it, and a body nothing is cut from.
	caret, caretStyle, nameStyle, preview := "▸", stDim, stBold, timeline.ToolPreview
	if expanded {
		caret = "▾"
	}
	if awaiting {
		caretStyle, nameStyle, preview = stWarn, stWarnBold, timeline.ToolPayloadPreview
	}
	head := caretStyle.Render(caret) + " " + stDim.Render(toolGlyph(call.Use.Name)) + " " + nameStyle.Render(timeline.ToolDisplayName(call.Use)) + "  "
	room := max(rowWidth-lipgloss.Width(head)-lipgloss.Width(state)-2, 1)
	rows := []string{spread(head+stDim.Render(clip(preview(call.Use), room)), state, rowWidth)}

	if lifecycle == timeline.Failed && !expanded && call.Result != nil {
		rows = append(rows, stBadgeErr.Render(" Error ")+" "+stErr.Render(clip(firstLine(timeline.EventText(*call.Result)), rowWidth-8)))
	}
	if !expanded {
		return rows
	}
	body := timeline.ToolBody(call)
	if !awaiting {
		body = timeline.TruncateDense(body)
	}
	for _, line := range body {
		for _, part := range wrapLines(line.Text, rowWidth-2) {
			rows = append(rows, stDim.Render("│ ")+styleBodyLine(line.Kind, part))
		}
	}
	return rows
}

func styleBodyLine(kind timeline.LineKind, text string) string {
	switch kind {
	case timeline.LineMeta, timeline.LineNote:
		return stDim.Render(text)
	case timeline.LineCmd:
		if rest, ok := strings.CutPrefix(text, "$ "); ok {
			return stDim.Render("$ ") + rest
		}
	case timeline.LineAdd, timeline.LineAllow:
		return stOK.Render(text)
	case timeline.LineDel, timeline.LineErr, timeline.LineDeny:
		return stErr.Render(text)
	}
	return text
}

// box draws an open-right card: "┌ head ───", "│ body", "└───".
func box(style lipgloss.Style, head string, body []string, width int) []string {
	top := " " + style.Render("┌") + " " + head + " "
	top += style.Render(strings.Repeat("─", max(0, width-3-lipgloss.Width(top))))
	out := []string{"", top}
	out = append(out, gutter(style, body)...)
	return append(out, " "+style.Render("└"+strings.Repeat("─", max(0, width-5))))
}

// gutter prefixes each line with the speaker-coloured "│" of a message bubble.
func gutter(style lipgloss.Style, lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = " " + style.Render("│") + " " + line
	}
	return out
}

// hang puts head before the first line and indents the rest to match.
func hang(head string, lines []string) []string {
	pad := strings.Repeat(" ", lipgloss.Width(head))
	out := make([]string, len(lines))
	for i, line := range lines {
		if i == 0 {
			out[i] = head + line
		} else {
			out[i] = pad + line
		}
	}
	return out
}

// styleAll renders every line in style, in place.
func styleAll(style lipgloss.Style, lines []string) []string {
	for i := range lines {
		lines[i] = style.Render(lines[i])
	}
	return lines
}
