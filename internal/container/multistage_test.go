// internal/container/multistage_test.go
package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildStage_Validate(t *testing.T) {
	t.Run("valid stage passes", func(t *testing.T) {
		stage := &BuildStage{
			Name:      "builder",
			BaseImage: "golang:1.21-alpine",
		}
		err := stage.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		stage := &BuildStage{BaseImage: "golang:1.21"}
		err := stage.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects empty base image", func(t *testing.T) {
		stage := &BuildStage{Name: "builder"}
		err := stage.Validate()
		assert.Error(t, err)
	})
}

func TestNewMultiStageBuilder(t *testing.T) {
	t.Run("creates builder", func(t *testing.T) {
		builder := NewMultiStageBuilder()
		assert.NotNil(t, builder)
	})
}

func TestMultiStageBuilder_AddStage(t *testing.T) {
	builder := NewMultiStageBuilder()

	t.Run("adds stage", func(t *testing.T) {
		err := builder.AddStage(&BuildStage{
			Name:      "builder",
			BaseImage: "golang:1.21-alpine",
		})
		assert.NoError(t, err)
		assert.Len(t, builder.Stages(), 1)
	})

	t.Run("rejects duplicate stage name", func(t *testing.T) {
		_ = builder.AddStage(&BuildStage{Name: "dup", BaseImage: "alpine"})
		err := builder.AddStage(&BuildStage{Name: "dup", BaseImage: "alpine"})
		assert.Error(t, err)
	})
}

func TestMultiStageBuilder_DependencyChain(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "base",
		BaseImage: "golang:1.21-alpine",
	})
	_ = builder.AddStage(&BuildStage{
		Name:      "builder",
		BaseImage: "base",
		DependsOn: []string{"base"},
	})
	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		DependsOn: []string{"builder"},
	})

	t.Run("resolves dependency order", func(t *testing.T) {
		order, err := builder.ResolveBuildOrder()
		require.NoError(t, err)
		assert.Equal(t, []string{"base", "builder", "runtime"}, order)
	})
}

func TestMultiStageBuilder_CyclicDependency(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "a",
		BaseImage: "alpine",
		DependsOn: []string{"b"},
	})
	_ = builder.AddStage(&BuildStage{
		Name:      "b",
		BaseImage: "alpine",
		DependsOn: []string{"a"},
	})

	t.Run("detects cyclic dependency", func(t *testing.T) {
		_, err := builder.ResolveBuildOrder()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cyclic")
	})
}

func TestMultiStageBuilder_BuildTarget(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{Name: "builder", BaseImage: "golang:1.21"})
	_ = builder.AddStage(&BuildStage{Name: "test", BaseImage: "builder", DependsOn: []string{"builder"}})
	_ = builder.AddStage(&BuildStage{Name: "runtime", BaseImage: "alpine", DependsOn: []string{"builder"}})

	t.Run("builds specific target", func(t *testing.T) {
		dockerfile := builder.BuildTarget("test")
		assert.Contains(t, dockerfile, "FROM golang:1.21 AS builder")
		assert.Contains(t, dockerfile, "FROM builder AS test")
	})
}

func TestMultiStageBuilder_StageInstructions(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "builder",
		BaseImage: "golang:1.21-alpine",
		Workdir:   "/app",
		Commands: []string{
			"COPY go.mod go.sum ./",
			"RUN go mod download",
			"COPY . .",
			"RUN go build -o /app/server ./cmd/server",
		},
	})

	t.Run("includes stage commands", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "COPY go.mod")
		assert.Contains(t, dockerfile, "RUN go mod download")
		assert.Contains(t, dockerfile, "RUN go build")
	})
}

func TestMultiStageBuilder_CopyFromStage(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "builder",
		BaseImage: "golang:1.21-alpine",
	})
	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		CopyFrom: []StageCopy{
			{FromStage: "builder", Src: "/app/server", Dst: "/usr/local/bin/server"},
		},
	})

	t.Run("copies from previous stage", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "COPY --from=builder /app/server /usr/local/bin/server")
	})
}

