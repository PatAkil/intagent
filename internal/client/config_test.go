package client

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// isolate points the user config at a temporary file and clears the
// environment Resolve reads.
func isolate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("INTAGENT_CONFIG", p)
	for _, k := range []string{"INTAGENT_URL", "INTAGENT_TOKEN", "INTAGENT_DISABLE", "INTAGENT_FAIL", "INTAGENT_TIMEOUT"} {
		t.Setenv(k, "")
	}
	return p
}

func enrol(t *testing.T, url string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, RepoFileName), []byte(`{"url": "`+url+`", "areas": ["services/*"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolvePrecedence(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		root    bool // inside an enrolled repository
		user    UserConfig
		env     map[string]string
		want    Settings
		enrolls bool
	}{
		{name: "nothing", want: Settings{SharePrompts: true, Timeout: DefaultTimeout}},
		{
			name: "repository and its token",
			root: true,
			user: UserConfig{Servers: map[string]ServerCredentials{"https://team.example": {Token: "ia_team"}}},
			want: Settings{URL: "https://team.example", Token: "ia_team", SharePrompts: true, Timeout: DefaultTimeout},
		},
		{
			name: "the default server outside a repository",
			user: UserConfig{Default: "https://team.example/", Servers: map[string]ServerCredentials{"https://team.example": {Token: "ia_team"}}},
			want: Settings{URL: "https://team.example", Token: "ia_team", SharePrompts: true, Timeout: DefaultTimeout},
		},
		{
			name: "the repository wins over the default",
			root: true,
			user: UserConfig{Default: "https://other.example"},
			want: Settings{URL: "https://team.example", SharePrompts: true, Timeout: DefaultTimeout},
		},
		{
			name: "the environment wins, and picks the token of the server it names",
			root: true,
			user: UserConfig{Servers: map[string]ServerCredentials{"https://env.example": {Token: "ia_env"}}},
			env:  map[string]string{"INTAGENT_URL": "https://env.example/"},
			want: Settings{URL: "https://env.example", Token: "ia_env", SharePrompts: true, Timeout: DefaultTimeout},
		},
		{
			name: "switches",
			root: true,
			user: UserConfig{SharePrompts: &no},
			env: map[string]string{"INTAGENT_TOKEN": "ia_x", "INTAGENT_DISABLE": "Yes", "INTAGENT_FAIL": "CLOSED",
				"INTAGENT_TIMEOUT": "750ms"},
			want: Settings{URL: "https://team.example", Token: "ia_x", Disabled: true, FailClosed: true, Timeout: 750 * time.Millisecond},
		},
		{
			name: "a timeout that does not parse keeps the default",
			env:  map[string]string{"INTAGENT_TIMEOUT": "soon"},
			want: Settings{SharePrompts: true, Timeout: DefaultTimeout},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := isolate(t)
			if err := SaveUserConfig(p, tt.user); err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			root := ""
			if tt.root {
				root = enrol(t, "https://team.example/")
			}
			got, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			if got.Enrolled != tt.root || (tt.root && len(got.Repo.Areas) != 1) {
				t.Errorf("enrolled = %v, repo = %+v", got.Enrolled, got.Repo)
			}
			got.Repo, got.Enrolled = RepoConfig{}, false
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve = %+v\nwant      %+v", got, tt.want)
			}
		})
	}
}

// INTAGENT_URL names a server; it does not enrol a repository, so hooks in a
// personal project stay quiet.
func TestEnvironmentDoesNotEnrol(t *testing.T) {
	isolate(t)
	t.Setenv("INTAGENT_URL", "https://team.example")
	got, err := Resolve(t.TempDir())
	if err != nil || got.Enrolled || got.URL != "https://team.example" {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
}

func TestResolveReportsBrokenFiles(t *testing.T) {
	p := isolate(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, RepoFileName), []byte(`{"url": "x",}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil {
		t.Fatal("a broken .intagent.json was accepted")
	}
	if err := os.WriteFile(p, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(""); err == nil {
		t.Fatal("a broken user config was accepted")
	}
}

func TestSaveUserConfigIsPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "config.json")
	t.Setenv("INTAGENT_CONFIG", p)
	if err := SaveUserConfig(p, UserConfig{Servers: map[string]ServerCredentials{"https://x": {Token: "ia_secret"}}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("stat = %v, %v", fi, err)
	}
	uc, path, err := LoadUserConfig()
	if err != nil || path != p || uc.Servers["https://x"].Token != "ia_secret" {
		t.Fatalf("LoadUserConfig = %+v, %q, %v", uc, path, err)
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://team.example": true, "http://127.0.0.1:7400": true,
		"team.example": false, "ftp://team.example": false, "https://": false, "": false, "http://%zz": false,
	} {
		if err := CheckURL(u); (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
	if got := NormalizeURL("  https://team.example//  "); got != "https://team.example" {
		t.Errorf("NormalizeURL = %q", got)
	}
}
