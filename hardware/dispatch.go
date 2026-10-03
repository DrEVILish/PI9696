package hardware

import (
	"log/slog"
	"sync"
)

// Front-panel events are delivered to the app on one goroutine, in the
// order they were read. They used to be launched as `go callback(...)` per
// event, and goroutines run in no guaranteed order: a quick spin could
// apply detents out of order (a CW/CCW pair netting the wrong way), and a
// click could overtake the rotation just before it. Callbacks take the app
// mutex, so they still run off the GPIO watchers - the watchers only queue.
//
// One dispatcher serves the encoder and all buttons, so ordering holds
// across controls too. It lives for the whole process and is never closed:
// a late event after Close must not panic on a closed channel, and Close
// must not wait on a callback that is itself waiting on the app mutex.

// inputQueueDepth bounds queued events. A handler stalled that long behind
// the app mutex means the UI is wedged anyway; dropping (logged) keeps the
// GPIO watchers responsive instead of blocking them.
const inputQueueDepth = 256

var (
	inputOnce  sync.Once
	inputQueue chan func()
)

func postInput(f func()) {
	inputOnce.Do(func() {
		inputQueue = make(chan func(), inputQueueDepth)
		go func() {
			for f := range inputQueue {
				f()
			}
		}()
	})
	select {
	case inputQueue <- f:
	default:
		slog.Warn("front-panel input dropped: handler queue full")
	}
}
