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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/fsutil"
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
	// MaxSessions and MaxDormantClaims bound what the board keeps
	// (board.Config); a server older than them refuses a file that sets one.
	MaxSessions      int `json:"max_sessions,omitempty"`
	MaxDormantClaims int `json:"max_dormant_claims,omitempty"`
	// Webhook sends stalls, silent agents and refused collisions to an endpoint.
	Webhook WebhookConfig `json:"webhook,omitzero"`
}

var memberName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// ValidMemberName reports whether name can identify a member.
func ValidMemberName(name string) bool { return memberName.MatchString(name) }

// LoadFileConfig reads a configuration file. A missing file is an error.
func LoadFileConfig(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, err
	}
	return parseFileConfig(path, data)
}

func parseFileConfig(path string, data []byte) (FileConfig, error) {
	var fc FileConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelt setting must not be silently ignored
	if err := dec.Decode(&fc); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	if err := fc.validate(); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	return fc, nil
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
	for _, e := range fc.Webhook.Events {
		if _, err := board.ParseActivityKind(string(e)); err != nil {
			return fmt.Errorf("webhook.events: %w", err)
		}
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
	return board.Config{
		StallAfter:       time.Duration(fc.StallAfter),
		ToolStallAfter:   time.Duration(fc.ToolStallAfter),
		IdleAfter:        time.Duration(fc.IdleAfter),
		DormantFor:       time.Duration(fc.DormantFor),
		ForgetAfter:      time.Duration(fc.ForgetAfter),
		Policy:           fc.Policy,
		NotesPerMinute:   fc.NotesPerMinute,
		MaxSessions:      fc.MaxSessions,
		MaxDormantClaims: fc.MaxDormantClaims,
	}.WithDefaults()
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	unlock, err := fsutil.Lock(path+".lock", lockWait)
	if errors.Is(err, fsutil.ErrLocked) {
		return "", fmt.Errorf("another 'intagent token' run is changing %s; try again when it ends", path)
	}
	if err != nil {
		return "", err
	}
	defer unlock()
	// A file the server would refuse is fixed first: a token added to it
	// would not work until it is.
	var fc FileConfig
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if fc, err = parseFileConfig(path, data); err != nil {
			return "", err
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
	if err := fsutil.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// lockWait is how long token add waits for another run to finish.
const lockWait = 10 * time.Second
