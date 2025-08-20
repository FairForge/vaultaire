package pipeline

import (
	"context"
	"fmt"
	"io"
)

// Pipeline processes operations through stages
type Pipeline interface {
	Process(ctx context.Context, data io.Reader) (io.Reader, error)
	AddStage(stage Stage)
	GetStages() []Stage
}

// Stage processes data
type Stage interface {
	Name() string
	Process(ctx context.Context, data io.Reader) (io.Reader, error)
	Skip(ctx context.Context) bool
}

// SimplePipeline chains stages together
type SimplePipeline struct {
	stages []Stage
}

// NewPipeline creates a new pipeline
func NewPipeline() *SimplePipeline {
	return &SimplePipeline{
		stages: make([]Stage, 0),
	}
}

// AddStage adds a processing stage
func (p *SimplePipeline) AddStage(stage Stage) {
	p.stages = append(p.stages, stage)
}

// GetStages returns all stages
func (p *SimplePipeline) GetStages() []Stage {
	return p.stages
}

// Process runs data through all stages
func (p *SimplePipeline) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	current := data

	for _, stage := range p.stages {
		if stage.Skip(ctx) {
			continue
		}

		processed, err := stage.Process(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("stage %s failed: %w", stage.Name(), err)
		}

		current = processed
	}

	return current, nil
}

// Future stages (currently dormant)

// CompressionStage will compress data
type CompressionStage struct{}

func (s *CompressionStage) Name() string                  { return "compression" }
func (s *CompressionStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *CompressionStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement compression
	return data, nil
}

// EncryptionStage will encrypt data
type EncryptionStage struct{}

func (s *EncryptionStage) Name() string                  { return "encryption" }
func (s *EncryptionStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *EncryptionStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement encryption
	return data, nil
}

// ChunkingStage will implement FastCDC
type ChunkingStage struct{}

func (s *ChunkingStage) Name() string                  { return "chunking" }
func (s *ChunkingStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *ChunkingStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement FastCDC chunking
	return data, nil
}

// DedupeStage will implement deduplication
type DedupeStage struct{}

func (s *DedupeStage) Name() string                  { return "dedupe" }
func (s *DedupeStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *DedupeStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement content-based deduplication
	return data, nil
}

// ErasureStage will implement RaptorQ
type ErasureStage struct{}

func (s *ErasureStage) Name() string                  { return "erasure" }
func (s *ErasureStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *ErasureStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement erasure coding
	return data, nil
}

// ComputeStage will run WASM functions
type ComputeStage struct{}

func (s *ComputeStage) Name() string                  { return "compute" }
func (s *ComputeStage) Skip(ctx context.Context) bool { return true } // Skip for MVP
func (s *ComputeStage) Process(ctx context.Context, data io.Reader) (io.Reader, error) {
	// Future: implement WASM compute
	return data, nil
}
