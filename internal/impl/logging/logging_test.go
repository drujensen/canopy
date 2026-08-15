package logging_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/drujensen/canopy/internal/impl/logging"
)

// TestSlogHandler_ForwardsToZap asserts that a record logged through the
// slog.Logger returned by NewSlogLogger actually reaches the underlying
// zap.Logger, with the message, level, and attrs intact.
func TestSlogHandler_ForwardsToZap(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	zapLogger := zap.New(core)

	slogLogger := logging.NewSlogLogger(zapLogger)
	slogLogger.WithGroup("run").Info("agent turn completed", slog.String("agent", "test-agent"), slog.Int("tokens", 42))

	entries := logs.All()
	require.Len(t, entries, 1)
	entry := entries[0]
	assert.Equal(t, "agent turn completed", entry.Message)
	assert.Equal(t, zapcore.InfoLevel, entry.Level)

	fields := entry.ContextMap()
	assert.Equal(t, "test-agent", fields["run.agent"])
	assert.EqualValues(t, 42, fields["run.tokens"])
}

// TestSlogHandler_LevelTranslation asserts every slog level maps to the
// expected zap level, including the Enabled() check gating what's recorded.
func TestSlogHandler_LevelTranslation(t *testing.T) {
	tests := []struct {
		name      string
		slogLevel slog.Level
		zapLevel  zapcore.Level
	}{
		{"debug", slog.LevelDebug, zapcore.DebugLevel},
		{"info", slog.LevelInfo, zapcore.InfoLevel},
		{"warn", slog.LevelWarn, zapcore.WarnLevel},
		{"error", slog.LevelError, zapcore.ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			zapLogger := zap.New(core)
			handler := logging.NewSlogHandler(zapLogger)

			assert.True(t, handler.Enabled(context.Background(), tt.slogLevel))

			slogLogger := slog.New(handler)
			slogLogger.Log(context.Background(), tt.slogLevel, "msg")

			require.Len(t, logs.All(), 1)
			assert.Equal(t, tt.zapLevel, logs.All()[0].Level)
		})
	}
}

// TestNewZapLogger asserts both the dev and production constructors succeed
// and return a usable logger.
func TestNewZapLogger(t *testing.T) {
	for _, dev := range []bool{true, false} {
		l, err := logging.NewZapLogger(dev)
		require.NoError(t, err)
		require.NotNil(t, l)
		defer func() { _ = l.Sync() }()
	}
}
