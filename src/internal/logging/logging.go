package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"overseer/internal/config"
)

var levelMap = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Logger wraps *slog.Logger and adds component/execution-context helpers.
// It embeds *slog.Logger so standard Info/Warn/Error/Debug calls work
// unchanged.
type Logger struct {
	*slog.Logger
	ring *Ring
}

// RecentErrors returns the latest ERROR records (redacted, bounded).
func (l *Logger) RecentErrors() []string { return l.ring.Recent() }

// New builds a structured logger from logging configuration.
// Supported formats: "json" and "text"; supported levels:
// debug|info|warn|error (validation happens in config.Validate; New
// still fails closed on an unknown level).
func New(cfg config.LoggingConfig, w io.Writer) (*Logger, error) {
	return NewRedacting(cfg, w, nil)
}

// NewRedacting is New with central secret redaction (WBS 18.75): every
// occurrence of a value returned by secrets — read at each log record, so
// rotated secrets are covered — is replaced with [REDACTED] in the message
// and in every attribute (errors included) before encoding.
func NewRedacting(cfg config.LoggingConfig, w io.Writer, secrets func() []string) (*Logger, error) {
	level, ok := levelMap[cfg.Level]
	if !ok {
		return nil, fmt.Errorf("logging: unsupported level %q", cfg.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	if secrets != nil {
		opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			return redactAttr(a, secrets())
		}
	}
	var h slog.Handler
	switch cfg.Format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: unsupported format %q", cfg.Format)
	}
	ring := newRing()
	ringOpts := &slog.HandlerOptions{Level: slog.LevelError, ReplaceAttr: opts.ReplaceAttr}
	tee := teeHandler{primary: h, ring: slog.NewJSONHandler(ring, ringOpts)}
	return &Logger{Logger: slog.New(contextHandler{tee}), ring: ring}, nil
}

// WithComponent returns a child logger tagged with the component name.
func (l *Logger) WithComponent(component string) *Logger {
	return l.child("component", component)
}

// WithProjectID returns a child logger carrying the project identifier.
func (l *Logger) WithProjectID(id string) *Logger {
	return l.child("project_id", id)
}

// WithPhaseID returns a child logger carrying the phase identifier.
func (l *Logger) WithPhaseID(id string) *Logger {
	return l.child("phase_id", id)
}

// WithWorkUnitID returns a child logger carrying the work-unit identifier.
func (l *Logger) WithWorkUnitID(id string) *Logger {
	return l.child("work_unit_id", id)
}

// WithExecutionID returns a child logger carrying the execution identifier.
func (l *Logger) WithExecutionID(id string) *Logger {
	return l.child("execution_id", id)
}

func (l *Logger) child(key, value string) *Logger {
	return &Logger{Logger: l.Logger.With(key, value), ring: l.ring}
}

// LogStartup emits a startup record for the application. It uses the
// config's redacted String() so credentials never enter the log stream.
func LogStartup(l *Logger, cfg config.Config) {
	l.WithComponent("bootstrap").Info("starting overseer", "config", cfg.String())
}

// RedactedText replaces a secret value in logs.
const RedactedText = "[REDACTED]"

func redactString(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 6 && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, RedactedText)
		}
	}
	return s
}

func redactAttr(a slog.Attr, secrets []string) slog.Attr {
	if len(secrets) == 0 {
		return a
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if r := redactString(a.Value.String(), secrets); r != a.Value.String() {
			a.Value = slog.StringValue(r)
		}
	case slog.KindAny:
		var s string
		switch v := a.Value.Any().(type) {
		case error:
			s = v.Error()
		case fmt.Stringer:
			s = v.String()
		default:
			s = fmt.Sprint(v)
		}
		if r := redactString(s, secrets); r != s {
			a.Value = slog.StringValue(r)
		}
	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, x := range attrs {
			out[i] = redactAttr(x, secrets)
		}
		a.Value = slog.GroupValue(out...)
	}
	return a
}
