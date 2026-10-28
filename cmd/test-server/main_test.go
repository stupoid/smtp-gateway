package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(nil, &out, &errOut)
	if err == nil {
		t.Fatal("run accepted no arguments")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error = %v, want the usage message", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestRunHelp(t *testing.T) {
	if err := run([]string{"-h"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("run -h: %v", err)
	}
}

func TestRunBadFlag(t *testing.T) {
	if err := run([]string{"-bogus"}, io.Discard, io.Discard); err == nil {
		t.Fatal("run accepted an unknown flag")
	}
}
