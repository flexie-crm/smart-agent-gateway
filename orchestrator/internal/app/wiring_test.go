package app

import (
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// What this build can actually reach, asked of the wiring rather than of a test.
//
// A driver and an analyzer register themselves from their own package, so they
// are in a binary only if something imports them, and the something is this
// package. That is easy to lose: an import nobody calls looks like an import
// nobody needs, and every test still passes, because a test binary imports what
// it needs for itself and never notices the server has stopped importing it.
//
// It went exactly that way once. The analyzer's import was dropped while the
// folders were being rearranged, every suite stayed green, and the first anybody
// knew of it was a tool refusing to save with "a policy cannot be enforced on
// this kind of database". This test is here so the next time it is a red build
// rather than somebody's afternoon.
//
// It deliberately asserts nothing about behaviour. It asks one question: is the
// thing the server needs present in the server?
func TestTheWiringRegistersWhatTheServerNeeds(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlserver"} {
		if _, ok := datasource.Get(driver); !ok {
			t.Errorf("no %s driver: internal/app must import internal/datasource/%s, or nothing can connect to that kind of database", driver, driver)
		}
	}
	for _, dialect := range []string{"mysql", "postgres", "sqlserver"} {
		if !sqlguard.Supports(dialect) {
			t.Errorf("no %s analyzer: internal/app must import internal/sqlguard/%s, or a query tool with a policy cannot be saved or run", dialect, dialect)
		}
	}
}
