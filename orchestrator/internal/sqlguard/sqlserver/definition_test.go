package sqlserver

import (
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// A view is the one shape where the statement in front of us names nothing that
// is wrong. These are the definitions SQL Server actually stores, read back into
// what the view reads and where each of its columns comes from.
//
// What arrives here is the whole CREATE VIEW, not the query inside it, because
// that is what sys.sql_modules keeps: the text of the statement that made the
// object. Nothing above this package unwraps it, so this is where it is unwrapped
// or nowhere.

func shopWithViews(t *testing.T, views ...sqlguard.Table) *sqlguard.Guard {
	t.Helper()
	tables := []sqlguard.Table{
		{Name: "customers", Namespace: "dbo", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Namespace: "dbo", Columns: []string{"id", "customer_id", "total"}},
		{Name: "payroll", Namespace: "dbo", Columns: []string{"id", "emp", "amount"}},
	}
	tables = append(tables, views...)
	g, err := sqlguard.New("sqlserver", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, sqlguard.NewCatalog("sqlserver", "shop", tables))
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

func view(name string, columns []string, definition string) sqlguard.Table {
	return sqlguard.Table{Name: name, Namespace: "dbo", View: true, Columns: columns, Definition: definition}
}

// A view over a table nobody may see is refused, though the statement names only
// the view. This is the whole reason a definition is read at all.
func TestAViewIsDecidedByWhatItsDefinitionReads(t *testing.T) {
	g := shopWithViews(t,
		view("safe_view", []string{"id", "email"},
			"CREATE VIEW dbo.safe_view AS SELECT c.id, c.email FROM dbo.customers c"),
		view("leaky_view", []string{"id", "amount"},
			"CREATE VIEW dbo.leaky_view AS SELECT p.id, p.amount FROM dbo.payroll p"),
		// A join where only one side is out of reach.
		view("mixed_view", []string{"id", "amount"},
			"CREATE VIEW dbo.mixed_view AS SELECT o.id, p.amount FROM dbo.orders o JOIN dbo.payroll p ON p.id = o.id"),
		// Reached only through a subquery, which the definition still reads.
		view("hidden_reach", []string{"id"},
			"CREATE VIEW dbo.hidden_reach AS SELECT o.id FROM dbo.orders o WHERE o.id IN (SELECT p.id FROM dbo.payroll p)"),
	)

	allowed(t, g, `SELECT id FROM safe_view`)
	refused(t, g, `SELECT id FROM leaky_view`)
	refused(t, g, `SELECT id FROM mixed_view`)
	refused(t, g, `SELECT id FROM hidden_reach`)
}

// A rule about customers.ssn has to hold when the column is reached through a
// view that calls it something else. The origin is what carries it, and it is
// read out of the definition rather than taken on trust.
func TestAViewsColumnIsFollowedBackToTheTableItReads(t *testing.T) {
	g := shopWithViews(t,
		view("renamed", []string{"id", "email", "code"},
			"CREATE VIEW dbo.renamed AS SELECT c.id, c.email, c.ssn AS code FROM dbo.customers c"),
	)
	// No rule anywhere mentions "code".
	masked(t, g, `SELECT code FROM renamed`)
	// And the columns that are not it are untouched.
	untouched(t, g, `SELECT id, email FROM renamed`)
	// It still behaves as itself where it is not returned.
	untouched(t, g, `SELECT id FROM renamed WHERE code = '1'`)
}

// A view column built out of an expression is not read from one place, so
// nothing can follow it anywhere. It is hidden whenever anything the view reads
// has something hidden in it: over-hiding costs a column in a report, and the
// other way costs the value.
func TestAViewColumnWithNoSingleOriginIsHidden(t *testing.T) {
	g := shopWithViews(t,
		view("computed", []string{"id", "blob"},
			"CREATE VIEW dbo.computed AS SELECT c.id, CONCAT(c.ssn, c.email) AS blob FROM dbo.customers c"),
	)
	masked(t, g, `SELECT blob FROM computed`)
	// A view over a table with nothing hidden in it keeps its computed column.
	g = shopWithViews(t,
		view("safe_computed", []string{"id", "blob"},
			"CREATE VIEW dbo.safe_computed AS SELECT o.id, CONCAT(o.total, o.id) AS blob FROM dbo.orders o"),
	)
	untouched(t, g, `SELECT blob FROM safe_computed`)
}

// A view nothing can account for is refused rather than assumed harmless. Each
// of these is a different way of being unaccountable, and the last one is the
// one that matters most in practice: SQL Server truncates a long definition in
// INFORMATION_SCHEMA.VIEWS, and a truncated definition does not parse.
func TestAViewNothingCanAccountForIsRefused(t *testing.T) {
	g := shopWithViews(t,
		// The account may not read the definition, so it comes back empty.
		view("no_definition", []string{"id"}, ""),
		// Unreadable.
		view("broken", []string{"id"}, "CREATE VIEW dbo.broken AS SELECT FROM WHERE"),
		// Reads something this side cannot resolve.
		view("from_function", []string{"id"},
			"CREATE VIEW dbo.from_function AS SELECT f.id FROM dbo.some_function(1) f"),
		// Reaches another database entirely.
		view("elsewhere", []string{"id"},
			"CREATE VIEW dbo.elsewhere AS SELECT c.id FROM other.dbo.customers c"),
		// Cut off mid-statement, which is exactly what a 4000-character column
		// does to a long view.
		view("truncated", []string{"id"},
			"CREATE VIEW dbo.truncated AS SELECT c.id, c.email FROM dbo.customers c WHERE c.id IN (SELECT p.id FROM dbo.pay"),
	)
	for _, name := range []string{"no_definition", "broken", "from_function", "elsewhere", "truncated"} {
		refused(t, g, `SELECT id FROM `+name)
	}
}

// A name a WITH introduced inside a view's definition looks exactly like a table
// and is not one. Crediting the view with reading it would be wrong in the
// direction that matters if the CTE happened to share a name with a real table.
func TestACTEInsideAViewIsNotATableItReads(t *testing.T) {
	g := shopWithViews(t,
		view("with_cte", []string{"id"},
			"CREATE VIEW dbo.with_cte AS WITH recent AS (SELECT o.id FROM dbo.orders o) SELECT r.id FROM recent r"),
	)
	allowed(t, g, `SELECT id FROM with_cte`)
}

// A view over a view over a table nobody may see is still refused: the catalog
// resolves through as many levels as there are.
func TestAViewOverAViewIsFollowedThrough(t *testing.T) {
	g := shopWithViews(t,
		view("inner", []string{"id", "amount"},
			"CREATE VIEW dbo.inner AS SELECT p.id, p.amount FROM dbo.payroll p"),
		view("outer", []string{"id"},
			"CREATE VIEW dbo.outer AS SELECT i.id FROM dbo.inner i"),
	)
	refused(t, g, `SELECT id FROM outer`)
}
