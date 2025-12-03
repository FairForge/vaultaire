// internal/streaming/stream.go
package streaming

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Offset constants
const (
	OffsetEarliest int64 = -2
	OffsetLatest   int64 = -1
)

// StreamConfig configures a stream
type StreamConfig struct {
	Name            string        `json:"name"`
	Partitions      int           `json:"partitions"`
	RetentionPeriod time.Duration `json:"retention_period"`
	MaxSize         int64         `json:"max_size"`
}

// DefaultStreamConfig returns sensible defaults
func DefaultStreamConfig() *StreamConfig {
	return &StreamConfig{
		Partitions:      1,
		RetentionPeriod: 7 * 24 * time.Hour,
		MaxSize:         1024 * 1024 * 1024, // 1GB
	}
}

// Validate checks configuration
func (c *StreamConfig) Validate() error {
	if c.Name == "" {
		return errors.New("stream: name is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *StreamConfig) ApplyDefaults() {
	defaults := DefaultStreamConfig()
	if c.Partitions == 0 {
		c.Partitions = defaults.Partitions
	}
	if c.RetentionPeriod == 0 {
		c.RetentionPeriod = defaults.RetentionPeriod
	}
	if c.MaxSize == 0 {
		c.MaxSize = defaults.MaxSize
	}
}

// Message represents a stream message
type Message struct {
	Key       string
	Value     []byte
	Headers   map[string]string
	Timestamp time.Time
	Partition int
	Offset    int64
}

// NewMessage creates a new message
func NewMessage(key string, value []byte) *Message {
	return &Message{
		Key:       key,
		Value:     value,
		Headers:   make(map[string]string),
		Timestamp: time.Now().UTC(),
	}
}

// Stream represents a message stream
type Stream struct {
	Name       string
	Partitions int
	Config     *StreamConfig
	partitions []*partition
}

// partition holds messages for one partition
type partition struct {
	id       int
	messages []*Message
	offset   int64
	size     int64
	mu       sync.RWMutex
}

// StreamStats contains stream statistics
type StreamStats struct {
	MessageCount int64
	CurrentSize  int64
	Partitions   int
	OldestOffset int64
	NewestOffset int64
}

// SubscribeOptions configures subscription behavior
type SubscribeOptions struct {
	StartOffset int64
}

// Subscription represents a stream subscription
type Subscription struct {
	id         string
	stream     *Stream
	group      string
	partitions []int
	positions  map[int]int64
	manager    *StreamManager
	msgChan    chan *Message
	closeChan  chan struct{}
	closed     bool
	mu         sync.Mutex
}

// Receive receives the next message
func (s *Subscription) Receive(ctx context.Context) (*Message, error) {
	select {
	case msg := <-s.msgChan:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closeChan:
		return nil, errors.New("subscription closed")
	}
}

// Commit commits the offset for a message
func (s *Subscription) Commit(msg *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.positions[msg.Partition] = msg.Offset
	s.manager.commitOffset(s.stream.Name, s.group, msg.Partition, msg.Offset)
	return nil
}

// AssignedPartitions returns assigned partition IDs
func (s *Subscription) AssignedPartitions() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partitions
}

// Close closes the subscription
func (s *Subscription) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.closeChan)
	}
	return nil
}

// ConsumerGroup tracks consumer group state
type ConsumerGroup struct {
	Name      string
	Stream    string
	Offsets   map[int]int64 // partition -> offset
	Consumers int
	mu        sync.RWMutex
}

// ManagerConfig configures the stream manager
type ManagerConfig struct {
	MaxStreams int
}

// StreamManager manages streams
type StreamManager struct {
	config         *ManagerConfig
	streams        map[string]*Stream
	consumerGroups map[string]map[string]*ConsumerGroup // stream -> group -> ConsumerGroup
	mu             sync.RWMutex
}

// NewStreamManager creates a new stream manager
func NewStreamManager(config *ManagerConfig) *StreamManager {
	if config == nil {
		config = &ManagerConfig{MaxStreams: 1000}
	}
	return &StreamManager{
		config:         config,
		streams:        make(map[string]*Stream),
		consumerGroups: make(map[string]map[string]*ConsumerGroup),
	}
}

// CreateStream creates a new stream
func (m *StreamManager) CreateStream(config *StreamConfig) (*Stream, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config.ApplyDefaults()

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.streams[config.Name]; exists {
		return nil, fmt.Errorf("stream: %s already exists", config.Name)
	}

	stream := &Stream{
		Name:       config.Name,
		Partitions: config.Partitions,
		Config:     config,
		partitions: make([]*partition, config.Partitions),
	}

	for i := 0; i < config.Partitions; i++ {
		stream.partitions[i] = &partition{
			id:       i,
			messages: make([]*Message, 0),
		}
	}

	m.streams[config.Name] = stream
	m.consumerGroups[config.Name] = make(map[string]*ConsumerGroup)

	return stream, nil
}

// DeleteStream deletes a stream
func (m *StreamManager) DeleteStream(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.streams[name]; !exists {
		return fmt.Errorf("stream: %s not found", name)
	}

	delete(m.streams, name)
	delete(m.consumerGroups, name)
	return nil
}

// GetStream returns a stream by name
func (m *StreamManager) GetStream(name string) *Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[name]
}

