package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"math"
	"path"
	"strconv"
	"strings"
)

// The server's memory grows with the board, and the Go runtime lets the heap
// grow to twice what is live before it collects. In a container with a memory
// limit, that second half can get the server killed while most of it is
// garbage. A soft limit makes the runtime collect harder as the heap nears
// it. It is a backstop: a board too large for the limit still runs out.

// cgroupShare is the share of a cgroup's limit given to the Go heap, which
// leaves room for the memory the runtime does not count.
const cgroupShare = 0.85

// memoryLimit is the soft memory limit serve sets, and where it comes from:
// the --memory-limit flag, which "off" turns off; GOMEMLIMIT, which the
// runtime has applied already; or 85% of the memory limit of the process's
// cgroup, found under root (the file system's root, "/", on Linux). Zero
// sets none.
func memoryLimit(flag string, getenv func(string) string, root fs.FS) (int64, string, error) {
	switch {
	case flag == "off":
		return 0, "", nil
	case flag != "":
		n, err := parseBytes(flag)
		if err != nil || n <= 0 {
			return 0, "", fmt.Errorf("--memory-limit %q: give a size such as 1536MiB or 2GiB, or off", flag)
		}
		return n, "--memory-limit", nil
	case getenv("GOMEMLIMIT") != "", root == nil:
		return 0, "", nil
	}
	if limit := cgroupMemory(root); limit > 0 {
		return int64(float64(limit) * cgroupShare), "cgroup", nil
	}
	return 0, "", nil
}

// parseBytes reads a size as GOMEMLIMIT takes one: a whole number of bytes,
// with B, KiB, MiB, GiB or TiB after it.
func parseBytes(s string) (int64, error) {
	units := []struct {
		suffix string
		shift  uint
	}{{"TiB", 40}, {"GiB", 30}, {"MiB", 20}, {"KiB", 10}, {"B", 0}}
	shift := uint(0)
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			s, shift = n, u.shift
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	return n << shift, nil
}

// cgroupMemory is the lowest memory limit of the process's cgroup and the
// cgroups it is in, as /proc/self/cgroup names them, in cgroup v2 or v1; 0
// if there is none, or none can be read.
func cgroupMemory(root fs.FS) int64 {
	data, err := fs.ReadFile(root, "proc/self/cgroup")
	if err != nil {
		return 0
	}
	var lowest int64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// hierarchy-ID:controllers:path; v2's has ID 0 and no controllers.
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		var dir, file string
		switch {
		case parts[0] == "0" && parts[1] == "":
			dir, file = "sys/fs/cgroup", "memory.max"
		case containsField(parts[1], "memory"):
			dir, file = "sys/fs/cgroup/memory", "memory.limit_in_bytes"
		default:
			continue
		}
		// From the process's cgroup up to the root: a parent's limit
		// bounds its children's.
		for p := path.Clean("/" + parts[2]); ; p = path.Dir(p) {
			if n := readLimit(root, path.Join(dir, p, file)); n > 0 && (lowest == 0 || n < lowest) {
				lowest = n
			}
			if p == "/" {
				break
			}
		}
	}
	return lowest
}

func containsField(list, name string) bool {
	for f := range strings.SplitSeq(list, ",") {
		if f == name {
			return true
		}
	}
	return false
}

// readLimit reads a cgroup memory limit: 0 for none ("max", or v1's
// largest page-aligned number), or for a file that cannot be read.
func readLimit(root fs.FS, name string) int64 {
	data, err := fs.ReadFile(root, name)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n <= 0 || n >= 1<<62 {
		return 0
	}
	return n
}
