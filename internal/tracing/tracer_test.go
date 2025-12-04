// internal/tracing/tracer_test.go
package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTracerConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &TracerConfig{
			ServiceName: "vaultaire",
			Endpoint:    "http://localhost:4317",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty service name", func(t *testing.T) {
		config := &TracerConfig{}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "service")
	})

	t.Run("applies defaults", func(t *testing.T) {
		config := &TracerConfig{ServiceName: "test"}
		config.ApplyDefaults()
		assert.Equal(t, 1.0, config.SampleRate)
	})
}

func TestNewTracer(t *testing.T) {
	t.Run("creates tracer", func(t *testing.T) {
		tracer := NewTracer(&TracerConfig{ServiceName: "test"})
		assert.NotNil(t, tracer)
	})
}

func TestTracer_StartSpan(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	t.Run("creates root span", func(t *testing.T) {
		ctx, span := tracer.StartSpan(context.Background(), "operation")
		defer span.End()

		assert.NotNil(t, span)
		assert.NotEmpty(t, span.TraceID())
		assert.NotEmpty(t, span.SpanID())
		assert.NotNil(t, ctx)
	})

	t.Run("creates child span", func(t *testing.T) {
		ctx, parent := tracer.StartSpan(context.Background(), "parent")
		_, child := tracer.StartSpan(ctx, "child")
		defer parent.End()
		defer child.End()

		assert.Equal(t, parent.TraceID(), child.TraceID())
		assert.NotEqual(t, parent.SpanID(), child.SpanID())
		assert.Equal(t, parent.SpanID(), child.ParentID())
	})
}

func TestSpan_Attributes(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})
	_, span := tracer.StartSpan(context.Background(), "test")
	defer span.End()

	t.Run("sets string attribute", func(t *testing.T) {
		span.SetAttribute("user.id", "123")
		attrs := span.Attributes()
		assert.Equal(t, "123", attrs["user.id"])
	})

	t.Run("sets int attribute", func(t *testing.T) {
		span.SetAttribute("http.status_code", 200)
		attrs := span.Attributes()
		assert.Equal(t, 200, attrs["http.status_code"])
	})

	t.Run("sets multiple attributes", func(t *testing.T) {
		span.SetAttributes(map[string]interface{}{
			"method": "GET",
			"path":   "/api/users",
		})
		attrs := span.Attributes()
		assert.Equal(t, "GET", attrs["method"])
		assert.Equal(t, "/api/users", attrs["path"])
	})
}

func TestSpan_Events(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})
	_, span := tracer.StartSpan(context.Background(), "test")
	defer span.End()

	t.Run("adds event", func(t *testing.T) {
		span.AddEvent("cache.miss", map[string]interface{}{
			"key": "user:123",
		})

		events := span.Events()
		assert.Len(t, events, 1)
		assert.Equal(t, "cache.miss", events[0].Name)
	})
}

func TestSpan_Status(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	t.Run("sets OK status", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "test")
		span.SetStatus(StatusOK, "")
		span.End()

		assert.Equal(t, StatusOK, span.Status())
	})

	t.Run("sets error status", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "test")
		span.SetStatus(StatusError, "something went wrong")
		span.End()

		assert.Equal(t, StatusError, span.Status())
		assert.Equal(t, "something went wrong", span.StatusMessage())
	})

	t.Run("records error", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "test")
		span.RecordError(assert.AnError)
		span.End()

		assert.Equal(t, StatusError, span.Status())
		events := span.Events()
		assert.NotEmpty(t, events)
		assert.Equal(t, "exception", events[0].Name)
	})
}

func TestSpan_Timing(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	t.Run("records duration", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "test")
		time.Sleep(10 * time.Millisecond)
		span.End()

		assert.Greater(t, span.Duration(), 10*time.Millisecond)
	})
}

