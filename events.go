package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type appEvent struct {
	name string
	data any
}

type eventHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan appEvent
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[uint64]chan appEvent)}
}

func (h *eventHub) subscribe() (<-chan appEvent, func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	ch := make(chan appEvent, 256)
	h.subscribers[id] = ch
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if subscriber, ok := h.subscribers[id]; ok {
				delete(h.subscribers, id)
				close(subscriber)
			}
			h.mu.Unlock()
		})
	}
}

func (h *eventHub) publish(name string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, subscriber := range h.subscribers {
		select {
		case subscriber <- appEvent{name: name, data: data}:
		default:
			delete(h.subscribers, id)
			close(subscriber)
		}
	}
}

func (h *eventHub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers) > 0
}

func (h *eventHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ch, unsubscribe := h.subscribe()
	defer unsubscribe()
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case event, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(event.data)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, data); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
