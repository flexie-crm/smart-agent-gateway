package sqlguard

import "strings"

// Catalog is what the database actually holds: its tables, its views, and the
// columns of each. The guard needs it for three things a statement alone cannot
// answer: which columns a * stands for, which table an unqualified column came
// from, and which real tables a view reads.
//
// It is a snapshot, read from the database and reused for a while. A snapshot
// can only be stale in one direction that matters: a table created after it was
// taken is unknown, and an unknown table is refused by an allowlist and permitted
// by a denylist, which is what each of those lists already means. A table RENAMED
// out from under a policy would be a real hole, which is why a governed tool
// refuses to run the statements that could rename one.
type Catalog struct {
	database string
	byName   map[string]*Table
	// ambiguous holds names that two tables answer to when case is ignored. It can
	// only happen on a server that regards case, and such a name is resolved to
	// neither table rather than to the wrong one.
	ambiguous map[string]bool
	order     []string
}

// Table is one table or view, as the database reports it.
type Table struct {
	// Name is the database's own spelling, kept for the messages and lists we
	// send back to it.
	Name string
	// Namespace is the schema this table lives in, on an engine that has them.
	// It is what makes a qualified name decidable: without it, other.users and
	// public.users are the same two words.
	Namespace string
	View      bool
	// Columns are in the order the database returns them, which is the order a *
	// stands for.
	Columns []string
	// Reads are the tables a view's definition names directly. It is empty for a
	// real table. Opaque says a view's definition could not be read, and such a
	// view is refused rather than assumed to be harmless.
	Reads  []string
	Opaque bool
	// Definition is a view's defining statement as the database reports it. When
	// it is set, what the view reads and where its columns come from are read out
	// of it rather than taken on trust.
	Definition string
	// Origin maps a view's own column names to the table column each one reads.
	// It is how a rule written about a table's column still holds when the column
	// is reached through a view that calls it something else. A view column built
	// out of an expression rather than read from one place has no entry here, and
	// is treated as reading everything the view reads.
	Origin map[string]Column
}

// Column names one table's column.
type Column struct {
	Table string
	Name  string
}

// NewCatalog builds a snapshot and reads every view definition in it, so the
// guard can decide a view by what it would actually read rather than by its
// name. A view nothing accounts for is left unaccounted for, and refused.
func NewCatalog(dialect, database string, tables []Table) *Catalog {
	c := &Catalog{
		database:  database,
		byName:    make(map[string]*Table, len(tables)),
		ambiguous: map[string]bool{},
	}
	for i := range tables {
		t := tables[i]
		key := strings.ToLower(t.Name)
		if _, clash := c.byName[key]; clash {
			c.ambiguous[key] = true
			continue
		}
		c.byName[key] = &t
		c.order = append(c.order, key)
	}
	// Definitions are read once the whole snapshot is indexed, because a view is
	// resolved against the tables beside it.
	c.resolve(dialect)
	return c
}

// Database is the database this snapshot was taken from. A statement that names
// any other one is refused: one tool is one database.
func (c *Catalog) Database() string { return c.database }

// Lookup finds a table by the name a statement used. Names are matched without
// regard to case, because the server may or may not regard it and the safe
// reading is that it does not. A name two tables answer to is not resolved.
func (c *Catalog) Lookup(name string) (*Table, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || c.ambiguous[key] {
		return nil, false
	}
	t, ok := c.byName[key]
	return t, ok
}

// Sources returns every real table a name ultimately reads: itself, or, for a
// view, the tables underneath it. It reports false when the answer cannot be
// known: an unknown name, or a view whose definition could not be read.
func (c *Catalog) Sources(name string) ([]string, bool) {
	t, ok := c.Lookup(name)
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	var walk func(t *Table) bool
	walk = func(t *Table) bool {
		key := strings.ToLower(t.Name)
		if seen[key] {
			return true
		}
		seen[key] = true
		if !t.View {
			out = append(out, t.Name)
			return true
		}
		if t.Opaque {
			return false
		}
		for _, read := range t.Reads {
			next, ok := c.Lookup(read)
			if !ok {
				// A view reading something this snapshot does not have is a view
				// whose reach cannot be established.
				return false
			}
			if !walk(next) {
				return false
			}
		}
		return true
	}
	if !walk(t) {
		return nil, false
	}
	return out, true
}

