package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// Member is a person who may connect agents, identified by a token hash.
type Member struct {
	Name        string `json:"name"`
	TokenSHA256 string `json:"token_sha256"`
}

// Duration is a time.Duration written as "10m" in JSON.
type Duration time.Duration

// MarshalText writes a duration such as "10m0s".
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText reads a duration such as "10m".
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// FileConfig is the server's configuration file. Zero values take defaults.
type FileConfig struct {
	Members        []Member     `json:"members"`
	Policy         board.Policy `json:"policy,omitzero"`
	StallAfter     Duration     `json:"stall_after,omitempty"`
	ToolStallAfter Duration     `json:"tool_stall_after,omitempty"`
	IdleAfter      Duration     `json:"idle_after,omitempty"`
	DormantFor     Duration     `json:"dormant_for,omitempty"`
	ForgetAfter    Duration     `json:"forget_after,omitempty"`
	NotesPerMinute int          `json:"notes_per_minute,omitempty"`
	// Webhook sends stalls, silent agents and refused collisions to an endpoint.
	Webhook WebhookConfig `json:"webhook,omitzero"`
}

var memberName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// ValidMemberName reports whether name can identify a member.
func ValidMemberName(name string) bool { return memberName.MatchString(name) }

// LoadFileConfig reads a configuration file. A missing file is an error.
func LoadFileConfig(path string) (FileConfig, error) {
	var fc FileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return fc, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelt setting must not be silently ignored
	if err := dec.Decode(&fc); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	return fc, fc.validate()
}

func (fc FileConfig) validate() error {
	seen := map[string]bool{}
	for _, m := range fc.Members {
		if !ValidMemberName(m.Name) {
			return fmt.Errorf("member %q: names are lower-case letters, digits, '.', '_' and '-'", m.Name)
		}
		if seen[m.Name] {
			return fmt.Errorf("member %q appears twice", m.Name)
		}
		seen[m.Name] = true
		if b, err := hex.DecodeString(m.TokenSHA256); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("member %q: token_sha256 must be 64 hex characters", m.Name)
		}
	}
	if u := fc.Webhook.URL; u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("webhook.url %q must be an http or https URL", u)
	}
	for _, a := range []board.Action{fc.Policy.Block, fc.Policy.Overlap, fc.Policy.Nearby} {
		if a == "" {
			continue
		}
		if _, err := board.ParseAction(string(a)); err != nil {
			return fmt.Errorf("policy: %w", err)
		}
	}
	return nil
}

// BoardConfig merges the file's settings over the defaults.
func (fc FileConfig) BoardConfig() board.Config {
	c := board.DefaultConfig()
	set := func(dst *time.Duration, d Duration) {
		if d > 0 {
			*dst = time.Duration(d)
		}
	}
	set(&c.StallAfter, fc.StallAfter)
	set(&c.ToolStallAfter, fc.ToolStallAfter)
	set(&c.IdleAfter, fc.IdleAfter)
	set(&c.DormantFor, fc.DormantFor)
	set(&c.ForgetAfter, fc.ForgetAfter)
	if fc.Policy.Block != "" {
		c.Policy.Block = fc.Policy.Block
	}
	if fc.Policy.Overlap != "" {
		c.Policy.Overlap = fc.Policy.Overlap
	}
	if fc.Policy.Nearby != "" {
		c.Policy.Nearby = fc.Policy.Nearby
	}
	if fc.NotesPerMinute > 0 {
		c.NotesPerMinute = fc.NotesPerMinute
	}
	return c
}

// NewToken returns a random token and its hash.
func NewToken() (token, hash string, err error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	token = "ia_" + hex.EncodeToString(buf[:])
	return token, HashToken(token), nil
}

// HashToken returns the hex SHA-256 of a token, as stored in the config file.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrMemberExists reports an attempt to add a member twice.
var ErrMemberExists = errors.New("member already exists")

// AddMember adds a member with a fresh token to the config file at path,
// creating the file if needed, and returns the token. With replace, an
// existing member's token is rotated.
func AddMember(path, name string, replace bool) (string, error) {
	if !ValidMemberName(name) {
		return "", fmt.Errorf("member %q: names are lower-case letters, digits, '.', '_' and '-'", name)
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	var fc FileConfig
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &fc); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	found := false
	for i := range fc.Members {
		if fc.Members[i].Name == name {
			if !replace {
				return "", fmt.Errorf("%w: %s (use --rotate to issue a new token)", ErrMemberExists, name)
			}
			fc.Members[i].TokenSHA256 = hash
			found = true
		}
	}
	if !found {
		fc.Members = append(fc.Members, Member{Name: name, TokenSHA256: hash})
	}
	out, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// lockFile takes an exclusive lock by creating path, so that two 'token add'
// runs cannot both rewrite the team file and lose one's change. A lock left
// by a crashed run is taken over after lockStale.
func lockFile(path string) (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another 'intagent token' run; remove it if none is running", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const lockStale = 30 * time.Second
