package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHubDropsSubscriberThatFallsBehind(t *testing.T) {
	h := newHub(nil)
	slow, fast := h.subscribe(), h.subscribe()
	for range cap(slow) + 1 {
		h.broadcast(frame{"x", nil})
		<-fast
	}
	for range cap(slow) {
		<-slow
	}
	_, open := <-slow
	require.False(t, open, "a page that stops reading is cut loose; its EventSource reconnects from a snapshot")
	h.broadcast(frame{"x", nil})
	require.Equal(t, "x", (<-fast).name, "other pages keep streaming")
	h.unsubscribe(slow) // serveEvents still defers this after a drop; must not double-close
}
