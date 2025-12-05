// internal/container/dockerfile_test.go
package container

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerfileConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &DockerfileConfig{
			BaseImage: "golang:1.21-alpine",
			AppName:   "vaultaire",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty base image", func(t *testing.T) {
		config := &DockerfileConfig{AppName: "app"}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects empty app name", func(t *testing.T) {
		config := &DockerfileConfig{BaseImage: "golang:1.21"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewDockerfileBuilder(t *testing.T) {
	t.Run("creates builder", func(t *testing.T) {
		builder := NewDockerfileBuilder(&DockerfileConfig{
			BaseImage: "golang:1.21-alpine",
			AppName:   "vaultaire",
		})
		assert.NotNil(t, builder)
	})
}

func TestDockerfileBuilder_MultiStage(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     "vaultaire",
		MultiStage:  true,
		RuntimeBase: "alpine:3.19",
	})

	t.Run("generates multi-stage dockerfile", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "FROM golang:1.21-alpine AS builder")
		assert.Contains(t, dockerfile, "FROM alpine:3.19")
	})
}

func TestDockerfileBuilder_BuildArgs(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		BuildArgs: map[string]string{
			"VERSION": "1.0.0",
		},
	})

	t.Run("includes build args", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "ARG VERSION")
	})
}

func TestDockerfileBuilder_Labels(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		Labels: map[string]string{
			"maintainer": "team@fairforge.io",
		},
	})

	t.Run("includes labels", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "LABEL")
	})
}

func TestDockerfileBuilder_Env(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		EnvVars: map[string]string{
			"CGO_ENABLED": "0",
		},
	})

	t.Run("includes environment variables", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "ENV CGO_ENABLED=0")
	})
}

func TestDockerfileBuilder_Expose(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		Ports:     []int{8080, 9090},
	})

	t.Run("exposes ports", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "EXPOSE 8080")
		assert.Contains(t, dockerfile, "EXPOSE 9090")
	})
}

func TestDockerfileBuilder_Healthcheck(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		Healthcheck: &HealthcheckConfig{
			Command:  "wget --spider http://localhost:8080/health",
			Interval: "30s",
			Timeout:  "10s",
			Retries:  3,
		},
	})

	t.Run("includes healthcheck", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "HEALTHCHECK")
		assert.Contains(t, dockerfile, "--interval=30s")
	})
}

func TestDockerfileBuilder_User(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     "vaultaire",
		RunAsUser:   "appuser",
		RunAsUserID: 1000,
	})

	t.Run("creates non-root user", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "USER appuser")
	})
}

func TestDockerfileBuilder_CacheMounts(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:     "golang:1.21-alpine",
		AppName:       "vaultaire",
		UseBuildCache: true,
	})

	t.Run("uses cache mounts", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "--mount=type=cache")
	})
}

func TestDockerfileBuilder_Entrypoint(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:  "golang:1.21-alpine",
		AppName:    "vaultaire",
		Entrypoint: []string{"/app/vaultaire"},
		Command:    []string{"serve"},
	})

	t.Run("sets entrypoint and cmd", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "ENTRYPOINT")
		assert.Contains(t, dockerfile, "CMD")
	})
}

func TestDockerfileBuilder_Workdir(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage: "golang:1.21-alpine",
		AppName:   "vaultaire",
		Workdir:   "/app",
	})

	t.Run("sets workdir", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "WORKDIR /app")
	})
}

func TestDockerfileBuilder_OptimizedGoBuild(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:    "golang:1.21-alpine",
		AppName:      "vaultaire",
		MultiStage:   true,
		RuntimeBase:  "scratch",
		OptimizeGo:   true,
		StripBinary:  true,
		StaticBinary: true,
	})

	t.Run("generates optimized go build", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "CGO_ENABLED=0")
		assert.Contains(t, dockerfile, "-ldflags")
	})
}

func TestDockerfileBuilder_Scratch(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     "vaultaire",
		MultiStage:  true,
		RuntimeBase: "scratch",
	})

	t.Run("uses scratch base", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "FROM scratch")
	})
}

func TestDockerfileBuilder_Distroless(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     "vaultaire",
		MultiStage:  true,
		RuntimeBase: "gcr.io/distroless/static:nonroot",
	})

	t.Run("uses distroless base", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "FROM gcr.io/distroless/static:nonroot")
	})
}

func TestDockerfileBuilder_Complete(t *testing.T) {
	builder := NewDockerfileBuilder(&DockerfileConfig{
		BaseImage:   "golang:1.21-alpine",
		AppName:     "vaultaire",
		MultiStage:  true,
		RuntimeBase: "alpine:3.19",
		Workdir:     "/app",
		Ports:       []int{8080},
		Entrypoint:  []string{"/app/vaultaire"},
	})

	t.Run("generates complete dockerfile", func(t *testing.T) {
		dockerfile := builder.Build()
		lines := strings.Split(dockerfile, "\n")
		require.NotEmpty(t, lines)
		assert.True(t, len(lines) > 10)
	})
}

func TestGenerateProductionDockerfile(t *testing.T) {
	t.Run("generates production-ready dockerfile", func(t *testing.T) {
		dockerfile := GenerateProductionDockerfile("vaultaire", "1.0.0")
		assert.Contains(t, dockerfile, "AS builder")
		assert.Contains(t, dockerfile, "USER")
		assert.Contains(t, dockerfile, "HEALTHCHECK")
	})
}

func TestCopyInstruction(t *testing.T) {
	t.Run("creates copy instruction", func(t *testing.T) {
		ci := CopyInstruction{Src: ".", Dst: "/app", From: "builder"}
		assert.Equal(t, "builder", ci.From)
	})
}

func TestHealthcheckConfig(t *testing.T) {
	t.Run("creates healthcheck config", func(t *testing.T) {
		hc := &HealthcheckConfig{Command: "curl localhost", Retries: 3}
		assert.Equal(t, 3, hc.Retries)
	})
}
