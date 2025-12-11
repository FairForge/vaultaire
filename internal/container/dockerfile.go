// internal/container/dockerfile.go
package container

import (
	"errors"
	"fmt"
	"strings"
)

// HealthcheckConfig configures container healthcheck
type HealthcheckConfig struct {
	Command     string `json:"command"`
	Interval    string `json:"interval"`
	Timeout     string `json:"timeout"`
	StartPeriod string `json:"start_period"`
	Retries     int    `json:"retries"`
}

// CopyInstruction represents a COPY instruction
type CopyInstruction struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	From  string `json:"from"`
	Chown string `json:"chown"`
}

// DockerfileConfig configures Dockerfile generation
type DockerfileConfig struct {
	BaseImage     string             `json:"base_image"`
	AppName       string             `json:"app_name"`
	MultiStage    bool               `json:"multi_stage"`
	RuntimeBase   string             `json:"runtime_base"`
	BuildArgs     map[string]string  `json:"build_args"`
	Labels        map[string]string  `json:"labels"`
	EnvVars       map[string]string  `json:"env_vars"`
	Ports         []int              `json:"ports"`
	Healthcheck   *HealthcheckConfig `json:"healthcheck"`
	RunAsUser     string             `json:"run_as_user"`
	RunAsUserID   int                `json:"run_as_user_id"`
	UseBuildCache bool               `json:"use_build_cache"`
	Entrypoint    []string           `json:"entrypoint"`
	Command       []string           `json:"command"`
	Workdir       string             `json:"workdir"`
	OptimizeGo    bool               `json:"optimize_go"`
	StripBinary   bool               `json:"strip_binary"`
	StaticBinary  bool               `json:"static_binary"`
}

// Validate checks configuration
func (c *DockerfileConfig) Validate() error {
	if c.BaseImage == "" {
		return errors.New("dockerfile: base image is required")
	}
	if c.AppName == "" {
		return errors.New("dockerfile: app name is required")
	}
	return nil
}

// DockerfileBuilder builds Dockerfiles
type DockerfileBuilder struct {
	config *DockerfileConfig
	lines  []string
}

// NewDockerfileBuilder creates a Dockerfile builder
func NewDockerfileBuilder(config *DockerfileConfig) *DockerfileBuilder {
	return &DockerfileBuilder{
		config: config,
		lines:  make([]string, 0),
	}
}

// Build generates the Dockerfile content
func (b *DockerfileBuilder) Build() string {
	b.lines = make([]string, 0)

	if b.config.MultiStage {
		b.buildMultiStage()
	} else {
		b.buildSingleStage()
	}

	return strings.Join(b.lines, "\n")
}

func (b *DockerfileBuilder) buildSingleStage() {
	b.addLine("FROM %s", b.config.BaseImage)
	b.addCommonInstructions()
}

func (b *DockerfileBuilder) buildMultiStage() {
	// Builder stage
	b.addLine("FROM %s AS builder", b.config.BaseImage)
	b.addLine("")

	if b.config.Workdir != "" {
		b.addLine("WORKDIR %s", b.config.Workdir)
	}

	// Build args
	for arg := range b.config.BuildArgs {
		b.addLine("ARG %s", arg)
	}

	// Environment for Go builds
	if b.config.OptimizeGo || b.config.StaticBinary {
		b.addLine("ENV CGO_ENABLED=0")
	}

	// Cache mounts for Go
	if b.config.UseBuildCache {
		b.addLine("RUN --mount=type=cache,target=/go/pkg/mod \\")
		b.addLine("    --mount=type=cache,target=/root/.cache/go-build \\")
		b.addLine("    go build -ldflags=\"-s -w\" -o /app/%s ./cmd/%s", b.config.AppName, b.config.AppName)
	} else if b.config.OptimizeGo {
		ldflags := "-s -w"
		b.addLine("RUN go build -ldflags=\"%s\" -o /app/%s ./cmd/%s", ldflags, b.config.AppName, b.config.AppName)
	}

	b.addLine("")

	// Runtime stage
	runtimeBase := b.config.RuntimeBase
	if runtimeBase == "" {
		runtimeBase = "alpine:3.19"
	}
	b.addLine("FROM %s", runtimeBase)
	b.addLine("")

	// Labels
	if len(b.config.Labels) > 0 {
		for k, v := range b.config.Labels {
			b.addLine("LABEL %s=\"%s\"", k, v)
		}
		b.addLine("")
	}

	// Create non-root user
	if b.config.RunAsUser != "" {
		if runtimeBase != "scratch" && !strings.Contains(runtimeBase, "distroless") {
			b.addLine("RUN adduser -D -u %d %s", b.config.RunAsUserID, b.config.RunAsUser)
		}
		b.addLine("USER %s", b.config.RunAsUser)
		b.addLine("")
	}

	if b.config.Workdir != "" {
		b.addLine("WORKDIR %s", b.config.Workdir)
	}

	// Copy binary from builder
	b.addLine("COPY --from=builder /app/%s /app/%s", b.config.AppName, b.config.AppName)
	b.addLine("")

	// Environment variables
	for k, v := range b.config.EnvVars {
		b.addLine("ENV %s=%s", k, v)
	}

	// Expose ports
	for _, port := range b.config.Ports {
		b.addLine("EXPOSE %d", port)
	}

	// Healthcheck
	if b.config.Healthcheck != nil {
		b.addHealthcheck()
	}

	// Entrypoint and CMD
	if len(b.config.Entrypoint) > 0 {
		b.addLine("ENTRYPOINT %s", b.formatStringArray(b.config.Entrypoint))
	}
	if len(b.config.Command) > 0 {
		b.addLine("CMD %s", b.formatStringArray(b.config.Command))
	}
}

