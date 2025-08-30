package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookDispatcher(t *testing.T) {
	received := make(chan bool, 1)

	// Mock webhook endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dispatcher := NewWebhookDispatcher()
	dispatcher.AddWebhook("test", server.URL)

	event := WatchEvent{
		Type: WatchEventCreate,
		Path: "/test/file.txt",
	}

	err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Failed to dispatch webhook: %v", err)
	}

	select {
	case <-received:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Webhook not received within timeout")
	}
}
