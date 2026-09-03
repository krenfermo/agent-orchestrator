//go:build !race

package store_test

// raceEnabled reports that this binary was built with -race. See its
// //go:build race counterpart for why the scale test consults it.
const raceEnabled = false
