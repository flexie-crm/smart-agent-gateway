package sqlstore_test

import (
	"os"
	"testing"

	"flexie.io/sag/internal/store/storetest"
	"flexie.io/sag/internal/testdb"
)

// TestSQLStore runs the shared conformance suite against a real MariaDB.
// Set SAG_TEST_DSN to a scratch database (this suite gets its own copy of
// it, so it never collides with other test packages).
func TestSQLStore(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping SQL store conformance suite")
	}
	st, reset := testdb.Open(t, dsn, dbSuffix)
	storetest.Run(t, st, reset)
}

// dbSuffix names this package's scratch database. Open makes it, TestMain
// takes it away, and they read it from here so they cannot drift apart.
const dbSuffix = "store"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }
