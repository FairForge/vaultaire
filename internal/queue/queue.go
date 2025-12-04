package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// QueueConfig configures a message queue
type QueueConfig struct {
	Name              string        `json:"name"`
	MaxRetries        int           `json:"max_retries"`
	VisibilityTimeout time.Duration `json:"visibility_timeout"`
	DeadLetterQueue   string        `json:"dead_letter_queue,omitempty"`
	PriorityLevels    int           `json:"priority_levels"`
	MaxSize           int64         `json:"max_size"`
}

// DefaultQueueConfig returns sensible defaults
func DefaultQueueConfig() *QueueConfig {
	return &QueueConfig{
		MaxRetries:        3,
		VisibilityTimeout: 30 * time.Second,
		PriorityLevels:    1,
		MaxSize:           1024 * 1024 * 1024, // 1GB
	}
}

// Validate checks configuration
func (c *QueueConfig) Validate() error {
	if c.Name == "" {
		return errors.New("queue: name is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *QueueConfig) ApplyDefaults() {
	defaults := DefaultQueueConfig()
	if c.MaxRetries == 0 {
		c.MaxRetries = defaults.MaxRetries
	}
	if c.VisibilityTimeout == 0 {
		c.VisibilityTimeout = defaults.VisibilityTimeout
	}
	if c.PriorityLevels == 0 {
		c.PriorityLevels = defaults.PriorityLevels
	}
}

// Message represents a queue message
type Message struct {
	ID            string
	Body          []byte
	Metadata      map[string]string
	Priority      int
	Delay         time.Duration
	RetryCount    int
	Timestamp     time.Time
	ReceiptHandle string
	visibleAt     time.Time
	enqueuedAt    time.Time
}

// NewMessage creates a new message
func NewMessage(body []byte) *Message {
	return &Message{
		ID:        uuid.New().String(),
		Body:      body,
		Metadata:  make(map[string]string),
		Timestamp: time.Now().UTC(),
	}
}

// Queue represents a message queue
type Queue struct {
	Name     string
	Config   *QueueConfig
	messages []*Message
	inFlight map[string]*Message // receiptHandle -> message
	mu       sync.Mutex
}

// QueueStats contains queue statistics
type QueueStats struct {
	Messages    int64
	InFlight    int64
	Delayed     int64
	DeadLetters int64
}

// ManagerConfig configures the queue manager
type ManagerConfig struct {
	MaxQueues int
}

// QueueManager manages message queues
type QueueManager struct {
	config *ManagerConfig
	queues map[string]*Queue
	mu     sync.RWMutex
}

// NewQueueManager creates a new queue manager
func NewQueueManager(config *ManagerConfig) *QueueManager {
	if config == nil {
		config = &ManagerConfig{MaxQueues: 1000}
	}
	return &QueueManager{
		config: config,
		queues: make(map[string]*Queue),
	}
}

// CreateQueue creates a new queue
func (m *QueueManager) CreateQueue(config *QueueConfig) (*Queue, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config.ApplyDefaults()

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.queues[config.Name]; exists {
		return nil, fmt.Errorf("queue: %s already exists", config.Name)
	}

	queue := &Queue{
		Name:     config.Name,
		Config:   config,
		messages: make([]*Message, 0),
		inFlight: make(map[string]*Message),
	}

	m.queues[config.Name] = queue
	return queue, nil
}

// DeleteQueue deletes a queue
func (m *QueueManager) DeleteQueue(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.queues[name]; !exists {
		return fmt.Errorf("queue: %s not found", name)
	}

	delete(m.queues, name)
	return nil
}

// GetQueue returns a queue by name
func (m *QueueManager) GetQueue(name string) *Queue {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.queues[name]
}

// Enqueue adds a message to a queue
func (m *QueueManager) Enqueue(ctx context.Context, queueName string, msg *Message) (string, error) {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("queue: %s not found", queueName)
	}

	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}
	msg.Timestamp = time.Now().UTC()
	msg.enqueuedAt = msg.Timestamp

	if msg.Delay > 0 {
		msg.visibleAt = msg.Timestamp.Add(msg.Delay)
	} else {
		msg.visibleAt = msg.Timestamp
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	// Insert by priority (higher priority first)
	inserted := false
	for i, m := range queue.messages {
		if msg.Priority > m.Priority {
			queue.messages = append(queue.messages[:i], append([]*Message{msg}, queue.messages[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		queue.messages = append(queue.messages, msg)
	}

	return msg.ID, nil
}

// EnqueueBatch adds multiple messages
func (m *QueueManager) EnqueueBatch(ctx context.Context, queueName string, messages []*Message) ([]string, error) {
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		id, err := m.Enqueue(ctx, queueName, msg)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Dequeue retrieves a message from a queue
func (m *QueueManager) Dequeue(ctx context.Context, queueName string) (*Message, error) {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("queue: %s not found", queueName)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	now := time.Now()

	// Find first visible message
	for i, msg := range queue.messages {
		if msg.visibleAt.Before(now) || msg.visibleAt.Equal(now) {
			// Remove from queue
			queue.messages = append(queue.messages[:i], queue.messages[i+1:]...)

			// Generate receipt handle
			msg.ReceiptHandle = uuid.New().String()
			msg.visibleAt = now.Add(queue.Config.VisibilityTimeout)

			// Add to in-flight
			queue.inFlight[msg.ReceiptHandle] = msg

			// Start visibility timer
			go m.visibilityTimer(queueName, msg.ReceiptHandle, queue.Config.VisibilityTimeout)

			return msg, nil
		}
	}

	return nil, nil
}

// DequeueBatch retrieves multiple messages
func (m *QueueManager) DequeueBatch(ctx context.Context, queueName string, maxMessages int) ([]*Message, error) {
	messages := make([]*Message, 0, maxMessages)
	for i := 0; i < maxMessages; i++ {
		msg, err := m.Dequeue(ctx, queueName)
		if err != nil {
			return messages, err
		}
		if msg == nil {
			break
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

// visibilityTimer returns message to queue if not acknowledged
func (m *QueueManager) visibilityTimer(queueName, receiptHandle string, timeout time.Duration) {
	time.Sleep(timeout)

	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	msg, exists := queue.inFlight[receiptHandle]
	if !exists {
		return // Already acknowledged or nacked
	}

	// Return to queue
	delete(queue.inFlight, receiptHandle)
	msg.ReceiptHandle = ""
	msg.RetryCount++
	msg.visibleAt = time.Now()

	// Check if should go to DLQ
	if msg.RetryCount >= queue.Config.MaxRetries && queue.Config.DeadLetterQueue != "" {
		m.moveToDLQ(queue.Config.DeadLetterQueue, msg)
		return
	}

	queue.messages = append(queue.messages, msg)
}

// moveToDLQ moves a message to the dead letter queue
func (m *QueueManager) moveToDLQ(dlqName string, msg *Message) {
	m.mu.RLock()
	dlq, exists := m.queues[dlqName]
	m.mu.RUnlock()

	if !exists {
		return
	}

	dlq.mu.Lock()
	defer dlq.mu.Unlock()

	msg.visibleAt = time.Now()
	dlq.messages = append(dlq.messages, msg)
}

// Acknowledge removes a message from in-flight
func (m *QueueManager) Acknowledge(ctx context.Context, queueName, receiptHandle string) error {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("queue: %s not found", queueName)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	if _, exists := queue.inFlight[receiptHandle]; !exists {
		return fmt.Errorf("queue: invalid receipt handle")
	}

	delete(queue.inFlight, receiptHandle)
	return nil
}

// Nack returns a message to the queue immediately
func (m *QueueManager) Nack(ctx context.Context, queueName, receiptHandle string) error {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("queue: %s not found", queueName)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	msg, exists := queue.inFlight[receiptHandle]
	if !exists {
		return fmt.Errorf("queue: invalid receipt handle")
	}

	delete(queue.inFlight, receiptHandle)
	msg.ReceiptHandle = ""
	msg.RetryCount++
	msg.visibleAt = time.Now()

	// Check if should go to DLQ
	if msg.RetryCount >= queue.Config.MaxRetries && queue.Config.DeadLetterQueue != "" {
		queue.mu.Unlock()
		m.moveToDLQ(queue.Config.DeadLetterQueue, msg)
		queue.mu.Lock()
		return nil
	}

	queue.messages = append(queue.messages, msg)
	return nil
}

// GetQueueStats returns queue statistics
func (m *QueueManager) GetQueueStats(name string) *QueueStats {
	m.mu.RLock()
	queue, exists := m.queues[name]
	m.mu.RUnlock()

	if !exists {
		return nil
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	stats := &QueueStats{
		InFlight: int64(len(queue.inFlight)),
	}

	now := time.Now()
	for _, msg := range queue.messages {
		if msg.visibleAt.After(now) {
			stats.Delayed++
		} else {
			stats.Messages++
		}
	}

	return stats
}

// ExtendVisibility extends the visibility timeout for a message
func (m *QueueManager) ExtendVisibility(ctx context.Context, queueName, receiptHandle string, extension time.Duration) error {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("queue: %s not found", queueName)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	msg, exists := queue.inFlight[receiptHandle]
	if !exists {
		return fmt.Errorf("queue: invalid receipt handle")
	}

	msg.visibleAt = time.Now().Add(extension)
	return nil
}

// Purge removes all messages from a queue
func (m *QueueManager) Purge(ctx context.Context, queueName string) error {
	m.mu.RLock()
	queue, exists := m.queues[queueName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("queue: %s not found", queueName)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()

	queue.messages = make([]*Message, 0)
	queue.inFlight = make(map[string]*Message)
	return nil
}

// ListQueues returns all queue names
func (m *QueueManager) ListQueues() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.queues))
	for name := range m.queues {
		names = append(names, name)
	}
	return names
}
