package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadDefaultsWhenAbsent(t *testing.T) {
	t.Setenv("USER", "alex")
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Person != "alex" {
		t.Errorf("Person = %q, want alex", c.Person)
	}
	if c.Machine == "" {
		t.Errorf("Machine is empty")
	}
	if c.Inbound != Hold {
		t.Errorf("Inbound = %q, want hold", c.Inbound)
	}
	if c.Port != 7777 {
		t.Errorf("Port = %d, want 7777", c.Port)
	}
}

func TestLoadFillsMissingFieldDefaults(t *testing.T) {
	t.Setenv("USER", "alex")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"person":"sam","machine":"box","folders":["/x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Person != "sam" || c.Machine != "box" || len(c.Folders) != 1 || c.Folders[0] != "/x" {
		t.Fatalf("existing fields were overwritten: %+v", c)
	}
	if c.Inbound != Hold {
		t.Errorf("Inbound = %q, want hold", c.Inbound)
	}
	if c.Port != 7777 {
		t.Errorf("Port = %d, want 7777", c.Port)
	}
}

func TestAllowed(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "c")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "a", "bc")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}

	c := Config{Folders: []string{root}}
	if !c.Allowed(inside) {
		t.Errorf("Allowed(%q) = false, want true", inside)
	}
	if c.Allowed(sibling) {
		t.Errorf("Allowed(%q) = true, want false", sibling)
	}

	empty := Config{}
	if empty.Allowed(inside) {
		t.Errorf("empty Folders: Allowed(%q) = true, want false", inside)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Config{Person: "alex", Machine: "mbp", Folders: []string{"/a/b"}, Inbound: Accept, Port: 7777, Paused: true}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Person != c.Person || got.Machine != c.Machine || got.Inbound != c.Inbound ||
		got.Port != c.Port || got.Paused != c.Paused || len(got.Folders) != 1 || got.Folders[0] != c.Folders[0] {
		t.Errorf("round trip: got %+v, want %+v", got, c)
	}
}

func TestPolicyFor(t *testing.T) {
	c := Config{Inbound: Hold}
	if c.PolicyFor("alex", "alex") != Accept {
		t.Errorf("self policy != Accept")
	}
	if c.PolicyFor("sam", "alex") != Hold {
		t.Errorf("other person policy != Hold")
	}
}

func TestUpdateAppliesFn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Update(path, func(c *Config) error {
		c.Paused = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Fatal("Paused = false, want true")
	}
}

func TestUpdatePropagatesFnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := fmt.Errorf("boom")
	if err := Update(path, func(*Config) error { return want }); err != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// TestUpdateConcurrentWritersBothSurvive proves two concurrent Updates to different fields don't
// lose each other's change: an unsynchronized load/modify/save would have one overwrite the other.
func TestUpdateConcurrentWritersBothSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (Config{}).Save(path); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := Update(path, func(c *Config) error {
				if c.People == nil {
					c.People = map[string]Policy{}
				}
				c.People[fmt.Sprintf("p%d", i)] = Accept
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.People) != n {
		t.Fatalf("People has %d entries, want %d: %+v", len(got.People), n, got.People)
	}
}
