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

// GoodParkedWait parks on the child's completion in the same statement list before it waits, and
// states that the Park joins the group the Wait joins.
func GoodParkedWait(rt hosting.Runtime, wg *sync.WaitGroup, done func() bool) {
	//hosting:park-joins wg
	rt.Park(done)
	wg.Wait()
}

// GoodTrailingMarker states the join at the end of the Park's own line.
func GoodTrailingMarker(rt hosting.Runtime, wg *sync.WaitGroup, done func() bool) {
	rt.Park(done) //hosting:park-joins wg
	wg.Wait()
}

// BadUnrelatedPark parks on a condition that says nothing about the group the Wait joins (#2215):
// a host that runs goroutines one at a time still cannot see that the parent depends on wg, and
// if unrelatedDone is true first the parent enters Wait and deadlocks the host.
func BadUnrelatedPark(rt hosting.Runtime, wg *sync.WaitGroup, unrelatedDone func() bool) {
	rt.Park(unrelatedDone)
	wg.Wait()
}

// BadParkNamesAnotherGroup states a join, but of another group than the Wait's.
func BadParkNamesAnotherGroup(rt hosting.Runtime, wg, other *sync.WaitGroup, done func() bool) {
	//hosting:park-joins other
	rt.Park(done)
	wg.Wait()
}
