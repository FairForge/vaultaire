// internal/webhooks/webhook_test.go
package webhooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &WebhookConfig{
			ID:       "wh-123",
			URL:      "https://example.com/webhook",
			Events:   []string{"object.created", "object.deleted"},
			Secret:   "webhook-secret",
			TenantID: "tenant-1",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty URL", func(t *testing.T) {
		config := &WebhookConfig{
			ID:     "wh-123",
			Events: []string{"object.created"},
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "URL")
	})

	t.Run("rejects non-HTTPS URL in production", func(t *testing.T) {
		config := &WebhookConfig{
			ID:           "wh-123",
			URL:          "http://example.com/webhook",
			Events:       []string{"object.created"},
			RequireHTTPS: true,
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "HTTPS")
	})

	t.Run("rejects empty events", func(t *testing.T) {
		config := &WebhookConfig{
			ID:  "wh-123",
			URL: "https://example.com/webhook",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "event")
	})
}

func TestNewWebhookManager(t *testing.T) {
	t.Run("creates manager with default config", func(t *testing.T) {
		manager := NewWebhookManager(nil)
		assert.NotNil(t, manager)
	})

	t.Run("creates manager with custom config", func(t *testing.T) {
		config := &ManagerConfig{
			MaxRetries:     5,
			RetryInterval:  time.Minute,
			RequestTimeout: 10 * time.Second,
			MaxConcurrent:  50,
		}
		manager := NewWebhookManager(config)
		assert.NotNil(t, manager)
		assert.Equal(t, 5, manager.Config().MaxRetries)
	})
}

