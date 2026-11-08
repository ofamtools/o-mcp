package main

import "ofam.dev/ochazuke"

// Namespace is the singular namespace o-mcp declares (lexicon plan): its
// operations are mcp.<noun>.<verb> and a host mounts its catalog under it.
const Namespace = "mcp"

func init() {
	if err := ochazuke.DeclareNamespace(Namespace); err != nil {
		panic(err)
	}
}
