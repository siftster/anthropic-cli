package live

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// sendTimeout bounds one POST of an event, on top of the caller's ctx.
const sendTimeout = 30 * time.Second

// SendMessage posts a user.message. SendMessage, Interrupt and Confirm block
// on the network and then on delivering an Update, so never call them from
// the goroutine that drains Updates().
func (c *Conn) SendMessage(ctx context.Context, text string) error {
	ev := anthropic.BetaManagedAgentsEventParamsOfUserMessage([]anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
		OfText: &anthropic.BetaManagedAgentsTextBlockParam{Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: text},
	}})
	ev.OfUserMessage.Type = anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage
	return c.send(ctx, ev)
}

// Interrupt posts a user.interrupt, asking the agent to stop its current turn.
func (c *Conn) Interrupt(ctx context.Context) error {
	return c.send(ctx, anthropic.BetaManagedAgentsEventParamsOfUserInterrupt(anthropic.BetaManagedAgentsUserInterruptEventParamsTypeUserInterrupt))
}

// Confirm answers a pending tool call. denyMessage is only sent with a
// denial. The verdict is stored as a queued placeholder before the POST so
// the call stops reading as awaiting approval at once; the server's echo (or
// a failure) then replaces it.
func (c *Conn) Confirm(ctx context.Context, toolUseID string, allow bool, denyMessage string) error {
	result := anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultAllow
	if !allow {
		result = anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultDeny
	}
	ev := anthropic.BetaManagedAgentsEventParamsOfUserToolConfirmation(result, toolUseID, anthropic.BetaManagedAgentsUserToolConfirmationEventParamsTypeUserToolConfirmation)
	if !allow && denyMessage != "" {
		ev.OfUserToolConfirmation.DenyMessage = anthropic.String(denyMessage)
	}
	placeholder := localEvent("local:confirm:"+toolUseID, ev.OfUserToolConfirmation)
	c.ingest(placeholder)
	echoes, err := c.post(ctx, ev)
	// Echo before retracting the placeholder, so there is no gap.
	c.ingestEchoes(echoes)
	c.retract(placeholder.ID)
	return err
}

// retract drops a local placeholder and has consumers rebuild without it.
func (c *Conn) retract(id string) {
	c.mu.Lock()
	c.store.remove(id)
	c.mu.Unlock()
	c.emitReordered()
}

// send inserts the server's echo optimistically: it carries the real id and
// no processed_at, so it shows as queued until the stream delivers the
// processed event with the same id.
func (c *Conn) send(ctx context.Context, ev anthropic.BetaManagedAgentsEventParamsUnion) error {
	echoes, err := c.post(ctx, ev)
	c.ingestEchoes(echoes)
	return err
}

// post returns the server's echo of the accepted event: id assigned, not yet
// processed.
func (c *Conn) post(ctx context.Context, ev anthropic.BetaManagedAgentsEventParamsUnion) ([]anthropic.BetaManagedAgentsSendSessionEventsDataUnion, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	res, err := c.client.Beta.Sessions.Events.Send(ctx, c.sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{ev},
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// ingestEchoes takes the echo union, the event union under a different Go type.
func (c *Conn) ingestEchoes(echoes []anthropic.BetaManagedAgentsSendSessionEventsDataUnion) {
	for _, echo := range echoes {
		var ev Event
		if json.Unmarshal([]byte(echo.RawJSON()), &ev) == nil && ev.ID != "" {
			c.ingest(ev)
		}
	}
}

// localEvent renders request params through JSON under a client-side id, so
// the result reads like a wire event the server has not processed yet.
func localEvent(id string, params any) Event {
	fields := map[string]any{}
	raw, _ := json.Marshal(params)
	_ = json.Unmarshal(raw, &fields)
	fields["id"] = id
	raw, _ = json.Marshal(fields)
	var ev Event
	_ = json.Unmarshal(raw, &ev)
	return ev
}
