//go:build windows

package dockerlock

import "testing"

// Acquire is a no-op on Windows, where these live tests do not run.
func Acquire(testing.TB) {}
