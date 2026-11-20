package main

import (
	"testing"
	"time"
)

func TestEventHubPublishesToActiveSubscriber(t *testing.T) {
	hub := newEventHub()
	events, unsubscribe := hub.subscribe()
	defer unsubscribe()

	hub.publish("ssh:data", "hello")
	select {
	case event := <-events:
		if event.name != "ssh:data" || event.data != "hello" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not delivered")
	}
}
