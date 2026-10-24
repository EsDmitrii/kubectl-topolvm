package dump

import (
	"fmt"
	"testing"
	"time"
)

func TestMessageSinglePass(t *testing.T) {
	a := newAnonymizer(true)
	for i := 1; i <= 12; i++ {
		a.note("node", fmt.Sprintf("node-%d", i))
	}
	a.note("pod", "b-node")
	a.freeze()
	done := make(chan string, 1)
	go func() { done <- a.message("moved b-node from node-10 to node-2. node-10x stays") }()
	select {
	case got := <-done:
		want := fmt.Sprintf("moved %s from %s to %s. node-10x stays",
			a.names["pod"]["b-node"], a.names["node"]["node-10"], a.names["node"]["node-2"])
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("message does not terminate when real names collide with generated ones")
	}
}
