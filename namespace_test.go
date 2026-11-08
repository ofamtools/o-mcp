package main

import (
	"encoding/json"
	"testing"

	"ofam.dev/ochazuke"
	ocli "ofam.dev/ochazuke/cli"
)

func TestCatalogDeclaresSingularNamespace(t *testing.T) {
	e := newEnv(t)
	var doc ocli.CatalogDoc
	if err := json.Unmarshal([]byte(e.ok("catalog")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Namespace != "mcp" || ochazuke.DefaultNamespace != "mcp" {
		t.Fatalf("catalog namespace %q, default %q; want mcp", doc.Namespace, ochazuke.DefaultNamespace)
	}
	if got, err := ocli.CatalogNamespace(doc, "o-mcp"); err != nil || got != "mcp" {
		t.Fatal(got, err)
	}
}
