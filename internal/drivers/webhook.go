package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type WebhookDispatcher struct {
	webhooks map[string]string
	mu       sync.RWMutex
	client   *http.Client
}

func NewWebhookDispatcher() *WebhookDispatcher {
	return &WebhookDispatcher{
		webhooks: make(map[string]string),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (w *WebhookDispatcher) AddWebhook(name, url string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.webhooks[name] = url
}

func (w *WebhookDispatcher) Dispatch(ctx context.Context, event WatchEvent) error {
	w.mu.RLock()
	urls := make([]string, 0, len(w.webhooks))
	for _, url := range w.webhooks {
		urls = append(urls, url)
	}
	w.mu.RUnlock()

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	// Fire webhooks in parallel
	var wg sync.WaitGroup
	for _, url := range urls {
		wg.Add(1)
		go func(endpoint string) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")

			_, _ = w.client.Do(req)
		}(url)
	}

	wg.Wait()
	return nil
}
