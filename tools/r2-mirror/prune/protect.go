package prune

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Protected reports whether stamp is pinned against retention: listed either
// as the bare stamp or as <comp>/<stamp>. Permanent pins are kept IN ADDITION
// to the newest keep-N.
func Protected(protect map[string]struct{}, comp, stamp string) bool {
	if len(protect) == 0 {
		return false
	}
	if _, ok := protect[stamp]; ok {
		return true
	}
	if _, ok := protect[comp+"/"+stamp]; ok {
		return true
	}
	return false
}

// ParseProtect reads a retain-permanent file: one tag or stamp per line,
// `#` comments and blank lines ignored.
func ParseProtect(r io.Reader) map[string]struct{} {
	out := make(map[string]struct{})
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = struct{}{}
	}
	return out
}

// LoadProtectFile reads path. A missing file is an empty set, not an error.
func LoadProtectFile(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("retain-permanent %s: %w", path, err)
	}
	defer f.Close()
	return ParseProtect(f), nil
}
