package message

import "testing"

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		want Address
	}{
		{"alice/api-claude", Address{Person: "alice", Session: "api-claude"}},
		{"alice/api-claude@xps", Address{Person: "alice", Session: "api-claude", Machine: "xps"}},
		{"api-claude", Address{Session: "api-claude"}},
		{"alice/*", Address{Person: "alice", Session: "*"}},
	}
	for _, c := range cases {
		got, err := ParseAddress(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseAddress(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
		if got.String() != c.in {
			t.Errorf("String() = %q, want %q", got.String(), c.in)
		}
	}
}

func TestParseAddressRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "a/b/c", "Alice/x", "a/ b", "a/b@", "../x"} {
		if _, err := ParseAddress(in); err == nil {
			t.Errorf("ParseAddress(%q) accepted", in)
		}
	}
}

func TestIDs(t *testing.T) {
	id := NewID()
	if !ValidID(id) || ValidID("nope") || ValidID(id+"x") || NewID() == id {
		t.Fatalf("ids wrong for %q", id)
	}
}
