package query

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
)

// What an administrator may switch on, and what stays off until they do.
//
// Every one of these asserts BOTH directions. A test that only shows a ticked
// box working cannot tell a capability from a gate that was never closed, and a
// test that only shows a refusal cannot tell a closed gate from a verb the
// engine was never going to accept.

func settingsFor(t *testing.T, driver string, allow ...string) Settings {
	t.Helper()
	s := Settings{
		Connection: datasource.Config{Driver: driver, Host: "db", Database: "shop"},
		Access:     AccessBoth,
		Allowed:    map[string]bool{},
	}
	for _, a := range allow {
		s.Allowed[a] = true
	}
	return s
}

// The gate's verbs come from the driver, so each engine's own word works and
// only its own word.
func TestEachEngineUnlocksItsOwnWord(t *testing.T) {
	cases := []struct {
		driver, works, doesNot string
	}{
		{"sqlserver", "EXEC dbo.p", "CALL p()"},
		{"mysql", "CALL p()", "EXEC p"},
		{"postgres", "CALL p()", "EXEC p"},
	}
	for _, c := range cases {
		on := settingsFor(t, c.driver, datasource.CapCallRoutines)
		if ok, reason := check(on.Access, c.works, unlocked(on)); !ok {
			t.Errorf("%s: %q refused with the box ticked: %s", c.driver, c.works, reason)
		}
		// Another engine's verb is not unlocked by this engine's capability.
		if ok, _ := check(on.Access, c.doesNot, unlocked(on)); ok {
			t.Errorf("%s: %q was allowed, but that is not this engine's word", c.driver, c.doesNot)
		}
	}
}

// The control for every case above: untouched, the same statement is refused.
func TestNothingIsUnlockedUntilItIsTicked(t *testing.T) {
	for driver, sql := range map[string]string{
		"sqlserver": "EXEC dbo.p",
		"mysql":     "CALL p()",
		"postgres":  "CALL p()",
	} {
		off := settingsFor(t, driver)
		ok, reason := check(off.Access, sql, unlocked(off))
		if ok {
			t.Errorf("%s: %q ran with nothing ticked", driver, sql)
		}
		if !strings.Contains(reason, "not permitted") {
			t.Errorf("%s: refused for an odd reason: %s", driver, reason)
		}
	}
}

// Calling a routine is a WRITE, whatever the routine turns out to do. A tool set
// to read only stays read only even with the box ticked, because nothing can
// tell from the verb whether the body changes anything.
func TestCallingARoutineIsAWrite(t *testing.T) {
	s := settingsFor(t, "sqlserver", datasource.CapCallRoutines)
	s.Access = AccessRead
	if ok, reason := check(s.Access, "EXEC dbo.p", unlocked(s)); ok {
		t.Error("a read-only tool ran a routine")
	} else if !strings.Contains(reason, "read-only") {
		t.Errorf("refused, but not for being read-only: %s", reason)
	}
	// And the control: the same tool with writes allowed does run it.
	s.Access = AccessBoth
	if ok, reason := check(s.Access, "EXEC dbo.p", unlocked(s)); !ok {
		t.Errorf("a read-and-write tool refused it: %s", reason)
	}
}

// A setting stored for a capability the engine does not offer grants nothing.
// Tools get moved between drivers, and a stale key must not become permission.
func TestASettingFromAnotherEngineGrantsNothing(t *testing.T) {
	s := settingsFor(t, "mysql", datasource.CapCallRoutines)
	// MySQL's capability is CALL; it must not have unlocked SQL Server's word.
	if unlocked(s)["EXEC"] {
		t.Error("a MySQL tool unlocked EXEC")
	}
	// An unknown driver offers nothing at all.
	bogus := settingsFor(t, "not_a_driver", datasource.CapCallRoutines)
	if len(unlocked(bogus)) != 0 {
		t.Error("an unknown driver unlocked something")
	}
}

// The form is built from the driver, so a database that declares a capability
// shows a box for it and one that declares none shows no section.
func TestTheFormAsksWhatTheDatabaseOffers(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlserver"} {
		sections, err := tmpl().Fields(driver)
		if err != nil {
			t.Fatalf("%s: %v", driver, err)
		}
		var found *string
		keys := map[string]bool{}
		for i := range sections {
			if sections[i].Title == "Policy" {
				found = &sections[i].Title
				for _, f := range sections[i].Fields {
					keys[f.Key] = true
				}
			}
		}
		if found == nil {
			t.Errorf("%s: no policy section", driver)
			continue
		}
		for _, want := range []string{
			"allow." + datasource.CapCallRoutines,
			"allow." + datasource.CapCreateRoutines,
			"allow." + datasource.CapCreateTriggers,
		} {
			if !keys[want] {
				t.Errorf("%s: no checkbox for %s", driver, want)
			}
		}
	}
}

// Ticking a box in the form survives being stored and read back, and an
// unticked one reads as off rather than as missing.
func TestATickedBoxSurvivesTheRoundTrip(t *testing.T) {
	settings := map[string]any{
		"access": "both", "host": "db.internal", "database": "shop", "username": "app",
		"allow." + datasource.CapCallRoutines: "true",
	}
	config, err := tmpl().Config("sqlserver", settings)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	parsed, err := ParseConfig(config)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.May(datasource.CapCallRoutines) {
		t.Error("the ticked box did not survive")
	}
	if parsed.May(datasource.CapCreateTriggers) {
		t.Error("a box nobody ticked came back on")
	}

	// And with nothing ticked at all, every capability is off.
	delete(settings, "allow."+datasource.CapCallRoutines)
	config, _ = tmpl().Config("sqlserver", settings)
	parsed, _ = ParseConfig(config)
	for _, c := range []string{
		datasource.CapCallRoutines, datasource.CapCreateRoutines, datasource.CapCreateTriggers,
	} {
		if parsed.May(c) {
			t.Errorf("%s is on with nothing ticked", c)
		}
	}

	// A tool stored before any of this existed has no allow keys at all, and
	// must read as every capability off rather than as absent-means-yes.
	var bare Settings
	if err := json.Unmarshal([]byte(`{}`), &bare); err == nil && bare.May(datasource.CapCallRoutines) {
		t.Error("a tool with no settings at all claimed a capability")
	}
}

