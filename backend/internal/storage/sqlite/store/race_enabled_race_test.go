//go:build race

package store_test

// raceEnabled reports that this binary was built with -race. The scale test
// reads it to skip: its assertions are WALL-CLOCK budgets meant to catch a lost
// index or a flattened CTE, and race instrumentation costs an order of
// magnitude, so under -race the budgets measure the instrumentation rather than
// the query plan. The correctness assertions in that file are covered by the
// non-race run.
const raceEnabled = true
