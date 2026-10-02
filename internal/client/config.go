// Package client talks to a team server and knows where its settings live.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RepoFileName is the checked-in file that enrols a repository.
const RepoFileName = ".intagent.json"

// RepoConfig is .intagent.json at a repository's root. It is committed, so it
// never holds a token.
type RepoConfig struct {
	// URL is the team server for this repository.
	URL string `json:"url"`
	// Repo overrides the repository identity derived from the origin URL.
	Repo string `json:"repo,omitempty"`
	// Areas are glob patterns that define the monorepo's areas, e.g. "services/*".
	Areas []string `json:"areas,omitempty"`
	// Ignore lists patterns intagent should not report, e.g. generated files.
	Ignore []string `json:"ignore,omitempty"`
}

// ServerCredentials holds a member's token for one server.
type ServerCredentials struct {
	Token string `json:"token"`
}

// UserConfig is a member's private settings, in the user config directory.
type UserConfig struct {
	// Servers maps a server URL to the member's token for it.
	Servers map[string]ServerCredentials `json:"servers"`
	// Default is the server used outside enrolled repositories.
	Default string `json:"default,omitempty"`
	// SharePrompts lets the first line of a session's first prompt describe the
	// claim's task. Nil means yes.
	SharePrompts *bool `json:"share_prompts,omitempty"`
}

// DefaultTimeout bounds each request to the server, so a slow server cannot
// stall an agent.
const DefaultTimeout = 2 * time.Second

// Settings is the effective configuration for one invocation.
type Settings struct {
	URL          string
	Token        string
	Repo         RepoConfig
	Enrolled     bool
	SharePrompts bool
	Disabled     bool
	FailClosed   bool
	Timeout      time.Duration
}

// UserConfigPath returns where the user config lives.
func UserConfigPath() (string, error) {
	if p := os.Getenv("INTAGENT_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "intagent", "config.json"), nil
}

// LoadUserConfig reads the user config; a missing file is an empty config.
func LoadUserConfig() (UserConfig, string, error) {
	var uc UserConfig
	p, err := UserConfigPath()
	if err != nil {
		return uc, "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return uc, p, nil
	}
	if err != nil {
		return uc, p, err
	}
	if err := json.Unmarshal(data, &uc); err != nil {
		return uc, p, fmt.Errorf("%s: %w", p, err)
	}
	return uc, p, nil
}

// SaveUserConfig writes the user config with owner-only permissions.
func SaveUserConfig(p string, uc UserConfig) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(uc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadRepoConfig reads .intagent.json from a worktree root.
func LoadRepoConfig(root string) (RepoConfig, bool, error) {
	var rc RepoConfig
	data, err := os.ReadFile(filepath.Join(root, RepoFileName))
	if errors.Is(err, os.ErrNotExist) {
		return rc, false, nil
	}
	if err != nil {
		return rc, false, err
	}
	if err := json.Unmarshal(data, &rc); err != nil {
		return rc, false, fmt.Errorf("%s: %w", RepoFileName, err)
	}
	return rc, true, nil
}

// NormalizeURL trims a server URL and drops a trailing slash.
func NormalizeURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// Resolve merges the repository config at root (if any), the user config and
// the environment. root may be empty outside a repository.
func Resolve(root string) (Settings, error) {
	s := Settings{SharePrompts: true, Timeout: DefaultTimeout}
	if root != "" {
		rc, ok, err := LoadRepoConfig(root)
		if err != nil {
			return s, err
		}
		s.Repo, s.Enrolled = rc, ok
		s.URL = NormalizeURL(rc.URL)
	}
	uc, _, err := LoadUserConfig()
	if err != nil {
		return s, err
	}
	if s.URL == "" {
		s.URL = NormalizeURL(uc.Default)
	}
	if uc.SharePrompts != nil {
		s.SharePrompts = *uc.SharePrompts
	}
	if v := os.Getenv("INTAGENT_URL"); v != "" {
		s.URL = NormalizeURL(v)
		s.Enrolled = true
	}
	if cred, ok := uc.Servers[s.URL]; ok {
		s.Token = cred.Token
	}
	if v := os.Getenv("INTAGENT_TOKEN"); v != "" {
		s.Token = v
	}
	switch strings.ToLower(os.Getenv("INTAGENT_DISABLE")) {
	case "1", "true", "yes":
		s.Disabled = true
	}
	s.FailClosed = strings.EqualFold(os.Getenv("INTAGENT_FAIL"), "closed")
	if v := os.Getenv("INTAGENT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			s.Timeout = d
		}
	}
	return s, nil
}

// Ready reports whether the settings name a server and a token.
func (s Settings) Ready() bool { return !s.Disabled && s.URL != "" && s.Token != "" }
