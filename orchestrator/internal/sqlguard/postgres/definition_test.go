package postgres

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// A view is the one shape where the statement in front of us names nothing that
// is wrong. These are the definitions PostgreSQL actually stores, read back into
// what the view reads and where each of its columns comes from.
//
// The definitions here are written the way information_schema.views reports
// them: fully spelled out, one column per line, tables unqualified when they sit
// in a schema the connection resolves names in.

func shopWithViews(t *testing.T, views ...sqlguard.Table) *sqlguard.Guard {
	t.Helper()
	tables := []sqlguard.Table{
		{Name: "customers", Namespace: "public", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Namespace: "public", Columns: []string{"id", "customer_id", "total"}},
		{Name: "secret_keys", Namespace: "public", Columns: []string{"id", "value"}},
	}
	tables = append(tables, views...)
	g, err := sqlguard.New("postgres", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, sqlguard.NewCatalog("postgres", "shop", tables))
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

func view(name string, columns []string, definition string) sqlguard.Table {
	return sqlguard.Table{Name: name, Namespace: "public", View: true, Columns: columns, Definition: definition}
}

func TestAViewIsDecidedByWhatItsDefinitionReads(t *testing.T) {
	g := shopWithViews(t,
		view("safe_view", []string{"id", "email"},
			"SELECT customers.id, customers.email FROM customers"),
		view("leaky_view", []string{"id", "value"},
			"SELECT secret_keys.id, secret_keys.value FROM secret_keys"),
		// A join, where only one side is out of reach.
		view("mixed_view", []string{"id", "value"},
			"SELECT o.id, k.value FROM (orders o JOIN secret_keys k ON ((k.id = o.id)))"),
		// One reached only through a subquery, which the definition still reads.
		view("hidden_reach", []string{"id"},
			"SELECT orders.id FROM orders WHERE (orders.id IN (SELECT secret_keys.id FROM secret_keys))"),
	)

	allowed(t, g, "SELECT * FROM safe_view")
	for _, sql := range []string{
		"SELECT * FROM leaky_view",
		"SELECT id FROM mixed_view",
		"SELECT id FROM hidden_reach",
	} {
		refused(t, g, sql)
	}
}

func TestAViewsColumnKeepsTheRuleWrittenAboutTheTableColumn(t *testing.T) {
	// The rule names customers.ssn. The view calls it something else entirely,
	// and the rule still holds.
	g := shopWithViews(t,
		view("renamed_view", []string{"id", "code"},
			"SELECT customers.id, customers.ssn AS code FROM customers"),
	)
	rewritten(t, g, "SELECT code FROM renamed_view", "SELECT '[hidden]' AS code FROM renamed_view")
	rewritten(t, g, "SELECT * FROM renamed_view",
		"SELECT renamed_view.id, '[hidden]' AS code FROM renamed_view")
	// Used as a condition through the view, it is left where it is: nothing of
	// it comes back.
	if d := allowed(t, g, "SELECT id FROM renamed_view WHERE code LIKE 'a%'"); d.Rewritten {
		t.Errorf("a condition returns nothing: %s", d.SQL)
	}
}

func TestAViewColumnBuiltOutOfAnExpressionIsHidden(t *testing.T) {
	// Nothing says where this column came from, so it came from anywhere the
	// view reads, and customers has something hidden in it.
	g := shopWithViews(t,
		view("computed_view", []string{"id", "blend"},
			"SELECT customers.id, (customers.email || customers.ssn) AS blend FROM customers"),
	)
	rewritten(t, g, "SELECT blend FROM computed_view", "SELECT '[hidden]' AS blend FROM computed_view")
	// A view over a table with nothing hidden in it keeps its computed column.
	g = shopWithViews(t,
		view("order_totals", []string{"id", "doubled"},
			"SELECT orders.id, (orders.total * 2) AS doubled FROM orders"),
	)
	if d := allowed(t, g, "SELECT doubled FROM order_totals"); d.Rewritten {
		t.Fatalf("nothing this view reads has anything hidden in it: %s", d.SQL)
	}
}

func TestAViewNothingCanAccountForIsRefused(t *testing.T) {
	g := shopWithViews(t,
		// The account may not read this one's definition.
		sqlguard.Table{Name: "no_definition", Namespace: "public", View: true, Columns: []string{"id"}},
		// The definition is not a statement this can read.
		view("unreadable", []string{"id"}, "SELECT FROM WHERE"),
		// It reads a table this snapshot does not have.
		view("reaches_unknown", []string{"id"}, "SELECT x.id FROM nosuchtable x"),
		// It reaches into a schema this snapshot never looked at.
		view("reaches_elsewhere", []string{"id"}, "SELECT x.id FROM other.customers x"),
	)
	for _, name := range []string{"no_definition", "unreadable", "reaches_unknown", "reaches_elsewhere"} {
		reason := refused(t, g, "SELECT id FROM "+name)
		if !strings.Contains(reason, "cannot establish") {
			t.Errorf("%s: expected it to say the view cannot be vouched for, got %q", name, reason)
		}
		if g.Shows(name) {
			t.Errorf("%s must not be listed either", name)
		}
	}
}

func TestAWithInsideADefinitionIsNotMistakenForATable(t *testing.T) {
	g := shopWithViews(t,
		view("cte_view", []string{"id"},
			"WITH recent AS (SELECT orders.id FROM orders) SELECT recent.id FROM recent"),
	)
	// "recent" is not a table the snapshot has, and mistaking it for one would
	// make this view unaccountable and refuse a perfectly ordinary definition.
	allowed(t, g, "SELECT id FROM cte_view")
}

func TestAViewOverAViewIsFollowedAllTheWayDown(t *testing.T) {
	g := shopWithViews(t,
		view("inner_view", []string{"id", "code"},
			"SELECT customers.id, customers.ssn AS code FROM customers"),
		view("outer_view", []string{"id", "code"},
			"SELECT inner_view.id, inner_view.code FROM inner_view"),
		view("outer_secret", []string{"id"}, "SELECT leaf_secret.id FROM leaf_secret"),
		view("leaf_secret", []string{"id"}, "SELECT secret_keys.id FROM secret_keys"),
	)
	// Two views deep, the column is still customers.ssn.
	rewritten(t, g, "SELECT code FROM outer_view", "SELECT '[hidden]' AS code FROM outer_view")
	// And two views deep, the table is still out of reach.
	refused(t, g, "SELECT id FROM outer_secret")
}

// A view whose definition is a set of queries takes its column names from the
// first of them, which is where the database takes them from too.
func TestAViewBuiltOutOfAUnionTakesItsNamesFromTheFirstQuery(t *testing.T) {
	g := shopWithViews(t,
		view("union_view", []string{"id", "code"},
			"SELECT customers.id, customers.ssn AS code FROM customers "+
				"UNION ALL SELECT orders.id, orders.total AS code FROM orders"),
	)
	rewritten(t, g, "SELECT code FROM union_view", "SELECT '[hidden]' AS code FROM union_view")
}

func TestReadDefinitionOnItsOwn(t *testing.T) {
	definition, ok := Analyzer{}.ReadDefinition(
		"SELECT c.ssn AS code, o.total FROM (customers c JOIN orders o ON ((o.customer_id = c.id)))")
	if !ok {
		t.Fatal("that is a perfectly readable definition")
	}
	var names []string
	for _, r := range definition.Reads {
		names = append(names, r.Schema+"."+r.Name)
	}
	if got := strings.Join(names, ","); got != ".customers,.orders" {
		t.Fatalf("reads = %q", got)
	}
	if origin, ok := definition.Origin["code"]; !ok || origin.Table != "customers" || origin.Name != "ssn" {
		t.Fatalf("code should come from customers.ssn, got %+v", origin)
	}
	if origin, ok := definition.Origin["total"]; !ok || origin.Table != "orders" || origin.Name != "total" {
		t.Fatalf("total should come from orders.total, got %+v", origin)
	}
	if _, ok := (Analyzer{}).ReadDefinition("not a statement"); ok {
		t.Fatal("that is not a definition")
	}
	// A schema in front of a name is kept, so a definition reaching outside what
	// the snapshot covers can be told apart from one that does not.
	definition, ok = Analyzer{}.ReadDefinition("SELECT x.id FROM other.customers x")
	if !ok || len(definition.Reads) != 1 || definition.Reads[0].Schema != "other" {
		t.Fatalf("reads = %+v", definition.Reads)
	}
}
