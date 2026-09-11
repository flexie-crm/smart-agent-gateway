package query

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/datasource"
)

// testConfig builds a connection pointing at the local test database from
// SAG_TEST_DSN, the same one the store suites use. The query tool talks to a
// real database, so its end-to-end behaviour is proven against a real one.
func testConfig(t *testing.T) datasource.Config {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping query end-to-end suite")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse SAG_TEST_DSN: %v", err)
	}
	host, portStr, _ := strings.Cut(cfg.Addr, ":")
	port, _ := strconv.Atoi(portStr)
	return datasource.Config{
		Driver: "mysql",
		Host:   host, Port: port,
		// information_schema always exists and is readable, so the end-to-end
		// tests need no fixture database of their own.
		Database: "information_schema",
		Username: cfg.User, Password: cfg.Passwd,
		TLS: datasource.TLS{Mode: "disable"},
	}
}

// A read returns columns and rows from a real query.
func TestRunReads(t *testing.T) {
	res, reason, err := run(context.Background(), read(t), "SELECT 1 AS n, 'hi' AS s", nil, 0)
	if err != nil || reason != "" {
		t.Fatalf("read failed: reason=%q err=%v", reason, err)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "n" || res.Columns[1] != "s" {
		t.Fatalf("columns wrong: %+v", res.Columns)
	}
	if res.RowCount != 1 || len(res.Rows) != 1 {
		t.Fatalf("expected one row: %+v", res.Rows)
	}
	if got := res.Rows[0][1]; got != "hi" {
		t.Fatalf("string cell not returned: %#v", got)
	}
}

// A read mode refuses a write before it ever touches the database.
func TestRunReadModeRefusesWrite(t *testing.T) {
	cfg := testConfig(t)
	// A wrong host proves the refusal happens before connecting: the gate stops
	// it, so the unreachable host is never dialled.
	cfg.Host = "203.0.113.1"
	_, reason, err := run(context.Background(), Settings{Connection: cfg, Access: AccessRead}, "DELETE FROM some_table", nil, 0)
	if err != nil {
		t.Fatalf("a gated write should not error, it should be refused: %v", err)
	}
	if reason == "" {
		t.Fatal("a write in read mode was not refused")
	}
}

// A write mode runs a write statement (a harmless session-variable set, so the
// test mutates nothing) through the exec path.
func TestRunWriteModeExecutes(t *testing.T) {
	res, reason, err := run(context.Background(), write(t), "SET @sag_test = 1", nil, 0)
	if err != nil || reason != "" {
		t.Fatalf("write exec failed: reason=%q err=%v", reason, err)
	}
	if res.Affected == nil {
		t.Fatal("a write result should report rows affected")
	}
}

// A large result is capped, and the cap is reported so the model does not read a
// truncated result as complete.
func TestRunTruncates(t *testing.T) {
	res, reason, err := run(context.Background(), read(t),
		"SELECT table_name FROM information_schema.columns", nil, 3)
	if err != nil || reason != "" {
		t.Fatalf("read failed: reason=%q err=%v", reason, err)
	}
	if res.RowCount != 3 || !res.Truncated {
		t.Fatalf("expected a capped, truncated result: count=%d truncated=%v", res.RowCount, res.Truncated)
	}
}

// read and write are the test's own settings: a live connection with one of the
// two access modes and no policy, which is the tool as it behaves for anybody
// who has not filled the policy in.
func read(t *testing.T) Settings  { return Settings{Connection: testConfig(t), Access: AccessRead} }
func write(t *testing.T) Settings { return Settings{Connection: testConfig(t), Access: AccessWrite} }
