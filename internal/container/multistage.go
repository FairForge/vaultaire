// internal/container/multistage.go
package container

import (
	"errors"
	"fmt"
	"strings"
)

// StageCopy represents copying from another stage
type StageCopy struct {
	FromStage string `json:"from_stage"`
	Src       string `json:"src"`
	Dst       string `json:"dst"`
	Chown     string `json:"chown"`
}

// BuildStage represents a single stage in multi-stage build
type BuildStage struct {
	Name        string             `json:"name"`
	BaseImage   string             `json:"base_image"`
	DependsOn   []string           `json:"depends_on"`
	Workdir     string             `json:"workdir"`
	Commands    []string           `json:"commands"`
	CopyFrom    []StageCopy        `json:"copy_from"`
	Args        map[string]string  `json:"args"`
	Env         map[string]string  `json:"env"`
	Labels      map[string]string  `json:"labels"`
	Expose      []int              `json:"expose"`
	User        string             `json:"user"`
	UserID      int                `json:"user_id"`
	Entrypoint  []string           `json:"entrypoint"`
	Cmd         []string           `json:"cmd"`
	Healthcheck *HealthcheckConfig `json:"healthcheck"`
}

// Validate checks the stage configuration
func (s *BuildStage) Validate() error {
	if s.Name == "" {
		return errors.New("multistage: stage name is required")
	}
	if s.BaseImage == "" {
		return errors.New("multistage: base image is required")
	}
	return nil
}

// MultiStageBuilder builds multi-stage Dockerfiles
type MultiStageBuilder struct {
	stages     []*BuildStage
	stageIndex map[string]int
}

// NewMultiStageBuilder creates a new multi-stage builder
func NewMultiStageBuilder() *MultiStageBuilder {
	return &MultiStageBuilder{
		stages:     make([]*BuildStage, 0),
		stageIndex: make(map[string]int),
	}
}

// AddStage adds a build stage
func (b *MultiStageBuilder) AddStage(stage *BuildStage) error {
	if err := stage.Validate(); err != nil {
		return err
	}
	if _, exists := b.stageIndex[stage.Name]; exists {
		return fmt.Errorf("multistage: stage %q already exists", stage.Name)
	}
	b.stageIndex[stage.Name] = len(b.stages)
	b.stages = append(b.stages, stage)
	return nil
}

// Stages returns all stages
func (b *MultiStageBuilder) Stages() []*BuildStage {
	return b.stages
}

// GetStage returns a stage by name
func (b *MultiStageBuilder) GetStage(name string) *BuildStage {
	if idx, exists := b.stageIndex[name]; exists {
		return b.stages[idx]
	}
	return nil
}

// RemoveStage removes a stage by name
func (b *MultiStageBuilder) RemoveStage(name string) error {
	idx, exists := b.stageIndex[name]
	if !exists {
		return fmt.Errorf("multistage: stage %q not found", name)
	}
	b.stages = append(b.stages[:idx], b.stages[idx+1:]...)
	// Rebuild index
	b.stageIndex = make(map[string]int)
	for i, s := range b.stages {
		b.stageIndex[s.Name] = i
	}
	return nil
}

