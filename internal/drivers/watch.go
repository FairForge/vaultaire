package drivers

import (
	"context"
)

// WatchEventType represents the type of file system event
type WatchEventType int

const (
	WatchEventCreate WatchEventType = iota
	WatchEventModify
	WatchEventDelete
	WatchEventRename
)

// WatchEvent represents a file system change event
type WatchEvent struct {
	Type WatchEventType
	Path string // Relative path from watched root
}

// Watcher interface for drivers that support file watching
type Watcher interface {
	Watch(ctx context.Context, prefix string) (<-chan WatchEvent, <-chan error, error)
}
