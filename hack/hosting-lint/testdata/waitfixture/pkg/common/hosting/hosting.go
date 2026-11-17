// Package hosting is a stand-in for the router's runtime seam, for the lint's own test.
package hosting

// Runtime has the one method the lint looks for.
type Runtime interface{ Park(ready func() bool) }
