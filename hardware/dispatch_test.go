package hardware

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// Encoder detents must reach the app in the order they were read. With one
// goroutine per event, a handler that takes a variable time (as the real
// ones do, contending for the app mutex) scrambled the sequence.
func TestEncoderEventsDeliveredInOrder(t *testing.T) {
	t.Setenv("PI9696_SIM", "1")
	e, err := NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	e.SetRotateCallback(func(dir int) {
		time.Sleep(time.Duration(rand.Intn(200)) * time.Microsecond)
		mu.Lock()
		got = append(got, dir)
		if len(got) == n {
			close(done)
		}
		mu.Unlock()
	})
	want := make([]int, n)
	for i := range want {
		want[i] = 1 - 2*(i%3%2) // +1,-1,+1,+1,-1,+1,...
		e.handleRotation(want[i])
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d of %d events delivered", len(got), n)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d delivered out of order: got %v, want %v", i, got[i:min(i+5, n)], want[i:min(i+5, n)])
		}
	}
}

// A full queue must drop rather than block the GPIO watcher that posts.
func TestPostInputNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	postInput(func() { <-release }) // wedge the dispatcher
	start := time.Now()
	for i := 0; i < inputQueueDepth*2; i++ {
		postInput(func() {})
	}
	close(release)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("postInput blocked for %s with a stalled handler", el)
	}
}
