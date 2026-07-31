package main

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false}, // bare ":9999" binds every interface, NOT loopback
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.0.0.5", true}, // whole 127.0.0.0/8
		{"::1", true},
		{"0.0.0.0", false},
		{"192.168.1.10", false},
		{"example.com", false}, // unresolvable/remote name: never assume safe
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
