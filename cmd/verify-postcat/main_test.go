package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	content := "S a@b\nR c@d\nT 2026-01-02T03:04:05Z\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "one.eml"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{dir}, &out, &errOut); code != 0 {
		t.Fatalf("run code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "1 postcat file(s) verified OK") {
		t.Errorf("stdout = %q, want the summary", out.String())
	}
}

func TestRunInvalidFile(t *testing.T) {
	dir := t.TempDir()
	content := "S a@b\nT not-a-timestamp\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.eml"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{dir}, &out, &errOut); code != 1 {
		t.Errorf("run code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "FAIL bad.eml") {
		t.Errorf("stdout = %q, want a FAIL line", out.String())
	}
}

func TestRunUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"no args", nil, 1},
		{"help", []string{"-h"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tt.args, &out, &errOut); code != tt.code {
				t.Errorf("run code = %d, want %d", code, tt.code)
			}
			if !strings.Contains(errOut.String(), "Usage: verify-postcat") {
				t.Errorf("stderr = %q, want usage", errOut.String())
			}
		})
	}
}
