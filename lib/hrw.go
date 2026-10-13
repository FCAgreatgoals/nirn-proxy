package lib

// Highest random weight (rendezvous) hashing: a bucket, or a bot's global
// limit, belongs to the node that scores highest for it. When a node leaves,
// only what it owned moves to another node; the rest keeps its owner and what
// that owner learned. Picking by hash modulo the member count instead moved
// most buckets on any change to the cluster, each new owner starting blind
// while the old one could still be sending.

// hrwOwner is the member of members that owns key. members is sorted,
// so that a tie, if one ever happens, is settled the same way on every node.
func hrwOwner(key uint64, members []string) string {
	var owner string
	var best uint64
	for i, name := range members {
		if score := mix64(key ^ HashCRC64(name)); i == 0 || score > best {
			owner, best = name, score
		}
	}
	return owner
}

// mix64 is splitmix64's finalizer: it spreads close inputs far apart, so that
// each node's scores look independent of the others'.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
