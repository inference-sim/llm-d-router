/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package internal

import "github.com/llm-d/llm-d-router/pkg/common/hosting"

// waitForCleanup blocks until the cleanup sweep has returned. The parent parks first, so a host that runs router
// goroutines one at a time sees that it waits on its child and resumes the child; under the real runtime Park returns
// at once and the WaitGroup does the waiting, as upstream does. A WaitGroup cannot be queried, so the sweep sets
// cleanupDone as it returns.
func (p *Processor) waitForCleanup(rt hosting.Runtime) {
	rt.Park(p.cleanupDone.Load)
	p.wg.Wait()
}
