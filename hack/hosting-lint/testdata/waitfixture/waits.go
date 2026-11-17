package waitfixture

import (
	"sync"

	"example.test/waitfixture/pkg/common/hosting"
)

// BadBareWait waits on a child with no Park first: a host that runs one goroutine at a time
// cannot see the dependency.
func BadBareWait(wg *sync.WaitGroup) {
	wg.Wait()
}

// BadParkElsewhere parks at the top of a loop, which does not cover the Wait after it.
func BadParkElsewhere(rt hosting.Runtime, wg *sync.WaitGroup, done func() bool) {
	for i := 0; i < 2; i++ {
		rt.Park(done)
	}
	if done() {
		wg.Wait()
	}
}

// GoodParkedWait parks on the child's completion in the same statement list before it waits.
func GoodParkedWait(rt hosting.Runtime, wg *sync.WaitGroup, done func() bool) {
	rt.Park(done)
	wg.Wait()
}
