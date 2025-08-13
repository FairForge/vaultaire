package events

import (
	"encoding/json"
	"time"

	"go.uber.org/zap"
)

type EventLogger struct {
	logger *zap.Logger
	buffer chan Event
}

type Event struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	TenantID  string                 `json:"tenant_id,omitempty"`
	Container string                 `json:"container,omitempty"`
	Artifact  string                 `json:"artifact,omitempty"`
	Operation string                 `json:"operation,omitempty"`
	Data      map[string]interface{} `json:"data"`
	Timestamp time.Time              `json:"timestamp"`
}

func NewEventLogger(logger *zap.Logger) *EventLogger {
	el := &EventLogger{
		logger: logger,
		buffer: make(chan Event, 1000),
	}
	go el.process()
	return el
}

func (el *EventLogger) Log(event Event) {
	event.Timestamp = time.Now()
	select {
	case el.buffer <- event:
	default:
		el.logger.Warn("Event buffer full, dropping event")
	}
}

func (el *EventLogger) process() {
	for event := range el.buffer {
		data, _ := json.Marshal(event)
		el.logger.Info("event",
			zap.String("type", event.Type),
			zap.String("data", string(data)),
		)
	}
}
