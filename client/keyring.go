package client

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Keyring holds secrets, one item per service and account. Sign-in tokens
// live only here: never in a file, the catalog, the call history or JSON
// output. Keychain is the real one (the macOS login keychain);
// MemoryKeyring is for tests.
type Keyring interface {
	// Get is the item's secret, ErrNoItem when there is none.
	Get(service, account string) ([]byte, error)
	// Set creates or replaces the item.
	Set(service, account string, secret []byte) error
	// Delete removes the item; ErrNoItem when there was none.
	Delete(service, account string) error
}

// ErrNoItem: the keyring has no item for the service and account.
var ErrNoItem = errors.New("no keychain item")

// KeychainService is the service name of o-mcp's Keychain items.
const KeychainService = "o-mcp"

// MemoryKeyring is a Keyring in memory, for tests.
type MemoryKeyring struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemoryKeyring is an empty MemoryKeyring.
func NewMemoryKeyring() *MemoryKeyring { return &MemoryKeyring{items: map[string][]byte{}} }

func (k *MemoryKeyring) Get(service, account string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, ok := k.items[service+"\x00"+account]
	if !ok {
		return nil, ErrNoItem
	}
	return append([]byte{}, b...), nil
}

func (k *MemoryKeyring) Set(service, account string, secret []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"\x00"+account] = append([]byte{}, secret...)
	return nil
}

func (k *MemoryKeyring) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.items[service+"\x00"+account]; !ok {
		return ErrNoItem
	}
	delete(k.items, service+"\x00"+account)
	return nil
}

// Accounts are the accounts the keyring holds for service, sorted.
func (k *MemoryKeyring) Accounts(service string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for key := range k.items {
		if s, a, _ := strings.Cut(key, "\x00"); s == service {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// Secrets are every secret the keyring holds, for a test that checks no
// output carries one.
func (k *MemoryKeyring) Secrets() [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out [][]byte
	for _, b := range k.items {
		out = append(out, append([]byte{}, b...))
	}
	return out
}

// Keychain is the macOS login keychain, reached through /usr/bin/security
// (it ships with macOS; no cgo, no dependency). Items are generic
// passwords. A secret never goes on a command line: writes are fed to
// `security -i` on stdin, reads come back on stdout. Because the security
// tool creates the items, its own later reads need no access prompt.
//
// security -i reads lines of at most about 4 KB, so a secret is base64
// and split into chunks: ACCOUNT holds the first, ACCOUNT#2 the second,
// and so on; Get joins them until one is missing. On another platform
// every call fails: o-mcp keeps sign-in tokens nowhere else.
func Keychain() Keyring { return keychain{path: "/usr/bin/security"} }

type keychain struct{ path string }

// keychainChunk is the base64 characters per item, well under security
// -i's line limit with the command around them.
const keychainChunk = 3000

// securityNotFound is security's exit status for a missing item.
const securityNotFound = 44

func (k keychain) available() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("no macOS Keychain on %s: o-mcp keeps sign-in tokens only in the Keychain", runtime.GOOS)
	}
	return nil
}

func chunkAccount(account string, i int) string {
	if i == 0 {
		return account
	}
	return fmt.Sprintf("%s#%d", account, i+1)
}

// keychainArg is s safe as one word of security -i (which splits on
// blanks and has no quoting o-mcp relies on).
func keychainArg(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, " \t\r\n\"'\\") {
		return "", fmt.Errorf("keychain name %q has blanks or quotes", s)
	}
	return s, nil
}

func (k keychain) find(service, account string) ([]byte, error) {
	out, err := exec.Command(k.path, "find-generic-password", "-s", service, "-a", account, "-w").Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == securityNotFound {
		return nil, ErrNoItem
	}
	if err != nil {
		return nil, fmt.Errorf("keychain: find %s: %v", account, err)
	}
	return bytes.TrimRight(out, "\r\n"), nil
}

func (k keychain) Get(service, account string) ([]byte, error) {
	if err := k.available(); err != nil {
		return nil, err
	}
	var b64 []byte
	for i := 0; ; i++ {
		part, err := k.find(service, chunkAccount(account, i))
		if errors.Is(err, ErrNoItem) && i > 0 {
			break
		}
		if err != nil {
			return nil, err
		}
		b64 = append(b64, part...)
		if len(part) < keychainChunk {
			break
		}
	}
	secret, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		return nil, fmt.Errorf("keychain: item %s is not o-mcp's (not base64)", account)
	}
	return secret, nil
}

func (k keychain) Set(service, account string, secret []byte) error {
	if err := k.available(); err != nil {
		return err
	}
	svc, err := keychainArg(service)
	if err != nil {
		return err
	}
	acct, err := keychainArg(account)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return errors.New("keychain: an empty secret")
	}
	b64 := base64.StdEncoding.EncodeToString(secret)
	var script strings.Builder
	n := 0
	for start := 0; start < len(b64); start += keychainChunk {
		end := min(start+keychainChunk, len(b64))
		fmt.Fprintf(&script, "add-generic-password -U -s %s -a %s -l %s -w %s\n", svc, chunkAccount(acct, n), svc, b64[start:end])
		n++
	}
	cmd := exec.Command(k.path, "-i")
	cmd.Stdin = strings.NewReader(script.String())
	out, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(out, []byte(": returned ")) {
		// security's messages name the command, never the -w value.
		return fmt.Errorf("keychain: store %s: %s%v", account, firstOutputLine(out), errOrEmpty(err))
	}
	// A shorter secret leaves no stale chunks behind.
	for i := n; ; i++ {
		if err := k.deleteOne(service, chunkAccount(account, i)); err != nil {
			break
		}
	}
	return nil
}

func (k keychain) deleteOne(service, account string) error {
	err := exec.Command(k.path, "delete-generic-password", "-s", service, "-a", account).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == securityNotFound {
		return ErrNoItem
	}
	if err != nil {
		return fmt.Errorf("keychain: delete %s: %v", account, err)
	}
	return nil
}

func (k keychain) Delete(service, account string) error {
	if err := k.available(); err != nil {
		return err
	}
	if err := k.deleteOne(service, account); err != nil {
		return err
	}
	for i := 1; ; i++ {
		if err := k.deleteOne(service, chunkAccount(account, i)); err != nil {
			return nil
		}
	}
}

func firstOutputLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}
