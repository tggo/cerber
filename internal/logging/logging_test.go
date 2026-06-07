package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_WritesDatedFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	logger, closer, err := New("debug", dir, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello", nil...)
	logger.Debug("dbg")
	if err := closer(); err != nil {
		t.Fatalf("closer: %v", err)
	}

	path := filepath.Join(dir, "2026-06-07.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(data) == 0 {
		t.Error("log file is empty")
	}
}

func TestNew_DefaultsAndInvalid(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(0, 0)
	// empty level -> info default, empty dir -> would use ./logs; pass dir to keep tidy.
	_, closer, err := New("", dir, now)
	if err != nil {
		t.Fatalf("default level: %v", err)
	}
	_ = closer()

	if _, _, err := New("not-a-level", dir, now); err == nil {
		t.Fatal("expected error for invalid level")
	}
}

func TestNew_BadDir(t *testing.T) {
	// A file used as a directory path makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New("info", filepath.Join(f, "sub"), time.Unix(0, 0)); err == nil {
		t.Fatal("expected error when dir cannot be created")
	}
}
