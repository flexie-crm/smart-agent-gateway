package sqlguard

import (
	"fmt"
	"strings"
	"sync"
)

// Decision is what the guard made of one statement.
type Decision struct {
	// SQL is the statement to run. It is the text exactly as it was written
	// unless something had to change: a statement rebuilt from its parts comes
	// back tidied rather than as it was typed (COUNT(*) is rebuilt as COUNT(1)),
	// and there is no reason to send somebody else's database anything other than
	// what was asked for when nothing needed hiding.
	SQL string
	// Rewritten says the statement was changed, so a caller that wants to show a
	// person what actually ran knows the two differ.
	Rewritten bool
	// SawUnknownTable says the statement named a table this snapshot has never
	// heard of. A snapshot is minutes old, and a view made since it was taken
	// would otherwise be read as a table nobody has kept back, whatever it reads
	// underneath. A caller that can take a fresh snapshot should take one and ask
	// again.
	SawUnknownTable bool
	// ListsTables says the result is a list of table names, and the rows must be
	// filtered to the ones this tool may see before they are returned. A SHOW
	// cannot be narrowed the way a catalog query can, so it is narrowed after it
	// has run.
	ListsTables bool
}

// Analyzer reads one dialect of SQL. A dialect the build does not ship refuses
// to guard anything at all, rather than letting statements past unread.
type Analyzer interface {
	// Check reads one statement and decides what may run.
	Check(g *Guard, sql string) (Decision, string, error)
	// ReadDefinition reads a view's defining statement into the tables it reads
	// and where each of its columns comes from. It reports false when the
	// statement cannot be read in full, which leaves the view unaccounted for.
	ReadDefinition(definition string) (Definition, bool)
}

var (
	mu        sync.RWMutex
	analyzers = map[string]Analyzer{}
)

// Register adds a dialect's analyzer. Analyzer files call it from init(), the
// same way a datasource driver registers itself.
func Register(dialect string, a Analyzer) {
	mu.Lock()
	defer mu.Unlock()
	analyzers[dialect] = a
}

// Supports reports whether a dialect can be governed, so a form can refuse a
// policy it would not be able to enforce instead of accepting one that does
// nothing.
func Supports(dialect string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := analyzers[dialect]
	return ok
}

// analyzerFor returns a dialect's analyzer, or nil when this build has none.
func analyzerFor(dialect string) Analyzer {
	mu.RLock()
	defer mu.RUnlock()
	return analyzers[dialect]
}

// Guard enforces one policy against one database.
type Guard struct {
	policy   Policy
	catalog  *Catalog
	analyzer Analyzer
	// hideable is every column name that could come back hidden, under any name
	// it can be read by. A rule names customers.ssn, and a view over customers is
	// free to offer that column as "code": the rule's own wording is then not
	// enough to know that a column called code needs looking into. Worked out
	// once, when the snapshot is taken, rather than per statement.
	hideable map[string]bool
}

// New builds the guard for a dialect. It fails when the dialect has no analyzer:
// a policy that cannot be enforced is not quietly ignored.
func New(dialect string, p Policy, c *Catalog) (*Guard, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	mu.RLock()
	a, ok := analyzers[dialect]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("a table and column policy cannot be enforced on a %s database in this build", dialect)
	}
	if c == nil {
		return nil, fmt.Errorf("the database's tables could not be read, so its policy cannot be enforced")
	}
	g := &Guard{policy: p, catalog: c, analyzer: a, hideable: map[string]bool{}}
	for _, table := range c.Names() {
		columns, ok := c.Columns(table)
		if !ok {
			continue
		}
		for _, column := range columns {
			if g.MasksColumn(table, column) {
				g.hideable[strings.ToLower(column)] = true
			}
		}
	}
	return g, nil
}

// CouldHide reports whether a column of this name could come back hidden,
// whatever table it turns out to belong to. It is the cheap first half of
// resolving a column: a name nothing could reach needs no resolving at all.
func (g *Guard) CouldHide(column string) bool {
	return g.policy.CouldHideColumn(column) || g.hideable[strings.ToLower(column)]
}

// Check reads a statement and decides what may run. A refused statement comes
// back as a reason written for the assistant to report rather than work around;
// an error is this side failing, not the statement being wrong.
func (g *Guard) Check(sql string) (Decision, string, error) {
	return g.analyzer.Check(g, sql)
}

// Policy and Catalog are what the analyzer decides with.
func (g *Guard) Policy() Policy    { return g.policy }
func (g *Guard) Catalog() *Catalog { return g.catalog }

