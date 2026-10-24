package netaddr

import "testing"

func TestIsVirtual(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"en0", false},
		{"eth0", false},
		{"wlan0", false},
		{"lo0", false},
		{"docker0", true},
		{"br-4a3f9c2e1d0b", true},
		{"veth1234abcd", true},
		{"virbr0", true},
		{"vmnet8", true},
		{"utun0", true},
		{"utun3", true},
		{"tailscale0", true},
		{"zt0", true},
		{"ztabcdefgh", true},
	}
	for _, c := range cases {
		if got := isVirtual(c.name); got != c.want {
			t.Errorf("isVirtual(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
