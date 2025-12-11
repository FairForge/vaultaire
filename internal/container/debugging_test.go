// internal/container/debugging_test.go
package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecConfig(t *testing.T) {
	t.Run("creates exec config", func(t *testing.T) {
		config := &ExecConfig{
			ContainerID: "abc123",
			Command:     []string{"/bin/sh", "-c", "ls -la"},
			Env:         []string{"DEBUG=true"},
			WorkingDir:  "/app",
			User:        "appuser",
			Tty:         true,
			AttachStdin: true,
		}
		assert.Equal(t, "abc123", config.ContainerID)
		assert.Equal(t, []string{"/bin/sh", "-c", "ls -la"}, config.Command)
		assert.True(t, config.Tty)
	})
}

func TestExecConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &ExecConfig{
			ContainerID: "abc123",
			Command:     []string{"ls"},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty container ID", func(t *testing.T) {
		config := &ExecConfig{Command: []string{"ls"}}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects empty command", func(t *testing.T) {
		config := &ExecConfig{ContainerID: "abc123"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestExecResult(t *testing.T) {
	t.Run("creates exec result", func(t *testing.T) {
		result := &ExecResult{
			ExitCode: 0,
			Stdout:   "file1.txt\nfile2.txt\n",
			Stderr:   "",
			Duration: 50 * time.Millisecond,
		}
		assert.Equal(t, 0, result.ExitCode)
		assert.Contains(t, result.Stdout, "file1.txt")
	})

	t.Run("checks success", func(t *testing.T) {
		result := &ExecResult{ExitCode: 0}
		assert.True(t, result.Success())

		result = &ExecResult{ExitCode: 1}
		assert.False(t, result.Success())
	})
}

func TestNewDebugger(t *testing.T) {
	t.Run("creates debugger", func(t *testing.T) {
		debugger := NewDebugger(&DebuggerConfig{
			Provider: "mock",
		})
		assert.NotNil(t, debugger)
	})
}

func TestDebugger_Exec(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("executes command in container", func(t *testing.T) {
		ctx := context.Background()
		config := &ExecConfig{
			ContainerID: "container-123",
			Command:     []string{"echo", "hello"},
		}
		result, err := debugger.Exec(ctx, config)
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})
}

func TestDebugger_Attach(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("attaches to container", func(t *testing.T) {
		ctx := context.Background()
		session, err := debugger.Attach(ctx, "container-123", &AttachOptions{
			Stdin:  true,
			Stdout: true,
			Stderr: true,
		})
		require.NoError(t, err)
		assert.NotNil(t, session)
		defer func() { _ = session.Close() }()
	})
}

func TestDebugger_Logs(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("gets container logs", func(t *testing.T) {
		ctx := context.Background()
		logs, err := debugger.Logs(ctx, "container-123", &LogsOptions{
			Tail:       100,
			Timestamps: true,
		})
		require.NoError(t, err)
		assert.NotEmpty(t, logs)
	})
}

func TestDebugger_Inspect(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("inspects container", func(t *testing.T) {
		ctx := context.Background()
		info, err := debugger.Inspect(ctx, "container-123")
		require.NoError(t, err)
		assert.NotNil(t, info)
		assert.Equal(t, "container-123", info.ID)
	})
}

func TestDebugger_Top(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("gets container processes", func(t *testing.T) {
		ctx := context.Background()
		procs, err := debugger.Top(ctx, "container-123")
		require.NoError(t, err)
		assert.NotEmpty(t, procs)
	})
}

func TestDebugger_CopyFrom(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("copies file from container", func(t *testing.T) {
		ctx := context.Background()
		data, err := debugger.CopyFrom(ctx, "container-123", "/app/config.yaml")
		require.NoError(t, err)
		assert.NotNil(t, data)
	})
}

func TestDebugger_CopyTo(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("copies file to container", func(t *testing.T) {
		ctx := context.Background()
		err := debugger.CopyTo(ctx, "container-123", "/app/config.yaml", []byte("key: value"))
		assert.NoError(t, err)
	})
}

func TestContainerInspect(t *testing.T) {
	t.Run("creates container inspect", func(t *testing.T) {
		info := &ContainerInspect{
			ID:      "abc123",
			Name:    "vaultaire",
			Image:   "ghcr.io/fairforge/vaultaire:1.0.0",
			Created: time.Now(),
			State: &ContainerState{
				Status:  StateRunning,
				Running: true,
			},
			Config: &InspectConfig{
				Hostname: "vaultaire-1",
				Env:      []string{"LOG_LEVEL=info"},
			},
			NetworkSettings: &NetworkSettings{
				IPAddress: "172.17.0.2",
				Ports: map[string][]PortBinding{
					"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}},
				},
			},
		}
		assert.Equal(t, "abc123", info.ID)
		assert.True(t, info.State.Running)
	})
}

func TestProcessInfo(t *testing.T) {
	t.Run("creates process info", func(t *testing.T) {
		proc := &ProcessInfo{
			PID:     1234,
			PPID:    1,
			User:    "root",
			CPU:     5.5,
			Memory:  2.3,
			Command: "/app/vaultaire serve",
			Started: time.Now(),
		}
		assert.Equal(t, 1234, proc.PID)
		assert.Equal(t, "root", proc.User)
	})
}

func TestAttachSession(t *testing.T) {
	t.Run("creates attach session", func(t *testing.T) {
		session := &AttachSession{
			ContainerID: "abc123",
			Connected:   true,
		}
		assert.True(t, session.Connected)
	})
}

func TestLogsOptions(t *testing.T) {
	t.Run("creates logs options", func(t *testing.T) {
		opts := &LogsOptions{
			Follow:     true,
			Tail:       100,
			Since:      time.Now().Add(-1 * time.Hour),
			Until:      time.Now(),
			Timestamps: true,
			Details:    true,
		}
		assert.True(t, opts.Follow)
		assert.Equal(t, 100, opts.Tail)
	})
}

func TestDebugger_Events(t *testing.T) {
	debugger := NewDebugger(&DebuggerConfig{Provider: "mock"})

	t.Run("streams container events", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		ch, err := debugger.Events(ctx, &EventsOptions{
			ContainerID: "container-123",
		})
		require.NoError(t, err)
		assert.NotNil(t, ch)
	})
}

func TestContainerEvent(t *testing.T) {
	t.Run("creates container event", func(t *testing.T) {
		event := &ContainerEvent{
			Type:        EventTypeContainer,
			Action:      "start",
			ContainerID: "abc123",
			Timestamp:   time.Now(),
			Attributes:  map[string]string{"name": "vaultaire"},
		}
		assert.Equal(t, "start", event.Action)
	})
}

func TestEventType(t *testing.T) {
	t.Run("event types", func(t *testing.T) {
		assert.Equal(t, EventType("container"), EventTypeContainer)
		assert.Equal(t, EventType("image"), EventTypeImage)
		assert.Equal(t, EventType("network"), EventTypeNetwork)
		assert.Equal(t, EventType("volume"), EventTypeVolume)
	})
}
