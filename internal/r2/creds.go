package r2

import (
	"fmt"
	"os"
	"strings"
)

// ReadCreds parses access_key_id + secret_access_key out of a minimal TOML
// file (`key = "value"`, one per line, '#' comments allowed).
//
// Shared by the cut's mirror and the manage service so there is one spelling
// of the file format. The secret is returned to the caller and never logged:
// every error here names the PATH, never the contents.
func ReadCreds(path string) (accessKeyID, secret string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read creds %q: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "access_key_id":
			accessKeyID = val
		case "secret_access_key":
			secret = val
		}
	}
	if accessKeyID == "" || secret == "" {
		return "", "", fmt.Errorf("creds %q: missing access_key_id or secret_access_key", path)
	}
	return accessKeyID, secret, nil
}
