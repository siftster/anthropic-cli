package tui

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anthropics/anthropic-cli/internal/sessions/live"
	"github.com/anthropics/anthropic-cli/internal/sessions/timeline"
)

// model is the bubbletea model: everything on screen, and the session state it
// is drawn from.
type model struct {
	ctx     context.Context
	session Session
	fold    *timeline.Fold

	viewport   viewport.Model
	composer   textarea.Model
	spinner    spinner.Model
	transcript transcript

	// Status bar and footer.
	title, agent          string
	sessionID, consoleURL string
	status                timeline.Status

	// Connection.
	connState  string
	lastErr    error // why the stream last dropped
	fatal      error // set once Updates closes on an error: nothing more will arrive
	backfilled bool
	eventCount int

	// The strip between the rules.
	mode            mode
	selected        int       // highlighted approval option
	approvalFor     string    // the call the approval prompt (and a typed deny reason) is aimed at
	approvalShownAt time.Time // when the prompt last appeared or changed target
	draft           string    // composer text set aside while the prompt has the strip

	// Layout and display.
	verbose       bool
	follow        bool // keep the viewport pinned to the bottom
	flash         string
	flashUntil    time.Time
	width, height int
	composerRows  int    // composer height at the last layout
	content       string // rendered transcript, before viewport padding
	animating     bool   // content shows a spinner frame

	renders int // counted so tests can pin the render budget
	now     func() time.Time
}

const (
	chromeRows     = 5 // status line, two rules, hint line, meta line
	maxUpdateBatch = 256
	flashDuration  = 4 * time.Second
)

type (
	// updatesMsg is everything the channel had buffered, so a backfill of
	// thousands of events costs a handful of renders rather than one each.
	updatesMsg struct {
		batch  []live.Update
		closed bool
	}
	// sendFailedMsg reports a sender's error. A non-empty draft is an unsent
	// message to put back in the composer.
	sendFailedMsg struct {
		err   error
		draft string
	}
)

func newModel(ctx context.Context, session Session, opts Options) *model {
	composer := textarea.New()
	composer.ShowLineNumbers = false
	composer.CharLimit = 0
	composer.SetPromptFunc(3, func(row int) string {
		if row == 0 {
			return " > "
		}
		return "   "
	})
	composer.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	focused, blurred := textarea.DefaultStyles()
	focused.CursorLine, focused.Prompt = lipgloss.NewStyle(), stBold
	composer.FocusedStyle, composer.BlurredStyle = focused, blurred
	composer.SetHeight(1)
	composer.Focus()

	transcriptView := viewport.New(0, 0)
	transcriptView.KeyMap = viewport.KeyMap{} // letters belong to the composer; wheel stays on

	sess := session.Session()
	m := &model{
		ctx:        ctx,
		session:    session,
		fold:       timeline.NewFold(),
		viewport:   transcriptView,
		composer:   composer,
		spinner:    spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		sessionID:  sess.ID,
		consoleURL: opts.ConsoleURL,
		connState:  "connecting…",
		verbose:    opts.Verbose,
		follow:     true,
		now:        time.Now,
	}
	m.setSession(sess)
	m.status.State = string(sess.Status)
	return m
}

// Init starts the spinner, the cursor blink and the wait for session updates.
func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, textarea.Blink, m.waitUpdates())
}

// waitUpdates blocks for one update, then takes whatever else is already
// buffered so the lot is applied under a single render.
func (m *model) waitUpdates() tea.Cmd {
	updates := m.session.Updates()
	return func() tea.Msg {
		first, ok := <-updates
		if !ok {
			return updatesMsg{closed: true}
		}
		batch := []live.Update{first}
		for len(batch) < maxUpdateBatch {
			select {
			case next, ok := <-updates:
				if !ok {
					return updatesMsg{batch: batch, closed: true}
				}
				batch = append(batch, next)
			default:
				return updatesMsg{batch: batch}
			}
		}
		return updatesMsg{batch: batch}
	}
}

// Update handles one bubbletea message.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.composer.SetWidth(max(msg.Width-1, 4))
		m.fitComposer()
		m.render()
	case updatesMsg:
		m.applyUpdates(msg.batch)
		if !msg.closed {
			m.refresh()
			return m, m.waitUpdates()
		}
		if m.lastErr == nil {
			return m, tea.Quit
		}
		m.fatal, m.connState = m.lastErr, "disconnected"
		m.refresh()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.animating {
			m.render()
		}
		return m, cmd
	case sendFailedMsg:
		m.flash, m.flashUntil = msg.err.Error(), m.now().Add(flashDuration)
		// Never overwrite what the user has typed since.
		if msg.draft != "" {
			switch {
			case m.mode != modeCompose:
				m.draft = cmp.Or(m.draft, msg.draft)
			case m.composer.Value() == "":
				m.composer.SetValue(msg.draft)
				m.fitComposer()
			}
		}
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, m.scrolled(cmd)
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	default:
		return m, m.forwardToComposer(msg)
	}
	return m, nil
}

