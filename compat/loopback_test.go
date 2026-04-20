package compat

import "testing"

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18741", true},
		{"127.0.0.2:18741", true}, // entire 127.0.0.0/8 is loopback
		{"[::1]:18741", true},
		{"[0:0:0:0:0:0:0:1]:18741", true},  // uncompressed IPv6 loopback
		{"[::ffff:127.0.0.1]:18741", true}, // IPv4-mapped IPv6 loopback
		{"localhost:18741", true},
		{"[::ffff:192.168.1.5]:18741", false}, // IPv4-mapped IPv6 non-loopback
		{"0.0.0.0:18741", false},
		{":18741", false},
		{"192.168.1.5:18741", false},
		{"example.com:18741", false},
		{"not-an-address", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isLoopback(tc.addr); got != tc.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