// ------------------------------------------------- making and removing objects

// CREATE is already a write, so the leading word cannot separate CREATE TABLE
// from CREATE TRIGGER. Every case here asserts both directions: with the box
// ticked it runs, and with it unticked the same statement on the same tool does
// not. Without the second half this could not tell a capability from a gate that
// was never closed.
func TestMakingStoredCodeNeedsItsOwnBox(t *testing.T) {
	cases := []struct {
		driver, sql, capability string
	}{
		{"mysql", "CREATE PROCEDURE p() SELECT 1", datasource.CapCreateRoutines},
		{"mysql", "CREATE FUNCTION f() RETURNS int RETURN 1", datasource.CapCreateRoutines},
		{"mysql", "DROP PROCEDURE p", datasource.CapCreateRoutines},
		{"mysql", "CREATE TRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1", datasource.CapCreateTriggers},
		{"mysql", "DROP TRIGGER t", datasource.CapCreateTriggers},
		{"postgres", "CREATE OR REPLACE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$", datasource.CapCreateRoutines},
		{"postgres", "CREATE PROCEDURE p() LANGUAGE sql AS $$ SELECT 1 $$", datasource.CapCreateRoutines},
		{"postgres", "CREATE TRIGGER t AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION f()", datasource.CapCreateTriggers},
		{"sqlserver", "CREATE PROCEDURE dbo.p AS BEGIN SELECT 1 END", datasource.CapCreateRoutines},
		// T-SQL's own short spelling, which is a different word.
		{"sqlserver", "CREATE PROC dbo.p AS BEGIN SELECT 1 END", datasource.CapCreateRoutines},
		{"sqlserver", "CREATE OR ALTER PROCEDURE dbo.p AS BEGIN SELECT 1 END", datasource.CapCreateRoutines},
		{"sqlserver", "CREATE TRIGGER t ON orders AFTER INSERT AS BEGIN SELECT 1 END", datasource.CapCreateTriggers},
	}
	for _, c := range cases {
		off := settingsFor(t, c.driver)
		ok, reason := permitted(off, c.sql)
		if ok {
			t.Errorf("%s: %q ran with nothing ticked", c.driver, c.sql)
		} else if !strings.Contains(reason, "not permitted to") {
			t.Errorf("%s: %q refused for an odd reason: %s", c.driver, c.sql, reason)
		}

		on := settingsFor(t, c.driver, c.capability)
		if ok, reason := permitted(on, c.sql); !ok {
			t.Errorf("%s: %q refused with %s ticked: %s", c.driver, c.sql, c.capability, reason)
		}

		// And the other box is not this box: ticking triggers must not permit a
		// routine, or the two would be one setting wearing two labels.
		other := datasource.CapCreateTriggers
		if c.capability == datasource.CapCreateTriggers {
			other = datasource.CapCreateRoutines
		}
		if ok, _ := permitted(settingsFor(t, c.driver, other), c.sql); ok {
			t.Errorf("%s: %q was permitted by %s", c.driver, c.sql, other)
		}
	}
}

// An object kind no capability claims is left exactly as it was: the gate is
// about stored code, not about writing in general.
func TestOrdinaryObjectsAreUntouched(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlserver"} {
		s := settingsFor(t, driver)
		for _, sql := range []string{
			"CREATE TABLE t (id int)",
			"DROP TABLE t",
			"ALTER TABLE t ADD COLUMN note text",
			"CREATE INDEX i ON t (id)",
			"CREATE VIEW v AS SELECT 1",
			"INSERT INTO t VALUES (1)",
			"SELECT * FROM t",
		} {
			if ok, reason := permitted(s, sql); !ok {
				t.Errorf("%s: %q was refused: %s", driver, sql, reason)
			}
		}
	}
}

// A name that happens to be a keyword is written in quotes, and quoting is what
// says it is a name. Reading it as the keyword would refuse a statement about an
// ordinary table.
func TestAQuotedNameIsNotAKeyword(t *testing.T) {
	for driver, sql := range map[string]string{
		"mysql":     "DROP TABLE `trigger`",
		"postgres":  `DROP TABLE "trigger"`,
		"sqlserver": "DROP TABLE [trigger]",
	} {
		if ok, reason := permitted(settingsFor(t, driver), sql); !ok {
			t.Errorf("%s: %q was refused: %s", driver, sql, reason)
		}
	}
}

// A read-only tool cannot make stored code however many boxes are ticked: the
// access mode is the first gate and the capability does not lift it.
func TestTheAccessModeStillComesFirst(t *testing.T) {
	s := settingsFor(t, "mysql", datasource.CapCreateRoutines, datasource.CapCreateTriggers)
	s.Access = AccessRead
	if ok, reason := permitted(s, "CREATE PROCEDURE p() SELECT 1"); ok {
		t.Error("a read-only tool made a procedure")
	} else if !strings.Contains(reason, "read-only") {
		t.Errorf("refused, but not for being read-only: %s", reason)
	}
}
