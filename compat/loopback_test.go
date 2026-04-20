package compat

import "testing"

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18741", true},
		{"[::1]:18741", true},
		{"localhost:18741", true},
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
