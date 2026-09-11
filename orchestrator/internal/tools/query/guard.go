package query

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// A tool's guard, kept for a while.
//
// Enforcing a policy needs to know what the database holds, and asking it that
// is two reads of its catalog plus a parse of every view definition in it. The
// answer changes when somebody changes the database, which is rarely, and the
// question would otherwise be asked again for every statement an assistant
// writes. So it is asked once and the answer reused.
//
// What is kept is the whole guard rather than the snapshot, because the work
// that turns a snapshot and a policy into decisions (which column names could
// come back hidden, under any name they can be read by) is done once when the
// two are put together.
const (
	// catalogLife is how long a snapshot is trusted. Long enough that an
	// assistant working through a problem does not re-read the catalog between
	// two questions; short enough that a table added this afternoon is in reach
	// this afternoon.
	catalogLife = 5 * time.Minute
	// sweepAbove is the number of kept guards past which the stale ones are
	// dropped. Editing a tool's policy makes a new one and orphans the old, and
	// a server that runs for months should not accumulate them.
	sweepAbove = 64
)

var guards = &guardCache{entries: map[string]*guardEntry{}}

type guardCache struct {
	mu      sync.Mutex
	entries map[string]*guardEntry
}

type guardEntry struct {
	// mu serializes reading the catalog for one tool, so a crowd of turns
	// arriving at a cold entry together produces one read rather than a crowd of
	// them. It is per entry, so a slow database holds up its own tool and no
	// other.
	mu    sync.Mutex
	guard *sqlguard.Guard
	taken time.Time
}

// get returns the guard for these settings, reading the database's catalog when
// there is no fresh one. The connection is the caller's: the statement is about
// to run on it, so the catalog is read down the same one rather than opening a
// second.
func (c *guardCache) get(ctx context.Context, settings Settings, conn *datasource.Conn) (*sqlguard.Guard, error) {
	return c.load(ctx, settings, conn, false)
}

// refresh takes a new snapshot whatever the age of the one it has. It is what a
// statement naming an unknown table asks for: a table made since the snapshot
// was taken is unknown, and so is a VIEW made since, which would otherwise be
// read as a table nobody kept back whatever it reads underneath.
func (c *guardCache) refresh(ctx context.Context, settings Settings, conn *datasource.Conn) (*sqlguard.Guard, error) {
	return c.load(ctx, settings, conn, true)
}

func (c *guardCache) load(ctx context.Context, settings Settings, conn *datasource.Conn, force bool) (*sqlguard.Guard, error) {
	entry := c.entryFor(settings)

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !force && entry.guard != nil && time.Since(entry.taken) < catalogLife {
		return entry.guard, nil
	}

	schema, err := conn.Schema(ctx)
	if err != nil {
		return nil, fmt.Errorf("read what the database holds: %w", err)
	}
	guard, err := sqlguard.New(settings.Connection.Driver, settings.Policy,
		catalogOf(settings.Connection.Driver, schema))
	if err != nil {
		return nil, err
	}
	entry.guard, entry.taken = guard, time.Now()
	return guard, nil
}

func (c *guardCache) entryFor(settings Settings) *guardEntry {
	key := cacheKey(settings)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > sweepAbove {
		c.sweep()
	}
	entry, ok := c.entries[key]
	if !ok {
		entry = &guardEntry{}
		c.entries[key] = entry
	}
	return entry
}

// sweep drops guards nothing has asked for in a while. It runs under the map's
// own lock and never touches an entry another call is loading, because a loading
// entry was taken long enough ago to be stale only if it has been loading for
// twice the life of a snapshot, which its own deadline does not allow.
func (c *guardCache) sweep() {
	for key, entry := range c.entries {
		if time.Since(entry.taken) > 2*catalogLife {
			delete(c.entries, key)
		}
	}
}

// cacheKey is what makes two calls the same tool: the same database reached the
// same way, under the same policy. Editing the policy makes a different key, so
// an edited tool is governed by what it now says rather than by what it said
// when its catalog was last read.
//
// "The same way" includes the bastion, because reaching one address through two
// different machines reaches two different databases. Private names repeat:
// db.internal on one customer's network and db.internal on another's are not
// the same host, and without this the first tool to read its catalog would
// impose that catalog on the second for as long as it was kept.
//
// No secret is part of it. A key is held in memory for as long as the entry is,
// and nothing here needs a password to say which database it is talking about.
func cacheKey(settings Settings) string {
	c, p := settings.Connection, settings.Policy
	parts := []string{
		c.Driver, c.Host, strconv.Itoa(c.Port), c.Database, c.Username,
		string(p.TableMode), p.Tables, string(p.FieldMode), p.Fields,
	}
	if c.SSH != nil {
		parts = append(parts, c.SSH.Host, strconv.Itoa(c.SSH.Port), c.SSH.User)
	}
	return strings.Join(parts, "\x00")
}

// hideTables drops from a list of tables the ones this tool may not see. A SHOW
// cannot be narrowed in the asking the way a read of the catalog can, so it is
// narrowed in the answering, before the rows go anywhere.
//
// A row whose name cannot be read is dropped rather than kept: leaving out a
// table that should have been listed is a gap in an answer, and keeping one that
// should not have been is the whole thing this exists to prevent.
func hideTables(guard *sqlguard.Guard, res *datasource.Result) {
	if guard == nil || res == nil {
		return
	}
	kept := res.Rows[:0]
	for _, row := range res.Rows {
		if len(row) == 0 {
			continue
		}
		name, ok := row[0].(string)
		if !ok || !guard.Shows(name) {
			continue
		}
		kept = append(kept, row)
	}
	res.Rows = kept
	res.RowCount = len(kept)
}

// catalogOf turns what a database said about itself into the snapshot the guard
// decides with.
func catalogOf(dialect string, schema *datasource.Schema) *sqlguard.Catalog {
	tables := make([]sqlguard.Table, 0, len(schema.Tables))
	for _, t := range schema.Tables {
		tables = append(tables, sqlguard.Table{
			Name:       t.Name,
			Namespace:  t.Namespace,
			View:       t.View,
			Columns:    t.Columns,
			Definition: t.Definition,
		})
	}
	return sqlguard.NewCatalog(dialect, schema.Database, tables)
}
