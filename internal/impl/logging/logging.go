// Package logging provides Canopy's structured logging (AGENTS.md "Logging"
// convention): a zap.Logger constructor, and a minimal slog.Handler that
// forwards records to it so agent.Config.Logger (a *slog.Logger, per Design
// §3.10) can be backed by zap without MAF-Go needing to know zap exists.
//
// This is intentionally small — a full-fidelity slog<->zap bridge (correct
// nested-group namespacing, zap.Object/zap.Array support for structured
// slog.Value kinds, etc.) is not needed for Canopy's use, which is just
// "route the framework's run/middleware/provider diagnostics into the same
// place everything else logs to."
package logging

import (
	"context"
	"log/slog"
	"slices"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewZapLogger constructs Canopy's default zap.Logger: zap.NewDevelopment()
// when dev is true (human-readable console output), zap.NewProduction()
// otherwise (structured JSON). Either constructor's own sensible defaults
// are used as-is — Canopy has no additional zap configuration of its own.
func NewZapLogger(dev bool) (*zap.Logger, error) {
	if dev {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// NewSlogLogger wraps a *zap.Logger in a *slog.Logger via NewSlogHandler, so
// it can be assigned to agent.Config.Logger (Design §3.10).
func NewSlogLogger(zapLogger *zap.Logger) *slog.Logger {
	return slog.New(NewSlogHandler(zapLogger))
}

// NewSlogHandler returns a slog.Handler that forwards every record to
// zapLogger, translating slog levels/attrs to zap levels/fields.
func NewSlogHandler(zapLogger *zap.Logger) slog.Handler {
	return &zapHandler{logger: zapLogger}
}

// zapHandler is a minimal slog.Handler backed by a *zap.Logger. Group
// nesting is approximated by dot-joining the group name onto each attr key
// (e.g. WithGroup("request").Handle logs a "user_id" attr as
// "request.user_id") rather than emitting a truly nested zap.Object — a
// deliberate simplification, since none of Canopy's own logging needs
// nested groups today.
type zapHandler struct {
	logger *zap.Logger
	attrs  []zap.Field
	group  string
}

func (h *zapHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.logger.Core().Enabled(toZapLevel(level))
}

func (h *zapHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make([]zap.Field, 0, len(h.attrs)+record.NumAttrs())
	fields = append(fields, h.attrs...)
	record.Attrs(func(a slog.Attr) bool {
		fields = append(fields, h.toZapField(a))
		return true
	})

	if ce := h.logger.Check(toZapLevel(record.Level), record.Message); ce != nil {
		if !record.Time.IsZero() {
			ce.Time = record.Time
		}
		ce.Write(fields...)
	}
	return nil
}

func (h *zapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	fields := make([]zap.Field, 0, len(attrs))
	for _, a := range attrs {
		fields = append(fields, h.toZapField(a))
	}
	return &zapHandler{
		logger: h.logger,
		attrs:  append(slices.Clone(h.attrs), fields...),
		group:  h.group,
	}
}

func (h *zapHandler) WithGroup(name string) slog.Handler {
	group := name
	if h.group != "" && name != "" {
		group = h.group + "." + name
	} else if h.group != "" {
		group = h.group
	}
	return &zapHandler{logger: h.logger, attrs: h.attrs, group: group}
}

func (h *zapHandler) toZapField(a slog.Attr) zap.Field {
	key := a.Key
	if h.group != "" {
		key = h.group + "." + key
	}
	return zap.Any(key, a.Value.Any())
}

func toZapLevel(level slog.Level) zapcore.Level {
	switch {
	case level >= slog.LevelError:
		return zapcore.ErrorLevel
	case level >= slog.LevelWarn:
		return zapcore.WarnLevel
	case level >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}
