package cli

import (
	"context"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"testing/fstest"
)

// The soft memory limit comes from the flag, or else, unless GOMEMLIMIT is
// set, from the lowest limit of the process's cgroup and those above it, in
// cgroup v2 or v1.
func TestMemoryLimit(t *testing.T) {
	v2 := fstest.MapFS{
		"proc/self/cgroup": {Data: []byte("0::/system.slice/intagent.service\n")},
		"sys/fs/cgroup/system.slice/intagent.service/memory.max": {Data: []byte("max\n")},
		"sys/fs/cgroup/system.slice/memory.max":                  {Data: []byte("2147483648\n")},
	}
	v1 := fstest.MapFS{
		"proc/self/cgroup": {Data: []byte("9:name=systemd:/\n4:cpu,memory:/docker/abc\n0::/\n")},
		"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": {Data: []byte("1073741824\n")},
		"sys/fs/cgroup/memory/memory.limit_in_bytes":            {Data: []byte("9223372036854771712\n")},
	}
	unlimited := fstest.MapFS{
		"proc/self/cgroup":         {Data: []byte("0::/\n")},
		"sys/fs/cgroup/memory.max": {Data: []byte("max\n")},
	}
	noEnv := func(string) string { return "" }
	for _, c := range []struct {
		name, flag string
		env        func(string) string
		root       fstest.MapFS
		want       int64
		from       string
	}{
		{"v2 parent", "", noEnv, v2, 2147483648 * 85 / 100, "cgroup"},
		{"v1", "", noEnv, v1, 1073741824 * 85 / 100, "cgroup"},
		{"no limit", "", noEnv, unlimited, 0, ""},
		{"no cgroup", "", noEnv, fstest.MapFS{}, 0, ""},
		{"GOMEMLIMIT", "", func(k string) string { return map[string]string{"GOMEMLIMIT": "1GiB"}[k] }, v2, 0, ""},
		{"flag", "1536MiB", noEnv, v2, 1536 << 20, "--memory-limit"},
		{"bytes", "1000000", noEnv, v2, 1000000, "--memory-limit"},
		{"off", "off", noEnv, v2, 0, ""},
	} {
		got, from, err := memoryLimit(c.flag, c.env, c.root)
		// fstest rounds nothing: 85% of these limits is exact to a byte or so.
		if err != nil || got/1024 != c.want/1024 || from != c.from {
			t.Errorf("%s: %d from %q (%v), want %d from %q", c.name, got, from, err, c.want, c.from)
		}
	}
	for _, bad := range []string{"12x", "-1GiB", "0", "GiB", "99999999TiB"} {
		if _, _, err := memoryLimit(bad, noEnv, v2); err == nil {
			t.Errorf("--memory-limit %s was accepted", bad)
		}
	}
}

// serve sets the limit it is given, and refuses one it cannot read.
func TestServeSetsTheMemoryLimit(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	dir := t.TempDir()
	var setup safeBuffer
	team := filepath.Join(dir, "team.json")
	if code := (&App{In: strings.NewReader(""), Out: &setup, Err: &setup, Version: "test", Dir: dir}).Run(context.Background(),
		[]string{"token", "add", "alice", "--config", team}); code != 0 {
		t.Fatalf("token add: %s", setup.String())
	}
	var errb safeBuffer
	startServe(t, &App{In: strings.NewReader(""), Out: &errb, Err: &errb, Version: "test", Dir: dir}, "--config", team,
		"--memory-limit", "3GiB")
	if got := debug.SetMemoryLimit(-1); got != 3<<30 || !strings.Contains(errb.String(), "memory limit set") {
		t.Fatalf("limit %d, log %s", got, errb.String())
	}
	var bad safeBuffer
	if code := (&App{In: strings.NewReader(""), Out: &bad, Err: &bad, Version: "test", Dir: dir}).Run(context.Background(),
		[]string{"serve", "--config", team, "--data", "", "--memory-limit", "lots"}); code == 0 ||
		!strings.Contains(bad.String(), "--memory-limit") {
		t.Fatalf("an unreadable limit: %d %s", code, bad.String())
	}
}
