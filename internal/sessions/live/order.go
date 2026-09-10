package live

import (
	"cmp"
	"slices"
	"strings"
)

// orderedEvents keeps events sorted by sortKey with O(log n) placement; keys
// are unique because seq is, so a key also locates its event.
type orderedEvents struct {
	events  []Event
	keys    map[string]sortKey
	lastSeq uint64
}

func newOrderedEvents() *orderedEvents { return &orderedEvents{keys: map[string]sortKey{}} }

// put inserts or replaces by id. moved reports that the consumer contract
// (a fresh id goes last but ahead of queued user events, a known id is
// replaced in place) would not reproduce the list. A fresh event landing
// ahead of nothing but the queued tail is the normal case while an echo
// waits to be ingested, not a reorder.
func (o *orderedEvents) put(ev Event) (fresh, moved bool) {
	oldKey, known := o.keys[ev.ID]
	if !known {
		o.lastSeq++
		at := o.insert(ev, keyFor(ev, o.lastSeq))
		return true, at+1 < len(o.events) && o.keys[o.events[at+1].ID].group != groupQueued
	}
	from := o.indexOf(oldKey)
	key := keyFor(ev, oldKey.seq)
	if key == oldKey {
		o.events[from] = ev
		return false, false
	}
	o.events = slices.Delete(o.events, from, from+1)
	return false, o.insert(ev, key) != from
}

func (o *orderedEvents) remove(id string) {
	key, ok := o.keys[id]
	if !ok {
		return
	}
	at := o.indexOf(key)
	o.events = slices.Delete(o.events, at, at+1)
	delete(o.keys, id)
}

func (o *orderedEvents) hasProcessed(id string) bool {
	key, ok := o.keys[id]
	return ok && key.group == groupProcessed
}

// insert places ev at key's position and returns that index.
func (o *orderedEvents) insert(ev Event, key sortKey) int {
	at := o.indexOf(key)
	o.events = slices.Insert(o.events, at, ev)
	o.keys[ev.ID] = key
	return at
}

// indexOf returns where key sits, or would be inserted, in events.
func (o *orderedEvents) indexOf(key sortKey) int {
	at, _ := slices.BinarySearchFunc(o.events, key, func(ev Event, key sortKey) int {
		return compareKeys(o.keys[ev.ID], key)
	})
	return at
}

// sortKey orders events the way the web viewer does, so both views agree:
// processed events by processed_at (Go's time parse keeps the API's
// microseconds), then everything still pending — streaming agent.* previews
// first, queued user.* after — with arrival order breaking ties throughout.
type sortKey struct {
	group          int
	processedNanos int64
	seq            uint64
}

const (
	groupProcessed = iota
	groupStreaming // pending agent.*
	groupQueued    // pending user.*
)

func keyFor(ev Event, seq uint64) sortKey {
	switch {
	case !ev.ProcessedAt.IsZero():
		return sortKey{groupProcessed, ev.ProcessedAt.UnixNano(), seq}
	case strings.HasPrefix(ev.Type, "agent."):
		return sortKey{group: groupStreaming, seq: seq}
	default:
		return sortKey{group: groupQueued, seq: seq}
	}
}

func compareKeys(a, b sortKey) int {
	return cmp.Or(
		cmp.Compare(a.group, b.group),
		cmp.Compare(a.processedNanos, b.processedNanos),
		cmp.Compare(a.seq, b.seq),
	)
}
