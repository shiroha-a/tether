// Package events fans out JSON events to UI subscribers.
package events

import (
	"encoding/json"
	"sync"
)

// Hub broadcasts events to all subscribers. Slow subscribers drop events
// instead of blocking publishers.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// Subscribe returns a channel of encoded events and a cancel function.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish encodes v as JSON and delivers it to every subscriber.
func (h *Hub) Publish(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
		}
	}
}