func TestWebhookManager_Register(t *testing.T) {
	manager := NewWebhookManager(nil)

	t.Run("registers webhook", func(t *testing.T) {
		config := &WebhookConfig{
			ID:       "wh-123",
			URL:      "https://example.com/webhook",
			Events:   []string{"object.created"},
			TenantID: "tenant-1",
		}
		err := manager.Register(config)
		assert.NoError(t, err)

		registered := manager.Get("wh-123")
		assert.NotNil(t, registered)
		assert.Equal(t, "https://example.com/webhook", registered.URL)
	})

	t.Run("rejects duplicate ID", func(t *testing.T) {
		config := &WebhookConfig{
			ID:       "wh-dup",
			URL:      "https://example.com/webhook",
			Events:   []string{"object.created"},
			TenantID: "tenant-1",
		}
		err := manager.Register(config)
		require.NoError(t, err)

		err = manager.Register(config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})
}

func TestWebhookManager_Unregister(t *testing.T) {
	manager := NewWebhookManager(nil)

	t.Run("unregisters webhook", func(t *testing.T) {
		config := &WebhookConfig{
			ID:       "wh-to-remove",
			URL:      "https://example.com/webhook",
			Events:   []string{"object.created"},
			TenantID: "tenant-1",
		}
		_ = manager.Register(config)

		err := manager.Unregister("wh-to-remove")
		assert.NoError(t, err)

		assert.Nil(t, manager.Get("wh-to-remove"))
	})

	t.Run("returns error for unknown webhook", func(t *testing.T) {
		err := manager.Unregister("unknown-id")
		assert.Error(t, err)
	})
}

func TestWebhookManager_ListByTenant(t *testing.T) {
	manager := NewWebhookManager(nil)

	_ = manager.Register(&WebhookConfig{ID: "wh-1", URL: "https://a.com", Events: []string{"e1"}, TenantID: "tenant-a"})
	_ = manager.Register(&WebhookConfig{ID: "wh-2", URL: "https://b.com", Events: []string{"e1"}, TenantID: "tenant-a"})
	_ = manager.Register(&WebhookConfig{ID: "wh-3", URL: "https://c.com", Events: []string{"e1"}, TenantID: "tenant-b"})

	t.Run("lists webhooks for tenant", func(t *testing.T) {
		webhooks := manager.ListByTenant("tenant-a")
		assert.Len(t, webhooks, 2)
	})

	t.Run("returns empty for unknown tenant", func(t *testing.T) {
		webhooks := manager.ListByTenant("tenant-unknown")
		assert.Empty(t, webhooks)
	})
}

func TestWebhookManager_Dispatch(t *testing.T) {
	t.Run("dispatches event to matching webhooks", func(t *testing.T) {
		received := make(chan *WebhookPayload, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload WebhookPayload
			_ = json.NewDecoder(r.Body).Decode(&payload)
			received <- &payload
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		manager := NewWebhookManager(nil)
		_ = manager.Register(&WebhookConfig{
			ID:       "wh-dispatch",
			URL:      server.URL,
			Events:   []string{"object.created"},
			TenantID: "tenant-1",
		})

		event := &Event{
			Type:     "object.created",
			TenantID: "tenant-1",
			Data: map[string]interface{}{
				"bucket": "test-bucket",
				"key":    "test-key",
			},
		}

		err := manager.Dispatch(context.Background(), event)
		assert.NoError(t, err)

		select {
		case payload := <-received:
			assert.Equal(t, "object.created", payload.Type)
			assert.Equal(t, "tenant-1", payload.TenantID)
		case <-time.After(2 * time.Second):
			t.Fatal("webhook not received")
		}
	})

	t.Run("skips non-matching events", func(t *testing.T) {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		manager := NewWebhookManager(nil)
		_ = manager.Register(&WebhookConfig{
			ID:       "wh-filter",
			URL:      server.URL,
			Events:   []string{"object.deleted"},
			TenantID: "tenant-1",
		})

		event := &Event{
			Type:     "object.created", // Different event
			TenantID: "tenant-1",
		}

		_ = manager.Dispatch(context.Background(), event)
		time.Sleep(100 * time.Millisecond)
		assert.False(t, called)
	})
}

func TestWebhookManager_Signing(t *testing.T) {
	manager := NewWebhookManager(nil)

	t.Run("generates signature header", func(t *testing.T) {
		payload := []byte(`{"type":"object.created"}`)
		secret := "webhook-secret"

		signature := manager.GenerateSignature(payload, secret)
		assert.NotEmpty(t, signature)
		assert.Contains(t, signature, "sha256=")
	})

	t.Run("verifies valid signature", func(t *testing.T) {
		payload := []byte(`{"type":"object.created"}`)
		secret := "webhook-secret"

		signature := manager.GenerateSignature(payload, secret)
		valid := manager.VerifySignature(payload, signature, secret)
		assert.True(t, valid)
	})

	t.Run("rejects invalid signature", func(t *testing.T) {
		payload := []byte(`{"type":"object.created"}`)
		valid := manager.VerifySignature(payload, "sha256=invalid", "secret")
		assert.False(t, valid)
	})
}

func TestWebhookManager_Retry(t *testing.T) {
	t.Run("retries on failure", func(t *testing.T) {
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			if attempts < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		config := &ManagerConfig{
			MaxRetries:    3,
			RetryInterval: 10 * time.Millisecond,
		}
		manager := NewWebhookManager(config)
		_ = manager.Register(&WebhookConfig{
			ID:       "wh-retry",
			URL:      server.URL,
			Events:   []string{"test.event"},
			TenantID: "tenant-1",
		})

		event := &Event{Type: "test.event", TenantID: "tenant-1"}
		err := manager.DispatchSync(context.Background(), event)
		assert.NoError(t, err)
		assert.Equal(t, 3, attempts)
	})
}

func TestWebhookPayload(t *testing.T) {
	t.Run("includes standard fields", func(t *testing.T) {
		payload := NewWebhookPayload("object.created", "tenant-1", map[string]interface{}{
			"bucket": "test",
		})

		assert.NotEmpty(t, payload.ID)
		assert.Equal(t, "object.created", payload.Type)
		assert.Equal(t, "tenant-1", payload.TenantID)
		assert.False(t, payload.Timestamp.IsZero())
		assert.Equal(t, "test", payload.Data["bucket"])
	})
}

func TestWebhookDelivery(t *testing.T) {
	t.Run("tracks delivery status", func(t *testing.T) {
		delivery := &WebhookDelivery{
			ID:         "del-123",
			WebhookID:  "wh-123",
			EventID:    "evt-456",
			Status:     DeliveryStatusSuccess,
			StatusCode: 200,
			Duration:   150 * time.Millisecond,
			Attempts:   1,
		}

		assert.Equal(t, DeliveryStatusSuccess, delivery.Status)
		assert.Equal(t, 200, delivery.StatusCode)
	})
}

func TestWebhookManager_DeliveryHistory(t *testing.T) {
	manager := NewWebhookManager(nil)

	t.Run("records delivery attempts", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		_ = manager.Register(&WebhookConfig{
			ID:       "wh-history",
			URL:      server.URL,
			Events:   []string{"test.event"},
			TenantID: "tenant-1",
		})

		event := &Event{Type: "test.event", TenantID: "tenant-1"}
		_ = manager.DispatchSync(context.Background(), event)

		history := manager.GetDeliveryHistory("wh-history", 10)
		assert.NotEmpty(t, history)
		assert.Equal(t, DeliveryStatusSuccess, history[0].Status)
	})
}

func TestEventTypes(t *testing.T) {
	t.Run("defines standard event types", func(t *testing.T) {
		assert.Equal(t, "object.created", EventObjectCreated)
		assert.Equal(t, "object.deleted", EventObjectDeleted)
		assert.Equal(t, "object.accessed", EventObjectAccessed)
		assert.Equal(t, "bucket.created", EventBucketCreated)
		assert.Equal(t, "bucket.deleted", EventBucketDeleted)
	})
}

func TestWebhookManager_Wildcard(t *testing.T) {
	t.Run("wildcard matches all events", func(t *testing.T) {
		received := make(chan string, 3)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload WebhookPayload
			_ = json.NewDecoder(r.Body).Decode(&payload)
			received <- payload.Type
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		manager := NewWebhookManager(nil)
		_ = manager.Register(&WebhookConfig{
			ID:       "wh-wildcard",
			URL:      server.URL,
			Events:   []string{"*"},
			TenantID: "tenant-1",
		})

		_ = manager.DispatchSync(context.Background(), &Event{Type: "object.created", TenantID: "tenant-1"})
		_ = manager.DispatchSync(context.Background(), &Event{Type: "object.deleted", TenantID: "tenant-1"})

		assert.Len(t, received, 2)
	})
}

func TestDefaultManagerConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultManagerConfig()
		assert.Equal(t, 3, config.MaxRetries)
		assert.Equal(t, 30*time.Second, config.RequestTimeout)
		assert.Equal(t, 100, config.MaxConcurrent)
	})
}