// applyUpdates folds a batch into state; the caller renders once. A reorder
// anywhere in the batch costs one rebuild from Snapshot at the end, which
// already holds every event the batch carried.
func (m *model) applyUpdates(batch []live.Update) {
	reordered := false
	for _, u := range batch {
		reordered = reordered || u.Reordered
		switch u.Kind {
		case live.KindEvent:
			if !reordered {
				m.fold.Upsert(u.Event)
			}
			m.eventCount++
			if !m.backfilled {
				m.connState = fmt.Sprintf("loading history… %d events", m.eventCount)
			}
		case live.KindSession:
			m.setSession(u.Session)
		case live.KindConn:
			m.connState, m.lastErr = "live", u.Err
			if !u.Connected {
				m.connState = "reconnecting…"
			}
			if u.Backfilled {
				m.backfilled, m.follow = true, true
			}
		}
	}
	if reordered {
		m.fold.Reset(m.session.Snapshot())
	}
}

// setSession takes the title and agent name for the status bar.
func (m *model) setSession(sess *anthropic.BetaManagedAgentsSession) {
	m.title, m.agent = oneLine(timeline.Sanitize(sess.Title)), oneLine(timeline.Sanitize(sess.Agent.Name))
	if m.title == "" {
		m.title = sess.ID
	}
}

// refresh re-derives status and mode from the fold, then re-renders. Nothing
// is drawn until backfill completes: rendering per event would be quadratic.
func (m *model) refresh() {
	m.status = m.fold.Status()
	m.status.State = cmp.Or(m.status.State, string(m.session.Session().Status))
	if m.mode == modeDenyReason && !m.isPending(m.approvalFor) {
		m.clearComposer("")
		m.showApproval()
	}
	switch {
	case m.fatal != nil || m.status.State == "terminated" || m.status.State == "deleted":
		m.mode = modeClosed
	case len(m.status.Pending) > 0:
		if m.mode != modeDenyReason {
			m.showApproval()
		}
	case m.mode == modeApproval:
		m.showComposer()
	}
	if m.backfilled || m.fatal != nil {
		m.render()
	} else {
		m.layout()
	}
}

// render redraws the transcript. It keeps rows for nodes the fold has not
// touched, so batching plus the backfill gate keep this affordable.
func (m *model) render() {
	m.content, m.animating = m.transcript.render(m.fold.Nodes(), m.fold.TakeDirty(),
		renderOpts{Width: m.width, Verbose: m.verbose, Agent: m.agent, SpinnerFrame: m.spinner.View(), Now: m.now()})
	m.renders++
	m.layout()
}

// layout gives the viewport whatever the strip between the rules leaves, and
// sits a short transcript on the rule rather than under the status line.
func (m *model) layout() {
	m.viewport.Width, m.viewport.Height = m.width, max(m.height-chromeRows-lipgloss.Height(m.inputStrip()), 1)
	m.viewport.SetContent(strings.Repeat("\n", max(0, m.viewport.Height-lipgloss.Height(m.content))) + m.content)
	if m.follow || m.viewport.PastBottom() {
		m.viewport.GotoBottom()
	}
}

// View stacks the screen: status bar, transcript, the input strip between two
// rules, key hints and the footer.
func (m *model) View() string {
	var usage *timeline.Usage
	if m.verbose {
		u := m.fold.TotalUsage()
		usage = &u
	}
	ruleStyle := stDim
	if m.mode == modeApproval || m.mode == modeDenyReason {
		ruleStyle = stWarn
	}
	rule := ruleStyle.Render(strings.Repeat("─", max(m.width, 0)))
	flash := ""
	if m.now().Before(m.flashUntil) {
		flash = m.flash
	}
	return strings.Join([]string{
		statusLine(m.title, m.agent, m.status, m.connState, usage, m.spinner.View(), m.width),
		m.viewport.View(),
		rule,
		m.inputStrip(),
		rule,
		hintLine(m.mode, m.verbose, m.status.State == "running", flash, m.width),
		metaLine(m.sessionID, m.consoleURL, m.width),
	}, "\n")
}

// inputStrip is what sits between the rules: the composer, the approval
// prompt, or why the session is closed.
func (m *model) inputStrip() string {
	switch m.mode {
	case modeApproval:
		if pending := m.status.Pending; len(pending) > 0 {
			return approvalBox(pending[0], len(pending), m.selected, m.width)
		}
	case modeClosed:
		if m.fatal != nil {
			return " " + stErr.Render("Disconnected: "+m.fatal.Error()+" — Ctrl-C to exit")
		}
		return " " + stDim.Render("Session "+m.status.State+" — Ctrl-C to exit")
	}
	return m.composer.View()
}
