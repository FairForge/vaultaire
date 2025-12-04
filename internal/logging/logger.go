// internal/logging/logger.go
package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

// Log levels
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// Log formats
const (
	FormatJSON   = "json"
	FormatText   = "text"
	FormatLogfmt = "logfmt"
)

// Context keys
type contextKey string

var (
	ContextKeyRequestID = contextKey("request_id")
	ContextKeyTenantID  = contextKey("tenant_id")
	ContextKeyUserID    = contextKey("user_id")
)

// LevelValue returns numeric value for level comparison
func LevelValue(level string) int {
	switch level {
	case LevelDebug:
		return 0
	case LevelInfo:
		return 1
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	case LevelFatal:
		return 4
	default:
		return 1
	}
}

// LoggerConfig configures a logger
type LoggerConfig struct {
	Level  string    `json:"level"`
	Format string    `json:"format"`
	Output io.Writer `json:"-"`
	Async  bool      `json:"async"`
}

// Validate checks configuration
func (c *LoggerConfig) Validate() error {
	validLevels := map[string]bool{
		LevelDebug: true, LevelInfo: true, LevelWarn: true,
		LevelError: true, LevelFatal: true, "": true,
	}
	if !validLevels[c.Level] {
		return fmt.Errorf("logging: invalid level: %s", c.Level)
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *LoggerConfig) ApplyDefaults() {
	if c.Level == "" {
		c.Level = LevelInfo
	}
	if c.Format == "" {
		c.Format = FormatJSON
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
}

// LogEntry represents a log entry
type LogEntry struct {
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Timestamp time.Time              `json:"timestamp"`
	Logger    string                 `json:"logger,omitempty"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// Logger is a structured logger
type Logger struct {
	config  *LoggerConfig
	fields  map[string]interface{}
	name    string
	asyncCh chan *LogEntry
	mu      sync.Mutex
}

// NewLogger creates a logger
func NewLogger(config *LoggerConfig) *Logger {
	if config == nil {
		config = &LoggerConfig{}
	}
	config.ApplyDefaults()

	l := &Logger{
		config: config,
		fields: make(map[string]interface{}),
	}

	if config.Async {
		l.asyncCh = make(chan *LogEntry, 1000)
		go l.asyncWriter()
	}

	return l
}

func (l *Logger) asyncWriter() {
	for entry := range l.asyncCh {
		l.writeEntry(entry)
	}
}

// Sync flushes async logs
func (l *Logger) Sync() {
	if l.asyncCh != nil {
		// Wait for channel to drain
		for len(l.asyncCh) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// With returns a logger with additional fields
func (l *Logger) With(keyvals ...string) *Logger {
	child := &Logger{
		config:  l.config,
		fields:  make(map[string]interface{}),
		name:    l.name,
		asyncCh: l.asyncCh,
	}

	// Copy parent fields
	for k, v := range l.fields {
		child.fields[k] = v
	}

	// Add new fields
	for i := 0; i < len(keyvals)-1; i += 2 {
		child.fields[keyvals[i]] = keyvals[i+1]
	}

	return child
}

// WithError returns a logger with error field
func (l *Logger) WithError(err error) *Logger {
	child := l.With()
	child.fields["error"] = err.Error()
	return child
}

// WithContext extracts fields from context
func (l *Logger) WithContext(ctx context.Context) *Logger {
	child := l.With()

	if v := ctx.Value(ContextKeyRequestID); v != nil {
		child.fields["request_id"] = v
	}
	if v := ctx.Value(ContextKeyTenantID); v != nil {
		child.fields["tenant_id"] = v
	}
	if v := ctx.Value(ContextKeyUserID); v != nil {
		child.fields["user_id"] = v
	}

	return child
}

// Named returns a named child logger
func (l *Logger) Named(name string) *Logger {
	child := l.With()
	if l.name != "" {
		child.name = l.name + "." + name
	} else {
		child.name = name
	}
	return child
}

func (l *Logger) log(level, message string) {
	if LevelValue(level) < LevelValue(l.config.Level) {
		return
	}

	entry := &LogEntry{
		Level:     level,
		Message:   message,
		Timestamp: time.Now().UTC(),
		Logger:    l.name,
		Fields:    l.fields,
	}

	if l.asyncCh != nil {
		select {
		case l.asyncCh <- entry:
		default:
			// Channel full, write synchronously
			l.writeEntry(entry)
		}
	} else {
		l.writeEntry(entry)
	}
}

func (l *Logger) writeEntry(entry *LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var output string
	switch l.config.Format {
	case FormatJSON:
		output = l.formatJSON(entry)
	case FormatText:
		output = l.formatText(entry)
	case FormatLogfmt:
		output = l.formatLogfmt(entry)
	default:
		output = l.formatJSON(entry)
	}

	_, _ = fmt.Fprint(l.config.Output, output)
}

func (l *Logger) formatJSON(entry *LogEntry) string {
	data := map[string]interface{}{
		"level":     entry.Level,
		"message":   entry.Message,
		"timestamp": entry.Timestamp.Format(time.RFC3339),
	}

	if entry.Logger != "" {
		data["logger"] = entry.Logger
	}

	for k, v := range entry.Fields {
		data[k] = v
	}

	bytes, _ := json.Marshal(data)
	return string(bytes) + "\n"
}

func (l *Logger) formatText(entry *LogEntry) string {
	var sb strings.Builder
	sb.WriteString(entry.Timestamp.Format("2006-01-02 15:04:05"))
	sb.WriteString(" ")
	sb.WriteString(strings.ToUpper(entry.Level))
	sb.WriteString(" ")
	sb.WriteString(entry.Message)

	for k, v := range entry.Fields {
		sb.WriteString(" ")
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(fmt.Sprintf("%v", v))
	}

	sb.WriteString("\n")
	return sb.String()
}

func (l *Logger) formatLogfmt(entry *LogEntry) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ts=%s ", entry.Timestamp.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("level=%s ", entry.Level))
	sb.WriteString(fmt.Sprintf("msg=%q ", entry.Message))

	for k, v := range entry.Fields {
		sb.WriteString(fmt.Sprintf("%s=%v ", k, v))
	}

	sb.WriteString("\n")
	return sb.String()
}

// Debug logs at debug level
func (l *Logger) Debug(message string) {
	l.log(LevelDebug, message)
}

// Info logs at info level
func (l *Logger) Info(message string) {
	l.log(LevelInfo, message)
}

// Warn logs at warn level
func (l *Logger) Warn(message string) {
	l.log(LevelWarn, message)
}

// Error logs at error level
func (l *Logger) Error(message string) {
	l.log(LevelError, message)
}

// Fatal logs at fatal level
func (l *Logger) Fatal(message string) {
	l.log(LevelFatal, message)
}

// AggregatorConfig configures the log aggregator
type AggregatorConfig struct {
	BufferSize    int           `json:"buffer_size"`
	FlushInterval time.Duration `json:"flush_interval"`
	MinLevel      string        `json:"min_level"`
	SampleRate    float64       `json:"sample_rate"`
}

// AggregatorStats contains aggregator statistics
type AggregatorStats struct {
	Buffered int64
	Flushed  int64
	Dropped  int64
	Sampled  int64
}

// Destination is a log destination
type Destination interface {
	Write(entries []*LogEntry) error
}

// WriterDestination writes to an io.Writer
type WriterDestination struct {
	Writer io.Writer
}

// Write writes entries to the writer
func (d *WriterDestination) Write(entries []*LogEntry) error {
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		_, _ = d.Writer.Write(append(data, '\n'))
	}
	return nil
}

// LogAggregator aggregates log entries
type LogAggregator struct {
	config       *AggregatorConfig
	buffer       []*LogEntry
	destinations []Destination
	onFlush      func([]*LogEntry)
	stats        AggregatorStats
	stopCh       chan struct{}
	mu           sync.Mutex
}

// NewLogAggregator creates a log aggregator
func NewLogAggregator(config *AggregatorConfig) *LogAggregator {
	if config == nil {
		config = &AggregatorConfig{}
	}
	if config.BufferSize == 0 {
		config.BufferSize = 1000
	}
	if config.FlushInterval == 0 {
		config.FlushInterval = 5 * time.Second
	}
	if config.SampleRate == 0 {
		config.SampleRate = 1.0
	}

	return &LogAggregator{
		config:       config,
		buffer:       make([]*LogEntry, 0, config.BufferSize),
		destinations: make([]Destination, 0),
		stopCh:       make(chan struct{}),
	}
}

// Add adds an entry to the buffer
func (a *LogAggregator) Add(entry *LogEntry) {
	// Level filtering
	if a.config.MinLevel != "" {
		if LevelValue(entry.Level) < LevelValue(a.config.MinLevel) {
			return
		}
	}

	// Sampling
	if a.config.SampleRate < 1.0 {
		if rand.Float64() > a.config.SampleRate {
			a.mu.Lock()
			a.stats.Sampled++
			a.mu.Unlock()
			return
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.buffer) >= a.config.BufferSize {
		a.stats.Dropped++
		return
	}

	a.buffer = append(a.buffer, entry)
	a.stats.Buffered++
}

// AddDestination adds a log destination
func (a *LogAggregator) AddDestination(dest Destination) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.destinations = append(a.destinations, dest)
}

// OnFlush sets the flush callback
func (a *LogAggregator) OnFlush(fn func([]*LogEntry)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onFlush = fn
}

// Start starts the flush loop
func (a *LogAggregator) Start() {
	go func() {
		ticker := time.NewTicker(a.config.FlushInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				a.Flush()
			case <-a.stopCh:
				return
			}
		}
	}()
}

// Stop stops the flush loop
func (a *LogAggregator) Stop() {
	close(a.stopCh)
	a.Flush()
}

// Flush flushes buffered entries
func (a *LogAggregator) Flush() {
	a.mu.Lock()
	if len(a.buffer) == 0 {
		a.mu.Unlock()
		return
	}

	entries := make([]*LogEntry, len(a.buffer))
	copy(entries, a.buffer)
	a.buffer = a.buffer[:0]
	a.stats.Flushed += int64(len(entries))
	a.stats.Buffered -= int64(len(entries))

	onFlush := a.onFlush
	destinations := a.destinations
	a.mu.Unlock()

	// Call flush callback
	if onFlush != nil {
		onFlush(entries)
	}

	// Write to destinations
	for _, dest := range destinations {
		_ = dest.Write(entries)
	}
}

// Stats returns aggregator statistics
func (a *LogAggregator) Stats() AggregatorStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// Ensure errors is used
var _ = errors.New
