// Package config is the person's config.json: who they are, which folders agentnet is on in, and inbound policy.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Policy says what happens to a person's first message on this machine.
type Policy string

const (
	Accept Policy = "accept"
	Hold   Policy = "hold"
	Refuse Policy = "refuse"
)

// Config is the person's settings, stored at paths.Config().
type Config struct {
	Person  string            `json:"person"`
	Machine string            `json:"machine"`
	Folders []string          `json:"folders"`          // opt-in roots; sessions elsewhere never register
	Inbound Policy            `json:"inbound"`          // for other people's first messages; default "hold"
	People  map[string]Policy `json:"people,omitempty"` // per-person overrides
	Port    int               `json:"port"`             // LAN listener, default 7777
	Peers   []string          `json:"peers,omitempty"`  // static "lan://host:port" hints
	Paused  bool              `json:"paused"`
}

// Load reads path, defaulting Person/Machine/Inbound/Port when the file doesn't exist yet.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		host, _ := os.Hostname()
		return Config{Person: slugify(os.Getenv("USER")), Machine: slugify(host), Inbound: Hold, Port: 7777}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes c to path atomically (temp file + rename), mode 0600.
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Allowed reports whether dir is inside one of c.Folders, resolving symlinks on both sides.
func (c Config) Allowed(dir string) bool {
	if len(c.Folders) == 0 {
		return false
	}
	rd := resolve(dir)
	for _, root := range c.Folders {
		rr := resolve(root)
		rel, err := filepath.Rel(rr, rd)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// PolicyFor is self's policy for person's first message: self is always Accept, else a People override, else Inbound.
func (c Config) PolicyFor(person, self string) Policy {
	if person == self {
		return Accept
	}
	if p, ok := c.People[person]; ok {
		return p
	}
	return c.Inbound
}

// resolve returns p's absolute, symlink-resolved form, falling back to filepath.Clean(abs) if p doesn't exist.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

var invalidSlugChar = regexp.MustCompile(`[^a-z0-9-]`)

// slugify lowercases s and replaces anything that isn't [a-z0-9-] with '-', for a default Person/Machine name.
func slugify(s string) string {
	s = invalidSlugChar.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}
