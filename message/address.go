package message

import (
	"fmt"
	"regexp"
	"strings"
)

// Address names a session: person/session[@machine]; Session "*" is a group.
type Address struct{ Person, Session, Machine string }

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Slug reports whether s is a valid person, session or machine name.
func Slug(s string) bool { return slug.MatchString(s) }

func ParseAddress(s string) (Address, error) {
	var a Address
	rest := s
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		a.Machine, rest = rest[i+1:], rest[:i]
		if !Slug(a.Machine) {
			return Address{}, fmt.Errorf("bad machine in %q", s)
		}
	}
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 1:
		a.Session = parts[0]
	case 2:
		a.Person, a.Session = parts[0], parts[1]
		if !Slug(a.Person) {
			return Address{}, fmt.Errorf("bad person in %q", s)
		}
	default:
		return Address{}, fmt.Errorf("bad address %q", s)
	}
	if a.Session != "*" && !Slug(a.Session) {
		return Address{}, fmt.Errorf("bad session in %q", s)
	}
	return a, nil
}

func (a Address) String() string {
	s := a.Session
	if a.Person != "" {
		s = a.Person + "/" + s
	}
	if a.Machine != "" {
		s += "@" + a.Machine
	}
	return s
}

func (a Address) IsGroup() bool { return a.Session == "*" }
