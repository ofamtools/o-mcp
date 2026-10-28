package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/unixsuperhero/o-mcp/internal/fake"
)

func json_(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }

func TestSnapshotRoundTripHoldsNoSecret(t *testing.T) {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	go fake.Server().Run(ctx, st)
	s := NewServer("fake", map[string]any{"url": "https://example.test/mcp", "headers": map[string]any{"Authorization": "Bearer very-secret-token"}})
	sess, err := ConnectTransport(ctx, s, ct, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sess.Tools(ctx)
	sess.Close()
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{Server: s, Fingerprint: s.Fingerprint(), DiscoveredAt: time.Now().UTC().Truncate(time.Second), Initialize: sess.Initialize(), Tools: tools}
	store := Store{Dir: t.TempDir()}
	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.ID != s.ID || got.Fingerprint != snap.Fingerprint || !got.DiscoveredAt.Equal(snap.DiscoveredAt) || got.Initialize.ServerInfo.Name != "fake" {
		t.Fatalf("loaded = %+v", got)
	}
	if !reflect.DeepEqual(names(got.Tools), []string{"echo", "fail", "json"}) {
		t.Fatalf("tools = %v", names(got.Tools))
	}
	echo, ok := got.Tool("echo")
	if !ok || echo.InputSchema.(map[string]any)["type"] != "object" {
		t.Fatalf("echo schema = %+v", echo)
	}
	files, _ := filepath.Glob(filepath.Join(store.Dir, "servers", "*"))
	if len(files) != 1 {
		t.Fatalf("files = %v (a temporary file left behind?)", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Contains(string(b), "very-secret-token") {
		t.Fatal("a header value was saved")
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v %v", list, err)
	}
	if err := store.Remove(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(s.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestCallHistoryRedactsAndOrders(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	t0 := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	store.RecordCall(Call{At: t0, Server: "a", Tool: "t", Arguments: map[string]any{"token": "ghp_abcdefghijklmnop", "q": "x"}})
	store.RecordCall(Call{At: t0.Add(time.Minute), Server: "a", Tool: "u"})
	calls, err := store.Calls()
	if err != nil || len(calls) != 2 || calls[0].Tool != "u" {
		t.Fatalf("calls = %+v %v", calls, err)
	}
	if calls[1].Arguments["token"] != "[REDACTED]" || calls[1].Arguments["q"] != "x" {
		t.Fatalf("arguments = %v", calls[1].Arguments)
	}
}
