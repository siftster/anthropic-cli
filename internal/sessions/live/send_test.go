package live

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmShowsPlaceholderUntilEchoOrFailure(t *testing.T) {
	use := event("tu1", "agent.tool_use", 1)
	api := newFakeAPI(t, use)
	api.streams <- newStream()
	api.sendGate = make(chan struct{})
	api.echoIDs = []string{"cf1"}
	c := openConn(t, api)
	nextUpdate(t, c) // tu1
	nextUpdate(t, c) // conn

	done := make(chan error, 1)
	go func() { done <- c.Confirm(context.Background(), "tu1", false, "nope") }()
	u := nextUpdate(t, c)
	assert.Equal(t, "local:confirm:tu1", u.Event.ID)
	assert.Equal(t, "user.tool_confirmation", u.Event.Type)
	assert.Equal(t, "tu1", u.Event.ToolUseID)
	assert.Equal(t, "deny", u.Event.Result)
	assert.Equal(t, "nope", u.Event.DenyMessage)
	assert.True(t, u.Event.ProcessedAt.IsZero())
	assert.Equal(t, []string{"tu1", "local:confirm:tu1?"}, ids(c.Snapshot()), "placeholder is visible before the POST returns")

	api.sendGate <- struct{}{}
	assert.Equal(t, "cf1", nextUpdate(t, c).Event.ID, "echo lands before the placeholder goes, so the call never re-reads as awaiting")
	u = nextUpdate(t, c)
	assert.True(t, u.Reordered, "placeholder retracted")
	require.NoError(t, <-done)
	assert.Equal(t, []string{"tu1", "cf1?"}, ids(c.Snapshot()))

	api.mu.Lock()
	api.sendStatus = 502
	api.mu.Unlock()
	go func() { done <- c.Confirm(context.Background(), "tu1", true, "") }()
	assert.Equal(t, "local:confirm:tu1", nextUpdate(t, c).Event.ID)
	api.sendGate <- struct{}{}
	assert.True(t, nextUpdate(t, c).Reordered)
	assert.Equal(t, 502, statusOf(<-done))
	assert.Equal(t, []string{"tu1", "cf1?"}, ids(c.Snapshot()), "failed verdict leaves no placeholder behind")
	expectNoUpdate(t, c)
}

func TestSendMethodsPostExpectedParams(t *testing.T) {
	api := newFakeAPI(t)
	api.streams <- newStream()
	c := openConn(t, api)
	ctx := context.Background()
	require.NoError(t, c.SendMessage(ctx, "hi"))
	require.NoError(t, c.Interrupt(ctx))
	require.NoError(t, c.Confirm(ctx, "tu1", true, "ignored"))
	require.NoError(t, c.Confirm(ctx, "tu2", false, ""))
	require.NoError(t, c.Confirm(ctx, "tu3", false, "nope"))

	var sent []map[string]any
	api.mu.Lock()
	raw, err := json.Marshal(api.sent)
	api.mu.Unlock()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Len(t, sent, 5)

	assert.Equal(t, "user.message", sent[0]["type"])
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "hi"}}, sent[0]["content"])
	assert.Equal(t, map[string]any{"type": "user.interrupt"}, sent[1])
	assert.Equal(t, map[string]any{"type": "user.tool_confirmation", "tool_use_id": "tu1", "result": "allow"}, sent[2])
	assert.Equal(t, map[string]any{"type": "user.tool_confirmation", "tool_use_id": "tu2", "result": "deny"}, sent[3])
	assert.Equal(t, "nope", sent[4]["deny_message"])
}