// Publish publishes a message to a stream
func (m *StreamManager) Publish(ctx context.Context, streamName string, msg *Message) (int64, error) {
	m.mu.RLock()
	stream, exists := m.streams[streamName]
	m.mu.RUnlock()

	if !exists {
		return 0, fmt.Errorf("stream: %s not found", streamName)
	}

	// Determine partition
	partIdx := 0
	if msg.Key != "" {
		h := fnv.New32a()
		h.Write([]byte(msg.Key))
		partIdx = int(h.Sum32()) % stream.Partitions
	}

	part := stream.partitions[partIdx]
	part.mu.Lock()
	defer part.mu.Unlock()

	msg.Partition = partIdx
	msg.Offset = part.offset
	msg.Timestamp = time.Now().UTC()

	part.messages = append(part.messages, msg)
	part.offset++
	part.size += int64(len(msg.Value))

	// Enforce max size
	m.enforceRetention(stream, part)

	// Notify subscribers
	m.notifySubscribers(streamName, msg)

	return msg.Offset, nil
}

// enforceRetention removes old messages if over size limit
func (m *StreamManager) enforceRetention(stream *Stream, part *partition) {
	for part.size > stream.Config.MaxSize && len(part.messages) > 0 {
		removed := part.messages[0]
		part.size -= int64(len(removed.Value))
		part.messages = part.messages[1:]
	}
}

// notifySubscribers sends message to waiting subscribers
func (m *StreamManager) notifySubscribers(streamName string, msg *Message) {
	// This would integrate with subscription channels
	// For simplicity, handled in subscription polling
}

// GetPartitionForOffset returns partition for an offset
func (m *StreamManager) GetPartitionForOffset(streamName string, offset int64) int {
	m.mu.RLock()
	stream := m.streams[streamName]
	m.mu.RUnlock()

	if stream == nil {
		return -1
	}

	for _, part := range stream.partitions {
		part.mu.RLock()
		for _, msg := range part.messages {
			if msg.Offset == offset {
				part.mu.RUnlock()
				return part.id
			}
		}
		part.mu.RUnlock()
	}
	return 0
}

// Subscribe creates a subscription
func (m *StreamManager) Subscribe(streamName, groupName string, opts SubscribeOptions) (*Subscription, error) {
	m.mu.Lock()
	stream, exists := m.streams[streamName]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("stream: %s not found", streamName)
	}

	// Get or create consumer group
	groups := m.consumerGroups[streamName]
	group, exists := groups[groupName]
	if !exists {
		group = &ConsumerGroup{
			Name:    groupName,
			Stream:  streamName,
			Offsets: make(map[int]int64),
		}
		groups[groupName] = group
	}
	group.Consumers++
	m.mu.Unlock()

	// Assign partitions (simple round-robin for now)
	assignedPartitions := make([]int, 0)
	for i := 0; i < stream.Partitions; i++ {
		assignedPartitions = append(assignedPartitions, i)
	}

	sub := &Subscription{
		id:         uuid.New().String(),
		stream:     stream,
		group:      groupName,
		partitions: assignedPartitions,
		positions:  make(map[int]int64),
		manager:    m,
		msgChan:    make(chan *Message, 100),
		closeChan:  make(chan struct{}),
	}

	// Initialize positions
	for _, p := range assignedPartitions {
		switch opts.StartOffset {
		case OffsetEarliest:
			sub.positions[p] = 0
		case OffsetLatest:
			sub.positions[p] = stream.partitions[p].offset
		default:
			if opts.StartOffset >= 0 {
				sub.positions[p] = opts.StartOffset
			} else {
				sub.positions[p] = 0
			}
		}
	}

	// Start polling goroutine
	go sub.poll()

	return sub, nil
}

// poll continuously checks for new messages
func (s *Subscription) poll() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.closeChan:
			return
		case <-ticker.C:
			s.mu.Lock()
			for _, partID := range s.partitions {
				part := s.stream.partitions[partID]
				part.mu.RLock()
				pos := s.positions[partID]
				for _, msg := range part.messages {
					if msg.Offset >= pos {
						select {
						case s.msgChan <- msg:
							s.positions[partID] = msg.Offset + 1
						default:
							// Channel full, try again later
						}
					}
				}
				part.mu.RUnlock()
			}
			s.mu.Unlock()
		}
	}
}

// commitOffset stores committed offset
func (m *StreamManager) commitOffset(stream, group string, partition int, offset int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if groups, ok := m.consumerGroups[stream]; ok {
		if cg, ok := groups[group]; ok {
			cg.mu.Lock()
			cg.Offsets[partition] = offset
			cg.mu.Unlock()
		}
	}
}

// GetCommittedOffset returns committed offset for a consumer group
func (m *StreamManager) GetCommittedOffset(stream, group string, partition int) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if groups, ok := m.consumerGroups[stream]; ok {
		if cg, ok := groups[group]; ok {
			cg.mu.RLock()
			defer cg.mu.RUnlock()
			return cg.Offsets[partition]
		}
	}
	return -1
}

// GetStreamStats returns statistics for a stream
func (m *StreamManager) GetStreamStats(name string) *StreamStats {
	m.mu.RLock()
	stream := m.streams[name]
	m.mu.RUnlock()

	if stream == nil {
		return nil
	}

	stats := &StreamStats{
		Partitions: stream.Partitions,
	}

	for _, part := range stream.partitions {
		part.mu.RLock()
		stats.MessageCount += int64(len(part.messages))
		stats.CurrentSize += part.size
		if len(part.messages) > 0 {
			if stats.OldestOffset == 0 || part.messages[0].Offset < stats.OldestOffset {
				stats.OldestOffset = part.messages[0].Offset
			}
			lastMsg := part.messages[len(part.messages)-1]
			if lastMsg.Offset > stats.NewestOffset {
				stats.NewestOffset = lastMsg.Offset
			}
		}
		part.mu.RUnlock()
	}

	return stats
}
