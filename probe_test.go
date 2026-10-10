package main

import (
	"net"
	"testing"
)

// closedAddress is an address on this machine that refuses
// connections: a listener's port, after the listener closed.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustSucceed(t, err)
	address := listener.Addr().String()
	mustSucceed(t, listener.Close())
	return address
}

func TestResultOf(t *testing.T) {
	cases := []struct {
		name   string
		failed []string
		want   probeResult
	}{
		{"nothing failed", nil, probeResult{passed: true}},
		{"one failure is the whole reason", []string{"a: refused"}, probeResult{reason: "a: refused"}},
		{"several failures start with a count", []string{"a: refused", "c: refused"},
			probeResult{reason: "2 of 3 targets failed\na: refused\nc: refused"}},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			mustMatch(t, resultOf(one.failed, 3, "targets"), one.want)
		})
	}
}
