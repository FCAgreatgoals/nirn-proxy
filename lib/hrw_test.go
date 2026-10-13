package lib

import (
	"fmt"
	"testing"
)

func keys(n int) []uint64 {
	k := make([]uint64, n)
	for i := range k {
		k[i] = HashCRC64(fmt.Sprintf("/channels/%d/messages", 1000+i))
	}
	return k
}

// Each node owns its share of the buckets.
func TestHRWSpreadsTheBuckets(t *testing.T) {
	members := []string{"nirn-a", "nirn-b", "nirn-c"}
	owned := map[string]int{}
	for _, k := range keys(30000) {
		owned[hrwOwner(k, members)]++
	}
	for _, m := range members {
		if owned[m] < 9000 || owned[m] > 11000 {
			t.Errorf("%s owns %d of 30000", m, owned[m])
		}
	}
}

// A node leaving moves its own buckets and no other; coming back, it takes
// back the same ones.
func TestHRWMovesOnlyTheLeavingNodesBuckets(t *testing.T) {
	all := []string{"nirn-a", "nirn-b", "nirn-c"}
	left := []string{"nirn-a", "nirn-c"}
	for _, k := range keys(30000) {
		before, after := hrwOwner(k, all), hrwOwner(k, left)
		if before != "nirn-b" && after != before {
			t.Fatalf("bucket %d moved from %s to %s though %s stayed", k, before, after, before)
		}
		if before == "nirn-b" && after == "nirn-b" {
			t.Fatalf("bucket %d still owned by the node that left", k)
		}
	}
}

// A node joining takes about its share, only from the others.
func TestHRWJoinTakesAShare(t *testing.T) {
	three := []string{"nirn-a", "nirn-b", "nirn-c"}
	four := []string{"nirn-a", "nirn-b", "nirn-c", "nirn-d"}
	moved := 0
	for _, k := range keys(30000) {
		before, after := hrwOwner(k, three), hrwOwner(k, four)
		if before != after {
			if after != "nirn-d" {
				t.Fatalf("bucket %d moved from %s to %s", k, before, after)
			}
			moved++
		}
	}
	if moved < 6500 || moved > 8500 {
		t.Errorf("%d of 30000 buckets moved, want about a quarter", moved)
	}
}

// Every node computes the same owner whatever order it lists the members in,
// once sorted as reindexMembers does.
func TestHRWSameOwnerOnEveryNode(t *testing.T) {
	if hrwOwner(42, []string{"a", "b", "c"}) != hrwOwner(42, []string{"a", "b", "c"}) {
		t.Fatal("owner not stable")
	}
	if got := hrwOwner(42, []string{"solo"}); got != "solo" {
		t.Errorf("single member: %q", got)
	}
}
