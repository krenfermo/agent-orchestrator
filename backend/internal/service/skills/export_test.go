package skills

import "time"

// export_test.go -- seams the tests in package skills_test need and production
// code must not have.
//
// The clock is here rather than on Marketplace itself because "move time
// forward" is a thing only a test wants, and a setter on the exported type
// would be a setter somebody could call from a controller.

// SetClockForTest replaces the marketplace's clock, which is also the clock
// every provider it opens uses. It is how a test reaches a stale cache without
// sleeping through a TTL.
func (m *Marketplace) SetClockForTest(now func() time.Time) {
	if m == nil || now == nil {
		return
	}
	m.now = now
}