func TestMultiStageBuilder_EnvAndArgs(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "builder",
		BaseImage: "golang:1.21-alpine",
		Args:      map[string]string{"VERSION": "1.0.0"},
		Env:       map[string]string{"CGO_ENABLED": "0"},
	})

	t.Run("includes args and env", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "ARG VERSION")
		assert.Contains(t, dockerfile, "ENV CGO_ENABLED=0")
	})
}

func TestMultiStageBuilder_Labels(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		Labels: map[string]string{
			"org.opencontainers.image.version": "1.0.0",
			"maintainer":                       "team@example.com",
		},
	})

	t.Run("includes labels", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "LABEL")
	})
}

func TestMultiStageBuilder_Expose(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		Expose:    []int{8080, 9090},
	})

	t.Run("exposes ports", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "EXPOSE 8080")
		assert.Contains(t, dockerfile, "EXPOSE 9090")
	})
}

func TestMultiStageBuilder_User(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		User:      "appuser",
		UserID:    1000,
	})

	t.Run("sets user", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "USER appuser")
	})
}

func TestMultiStageBuilder_Entrypoint(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:       "runtime",
		BaseImage:  "alpine:3.19",
		Entrypoint: []string{"/usr/local/bin/server"},
		Cmd:        []string{"--config", "/etc/config.yaml"},
	})

	t.Run("sets entrypoint and cmd", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "ENTRYPOINT")
		assert.Contains(t, dockerfile, "CMD")
	})
}

func TestMultiStageBuilder_Healthcheck(t *testing.T) {
	builder := NewMultiStageBuilder()

	_ = builder.AddStage(&BuildStage{
		Name:      "runtime",
		BaseImage: "alpine:3.19",
		Healthcheck: &HealthcheckConfig{
			Command:  "wget -q --spider http://localhost:8080/health",
			Interval: "30s",
			Timeout:  "5s",
			Retries:  3,
		},
	})

	t.Run("includes healthcheck", func(t *testing.T) {
		dockerfile := builder.Build()
		assert.Contains(t, dockerfile, "HEALTHCHECK")
	})
}

func TestMultiStageBuilder_GetStage(t *testing.T) {
	builder := NewMultiStageBuilder()
	_ = builder.AddStage(&BuildStage{Name: "builder", BaseImage: "golang:1.21"})

	t.Run("gets existing stage", func(t *testing.T) {
		stage := builder.GetStage("builder")
		assert.NotNil(t, stage)
		assert.Equal(t, "builder", stage.Name)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		stage := builder.GetStage("unknown")
		assert.Nil(t, stage)
	})
}

func TestMultiStageBuilder_RemoveStage(t *testing.T) {
	builder := NewMultiStageBuilder()
	_ = builder.AddStage(&BuildStage{Name: "builder", BaseImage: "golang:1.21"})

	t.Run("removes stage", func(t *testing.T) {
		err := builder.RemoveStage("builder")
		assert.NoError(t, err)
		assert.Len(t, builder.Stages(), 0)
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := builder.RemoveStage("unknown")
		assert.Error(t, err)
	})
}

func TestStageCopy(t *testing.T) {
	t.Run("creates stage copy", func(t *testing.T) {
		sc := StageCopy{
			FromStage: "builder",
			Src:       "/app/bin",
			Dst:       "/usr/local/bin",
			Chown:     "app:app",
		}
		assert.Equal(t, "builder", sc.FromStage)
	})
}

func TestCreateGoBuildStages(t *testing.T) {
	t.Run("creates standard go build stages", func(t *testing.T) {
		builder := CreateGoBuildStages("vaultaire", "1.0.0")
		require.NotNil(t, builder)

		stages := builder.Stages()
		assert.GreaterOrEqual(t, len(stages), 2)
	})
}
