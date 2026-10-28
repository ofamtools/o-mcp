package client

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Snapshot is a server's discovered tools, bound to the configuration
// they came from (Server: its ID, config file and public definition, and
// Fingerprint). It holds no secret value.
type Snapshot struct {
	Server       Server                `json:"server"`
	Fingerprint  string                `json:"fingerprint"`
	DiscoveredAt time.Time             `json:"discoveredAt"`
	Initialize   *mcp.InitializeResult `json:"initialize,omitempty"`
	Tools        []*mcp.Tool           `json:"tools"`
}

// Tool is the snapshot's tool of that name.
func (s Snapshot) Tool(name string) (*mcp.Tool, bool) {
	for _, t := range s.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}

// Discover connects to the server, lists every tool and returns the
// snapshot (not saved: Store.Save does that).
func Discover(ctx context.Context, s Server, o Options) (Snapshot, error) {
	sess, err := Connect(ctx, s, o)
	if err != nil {
		return Snapshot{}, err
	}
	defer sess.Close()
	tools, err := sess.Tools(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if tools == nil {
		tools = []*mcp.Tool{}
	}
	return Snapshot{Server: s, Fingerprint: s.Fingerprint(), DiscoveredAt: time.Now().UTC(), Initialize: sess.Initialize(), Tools: tools}, nil
}

// Store is o-mcp's data directory: one snapshot file per server under
// servers/ and the call history, calls.jsonl.
type Store struct{ Dir string }

// DefaultStore is $O_MCP_DATA, else $XDG_DATA_HOME/o-mcp, else
// ~/.local/share/o-mcp.
func DefaultStore(home string, getenv func(string) string) Store {
	if getenv == nil {
		getenv = os.Getenv
	}
	if d := getenv("O_MCP_DATA"); d != "" {
		return Store{Dir: d}
	}
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	return Store{Dir: filepath.Join(base, "o-mcp")}
}

// snapshotPath is the file of a server ID: the ID made safe for a file
// name, then a short hash of it so two IDs never share a file.
func (st Store) snapshotPath(id string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(st.Dir, "servers", safe+"-"+hex.EncodeToString(sum[:4])+".json")
}

// Save writes the snapshot atomically (a temporary file renamed over the
// old one): a reader sees the old snapshot or the new, never part of one.
func (st Store) Save(s Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(st.snapshotPath(s.Server.ID), append(b, '\n'))
}

// Load is the saved snapshot of a server ID; os.ErrNotExist when none.
func (st Store) Load(id string) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(st.snapshotPath(id))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("snapshot of %s: %w", id, err)
	}
	return s, nil
}

// Remove deletes a server's snapshot (never its configuration).
func (st Store) Remove(id string) error { return os.Remove(st.snapshotPath(id)) }

// List is every saved snapshot, by server ID. An unreadable file is
// skipped and named in the error, the rest still listed.
func (st Store) List() ([]Snapshot, error) {
	files, _ := filepath.Glob(filepath.Join(st.Dir, "servers", "*.json"))
	var out []Snapshot
	var bad []string
	for _, f := range files {
		var s Snapshot
		b, err := os.ReadFile(f)
		if err == nil {
			err = json.Unmarshal(b, &s)
		}
		if err != nil || s.Server.ID == "" {
			bad = append(bad, filepath.Base(f))
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server.ID < out[j].Server.ID })
	if len(bad) > 0 {
		return out, fmt.Errorf("unreadable snapshots: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// Call is one recorded tool call. Arguments pass the secret-value
// redaction (RedactValues); Preview is the start of the result's text.
type Call struct {
	ID         string         `json:"id"`
	At         time.Time      `json:"at"`
	Server     string         `json:"server"` // the server ID
	Tool       string         `json:"tool"`
	Arguments  map[string]any `json:"arguments"`
	IsError    bool           `json:"isError,omitempty"`
	Error      string         `json:"error,omitempty"`
	DurationMS int64          `json:"durationMs"`
	Preview    string         `json:"preview,omitempty"`
}

// RecordCall appends a call to the history.
func (st Store) RecordCall(c Call) error {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return err
	}
	if c.ID == "" {
		c.ID = c.At.UTC().Format("20060102T150405.000000000Z")
	}
	c.Arguments, _ = RedactValues(c.Arguments).(map[string]any)
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(st.Dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Calls is the call history, newest first.
func (st Store) Calls() ([]Call, error) {
	f, err := os.Open(filepath.Join(st.Dir, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var c Call
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.ID != "" {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, sc.Err()
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename there is nothing to remove
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
