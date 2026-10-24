package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SessionFromRegistry reads sessionId from <dir>/<pid>.json.
func SessionFromRegistry(dir string, pid int) (id string, ok bool) {
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	s, ok := m["sessionId"].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
