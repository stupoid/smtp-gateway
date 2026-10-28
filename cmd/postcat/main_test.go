package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleFile = "S a@b\nR c@d\nT 2026-01-02T03:04:05Z\n\nSubject: x\n\nbody\n"

func writeSample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.eml")
	if err := os.WriteFile(path, []byte(sampleFile), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestRun(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{writeSample(t)}, &out, &errOut); code != 0 {
		t.Fatalf("run code = %d, stderr = %s", code, errOut.String())
	}
	for _, want := range []string{
		"Sender:     a@b",
		"Recipients: [c@d]",
		"Body size:  17 bytes",
		"Subject: x",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout does not contain %q:\n%s", want, out.String())
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunUsageAndErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no args", nil, 1, "Usage: postcat"},
		{"help", []string{"-h"}, 0, "Usage: postcat"},
		{"missing file", []string{filepath.Join(t.TempDir(), "missing.eml")}, 1, "postcat:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tt.args, &out, &errOut); code != tt.code {
				t.Errorf("run code = %d, want %d", code, tt.code)
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tt.want)
			}
		})
	}
}
