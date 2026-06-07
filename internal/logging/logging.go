// Package logging builds cerber's zap logger. It writes JSON logs to a dated file
// under the configured directory (./logs/<YYYY-MM-DD>.log) and human-readable logs
// to stdout, both at the configured level. Secrets are never logged (callers log
// credential names, never token material — see CLAUDE.md).
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// DefaultDir is used when no log directory is configured.
const DefaultDir = "./logs"

// New builds a zap logger. level is a zap level string ("debug", "info",
// "warn", "error"); empty defaults to "info". dir is the log directory; empty
// defaults to DefaultDir. now selects the dated filename. The returned closer
// flushes and closes the log file.
func New(level, dir string, now time.Time) (*zap.Logger, func() error, error) {
	if level == "" {
		level = "info"
	}
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: invalid level %q: %w", level, err)
	}
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("logging: create dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, now.Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: open %q: %w", path, err)
	}

	fileEnc := zapcore.NewJSONEncoder(prodEncoder())
	consoleEnc := zapcore.NewConsoleEncoder(consoleEncoder())
	core := zapcore.NewTee(
		zapcore.NewCore(fileEnc, zapcore.AddSync(f), lvl),
		zapcore.NewCore(consoleEnc, zapcore.AddSync(os.Stdout), lvl),
	)
	logger := zap.New(core)
	closer := func() error {
		_ = logger.Sync()
		return f.Close()
	}
	return logger, closer, nil
}

func prodEncoder() zapcore.EncoderConfig {
	c := zap.NewProductionEncoderConfig()
	c.EncodeTime = zapcore.ISO8601TimeEncoder
	return c
}

func consoleEncoder() zapcore.EncoderConfig {
	c := zap.NewDevelopmentEncoderConfig()
	c.EncodeTime = zapcore.ISO8601TimeEncoder
	c.EncodeLevel = zapcore.CapitalColorLevelEncoder
	return c
}
