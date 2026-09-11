package mysql

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// A view is the one shape where the statement in front of us names nothing that
// is wrong. These are the definitions a database actually stores, read back into
// what the view reads and where each of its columns comes from.

func shopWithViews(t *testing.T, views ...sqlguard.Table) *sqlguard.Guard {
	t.Helper()
	tables := []sqlguard.Table{
		{Name: "customers", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Columns: []string{"id", "customer_id", "total"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
	}
	tables = append(tables, views...)
	g, err := sqlguard.New("mysql", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, sqlguard.NewCatalog("mysql", "shop", tables))
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

func TestAViewIsDecidedByWhatItsDefinitionReads(t *testing.T) {
	g := shopWithViews(t,
		sqlguard.Table{Name: "safe_view", View: true, Columns: []string{"id", "email"},
			Definition: "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`email` AS `email` from `shop`.`customers`"},
		sqlguard.Table{Name: "leaky_view", View: true, Columns: []string{"id", "value"},
			Definition: "select `shop`.`secret_keys`.`id` AS `id`,`shop`.`secret_keys`.`value` AS `value` from `shop`.`secret_keys`"},
		// A join, where only one side is out of reach.
		sqlguard.Table{Name: "mixed_view", View: true, Columns: []string{"id", "value"},
			Definition: "select `o`.`id` AS `id`,`k`.`value` AS `value` from (`shop`.`orders` `o` join `shop`.`secret_keys` `k` on(`k`.`id` = `o`.`id`))"},
		// One reached only through a subquery, which the definition still reads.
		sqlguard.Table{Name: "hidden_reach", View: true, Columns: []string{"id"},
			Definition: "select `shop`.`orders`.`id` AS `id` from `shop`.`orders` where `shop`.`orders`.`id` in (select `shop`.`secret_keys`.`id` from `shop`.`secret_keys`)"},
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
		sqlguard.Table{Name: "renamed_view", View: true, Columns: []string{"id", "code"},
			Definition: "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`ssn` AS `code` from `shop`.`customers`"},
	)
	rewritten(t, g, "SELECT code FROM renamed_view",
		"SELECT '[hidden]' AS `code` FROM `renamed_view`")
	rewritten(t, g, "SELECT * FROM renamed_view",
		"SELECT `renamed_view`.`id`,'[hidden]' AS `code` FROM `renamed_view`")
	// Used as a condition through the view, it is left where it is: nothing of
	// it comes back.
	if d := allowed(t, g, "SELECT id FROM renamed_view WHERE code LIKE 'a%'"); d.Rewritten {
		t.Errorf("a condition returns nothing: %s", d.SQL)
	}
}

func TestAViewColumnBuiltOutOfAnExpressionIsHidden(t *testing.T) {
	// Nothing says where this column came from, so it came from anywhere the view
	// reads, and customers has something hidden in it.
	g := shopWithViews(t,
		sqlguard.Table{Name: "computed_view", View: true, Columns: []string{"id", "blend"},
			Definition: "select `shop`.`customers`.`id` AS `id`,concat(`shop`.`customers`.`email`,`shop`.`customers`.`ssn`) AS `blend` from `shop`.`customers`"},
	)
	rewritten(t, g, "SELECT blend FROM computed_view",
		"SELECT '[hidden]' AS `blend` FROM `computed_view`")
	// A view over a table with nothing hidden in it keeps its computed column.
	g = shopWithViews(t,
		sqlguard.Table{Name: "order_totals", View: true, Columns: []string{"id", "doubled"},
			Definition: "select `shop`.`orders`.`id` AS `id`,(`shop`.`orders`.`total` * 2) AS `doubled` from `shop`.`orders`"},
	)
	if d := allowed(t, g, "SELECT doubled FROM order_totals"); d.Rewritten {
		t.Fatalf("nothing this view reads has anything hidden in it: %s", d.SQL)
	}
}

func TestAViewNothingCanAccountForIsRefused(t *testing.T) {
	g := shopWithViews(t,
		// The account may not read this one's definition.
		sqlguard.Table{Name: "no_definition", View: true, Columns: []string{"id"}},
		// The definition is not a statement this can read.
		sqlguard.Table{Name: "unreadable", View: true, Columns: []string{"id"}, Definition: "select from where"},
		// It reads a table this snapshot does not have.
		sqlguard.Table{Name: "reaches_unknown", View: true, Columns: []string{"id"},
			Definition: "select `x`.`id` AS `id` from `shop`.`nosuchtable` `x`"},
		// It reaches into another database entirely.
		sqlguard.Table{Name: "reaches_elsewhere", View: true, Columns: []string{"id"},
			Definition: "select `x`.`id` AS `id` from `otherdb`.`customers` `x`"},
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
		sqlguard.Table{Name: "cte_view", View: true, Columns: []string{"id"},
			Definition: "with `recent` as (select `shop`.`orders`.`id` AS `id` from `shop`.`orders`) " +
				"select `recent`.`id` AS `id` from `recent`"},
	)
	// "recent" is not a table the snapshot has, and mistaking it for one would
	// make this view unaccountable and refuse a perfectly ordinary definition.
	allowed(t, g, "SELECT id FROM cte_view")
}

func TestAViewOverAViewIsFollowedAllTheWayDown(t *testing.T) {
	g := shopWithViews(t,
		sqlguard.Table{Name: "inner_view", View: true, Columns: []string{"id", "code"},
			Definition: "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`ssn` AS `code` from `shop`.`customers`"},
		sqlguard.Table{Name: "outer_view", View: true, Columns: []string{"id", "code"},
			Definition: "select `shop`.`inner_view`.`id` AS `id`,`shop`.`inner_view`.`code` AS `code` from `shop`.`inner_view`"},
		sqlguard.Table{Name: "outer_secret", View: true, Columns: []string{"id"},
			Definition: "select `shop`.`leaf_secret`.`id` AS `id` from `shop`.`leaf_secret`"},
		sqlguard.Table{Name: "leaf_secret", View: true, Columns: []string{"id"},
			Definition: "select `shop`.`secret_keys`.`id` AS `id` from `shop`.`secret_keys`"},
	)
	// Two views deep, the column is still customers.ssn.
	rewritten(t, g, "SELECT code FROM outer_view",
		"SELECT '[hidden]' AS `code` FROM `outer_view`")
	// And two views deep, the table is still out of reach.
	refused(t, g, "SELECT id FROM outer_secret")
}

func TestReadDefinitionOnItsOwn(t *testing.T) {
	definition, ok := mysqlAnalyzer{}.ReadDefinition(
		"select `c`.`ssn` AS `code`,`o`.`total` AS `total` from (`shop`.`customers` `c` join `shop`.`orders` `o` on(`o`.`customer_id` = `c`.`id`))")
	if !ok {
		t.Fatal("that is a perfectly readable definition")
	}
	var names []string
	for _, r := range definition.Reads {
		names = append(names, r.Schema+"."+r.Name)
	}
	if got := strings.Join(names, ","); got != "shop.customers,shop.orders" {
		t.Fatalf("reads = %q", got)
	}
	if origin, ok := definition.Origin["code"]; !ok || origin.Table != "customers" || origin.Name != "ssn" {
		t.Fatalf("code should come from customers.ssn, got %+v", origin)
	}
	if origin, ok := definition.Origin["total"]; !ok || origin.Table != "orders" || origin.Name != "total" {
		t.Fatalf("total should come from orders.total, got %+v", origin)
	}
	if _, ok := (mysqlAnalyzer{}).ReadDefinition("not a statement"); ok {
		t.Fatal("that is not a definition")
	}
}
