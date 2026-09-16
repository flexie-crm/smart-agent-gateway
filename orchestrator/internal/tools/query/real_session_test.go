package query

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
)

// Statements a real agent really wrote, that really worked, replayed through the
// gate. Every refusal here is a false one.
//
// These came out of a personal installation's own transcript: 88 calls that
// succeeded against a live SQL Server while somebody built an accounting
// sandbox. A corpus somebody invents is shaped by what they already believe the
// code does; this one is not, which is why it found in one run what the invented
// one did not: a CREATE FUNCTION refused as "several statements joined
// together", because a function is a Batch_level_statement and its body's
// clauses have no Sql_clauses above them to be nested under.
func TestARealSessionIsNotRefused(t *testing.T) {
	f, err := os.Open("testdata/real_session.sql")
	if err != nil {
		t.Skip("no real-session corpus")
	}
	defer func() { _ = f.Close() }()

	s := settingsFor(t, "sqlserver",
		datasource.CapCallRoutines, datasource.CapCreateRoutines, datasource.CapCreateTriggers)
	var ran, refused int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		sql := strings.TrimSpace(sc.Text())
		if sql == "" || sql == "NULL" {
			continue
		}
		if ok, reason := permitted(s, sql); !ok {
			refused++
			t.Errorf("FALSE REFUSAL  %.110s\n    %s", sql, reason)
		} else {
			ran++
		}
	}
	t.Logf("  real session: %d allowed, %d FALSELY REFUSED", ran, refused)
}
