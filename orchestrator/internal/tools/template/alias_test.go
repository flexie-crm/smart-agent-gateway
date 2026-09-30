package template

import "testing"

// One name on the form, and this is what the second one used to be.
func TestTheAliasIsDerivedFromTheNameSomebodyTyped(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Production orders", "production_orders"},
		{"Invoice API", "invoice_api"},
		{"  Spaced  out  ", "spaced_out"},
		{"Orders (EU)", "orders_eu"},
		{"CSV / TSV reports", "csv_tsv_reports"},
		{"already_fine", "already_fine"},
		{"Ledger v2", "ledger_v2"},
		// A leading digit cannot start an identifier, and forcing one in
		// (prefixing a letter, say) would invent a name nobody typed.
		{"2024 invoices", "invoices"},
		{"99", ""},
		// Nothing usable at all, which the caller reports rather than guessing.
		{"", ""},
		{"!!!", ""},
		{"   ", ""},
	} {
		if got := AliasFrom(tc.name); got != tc.want {
			t.Errorf("AliasFrom(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Whatever it produces has to be something the templates accept, because that
// is the whole promise: the person typed a name and never sees the rule.
func TestWhatIsDerivedIsAlwaysUsable(t *testing.T) {
	for _, name := range []string{
		"Production orders", "Invoice API", "A", "Orders (EU) // live",
		"a very long name that goes on and on and really does keep going past the limit",
		"Ledger v2", "2024 invoices",
	} {
		alias := AliasFrom(name)
		if alias == "" {
			t.Errorf("AliasFrom(%q) produced nothing", name)
			continue
		}
		if len(alias) > 49 {
			t.Errorf("AliasFrom(%q) = %q, which is %d long", name, alias, len(alias))
		}
		if c := alias[0]; c < 'a' || c > 'z' {
			t.Errorf("AliasFrom(%q) = %q, which does not start with a letter", name, alias)
		}
		for i := 0; i < len(alias); i++ {
			c := alias[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
			if !ok {
				t.Errorf("AliasFrom(%q) = %q, which carries %q", name, alias, string(c))
			}
		}
		if alias[len(alias)-1] == '_' {
			t.Errorf("AliasFrom(%q) = %q, which trails an underscore", name, alias)
		}
	}
}
