package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureHome copies testdata/home into a temporary home and clears the
// harnesses' override variables: tests never read the real home.
func fixtureHome(t *testing.T) Options {
	t.Helper()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR"} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	err := filepath.Walk("testdata/home", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("testdata/home", p)
		dst := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Home: home, Project: t.TempDir()}
}

func TestConfiguredInventory(t *testing.T) {
	servers, warnings := Configured(fixtureHome(t))
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	byID := map[string]Server{}
	var ids []string
	for _, s := range servers {
		ids = append(ids, s.ID)
		byID[s.ID] = s
	}
	want := []string{"claude:user:slack", "claude:user:tuios", "codex:user:gh", "codex:user:off", "codex:user:tuios"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if !byID["codex:user:off"].Disabled {
		t.Fatal("a disabled entry must be listed as disabled")
	}
	gh := byID["codex:user:gh"]
	if gh.BearerTokenEnv != "GH_MCP_TOKEN" || !reflect.DeepEqual(gh.HeaderEnv, []string{"X-Team=TEAM_HEADER"}) || gh.Transport != "http" {
		t.Fatalf("gh = %+v", gh)
	}
	b, _ := json.Marshal(servers)
	if strings.Contains(string(b), "xoxb-not-a-real-token") {
		t.Fatal("a header value is in the public records")
	}
	if !strings.Contains(string(b), `"headers":["Authorization"]`) {
		t.Fatalf("header names missing: %s", b)
	}
	// tuios is configured the same way twice: the name finds one.
	if s, err := Find(servers, "tuios"); err != nil || s.ID != "claude:user:tuios" {
		t.Fatalf("Find tuios = %v %v", s.ID, err)
	}
	// The bearer variable unset is a connect error, before any request.
	_, _, err := transport(gh, Options{Getenv: func(string) string { return "" }}.withDefaults(), nil)
	if err == nil || !strings.Contains(err.Error(), "GH_MCP_TOKEN") {
		t.Fatalf("unset bearer = %v", err)
	}
}
