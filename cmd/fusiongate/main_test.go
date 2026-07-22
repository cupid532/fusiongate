package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretEnvPrefersDirectValue(t *testing.T) {
	t.Setenv("TEST_SECRET", "direct")
	t.Setenv("TEST_SECRET_FILE", filepath.Join(t.TempDir(), "missing"))
	got, err := secretEnv("TEST_SECRET")
	if err != nil || got != "direct" {
		t.Fatalf("secretEnv() = %q, %v", got, err)
	}
}

func TestSecretEnvReadsFileAndTrimsOnlyLineEnding(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("  file-secret  \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", file)
	got, err := secretEnv("TEST_SECRET")
	if err != nil || got != "  file-secret  " {
		t.Fatalf("secretEnv() = %q, %v", got, err)
	}
}
