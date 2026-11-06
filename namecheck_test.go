package main

import (
	"io"
	"strings"
	"testing"

	"ofam.dev/ochazuke/namecheck"
)

// TestNoNewNameBranches holds the non-test Go to namecheck: no new
// comparison, case label or lookup table keyed by a noun, verb, mode,
// motion or parameter name the runtime declares, and no function grows
// or passes complexity 25. The accepted uses are testdata/namecheck.txt,
// which only shrinks.
func TestNoNewNameBranches(t *testing.T) {
	home := t.TempDir()
	tool := &Tool{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader(""), Home: home, Cwd: home, Prog: "o-mcp", Env: []string{"HOME=" + home}}
	namecheck.Check(t, namecheck.Config{
		Vocab:    tool.shell().Runtime.NameVocab(),
		Baseline: "testdata/namecheck.txt",
		Dirs:     []string{"."},
	})
}
