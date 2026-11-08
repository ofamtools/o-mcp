package main

import (
	"io"
	"strings"
	"testing"

	"ofam.dev/ochazuke/lexiconcheck"
)

// o-mcp's registered operations are held to the lexicon's naming rules
// (lexicon plan): one word has one part of speech, nouns are singular, a
// namespace is not a noun or action word, a key has at most three parts.
// testdata/lexicon-words.tsv is a copy of ochazuke's word list.
func TestLexicon(t *testing.T) {
	home := t.TempDir()
	tool := &Tool{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader(""), Home: home, Cwd: home, Prog: "o-mcp", Env: []string{"HOME=" + home}}
	lexiconcheck.Check(t, lexiconcheck.Config{
		Operations: &tool.shell().Runtime.Operations,
		Words:      "testdata/lexicon-words.tsv",
		Baseline:   "testdata/lexiconcheck.txt",
	})
}
