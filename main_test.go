package main

import (
	"flag"
	"io"
	"testing"
)

func TestParseFlags(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Bool("json", false, "")
		return fs
	}
	if code, ok := parseFlags(newSet(), []string{"--json"}); !ok || code != exitOK {
		t.Errorf("valid flags: code=%d ok=%v", code, ok)
	}
	if code, ok := parseFlags(newSet(), []string{"-h"}); ok || code != exitOK {
		t.Errorf("-h: code=%d ok=%v, want stop with 0", code, ok)
	}
	// A usage error is an error (1), never 2, which check reserves for
	// "updates available".
	if code, ok := parseFlags(newSet(), []string{"--nope"}); ok || code != exitError {
		t.Errorf("malformed flag: code=%d ok=%v, want stop with 1", code, ok)
	}
}