// HasColumn reports whether a table has a column of this name, which is how an
// unqualified column in a join is traced back to the table it came from.
func (c *Catalog) HasColumn(table, column string) bool {
	t, ok := c.Lookup(table)
	if !ok {
		return false
	}
	for _, have := range t.Columns {
		if strings.EqualFold(have, column) {
			return true
		}
	}
	return false
}

// Columns returns a table's columns in the order a * stands for them.
func (c *Catalog) Columns(table string) ([]string, bool) {
	t, ok := c.Lookup(table)
	if !ok || len(t.Columns) == 0 {
		return nil, false
	}
	return t.Columns, true
}

// Knows reports whether a schema is one this snapshot covers. A statement
// naming any other one is naming something nothing here has looked at.
func (c *Catalog) Knows(namespace string) bool {
	for _, key := range c.order {
		if strings.EqualFold(c.byName[key].Namespace, namespace) {
			return true
		}
	}
	return false
}

// Names returns every table in the snapshot, in the order it was read. It is how
// a policy written in patterns becomes the concrete list of names the database
// is asked about when a catalog query is narrowed.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.order))
	for _, key := range c.order {
		out = append(out, c.byName[key].Name)
	}
	return out
}

// Definition is what a view's defining statement says: the tables it reads, and
// where each of the columns it offers comes from.
type Definition struct {
	Reads  []Reference
	Origin map[string]Column
}

// Reference is a table a definition names, as it was written: the name, and the
// database in front of it when there was one.
type Reference struct {
	Schema string
	Name   string
}

// resolve fills in what a view reads, by reading its defining statement with the
// dialect's own analyzer. A view whose definition cannot be read, or that reads
// something this snapshot cannot account for (a table it cannot see, a database
// this tool does not reach), is left unaccounted for: the guard refuses one of
// those rather than assuming a view nobody could vouch for is harmless.
func (c *Catalog) resolve(dialect string) {
	analyzer := analyzerFor(dialect)
	for _, key := range c.order {
		t := c.byName[key]
		if !t.View || t.Opaque {
			continue
		}
		if t.Definition == "" {
			// Nothing says what it reads. That is either a view whose definition
			// this account may not see, or one somebody described in full when they
			// built the snapshot by hand.
			if len(t.Reads) == 0 {
				t.Opaque = true
			}
			continue
		}
		if analyzer == nil {
			t.Opaque = true
			continue
		}
		definition, ok := analyzer.ReadDefinition(t.Definition)
		if !ok {
			t.Opaque = true
			continue
		}
		reads, ok := c.resolveReads(definition.Reads)
		if !ok {
			t.Opaque = true
			continue
		}
		t.Reads, t.Origin = reads, definition.Origin
	}
}

// resolveReads turns the names a definition used into the database's own names.
// It reports false as soon as one of them cannot be accounted for.
func (c *Catalog) resolveReads(refs []Reference) ([]string, bool) {
	var out []string
	for _, ref := range refs {
		if ref.Schema != "" && !strings.EqualFold(ref.Schema, c.database) && !c.Knows(ref.Schema) {
			// A view reaching somewhere this snapshot never looked reaches past
			// everything this tool's policy can say anything about. What counts as
			// "somewhere else" differs by engine: on MySQL the qualifier in front of
			// a name is a database, and on an engine with schemas inside a database
			// it is one of those, so both are accepted and anything else is not.
			return nil, false
		}
		t, known := c.Lookup(ref.Name)
		if !known {
			return nil, false
		}
		out = append(out, t.Name)
	}
	return out, true
}