// Reaches reports whether a name a statement used may be reached, and why not
// when it may not. A view is decided by the tables underneath it, so a view over
// a table this tool may not see is a table this tool may not see. A name that
// cannot be resolved at all is refused: the guard permits what it recognises.
func (g *Guard) Reaches(name string) (bool, string) {
	sources, ok := g.catalog.Sources(name)
	if !ok {
		if _, known := g.catalog.Lookup(name); known {
			return false, fmt.Sprintf("this tool cannot establish what %q reads, so it will not read it", name)
		}
		// A name the snapshot has not seen, most likely a table made since it was
		// taken. The policy's own reading of a name it was never told about is the
		// answer: an allowlist has not permitted it, a denylist has not refused it.
		if !g.policy.AllowsTable(name) {
			return false, fmt.Sprintf("this tool has no access to %q", name)
		}
		return true, ""
	}
	for _, source := range sources {
		if !g.policy.AllowsTable(source) {
			return false, fmt.Sprintf("this tool has no access to %q", name)
		}
	}
	return true, ""
}

// MasksColumn reports whether a column comes back hidden, following a view's
// column back to the table it reads.
//
// A view is the reason this is not simply the policy's own answer. A rule about
// customers.ssn has to hold when the column is read through a view over
// customers, whatever the view calls it; and a view column built out of an
// expression cannot be followed anywhere, so it is hidden whenever anything the
// view reads has something hidden in it. Hiding a column that did not need it is
// a report with a gap in it; the other way round is the value on the screen.
func (g *Guard) MasksColumn(table, column string) bool {
	return g.masksColumn(table, column, 0)
}

func (g *Guard) masksColumn(table, column string, depth int) bool {
	// A view over a view over a view has to end somewhere, and a snapshot that
	// somehow described a loop must not spin here.
	if depth > 16 {
		return true
	}
	t, known := g.catalog.Lookup(table)
	if !known || !t.View {
		return g.policy.MasksColumn(table, column)
	}
	if origin, ok := t.Origin[strings.ToLower(column)]; ok {
		return g.masksColumn(origin.Table, origin.Name, depth+1)
	}
	// Nothing says where this column came from, so it came from anywhere the
	// view reads.
	if g.policy.MasksColumn(table, column) {
		return true
	}
	for _, read := range t.Reads {
		source, ok := g.catalog.Lookup(read)
		if !ok {
			return true
		}
		for _, c := range source.Columns {
			if g.masksColumn(read, c, depth+1) {
				return true
			}
		}
	}
	return false
}

// MasksAnyColumn reports whether a table has a column that comes back hidden, so
// a caller knows whether expanding a * over it would change anything.
func (g *Guard) MasksAnyColumn(table string, columns []string) bool {
	for _, c := range columns {
		if g.MasksColumn(table, c) {
			return true
		}
	}
	return false
}

// Shows reports whether a table's name may appear in a list of tables. An entry
// whose reach cannot be established is left out: a list is the one place where
// saying nothing is always safe.
func (g *Guard) Shows(name string) bool {
	if _, known := g.catalog.Lookup(name); !known {
		// Something the snapshot has not seen. The policy's own reading of a name
		// it does not know is the answer: an allowlist has not named it, a
		// denylist has not denied it.
		return g.policy.AllowsTable(name)
	}
	ok, _ := g.Reaches(name)
	return ok
}

// VisibleTables is every table this tool may see, in the database's own
// spelling. It is how a policy written in patterns becomes the concrete list of
// names a catalog query is narrowed to.
func (g *Guard) VisibleTables() []string {
	var out []string
	for _, name := range g.catalog.Names() {
		if ok, _ := g.Reaches(name); ok {
			out = append(out, name)
		}
	}
	return out
}

// HiddenTables is every table this tool may not see. A catalog query is narrowed
// with whichever of the two lists is shorter, so a database of four hundred
// tables with one secret in it is not asked about four hundred names.
func (g *Guard) HiddenTables() []string {
	var out []string
	for _, name := range g.catalog.Names() {
		if ok, _ := g.Reaches(name); !ok {
			out = append(out, name)
		}
	}
	return out
}

// Unmatched names the policy entries that match nothing the database has.
//
// A mistyped name is the one mistake in a policy that does not announce itself.
// Under a denylist it silently keeps nothing back, which is the failure worth
// catching: the administrator believes a table is out of reach, and it is not.
// So it is reported where a policy is written rather than left to be discovered
// by whoever reads the answer it should not have given. A pattern that matches
// nothing today is left alone, because it is written for what the database will
// hold later as much as for what it holds now.
func (g *Guard) Unmatched() []string {
	var out []string
	for _, entry := range g.policy.tableEntries() {
		if strings.Contains(entry, "*") {
			continue
		}
		if _, known := g.catalog.Lookup(entry); !known {
			out = append(out, entry)
		}
	}
	for _, entry := range g.policy.fieldEntries() {
		if entry.err != "" || strings.Contains(entry.table, "*") {
			continue
		}
		table, known := g.catalog.Lookup(entry.table)
		if !known {
			out = append(out, entry.table+"."+entry.column)
			continue
		}
		if strings.Contains(entry.column, "*") {
			continue
		}
		if !g.catalog.HasColumn(table.Name, entry.column) {
			out = append(out, entry.table+"."+entry.column)
		}
	}
	return out
}
