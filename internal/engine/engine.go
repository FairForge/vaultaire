package engine

import (
	"context"
	"fmt"
)

type Engine interface {
	CreateContainer(ctx context.Context, container *Container) error
	GetContainer(ctx context.Context, name string) (*Container, error)
	DeleteContainer(ctx context.Context, name string) error
	ListContainers(ctx context.Context) ([]*Container, error)
	Execute(ctx context.Context, op Operation) (*Result, error)
	Query(ctx context.Context, query string) (*Result, error)
	Compute(ctx context.Context, wasm []byte, data []byte) (*Result, error)
	Subscribe(ctx context.Context, pattern string) (<-chan Event, error)
}

type Backend interface {
	Name() string
	Execute(ctx context.Context, op Operation) (*Result, error)
	Capabilities() Capabilities
	HealthCheck(ctx context.Context) error
}

type Event struct {
	ID        string
	Type      string
	Timestamp int64
	TenantID  string
	Container string
	Artifact  string
	Data      map[string]interface{}
}

var (
	ErrNotImplemented    = fmt.Errorf("not implemented")
	ErrContainerNotFound = fmt.Errorf("container not found")
	ErrArtifactNotFound  = fmt.Errorf("artifact not found")
)
