package lib

import (
	"errors"
	"fmt"
	"net"
)

// ResolveAdvertiseAddr turns CLUSTER_ADVERTISE_ADDR into the address this node
// advertises to the rest of the cluster.
//
// Left empty, memberlist picks the address itself, as upstream always let it.
// Inside a container that guess can be an address the other nodes cannot
// reach, and two forks worked around it in two ways, both kept here:
//
//   - "auto" takes the first non-loopback IPv4 of the host, DraftBot's fix.
//     It is a guess too, only a different one: on a host with several
//     interfaces, set the address explicitly.
//   - an IP address or a host name is used as given, a name being resolved
//     first, as PluralKit's fork does.
func ResolveAdvertiseAddr(value string) (string, error) {
	switch value {
	case "":
		return "", nil
	case "auto":
		return firstNonLoopbackIPv4()
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.String(), nil
	}
	addrs, err := net.LookupHost(value)
	if err != nil {
		return "", fmt.Errorf("CLUSTER_ADVERTISE_ADDR %q: %w", value, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("CLUSTER_ADVERTISE_ADDR %q resolves to no address", value)
	}
	return addrs[0], nil
}

func firstNonLoopbackIPv4() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String(), nil
		}
	}
	return "", errors.New("CLUSTER_ADVERTISE_ADDR=auto found no non-loopback IPv4 address")
}
