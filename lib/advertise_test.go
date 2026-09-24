package lib

import (
	"net"
	"testing"
)

func TestResolveAdvertiseAddr(t *testing.T) {
	if got, err := ResolveAdvertiseAddr(""); got != "" || err != nil {
		t.Errorf("empty = %q, %v: memberlist must keep choosing", got, err)
	}
	if got, _ := ResolveAdvertiseAddr("10.0.0.42"); got != "10.0.0.42" {
		t.Errorf("IP = %q", got)
	}
	if got, err := ResolveAdvertiseAddr("localhost"); err != nil || net.ParseIP(got) == nil {
		t.Errorf("host name = %q, %v: want a resolved address", got, err)
	}
	if _, err := ResolveAdvertiseAddr("no-such-host.invalid"); err == nil {
		t.Error("an unresolvable name was accepted")
	}

	got, err := ResolveAdvertiseAddr("auto")
	if err != nil {
		t.Skipf("no non-loopback IPv4 on this machine: %v", err)
	}
	if ip := net.ParseIP(got); ip == nil || ip.IsLoopback() || ip.To4() == nil {
		t.Errorf("auto = %q, want a non-loopback IPv4", got)
	}
}