func (b *DockerfileBuilder) addCommonInstructions() {
	// Build args
	for arg := range b.config.BuildArgs {
		b.addLine("ARG %s", arg)
	}

	// Labels
	if len(b.config.Labels) > 0 {
		for k, v := range b.config.Labels {
			b.addLine("LABEL %s=\"%s\"", k, v)
		}
	}

	// Environment variables
	for k, v := range b.config.EnvVars {
		b.addLine("ENV %s=%s", k, v)
	}

	if b.config.Workdir != "" {
		b.addLine("WORKDIR %s", b.config.Workdir)
	}

	// User
	if b.config.RunAsUser != "" {
		b.addLine("USER %s", b.config.RunAsUser)
	}

	// Cache mounts
	if b.config.UseBuildCache {
		b.addLine("RUN --mount=type=cache,target=/go/pkg/mod go build")
	}

	// Expose ports
	for _, port := range b.config.Ports {
		b.addLine("EXPOSE %d", port)
	}

	// Healthcheck
	if b.config.Healthcheck != nil {
		b.addHealthcheck()
	}

	// Entrypoint and CMD
	if len(b.config.Entrypoint) > 0 {
		b.addLine("ENTRYPOINT %s", b.formatStringArray(b.config.Entrypoint))
	}
	if len(b.config.Command) > 0 {
		b.addLine("CMD %s", b.formatStringArray(b.config.Command))
	}
}

func (b *DockerfileBuilder) addHealthcheck() {
	hc := b.config.Healthcheck
	parts := []string{"HEALTHCHECK"}

	if hc.Interval != "" {
		parts = append(parts, fmt.Sprintf("--interval=%s", hc.Interval))
	}
	if hc.Timeout != "" {
		parts = append(parts, fmt.Sprintf("--timeout=%s", hc.Timeout))
	}
	if hc.StartPeriod != "" {
		parts = append(parts, fmt.Sprintf("--start-period=%s", hc.StartPeriod))
	}
	if hc.Retries > 0 {
		parts = append(parts, fmt.Sprintf("--retries=%d", hc.Retries))
	}

	parts = append(parts, fmt.Sprintf("CMD %s", hc.Command))
	b.addLine("%s", strings.Join(parts, " "))
}

func (b *DockerfileBuilder) addLine(format string, args ...interface{}) {
	b.lines = append(b.lines, fmt.Sprintf(format, args...))
}

func (b *DockerfileBuilder) formatStringArray(arr []string) string {
	quoted := make([]string, len(arr))
	for i, s := range arr {
		quoted[i] = fmt.Sprintf("\"%s\"", s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// GenerateProductionDockerfile generates a production-ready Dockerfile
func GenerateProductionDockerfile(appName, version string) string {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     appName,
		MultiStage:  true,
		RuntimeBase: "alpine:3.19",
		Workdir:     "/app",
		BuildArgs: map[string]string{
			"VERSION": version,
		},
		Labels: map[string]string{
			"org.opencontainers.image.version": version,
		},
		Ports:       []int{8080},
		RunAsUser:   "appuser",
		RunAsUserID: 1000,
		OptimizeGo:  true,
		StripBinary: true,
		Healthcheck: &HealthcheckConfig{
			Command:  "wget --spider -q http://localhost:8080/health || exit 1",
			Interval: "30s",
			Timeout:  "10s",
			Retries:  3,
		},
		Entrypoint: []string{"/app/" + appName},
	})

	return builder.Build()
}
