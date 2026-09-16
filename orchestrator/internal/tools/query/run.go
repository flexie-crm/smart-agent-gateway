package query

import (
	"context"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Row limits. A query tool returns rows into a model's context, so an unbounded
// result would be both a cost and a correctness problem (a truncated answer read
// as complete). The default caps a careless SELECT; a call may lower it but not
// raise it past the hard ceiling.
const (
	defaultMaxRows = 200
	hardMaxRows    = 2000
)

// queryTimeout is the hard ceiling on a single read. Past it the statement is
// cancelled on the server (so a heavy query cannot keep locking the database)
// and its EXPLAIN plan is returned instead of rows. It is a var so a test can
// lower it; it is never configurable by an administrator.
var queryTimeout = 20 * time.Second

// run executes one statement against the connection, through the two gates it
// has to pass. The access mode comes first and costs nothing: a write asked of a
// read-only tool is refused before anything is opened. The policy comes second,
// because deciding what a statement may touch means knowing what the database
// holds, and that is a question for the database.
//
// A refused statement comes back as a reason (a bad-arguments outcome the caller
// reports to the model); a real failure comes back as an error. Nothing runs
// while either gate is unsure: a policy that cannot be read is a policy that
// cannot be enforced, and that stops the statement rather than waving it on.
func run(ctx context.Context, settings Settings, statement string, args []any, maxRows int) (*datasource.Result, string, error) {
	if ok, reason := permitted(settings, statement); !ok {
		return nil, reason, nil
	}
	if maxRows <= 0 || maxRows > hardMaxRows {
		maxRows = defaultMaxRows
	}

	conn, err := datasource.Open(settings.Connection)
	if err != nil {
		return nil, "", fmt.Errorf("open connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		return nil, "", fmt.Errorf("connect: %w", err)
	}

	// Without a policy nothing is read and nothing is rewritten: the tool behaves
	// exactly as it did before there was one.
	decision := sqlguard.Decision{SQL: statement}
	var guard *sqlguard.Guard
	if settings.Policy.Active() {
		if guard, err = guards.get(ctx, settings, conn); err != nil {
			return nil, "", err
		}
		checked, reason, err := guard.Check(statement)
		if err != nil {
			return nil, "", err
		}
		// A table this snapshot has never heard of is a reason to take a fresh
		// one, not to decide from an old one. A view made since the snapshot was
		// taken would otherwise be read as a table nobody kept back, whatever it
		// reads underneath, for as long as the snapshot was still being trusted.
		if checked.SawUnknownTable {
			if guard, err = guards.refresh(ctx, settings, conn); err != nil {
				return nil, "", err
			}
			if checked, reason, err = guard.Check(statement); err != nil {
				return nil, "", err
			}
		}
		if reason != "" {
			return nil, reason, nil
		}
		decision = checked
	}

	// Which way a statement goes is about whether it can RETURN ROWS, not about
	// whether it changes anything. A routine call is the case that separates the
	// two: it is treated as a write for permission, because nothing can tell
	// from the verb what its body does, and it is run as a query, because
	// returning a result set is usually the whole point of calling it. Sent
	// through Exec it came back with a row count and no rows, which is a
	// procedure call that answers nothing.
	if isReadOnly(statement) || callsRoutine(settings, statement) {
		// A read is held to a hard deadline: past it the driver kills it on the
		// server and returns its plan, so a heavy query cannot lock the database.
		readCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		res, err := conn.Query(readCtx, decision.SQL, args, maxRows)
		if err != nil {
			return nil, "", err
		}
		if decision.ListsTables {
			hideTables(guard, res)
		}
		return withNotes(res), "", nil
	}
	res, err := conn.Exec(ctx, decision.SQL, args)
	if err == nil && makesStoredCode(settings, statement) {
		// The snapshot no longer describes this database: a routine was made,
		// changed or removed. Without this, calling the procedure just created
		// was refused as one this tool had never heard of.
		guards.stale(settings)
	}
	return withNotes(res), "", err
}

// withNotes says out loud what a field alone would not.
//
// Truncation was a boolean on the result. A model that does not look at it reads
// 200 rows as the whole answer and draws a conclusion from a fifth of the data,
// and nothing anywhere says otherwise. A second result set was worse: a
// procedure ending in two SELECTs handed back the first and lost the other in
// silence. One statement per call is this tool's rule and is not changing, so
// the answer is to SAY so rather than to carry it.
func withNotes(res *datasource.Result) *datasource.Result {
	if res == nil {
		return res
	}
	var notes []string
	if res.Truncated {
		notes = append(notes, fmt.Sprintf("Only the first %d rows are here and there are more. "+
			"Narrow the query, or COUNT first, if you need the whole answer", res.RowCount))
	}
	if len(res.BinaryColumns) > 0 {
		notes = append(notes, fmt.Sprintf("Binary data is not returned, so %s came back as a size rather "+
			"than as bytes. Leave those columns out, or convert them in the query if you need something readable",
			strings.Join(res.BinaryColumns, ", ")))
	}
	if res.MoreResults {
		notes = append(notes, "The statement produced more than one result set and only the first is here. "+
			"One is returned per call, so ask for them one at a time")
	}
	if len(notes) > 0 {
		res.Note = strings.Join(notes, ". ")
	}
	return res
}
