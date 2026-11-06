package main

import (
	"io"
	"strings"
	"testing"

	"ofam.dev/ochazuke/namecheck"
	"ofam.dev/ochazuke/nouncheck"
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

// TestVerbReferences holds every tracked Go, JSON and Markdown file to the
// verbs the catalog has: a verb the framework purged (rename, move,
// restore, duplicate, patch; the renamed command verbs) fails here until
// the last reference to it is gone. A dated CHANGELOG section is history.
func TestVerbReferences(t *testing.T) {
	home := t.TempDir()
	tool := &Tool{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader(""), Home: home, Cwd: home, Prog: "o-mcp", Env: []string{"HOME=" + home}}
	namecheck.VerbsCheck(t, namecheck.VerbsConfig{
		Catalog: tool.shell().Runtime.VerbCatalog(),
		Removed: tool.shell().Runtime.RemovedVerbs(),
		Root:    ".",
	})
}

// TestNouns holds every thing noun to the core verbs and a key prop, and
// every command noun off the core verbs' names (Runtime.ValidateNouns).
func TestNouns(t *testing.T) {
	home := t.TempDir()
	tool := &Tool{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader(""), Home: home, Cwd: home, Prog: "o-mcp", Env: []string{"HOME=" + home}}
	nouncheck.Check(t, tool.shell().Runtime, nouncheck.Config{Baseline: "testdata/nouncheck.txt"})
}
