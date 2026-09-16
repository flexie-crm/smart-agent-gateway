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
	// bareRoutines is how many routines answer to each unqualified name.
	bareRoutines map[string]int
	// dialect is kept so a routine body can be read after the routines arrive,
	// which is later than the views are resolved.
	dialect  string
	database string
	byName   map[string]*Table
	// ambiguous holds names that two tables answer to when case is ignored. It can
	// only happen on a server that regards case, and such a name is resolved to
	// neither table rather than to the wrong one.
	ambiguous map[string]bool
	order     []string
	// routines are the stored procedures and functions, by lower-cased name,
	// with the body of each. A name absent here is a routine nothing can account
	// for, and the guard refuses one of those: an empty catalogue therefore
	// refuses every call, which is the right answer when nothing is known.
	routines map[string]string
}

// WithQualifiedRoutines adds the stored routines to a snapshot and returns it,
// keyed by the schema a routine lives in as well as by its name.
//
// Separate from NewCatalog so that a caller which does not care about routines
// is untouched, and so that not calling it means every routine is unaccounted
// for rather than every routine being allowed.
//
// A routine's identity is its schema AND its name, and for a while this held
// only the name. Measured on a live SQL Server: with dbo.f_same returning a
// constant and app.f_same reading a denied table, the map kept one of the two
// bodies and SELECT app.f_same() ran the other one. Which body survived was
// decided by the order the database happened to return its rows in.
func (c *Catalog) WithQualifiedRoutines(routines []QualifiedRoutine) *Catalog {
	c.routines = map[string]string{}
	for _, r := range routines {
		name := strings.ToLower(r.Name)
		if r.Namespace != "" {
			c.routines[strings.ToLower(r.Namespace)+"."+name] = r.Body
		}
		c.routines[name] = r.Body
	}
	c.countBareNames()
	c.resolveRoutineCalls()
	return c
}

// QualifiedRoutine is one stored routine as the database reports it.
type QualifiedRoutine struct {
	Namespace string
	Name      string
	Body      string
}

// countBareNames records how many routines answer to each unqualified name, so
// one that two schemas both offer can be refused rather than guessed at.
func (c *Catalog) countBareNames() {
	c.bareRoutines = map[string]int{}
	for key := range c.routines {
		if dot := strings.IndexByte(key, '.'); dot >= 0 {
			c.bareRoutines[key[dot+1:]]++
		}
	}
}

// resolveRoutineCalls folds what a called routine READS into the view that calls
// it.
//
// A view's column can be a stored function call, and then the tables in the
// view's own FROM are not the whole story. Measured on all three engines before
// this existed, with a function returning a column from a denied table:
//
//	CREATE VIEW leak_view AS SELECT o.id AS id, f_leak() AS code FROM orders o
//	SELECT code FROM leak_view   ->   the-key
//
// The view was credited with reading orders, which nobody keeps back, so the
// policy had nothing to object to and the statement named no function for the
// routine gate to see. Nothing in the statement was wrong; the snapshot was.
//
// It runs here rather than in resolve because resolve happens while the catalog
// is being built and the routines arrive afterwards. A routine whose body cannot
// be read, and one that calls another routine, leave the view unaccounted for
// rather than half accounted for.
func (c *Catalog) resolveRoutineCalls() {
	analyzer := analyzerFor(c.dialect)
	for _, key := range c.order {
		t := c.byName[key]
		if !t.View || t.Opaque || len(t.calls) == 0 {
			continue
		}
		for _, called := range t.calls {
			reads, ok := c.routineReads(analyzer, called, map[string]bool{})
			if !ok {
				t.Opaque = true
				break
			}
			t.Reads = append(t.Reads, reads...)
		}
	}
}

// routineReads is every table a routine reads, INCLUDING through the routines it
// calls, and whether the whole of that could be read.
//
// The chain used to stop at the first link, which made a view over a function
// built out of other functions opaque, and an opaque view is refused. Every body
// is in this same snapshot, so the walk goes to the end. onPath is the walk's own
// route, so two functions calling each other end it rather than running forever.
func (c *Catalog) routineReads(analyzer Analyzer, name string, onPath map[string]bool) ([]string, bool) {
	body, isRoutine := c.Routine(name)
	if !isRoutine {
		// A built-in. Refusing every view that calls lower() would refuse most
		// views worth having.
		return nil, true
	}
	key := strings.ToLower(name)
	if onPath[key] {
		return nil, false
	}
	onPath[key] = true
	defer delete(onPath, key)

	if analyzer == nil || strings.TrimSpace(body) == "" {
		return nil, false
	}
	definition, ok := analyzer.ReadDefinition(body)
	if !ok {
		return nil, false
	}
	reads, ok := c.resolveReads(definition.Reads)
	if !ok {
		return nil, false
	}
	for _, nested := range definition.Calls {
		deeper, ok := c.routineReads(analyzer, nested, onPath)
		if !ok {
			return nil, false
		}
		reads = append(reads, deeper...)
	}
	return reads, true
}

// Routine returns a stored routine's body, and whether the snapshot has one at
// all. A routine present with an empty body is one whose text this account may
// not read, which is not the same as absent and is refused just as firmly.
func (c *Catalog) Routine(name string) (string, bool) {
	key := strings.ToLower(name)
	if dot := strings.IndexByte(key, '.'); dot >= 0 {
		// Qualified: only an exact match will do. A schema this snapshot never
		// read is a routine this tool cannot account for, not a built-in.
		body, ok := c.routines[key]
		return body, ok
	}
	if c.bareRoutines[key] > 1 {
		// Two schemas offer this name and the statement did not say which. Known,
		// so it is decided about rather than waved through as a built-in, and
		// bodyless, so it is refused as one whose text cannot be read.
		return "", true
	}
	body, ok := c.routines[key]
	return body, ok
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
	// calls is every function a view's definition invokes, kept from resolve so
	// that the pass which runs once the routines are known can fold in what a
	// called routine reads. Nothing outside this package sets it.
	calls []string
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
		dialect:   dialect,
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
	// Calls are the routines this definition invokes by name. It is filled where
	// a dialect can name them but cannot tell a stored routine from a built-in:
	// the guard holds the catalogue and can. A dialect that refuses a nested call
	// outright leaves this empty.
	Calls []string
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
		t.Reads, t.Origin, t.calls = reads, definition.Origin, definition.Calls
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
