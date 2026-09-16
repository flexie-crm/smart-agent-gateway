package sqlserver

import "testing"

// A routine written with its parts quoted is the same routine.
func TestAQuotedRoutineNameIsStillThatRoutine(t *testing.T) {
	g := withRoutines(t, true) // denies payroll, hides customers.ssn
	for _, sql := range []string{
		"SELECT dbo.r_denied()",
		"SELECT [dbo].[r_denied]()",
		"SELECT \"dbo\".\"r_denied\"()",
		"EXEC [dbo].[r_denied]",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason == "" {
			t.Errorf("ALLOWED, and should not be: %s", sql)
		}
	}
	// And the clean one still runs, quoted or not.
	for _, sql := range []string{"SELECT dbo.r_clean()", "EXEC [dbo].[r_clean]"} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}
}