func TestTracer_HTTPMiddleware(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := SpanFromContext(r.Context())
		assert.NotNil(t, span)
		w.WriteHeader(http.StatusOK)
	})

	t.Run("traces HTTP requests", func(t *testing.T) {
		wrapped := tracer.HTTPMiddleware(handler)
		req := httptest.NewRequest("GET", "/api/users", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("extracts trace context from headers", func(t *testing.T) {
		wrapped := tracer.HTTPMiddleware(handler)
		req := httptest.NewRequest("GET", "/api/users", nil)
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestTracer_InjectExtract(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})
	ctx, span := tracer.StartSpan(context.Background(), "test")
	defer span.End()

	t.Run("injects into headers", func(t *testing.T) {
		headers := make(http.Header)
		tracer.Inject(ctx, headers)

		assert.NotEmpty(t, headers.Get("traceparent"))
	})

	t.Run("extracts from headers", func(t *testing.T) {
		headers := make(http.Header)
		tracer.Inject(ctx, headers)

		extractedCtx := tracer.Extract(context.Background(), headers)
		extractedSpan := SpanFromContext(extractedCtx)

		// When extracted, we get a span context, not a full span
		assert.NotNil(t, extractedCtx)
		// The trace ID should propagate
		if extractedSpan != nil {
			assert.Equal(t, span.TraceID(), extractedSpan.TraceID())
		}
	})
}

func TestTracer_Sampling(t *testing.T) {
	t.Run("samples based on rate", func(t *testing.T) {
		tracer := NewTracer(&TracerConfig{
			ServiceName: "test",
			SampleRate:  0.5,
		})

		sampled := 0
		for i := 0; i < 100; i++ {
			_, span := tracer.StartSpan(context.Background(), "test")
			if span.IsSampled() {
				sampled++
			}
			span.End()
		}

		// Should be roughly 50%, allow variance
		assert.Greater(t, sampled, 20)
		assert.Less(t, sampled, 80)
	})
}

func TestSpanContext(t *testing.T) {
	t.Run("stores span in context", func(t *testing.T) {
		tracer := NewTracer(&TracerConfig{ServiceName: "test"})
		ctx, span := tracer.StartSpan(context.Background(), "test")
		defer span.End()

		retrieved := SpanFromContext(ctx)
		assert.Equal(t, span.SpanID(), retrieved.SpanID())
	})

	t.Run("returns nil for empty context", func(t *testing.T) {
		span := SpanFromContext(context.Background())
		assert.Nil(t, span)
	})
}

func TestTraceExporter(t *testing.T) {
	exporter := NewMemoryExporter()
	tracer := NewTracer(&TracerConfig{
		ServiceName: "test",
		Exporter:    exporter,
	})

	t.Run("exports completed spans", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "test-op")
		span.SetAttribute("key", "value")
		span.End()

		spans := exporter.Spans()
		require.Len(t, spans, 1)
		assert.Equal(t, "test-op", spans[0].Name)
	})

	t.Run("clears exported spans", func(t *testing.T) {
		exporter.Clear()
		spans := exporter.Spans()
		assert.Empty(t, spans)
	})
}

func TestSpanKind(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	t.Run("sets span kind", func(t *testing.T) {
		_, span := tracer.StartSpan(context.Background(), "server-op", WithSpanKind(SpanKindServer))
		defer span.End()

		assert.Equal(t, SpanKindServer, span.Kind())
	})
}

func TestSpanLinks(t *testing.T) {
	tracer := NewTracer(&TracerConfig{ServiceName: "test"})

	t.Run("adds span links", func(t *testing.T) {
		_, span1 := tracer.StartSpan(context.Background(), "span1")
		span1.End()

		_, span2 := tracer.StartSpan(context.Background(), "span2")
		span2.AddLink(span1.TraceID(), span1.SpanID(), nil)
		span2.End()

		links := span2.Links()
		assert.Len(t, links, 1)
	})
}

func TestStatusCodes(t *testing.T) {
	t.Run("defines status codes", func(t *testing.T) {
		assert.Equal(t, 0, StatusUnset)
		assert.Equal(t, 1, StatusOK)
		assert.Equal(t, 2, StatusError)
	})
}

func TestSpanKinds(t *testing.T) {
	t.Run("defines span kinds", func(t *testing.T) {
		assert.Equal(t, 0, SpanKindInternal)
		assert.Equal(t, 1, SpanKindServer)
		assert.Equal(t, 2, SpanKindClient)
		assert.Equal(t, 3, SpanKindProducer)
		assert.Equal(t, 4, SpanKindConsumer)
	})
}
