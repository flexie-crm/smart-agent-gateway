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
	// BodyKeepsPolicy reads a routine's stored body and reports whether running
	// it would hand back anything the policy keeps back.
	//
	// It is the same reading the create path does, and deliberately the same
	// code: a body is judged by what its statements RETURN, not by which tables
	// they touch. Knowing only the tables is too blunt in both directions. It
	// refuses a routine that reads customers merely to filter on a hidden field,
	// and it refuses one whose body already says '[hidden]' because this tool
	// rewrote it when it was created.
	BodyKeepsPolicy(g *Guard, body string) (ok bool, reason string)
	// CountStatements says how many statements this text really is, and whether
	// the parser could read it at all.
	//
	// Counting semicolons cannot answer this and must not be asked to. A stored
	// routine's body is made of statements and is part of the ONE statement that
	// creates it, so a text scan refuses every procedure worth writing: measured,
	// an agent given this tool wrote its validation with RAISERROR and RETURN
	// because THROW needs a semicolon before it. Only something that has read the
	// grammar can tell a body from a statement somebody joined on the end.
	CountStatements(sql string) (int, bool)
}

// CountStatements asks a dialect how many statements a text really is. It
// reports false when this build has no reader for that dialect, or when the
// reader could not read the text, and the caller then falls back to counting
// separators, which is all there is.
func CountStatements(dialect, sql string) (int, bool) {
	a := analyzerFor(dialect)
	if a == nil {
		return 0, false
	}
	return a.CountStatements(sql)
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

	// mayCallRoutines is the administrator's own decision, ticked per tool. It
	// is here rather than in the caller because the caller can only see the
	// statement's leading word: EXEC and CALL announce themselves, and a stored
	// function called in the middle of a SELECT does not.
	mayCallRoutines bool

	// walk is where one decision about a routine has already been, and is nil on
	// the guard everything else holds. It has to live here because the walk does
	// not stay in this file: deciding a routine reads its body, reading a body is
	// the dialect's job, and the dialect asks back about every routine that body
	// calls. The guard is what travels across that seam, so the state travels on
	// a COPY of it, made when a walk starts. The shared guard is never written to
	// and two requests deciding at once each have their own.
	walk *routineWalk
}

// routineWalk is one decision about a routine as it follows the chain, in the
// three states such a walk has.
//
// onPath is grey: routines the walk is in the middle of deciding. Meeting one
// again is a loop, and the walk ends rather than running forever. It is a PATH
// and not a visited-set, so an entry comes off on the way back out; the two were
// one map to begin with and a routine two branches both called was reported as a
// loop, which refused a perfectly ordinary utility function.
//
// decided is black: routines this walk has settled, mapped to the reason they
// were refused, or "" for allowed. It is what keeps a diamond from being walked
// twice and a mesh from being walked exponentially. A verdict is a property of
// the routine and the policy, never of the route taken to it, so remembering one
// is sound: even a refusal for being in a loop, because a loop reachable from a
// routine is reachable from it however it was entered.
type routineWalk struct {
	onPath  map[string]bool
	decided map[string]string
	// route is the same routines as onPath, in the order the walk reached them,
	// so a refusal can name the cycle rather than just say there is one.
	route []string
	// loop is what recursion has to say, set once and then final. Every other
	// refusal is wrapped by the link above it ("A calls B, and B ..."), which is
	// how a chain explains itself. Recursion is not one link's problem, it is the
	// shape of the whole chain, so it is said ONCE and the wrapping is dropped:
	// measured before this, a function calling itself was reported as `"self"
	// runs a statement this tool does not allow. "self" is part of a loop`, whose
	// first half is untrue and whose second half is in the wrong voice.
	loop string
	// final is a sentence already composed for this walk, deeper down. A chain
	// explains itself once: without this, a routine whose body calls the one
	// that fails read "You cannot run X: it has a body that is not permitted:
	// you cannot run X: it calls Y ...", the same sentence inside itself.
	final string
}

// Option changes what a guard permits beyond its policy.
type Option func(*Guard)

