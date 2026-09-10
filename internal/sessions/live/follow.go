package live

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Tunables; tests shorten them.
var (
	// previewInterval is how often streaming previews are flushed.
	previewInterval = time.Second / 30
	// backoffBase is the initial reconnect delay; it doubles up to 32× base.
	backoffBase = 250 * time.Millisecond
	// pollInterval is how often a source without a stream is re-listed.
	pollInterval = 5 * time.Second
)

// streamBuffer holds frames that arrive while backfill is still being
// delivered; beyond it the SSE connection simply back-pressures.
const streamBuffer = 1024

var errStreamEnded = errors.New("event stream ended")

// run keeps the Conn attached, reconnecting with jittered exponential backoff,
// until ctx is done, a fatal error arrives or the source is finished.
func (c *Conn) run() {
	defer close(c.done)
	defer func() {
		c.emitMu.Lock()
		c.closed = true
		close(c.updates)
		c.emitMu.Unlock()
	}()
	defer c.cancel() // on a fatal exit, release any writer parked in emit
	delay := backoffBase
	for {
		delivered, err := c.attach()
		// attach only returns nil once a once/finishing source is caught up.
		if c.ctx.Err() != nil || err == nil {
			return
		}
		c.emit(Update{Kind: KindConn, Connected: false, Err: err, Reordered: c.sweepPreviews()})
		if isFatal(err) {
			return
		}
		if delivered {
			delay = backoffBase
		}
		jittered := time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		select {
		case <-time.After(jittered):
		case <-c.ctx.Done():
			return
		}
		delay = min(2*delay, 32*backoffBase)
	}
}

func isFatal(err error) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403 || apiErr.StatusCode == 404)
}

// attach subscribes to the stream before fetching history and lets frames
// buffer meanwhile, so nothing processed between the two is lost. delivered
// reports whether this attach delivered anything (a frame, or a successful
// poll), which resets the backoff.
func (c *Conn) attach() (delivered bool, err error) {
	if c.src.stream == nil || c.isFinishing() {
		return c.poll()
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	relay, err := c.src.stream(ctx)
	if err != nil {
		return false, err
	}
	frames := make(chan streamEvent, streamBuffer)
	relayDone := make(chan struct{})
	var streamErr error
	go func() {
		defer close(relayDone)
		defer close(frames)
		streamErr = relay(frames)
	}()
	defer func() { cancel(); <-relayDone }()

	if err := c.catchUp(); err != nil {
		return false, err
	}
	c.markConnected()

	flushTimer, flushArmed := time.NewTimer(previewInterval), false
	flushTimer.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				<-relayDone
				if streamErr == nil {
					streamErr = errStreamEnded
				}
				return delivered, streamErr
			}
			delivered = true
			if c.handleFrame(frame) && !flushArmed {
				flushTimer.Reset(previewInterval)
				flushArmed = true
			}
		case <-flushTimer.C:
			flushArmed = false
			c.flushPreviews()
		case <-c.finishing:
			return delivered, c.catchUp()
		case <-c.ctx.Done():
			return delivered, c.ctx.Err()
		}
	}
}

// finish asks for one final catch-up, after which the Conn stops on its own
// (Updates closes; Snapshot keeps what it had).
func (c *Conn) finish() { c.finishOnce.Do(func() { close(c.finishing) }) }

func (c *Conn) isFinishing() bool {
	select {
	case <-c.finishing:
		return true
	default:
		return false
	}
}

// poll stands in for a stream: catch up, report connected once, and repeat
// every pollInterval. It returns nil only when the source is done for good.
func (c *Conn) poll() (delivered bool, err error) {
	for {
		if err := c.catchUp(); err != nil {
			return delivered, err
		}
		if !delivered {
			c.markConnected()
			delivered = true
		}
		if c.src.once || c.isFinishing() {
			return true, nil
		}
		select {
		case <-time.After(pollInterval):
		case <-c.finishing:
		case <-c.ctx.Done():
			return delivered, c.ctx.Err()
		}
	}
}

// catchUp delivers full history on first success; afterwards it pages
// newest-first only until it reaches an event we already hold or, on a
// source that cannot, re-reads everything and lets ingest drop what we hold.
func (c *Conn) catchUp() error {
	if !c.backfilled || !c.src.canListNewestFirst {
		it := c.src.list(c.ctx, false)
		for it.Next() {
			c.ingest(it.Current())
		}
		return it.Err()
	}
	it := c.src.list(c.ctx, true)
	var missed []Event
	for it.Next() {
		ev := it.Current()
		if c.hasProcessed(ev.ID) {
			break
		}
		missed = append(missed, ev)
	}
	if err := it.Err(); err != nil {
		return err
	}
	slices.Reverse(missed)
	for _, ev := range missed {
		c.ingest(ev)
	}
	return nil
}

func (c *Conn) markConnected() {
	first := !c.backfilled
	c.backfilled = true
	c.emit(Update{Kind: KindConn, Connected: true, Backfilled: first})
}

// handleFrame applies one stream frame. needsFlush reports that a delta was
// accumulated and not yet emitted.
func (c *Conn) handleFrame(frame streamEvent) (needsFlush bool) {
	switch frame.Type {
	case "event_start":
		id := frame.Event.ID
		// A replayed event_start must not wipe text already accumulated.
		if _, known := c.previews[id]; known || frame.Event.Type != "agent.message" || c.hasProcessed(id) {
			return false
		}
		c.acc.Accumulate(frame)
		c.emitPreview(id)
		return false
	case "event_delta":
		if _, known := c.previews[frame.EventID]; !known {
			return false
		}
		c.acc.Accumulate(frame)
		c.previews[frame.EventID] = true
		return true
	}
	c.acc.Accumulate(frame)
	var ev Event
	if json.Unmarshal([]byte(frame.RawJSON()), &ev) != nil {
		return false
	}
	if frame.Type == "span.model_request_end" && c.dropStalePreviews() {
		c.emitReordered()
	}
	c.ingest(ev)
	if ev.Type == "agent.message" {
		delete(c.previews, ev.ID)
		delete(c.acc.AgentMessages, ev.ID)
	}
	return false
}
