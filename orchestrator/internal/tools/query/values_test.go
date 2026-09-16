package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
)

// What a real database hands back, against real databases.
//
// internal/datasource/cell_test.go pins the rendering; this pins that each
// driver asks for the column type at all, which is the half a unit test cannot
// see. Every expectation here is what the engine returned BEFORE the change,
// corrected.
func TestValuesComeBackFitToRead(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    func(*testing.T) datasource.Config
		setup  []string
		query  string
		expect map[string]string // column -> what it must be
		moment []string          // columns that must keep their time
	}{
		{
			name: "mysql", cfg: fixture,
			setup: []string{
				"DROP TABLE IF EXISTS vals",
				"CREATE TABLE vals (d DATE, dt DATETIME, t TIME, raw VARBINARY(64), txt VARCHAR(20))",
				"INSERT INTO vals VALUES ('2027-01-15','2027-01-15 13:45:02','13:45:02', UNHEX('89504E470D0A1A0A'), 'hello')",
			},
			query:  "SELECT d, dt, t, raw, txt FROM vals",
			expect: map[string]string{"d": "2027-01-15", "t": "13:45:02", "raw": "[binary, 8 bytes, not returned]", "txt": "hello"},
			moment: []string{"dt"},
		},
		{
			name: "postgres", cfg: pgFixture,
			setup: []string{
				"DROP TABLE IF EXISTS vals",
				"CREATE TABLE vals (d date, ts timestamp, t time, raw bytea, txt text)",
				"INSERT INTO vals VALUES ('2027-01-15','2027-01-15 13:45:02','13:45:02','\\x89504e470d0a1a0a'::bytea,'hello')",
			},
			query:  "SELECT d, ts, t, raw, txt FROM vals",
			expect: map[string]string{"d": "2027-01-15", "t": "13:45:02", "raw": "[binary, 8 bytes, not returned]", "txt": "hello"},
			moment: []string{"ts"},
		},
		{
			name: "sqlserver", cfg: msFixture,
			setup: []string{
				"CREATE TABLE vals (d DATE, dt DATETIME2, t TIME, raw VARBINARY(64), txt NVARCHAR(20))",
				"INSERT INTO vals VALUES ('2027-01-15','2027-01-15 13:45:02','13:45:02', 0x89504E470D0A1A0A, N'hello')",
			},
			query:  "SELECT d, dt, t, raw, txt FROM vals",
			expect: map[string]string{"d": "2027-01-15", "t": "13:45:02", "raw": "[binary, 8 bytes, not returned]", "txt": "hello"},
			moment: []string{"dt"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(t)
			conn, err := datasource.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.setup {
				if _, err := conn.Exec(context.Background(), stmt, nil); err != nil &&
					!strings.Contains(err.Error(), "does not exist") {
					t.Fatalf("setup %.50s: %v", stmt, err)
				}
			}
			_ = conn.Close()

			res, reason, err := run(context.Background(), Settings{Connection: cfg, Access: AccessBoth}, c.query, nil, 0)
			if reason != "" || err != nil {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("want one row, got %d", len(res.Rows))
			}
			// The model is told once, in words, rather than left to work it out
			// from every cell.
			if !strings.Contains(res.Note, "Binary data is not returned") || !strings.Contains(res.Note, "raw") {
				t.Errorf("nothing told the model the binary column was refused: note=%q", res.Note)
			}
			for i, col := range res.Columns {
				got := fmt.Sprint(res.Rows[0][i])
				if want, ok := c.expect[col]; ok && got != want {
					t.Errorf("%s: got %q, want %q", col, got, want)
				}
				for _, m := range c.moment {
					if col == m && !strings.Contains(got, "13:45:02") {
						t.Errorf("%s is a moment and lost its time: %q", col, got)
					}
				}
			}
		})
	}
}

// A capped answer says so in words, not only in a field a model may not read.
func TestACappedAnswerSaysSo(t *testing.T) {
	cfg := fixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS many", nil)
	_, _ = conn.Exec(context.Background(), "CREATE TABLE many (id int)", nil)
	for i := 0; i < 12; i++ {
		_, _ = conn.Exec(context.Background(), "INSERT INTO many VALUES (1),(2),(3),(4),(5)", nil)
	}
	_ = conn.Close()

	res, reason, err := run(context.Background(), Settings{Connection: cfg, Access: AccessRead},
		"SELECT id FROM many", nil, 10)
	if reason != "" || err != nil {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if !res.Truncated {
		t.Fatal("it was not capped, so this proves nothing")
	}
	if !strings.Contains(res.Note, "Only the first 10 rows") {
		t.Errorf("a capped answer said nothing a model would read: note=%q", res.Note)
	}
	// And an answer that was NOT capped says nothing, so the note means something
	// when it is there.
	full, _, _ := run(context.Background(), Settings{Connection: cfg, Access: AccessRead},
		"SELECT id FROM many LIMIT 3", nil, 10)
	if full.Note != "" {
		t.Errorf("an uncapped answer carried a note: %q", full.Note)
	}
}
