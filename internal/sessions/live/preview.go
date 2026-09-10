package live

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
)

// emitPreview publishes the text accumulated so far for the streaming
// agent.message id and marks it flushed.
func (c *Conn) emitPreview(id string) {
	c.previews[id] = false
	if msg, ok := c.acc.AgentMessages[id]; ok {
		c.ingest(previewEvent(msg))
	}
}

func (c *Conn) flushPreviews() {
	for id, unflushed := range c.previews {
		if unflushed {
			c.emitPreview(id)
		}
	}
}

// dropStalePreviews forgets previews the accumulator no longer holds; at a
// span.model_request_end that is those whose model request ended without
// producing them.
func (c *Conn) dropStalePreviews() (dropped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.previews {
		if _, accumulating := c.acc.AgentMessages[id]; accumulating {
			continue
		}
		delete(c.previews, id)
		if c.store.hasProcessed(id) {
			continue
		}
		c.store.remove(id)
		dropped = true
	}
	return dropped
}

// sweepPreviews runs between attaches: a dead stream's unfinished previews
// will not be continued by the next one, so start a fresh accumulator and
// drop them all (catch-up restores any that did complete).
func (c *Conn) sweepPreviews() (dropped bool) {
	c.acc = anthropic.BetaManagedAgentsEventAccumulator{}
	return c.dropStalePreviews()
}

// previewEvent renders an accumulator snapshot through JSON so the result is
// indistinguishable from a wire event (RawJSON, AsAny) except for the absent
// processed_at.
func previewEvent(msg anthropic.BetaManagedAgentsAgentMessageEvent) Event {
	type textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	blocks := make([]textBlock, len(msg.Content))
	for i, block := range msg.Content {
		blocks[i] = textBlock{string(block.Type), block.Text}
	}
	raw, _ := json.Marshal(struct {
		ID      string      `json:"id"`
		Type    string      `json:"type"`
		Content []textBlock `json:"content"`
	}{msg.ID, string(msg.Type), blocks})
	var ev Event
	_ = json.Unmarshal(raw, &ev)
	return ev
}