// ResolveBuildOrder returns stages in dependency order
func (b *MultiStageBuilder) ResolveBuildOrder() ([]string, error) {
	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	order := make([]string, 0, len(b.stages))

	var visit func(name string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("multistage: cyclic dependency detected at %q", name)
		}
		visiting[name] = true

		stage := b.GetStage(name)
		if stage != nil {
			for _, dep := range stage.DependsOn {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}

		visiting[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for _, stage := range b.stages {
		if err := visit(stage.Name); err != nil {
			return nil, err
		}
	}

	return order, nil
}

// Build generates the complete Dockerfile
func (b *MultiStageBuilder) Build() string {
	order, err := b.ResolveBuildOrder()
	if err != nil {
		// Fall back to original order
		order = make([]string, len(b.stages))
		for i, s := range b.stages {
			order[i] = s.Name
		}
	}

	var lines []string
	for _, name := range order {
		stage := b.GetStage(name)
		if stage == nil {
			continue
		}
		lines = append(lines, b.buildStage(stage)...)
		lines = append(lines, "")
	}

	return strings.Join(lines, "\n")
}

// BuildTarget builds up to a specific target stage
func (b *MultiStageBuilder) BuildTarget(target string) string {
	order, _ := b.ResolveBuildOrder()

	var lines []string
	for _, name := range order {
		stage := b.GetStage(name)
		if stage == nil {
			continue
		}
		lines = append(lines, b.buildStage(stage)...)
		lines = append(lines, "")

		if name == target {
			break
		}
	}

	return strings.Join(lines, "\n")
}

func (b *MultiStageBuilder) buildStage(stage *BuildStage) []string {
	var lines []string

	// FROM line
	lines = append(lines, fmt.Sprintf("FROM %s AS %s", stage.BaseImage, stage.Name))

	// ARGs
	for arg := range stage.Args {
		lines = append(lines, fmt.Sprintf("ARG %s", arg))
	}

	// Labels
	for k, v := range stage.Labels {
		lines = append(lines, fmt.Sprintf("LABEL %s=\"%s\"", k, v))
	}

	// ENV
	for k, v := range stage.Env {
		lines = append(lines, fmt.Sprintf("ENV %s=%s", k, v))
	}

	// WORKDIR
	if stage.Workdir != "" {
		lines = append(lines, fmt.Sprintf("WORKDIR %s", stage.Workdir))
	}

	// Commands
	lines = append(lines, stage.Commands...)

	// COPY --from
	for _, cp := range stage.CopyFrom {
		copyLine := fmt.Sprintf("COPY --from=%s", cp.FromStage)
		if cp.Chown != "" {
			copyLine += fmt.Sprintf(" --chown=%s", cp.Chown)
		}
		copyLine += fmt.Sprintf(" %s %s", cp.Src, cp.Dst)
		lines = append(lines, copyLine)
	}

	// User
	if stage.User != "" {
		lines = append(lines, fmt.Sprintf("USER %s", stage.User))
	}

	// Expose
	for _, port := range stage.Expose {
		lines = append(lines, fmt.Sprintf("EXPOSE %d", port))
	}

	// Healthcheck
	if stage.Healthcheck != nil {
		hc := stage.Healthcheck
		parts := []string{"HEALTHCHECK"}
		if hc.Interval != "" {
			parts = append(parts, fmt.Sprintf("--interval=%s", hc.Interval))
		}
		if hc.Timeout != "" {
			parts = append(parts, fmt.Sprintf("--timeout=%s", hc.Timeout))
		}
		if hc.Retries > 0 {
			parts = append(parts, fmt.Sprintf("--retries=%d", hc.Retries))
		}
		parts = append(parts, fmt.Sprintf("CMD %s", hc.Command))
		lines = append(lines, strings.Join(parts, " "))
	}

	// Entrypoint
	if len(stage.Entrypoint) > 0 {
		lines = append(lines, fmt.Sprintf("ENTRYPOINT %s", formatArray(stage.Entrypoint)))
	}

	// CMD
	if len(stage.Cmd) > 0 {
		lines = append(lines, fmt.Sprintf("CMD %s", formatArray(stage.Cmd)))
	}

	return lines
}

func formatArray(arr []string) string {
	quoted := make([]string, len(arr))
	for i, s := range arr {
		quoted[i] = fmt.Sprintf("\"%s\"", s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// CreateGoBuildStages creates standard Go multi-stage build
func CreateGoBuildStages(appName, version string) *MultiStageBuilder {
	builder := NewMultiStageBuilder()

	// Builder stage
	_ = builder.AddStage(&BuildStage{
		Name:      "builder",
		BaseImage: "golang:1.21-alpine",
		Workdir:   "/app",
		Args:      map[string]string{"VERSION": version},
		Env:       map[string]string{"CGO_ENABLED": "0"},
		Commands: []string{
			"COPY go.mod go.sum ./",
			"RUN go mod download",
			"COPY . .",
			fmt.Sprintf("RUN go build -ldflags=\"-s -w -X main.version=${VERSION}\" -o /app/%s ./cmd/%s", appName, appName),
		},
	})

	// Runtime stage
	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		DependsOn: []string{"builder"},
		Workdir:   "/app",
		CopyFrom: []StageCopy{
			{FromStage: "builder", Src: fmt.Sprintf("/app/%s", appName), Dst: fmt.Sprintf("/usr/local/bin/%s", appName)},
		},
		User:   "appuser",
		UserID: 1000,
		Expose: []int{8080},
		Labels: map[string]string{
			"org.opencontainers.image.version": version,
		},
		Healthcheck: &HealthcheckConfig{
			Command:  "wget -q --spider http://localhost:8080/health || exit 1",
			Interval: "30s",
			Timeout:  "5s",
			Retries:  3,
		},
		Entrypoint: []string{fmt.Sprintf("/usr/local/bin/%s", appName)},
	})

	return builder
}
