package main

import (
	"path/filepath"
	"testing"
)

func TestWriterLockExcludesSecondProcessOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	first, err := acquireWriterLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireWriterLock(path); err == nil {
		_ = second.Close()
		t.Fatal("second writer acquired lock")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireWriterLock(path)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	_ = third.Close()
}

func TestProjectionStreamValidation(t *testing.T) {
	for _, name := range []string{"factory_measurements", "smoke_123_abc"} {
		if !validProjectionStream(name) {
			t.Fatalf("valid stream rejected %q", name)
		}
	}
	for _, name := range []string{"", "has/slash", "has space", ".hidden", "Uppercase", "1leading"} {
		if validProjectionStream(name) {
			t.Fatalf("invalid stream accepted %q", name)
		}
	}
}
