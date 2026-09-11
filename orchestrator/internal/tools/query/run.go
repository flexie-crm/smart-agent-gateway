package query

import (
	"context"
	"fmt"
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
	if ok, reason := check(settings.Access, statement); !ok {
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

	if isReadOnly(statement) {
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
		return res, "", nil
	}
	res, err := conn.Exec(ctx, decision.SQL, args)
	return res, "", err
}