// MayCallRoutines lets statements run the database's own stored code. Off, every
// routine is refused wherever it is called from; on, each one is still read and
// held to the policy like anything else.
func MayCallRoutines() Option { return func(g *Guard) { g.mayCallRoutines = true } }

// New builds the guard for a dialect. It fails when the dialect has no analyzer:
// a policy that cannot be enforced is not quietly ignored.
func New(dialect string, p Policy, c *Catalog, opts ...Option) (*Guard, error) {
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
	for _, opt := range opts {
		opt(g)
	}
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

// CallAllowed decides whether a stored routine may be run.
//
// A statement that calls one says nothing about what it does, so the only way to
// hold it to a policy is to read what it was written as. The body is parsed by
// the dialect's own reader, the same one that resolves a view, and every table
// it reads is put to the policy.
//
// The answer is allow or refuse, never mask. A query can be rewritten so a
// hidden field comes back as a stand-in; a stored body cannot, because it
// belongs to the database and to everybody else using it. So a routine that
// reads a hidden field is refused rather than quietly returning something else.
// That is the same rule this package already applies wherever there is no item
// to replace.
//
// Anything that cannot be accounted for is refused: a routine the snapshot has
// never heard of, a body this account may not read, one the parser cannot read
// in full, and one that reads a table which is itself out of reach.
func (g *Guard) CallAllowed(name string) (bool, string) {
	if !g.mayCallRoutines {
		return false, fmt.Sprintf("You are not permitted to run stored routines, so %q cannot be run.", name)
	}
	walking := g
	if walking.walk == nil {
		// The start of a walk. The state goes on a copy, so the guard this was
		// called on stays the read-only thing every request shares.
		fresh := *g
		fresh.walk = &routineWalk{onPath: map[string]bool{}, decided: map[string]string{}}
		walking = &fresh
	}
	return walking.callAllowed(name)
}

// callAllowed decides one routine, FOLLOWING the ones it calls.
//
// A routine that calls another used to be refused outright, on the reasoning
// that its real work happened somewhere this never saw. That was not true: every
// routine's body is in the same snapshot, so the one it calls can be read as
// easily as the one that was named. Refusing was the easy answer, not the right
// one, and it made a procedure built out of other procedures unusable, which is
// most of the procedures anybody writes.
//
// So the chain is followed to the end. Every link is read and held to the policy,
// and a refusal says WHICH link failed rather than that one did. A link that
// cannot be read, and a loop, stop the walk: what cannot be accounted for is
// refused, the same rule as everywhere else here.
func (g *Guard) callAllowed(name string) (bool, string) {
	key := strings.ToLower(name)
	if why, settled := g.walk.decided[key]; settled {
		return why == "", why
	}
	if g.walk.onPath[key] {
		g.walk.loop = recursion(g.walk.route, key)
		g.walk.final = g.walk.loop
		return false, g.walk.loop
	}
	g.walk.onPath[key] = true
	g.walk.route = append(g.walk.route, name)
	ok, why := g.decideRoutine(name)
	delete(g.walk.onPath, key)
	g.walk.route = g.walk.route[:len(g.walk.route)-1]
	if g.walk.loop != "" {
		// Recursion was met somewhere below. It is the whole answer, so it
		// replaces what the links in between would have said about each other.
		ok, why = false, g.walk.loop
	}
	g.walk.decided[key] = why
	return ok, why
}

// recursion says what a routine that comes back to itself is, and names the
// routines it goes round.
//
// It is deliberately about READING rather than about danger. Nothing here
// objects to recursion in a database, which is ordinary and often the right
// thing to write: this tool reads a routine's code before it runs it, so that
// every table and every field in it can be held to the policy, and a chain that
// returns to where it started has no end to read to. That is a limit of this
// tool and the message says so, rather than implying the database refused.
func recursion(route []string, back string) string {
	start := 0
	for i, name := range route {
		if strings.EqualFold(name, back) {
			start = i
			break
		}
	}
	cycle := route[start:]
	if len(cycle) == 1 {
		return fmt.Sprintf("%q calls itself. Recursive routines are not permitted.", back)
	}
	var quoted []string
	for _, name := range cycle {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return fmt.Sprintf("%s calls %q. Recursive routines are not permitted.",
		strings.Join(quoted, " calls "), back)
}

// chainRefusal is one sentence for the whole chain: what cannot be run, how the
// walk got from there to the problem, and what the problem is.
//
// The route is what makes it one sentence rather than one per link. Each link
// used to wrap the one below it, which read "A runs a statement this tool does
// not allow. B runs a statement this tool does not allow. C reads payroll":
// three clauses, two of them about this tool rather than about the database, to
// say one thing.
//
// cause is a verb phrase about the LAST routine on the route, so it reads on
// from "which" whatever the depth.
func (g *Guard) chainRefusal(cause string) string {
	if g.walk.final != "" {
		return g.walk.final
	}
	route := g.walk.route
	if len(route) == 0 {
		return cause
	}
	out := fmt.Sprintf("You cannot run %q: it ", route[0])
	for _, through := range route[1:] {
		out += fmt.Sprintf("calls %q, which ", through)
	}
	g.walk.final = out + cause + "."
	return g.walk.final
}

// decideRoutine is what one link of the chain is held to: it is read, every
// table it reads goes through the policy, what it hands back goes through the
// policy, and then the routines it calls are each put through the whole of this
// again.
func (g *Guard) decideRoutine(name string) (bool, string) {
	body, known := g.catalog.Routine(name)
	if !known {
		return false, g.chainRefusal("is not a routine available here")
	}
	if strings.TrimSpace(body) == "" {
		return false, g.chainRefusal("has no body available to read, and only routines with a " +
			"readable SQL body are permitted")
	}
	definition, ok := g.analyzer.ReadDefinition(body)
	if !ok {
		return false, g.chainRefusal("has a body that could not be read, and only routines with a " +
			"readable SQL body are permitted")
	}

	// Every table the body reads goes through the policy, as if the statement had
	// named it. This is the coarse half and it stays coarse on purpose: a table
	// nobody may reach is refused whether the body returns its columns or not,
	// and it catches shapes the statement walk below does not see, such as a
	// T-SQL function whose whole body is RETURN (SELECT ... FROM denied).
	for _, read := range definition.Reads {
		if allowed, _ := g.Reaches(read.Name); !allowed {
			return false, g.chainRefusal(fmt.Sprintf("reads %q, which you do not have access to", read.Name))
		}
	}
	// And the body itself, statement by statement, judged on what it RETURNS.
	// This is the fine half: which FIELDS come back. A stored body cannot be
	// rewritten at the moment it is called, so one that would have needed
	// rewriting is refused; one this tool already rewrote when it was created
	// needs none and runs.
	//
	// The guard handed over is THIS one, carrying the walk, because reading a
	// body is where the chain is discovered: the dialect meets the routines this
	// body calls and asks back about each of them.
	if keeps, why := g.analyzer.BodyKeepsPolicy(g, body); !keeps {
		return false, g.chainRefusal(why)
	}

	// And the routines this one calls by name rather than in an expression, which
	// is the shape the body reader hands back and the one the fine half does not
	// meet. What counts as a routine is IsRoutine's answer and not the
	// catalogue's own, so the rule here is the rule at the top level: a bare name
	// nobody has heard of is a built-in, because refusing every body that calls
	// lower() would refuse every body, and a QUALIFIED one is stored code
	// whatever the snapshot knows, because a schema the snapshot never read is
	// exactly where a routine hides.
	for _, called := range definition.Calls {
		if !g.IsRoutine(called) {
			continue
		}
		if ok, why := g.callAllowed(called); !ok {
			// The walk below composed the whole sentence, route and all.
			return false, why
		}
	}
	return true, ""
}

// BodyAllowed holds a routine's body to the policy: what it reads goes through
// the same rules as a table a statement named outright.
//
// It is shared by the two moments a body matters, and deliberately so. RUNNING a
// stored routine reads its body out of the catalogue; CREATING one has the body
// in the statement. Both are code this tool cannot see through afterwards, so
// both are decided here, by the same rules, with the same words.
//
// Creating one used to be refused outright, for no better reason than routine
// DDL not being on any analyzer's statement list. That made the checkbox a door
// with nothing behind it: an administrator turned on "create procedures" and
// every CREATE was still refused. Safe and useless is not what a policy is for.
//
// verb is what the caller is about to do with it, so the refusal says which.
func (g *Guard) BodyAllowed(name, verb string, definition Definition) (bool, string) {
	// A routine that runs another is read through to the end, the same walk that
	// decides one being CALLED. This used to refuse outright, which meant a
	// procedure could not be written out of the procedures already in the
	// database: the reason given was that its real work happened somewhere this
	// pass never saw, and that was never true, because the one it calls is in the
	// same snapshot.
	for _, called := range definition.Calls {
		if !g.IsRoutine(called) {
			// A built-in. Refusing every body that calls lower() would refuse
			// every body.
			continue
		}
		if ok, why := g.CallAllowed(called); !ok {
			return false, fmt.Sprintf("%q calls %q. %s", name, called, why)
		}
	}
	for _, read := range definition.Reads {
		if allowed, _ := g.Reaches(read.Name); !allowed {
			return false, fmt.Sprintf("%q reads %q, which you do not have access to.", name, read.Name)
		}
		columns, hasColumns := g.catalog.Columns(read.Name)
		if !hasColumns {
			return false, fmt.Sprintf("What %q reads cannot be established, so you cannot %s it.", name, verb)
		}
		// A routine cannot be rewritten, so a hidden field in what it reads is a
		// refusal rather than a stand-in.
		if hidden := g.hiddenColumnsOf(read.Name, columns); len(hidden) > 0 {
			return false, fmt.Sprintf("%q reads the hidden %s %s. A value inside a routine cannot be "+
				"hidden, so you cannot %s one that reads %s.",
				name, plural(hidden, "field", "fields"), strings.Join(hidden, " and "),
				verb, pronoun(hidden))
		}
	}
	return true, ""
}

// hiddenColumnsOf names the columns of a table that this tool keeps back, so a
// refusal can say WHICH field is the problem rather than that one exists.
func (g *Guard) hiddenColumnsOf(table string, columns []string) []string {
	var out []string
	for _, column := range columns {
		if g.MasksColumn(table, column) {
			out = append(out, table+"."+column)
		}
	}
	return out
}

// IsRoutine reports whether a name the statement used is the database's own
// stored code rather than something the engine ships with.
//
// It is the question an analyzer has to ask before it can do anything about a
// function call, because in every dialect a call to a stored function is
// indistinguishable in the tree from a call to lower() or now(). Refusing every
// function call would refuse every statement worth running; this is what
// separates the two, and the catalog is the only thing that can.
func (g *Guard) IsRoutine(name string) bool {
	if strings.Contains(name, ".") {
		// A function call somebody QUALIFIED is the database's own stored code,
		// whether or not this snapshot has heard of it. A built-in is written
		// bare, and a schema the snapshot never read is exactly where a routine
		// hides: measured on a live server, hidden_schema.f_out() read a denied
		// table because an unknown name was taken for a built-in and waved past.
		// CallAllowed refuses the ones it cannot account for.
		return true
	}
	_, ok := g.catalog.Routine(name)
	return ok
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
			return false, fmt.Sprintf("What %q reads cannot be established, so you cannot read it.", name)
		}
		// A name the snapshot has not seen, most likely a table made since it was
		// taken. The policy's own reading of a name it was never told about is the
		// answer: an allowlist has not permitted it, a denylist has not refused it.
		if !g.policy.AllowsTable(name) {
			return false, fmt.Sprintf("You do not have access to %q.", name)
		}
		return true, ""
	}
	for _, source := range sources {
		if !g.policy.AllowsTable(source) {
			return false, fmt.Sprintf("You do not have access to %q.", name)
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

// plural picks the word that agrees with how many there are, so a message about
// one field does not say "fields".
func plural(items []string, one, many string) string {
	if len(items) == 1 {
		return one
	}
	return many
}

// pronoun is what to call those fields the second time they come up, so the
// sentence does not list them twice.
func pronoun(items []string) string {
	if len(items) == 1 {
		return "it"
	}
	return "them"
}
