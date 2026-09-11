package sqlguard

import (
	"strings"
	"testing"
)

func testCatalog() *Catalog {
	return NewCatalog("mysql", "shop", []Table{
		{Name: "customers", Columns: []string{"id", "email", "ssn", "profile"}},
		{Name: "orders", Columns: []string{"id", "customer_id", "total"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
		{Name: "customer_view", View: true, Reads: []string{"customers"}, Columns: []string{"id", "email", "ssn"}},
		{Name: "leaky_view", View: true, Reads: []string{"secret_keys"}, Columns: []string{"id", "value"}},
		{Name: "nested_view", View: true, Reads: []string{"leaky_view", "orders"}, Columns: []string{"id"}},
		{Name: "unreadable_view", View: true, Opaque: true, Columns: []string{"id"}},
	})
}

func TestCatalogResolvesWithoutRegardToCase(t *testing.T) {
	c := testCatalog()
	for _, name := range []string{"customers", "CUSTOMERS", "Customers", "  customers  "} {
		if _, ok := c.Lookup(name); !ok {
			t.Errorf("%q must resolve to the customers table", name)
		}
	}
	if _, ok := c.Lookup("nosuch"); ok {
		t.Error("a table the database does not have must not resolve")
	}
	if _, ok := c.Lookup(""); ok {
		t.Error("an empty name must not resolve")
	}
}

func TestCatalogRefusesToGuessBetweenTwoTablesOfTheSameName(t *testing.T) {
	// Only a server that regards case can hold both, and resolving to either one
	// would be resolving to the wrong one half the time.
	c := NewCatalog("mysql", "shop", []Table{
		{Name: "Orders", Columns: []string{"id"}},
		{Name: "orders", Columns: []string{"id"}},
	})
	if _, ok := c.Lookup("orders"); ok {
		t.Fatal("a name two tables answer to must not resolve")
	}
	if _, ok := c.Sources("ORDERS"); ok {
		t.Fatal("and it must not resolve to sources either")
	}
}

func TestViewResolvesToTheTablesUnderneathIt(t *testing.T) {
	c := testCatalog()
	cases := []struct {
		name  string
		want  []string
		known bool
	}{
		{"customers", []string{"customers"}, true},
		{"customer_view", []string{"customers"}, true},
		{"leaky_view", []string{"secret_keys"}, true},
		{"nested_view", []string{"secret_keys", "orders"}, true},
		{"unreadable_view", nil, false},
		{"nosuch", nil, false},
	}
	for _, c2 := range cases {
		got, ok := c.Sources(c2.name)
		if ok != c2.known {
			t.Errorf("%s: known = %v, want %v", c2.name, ok, c2.known)
			continue
		}
		if strings.Join(got, ",") != strings.Join(c2.want, ",") {
			t.Errorf("%s: sources = %v, want %v", c2.name, got, c2.want)
		}
	}
}

func TestAViewOverADeniedTableIsDenied(t *testing.T) {
	// The whole point of resolving a view: the statement never names the denied
	// table, and it must be refused anyway.
	c := testCatalog()
	p := Policy{TableMode: ModeDenylist, Tables: "secret_keys"}
	for _, name := range []string{"leaky_view", "nested_view"} {
		sources, ok := c.Sources(name)
		if !ok {
			t.Fatalf("%s should resolve", name)
		}
		denied := false
		for _, s := range sources {
			if !p.AllowsTable(s) {
				denied = true
			}
		}
		if !denied {
			t.Errorf("%s reads a denied table and must be refused", name)
		}
	}
}

func TestCatalogColumns(t *testing.T) {
	c := testCatalog()
	cols, ok := c.Columns("CUSTOMERS")
	if !ok || strings.Join(cols, ",") != "id,email,ssn,profile" {
		t.Fatalf("columns = %v, %v", cols, ok)
	}
	if _, ok := c.Columns("nosuch"); ok {
		t.Error("an unknown table has no columns to expand a * into")
	}
	if !c.HasColumn("customers", "SSN") {
		t.Error("a column must be found without regard to case")
	}
	if c.HasColumn("orders", "ssn") {
		t.Error("orders has no ssn")
	}
}

func TestCatalogNames(t *testing.T) {
	c := testCatalog()
	got := strings.Join(c.Names(), ",")
	want := "customers,orders,secret_keys,customer_view,leaky_view,nested_view,unreadable_view"
	if got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	if c.Database() != "shop" {
		t.Fatalf("database = %q", c.Database())
	}
}
