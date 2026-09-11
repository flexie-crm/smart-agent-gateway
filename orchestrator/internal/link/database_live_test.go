package link

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/datasource"
	_ "flexie.io/sag/internal/datasource/mysql"
)

// A real database, through the real client, at real sizes.
//
// Everything else proves the pipe carries bytes. This proves it carries a
// PROTOCOL: a driver that negotiates a handshake, sends packets of its own
// choosing, reads a result set in pieces and expects each one whole and in
// order. A byte pipe that is subtly wrong passes an echo test and fails here.
//
// Sizes are the point. A result set of a few kilobytes proves nothing about a
// route: the limit that killed a connection at 32 KB was invisible until
// something asked for more. So this asks for megabytes, in one value and in
// many rows, and checks what came back rather than that something came back.
//
//	SAG_LINK_E2E=1 SAG_TEST_DSN='...' make link-e2e
func TestARealDatabaseThroughTheRealClient(t *testing.T) {
	if os.Getenv("SAG_LINK_E2E") == "" {
		t.Skip("SAG_LINK_E2E not set; skipping the cross-language link gate (make link-e2e)")
	}
	host, port, user, password, database := databaseFromEnv(t)

	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		if token == "a-real-looking-token" {
			return 11, 5, "the-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/link", r.ServeControl)
	mux.HandleFunc("/v1/link/stream", r.ServeStream)
	gateway := newRestartableServer(t, mux)
	defer gateway.stop()

	client := startRustClient(t, gateway.url, "a-real-looking-token")
	defer client.stop()
	waitFor(t, "the real client to link", func() bool { return r.Online(5, 11, "the-laptop") })

	// The tool's connection, exactly as the query tool builds it: the database
	// is named by an address this side does not use, and every connection to it
	// is carried by the client.
	config := datasource.Config{
		Driver: "mysql", Host: host, Port: port,
		Database: database, Username: user, Password: password,
		TLS: datasource.TLS{Mode: "disable"},
		Reach: &datasource.Reach{
			Describe: "the chat application",
			Dial: func(ctx context.Context, host string, port int) (net.Conn, error) {
				return r.Dial(ctx, 5, 11, "the-laptop", host, port, "a database query")
			},
		},
	}

	conn, err := datasource.Open(config)
	if err != nil {
		t.Fatalf("open the database through the client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("the driver could not reach the database: %v", err)
	}

	t.Run("a query and its answer", func(t *testing.T) {
		res, err := conn.Query(ctx, "SELECT 1 + 1 AS answer", nil, 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "2" {
			t.Fatalf("the database answered %+v", res.Rows)
		}
	})

	// One value of four megabytes, checked by its digest. A pipe that dropped,
	// duplicated or reordered a single frame changes it.
	t.Run("a four megabyte value arrives exactly", func(t *testing.T) {
		const size = 4 * 1024 * 1024
		res, err := conn.Query(ctx,
			"SELECT REPEAT('x', ?) AS big, SHA2(REPEAT('x', ?), 256) AS digest", []any{size, size}, 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("rows: %d", len(res.Rows))
		}
		got := fmt.Sprint(res.Rows[0][0])
		if len(got) != size {
			t.Fatalf("the value came back as %d bytes, not %d", len(got), size)
		}
		// The database's own digest of what it sent, against ours of what we
		// received. Nothing in between can fake this.
		theirs := strings.ToLower(fmt.Sprint(res.Rows[0][1]))
		ours := fmt.Sprintf("%x", sha256.Sum256([]byte(got)))
		if theirs != ours {
			t.Fatalf("what arrived is not what was sent:\n  database %s\n  received %s", theirs, ours)
		}
	})

	// Many rows rather than one big value: thousands of packets, each read and
	// assembled, which is what a real result set is.
	t.Run("fifty thousand rows arrive in order", func(t *testing.T) {
		const rows = 50000
		// The sequence engine rather than a recursive CTE: MariaDB stops a CTE
		// at max_recursive_iterations, which defaults to 1000, and a test that
		// asked for fifty thousand rows and asserted on what it got would have
		// been reporting the database's limit as a fault in the pipe.
		res, err := conn.Query(ctx,
			"SELECT seq AS n, REPEAT(CHAR(64 + (seq % 26) + 1), 64) AS filler FROM seq_1_to_"+
				strconv.Itoa(rows), nil, rows)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != rows {
			t.Fatalf("got %d rows, not %d", len(res.Rows), rows)
		}
		for i, row := range res.Rows {
			if fmt.Sprint(row[0]) != strconv.Itoa(i+1) {
				t.Fatalf("row %d is numbered %v: the result set is out of order", i, row[0])
			}
			want := strings.Repeat(string(rune('A'+((i+1)%26))), 64)
			if fmt.Sprint(row[1]) != want {
				t.Fatalf("row %d carries the wrong value", i)
			}
		}
	})

	// What it costs. Not an assertion about a number, which would be a test that
	// fails on a busy machine; a measurement printed where somebody comparing
	// two builds can see it.
	t.Run("throughput", func(t *testing.T) {
		const size = 8 * 1024 * 1024
		start := time.Now()
		res, err := conn.Query(ctx, "SELECT REPEAT('y', ?) AS big", []any{size}, 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		took := time.Since(start)
		if len(fmt.Sprint(res.Rows[0][0])) != size {
			t.Fatalf("short read")
		}
		t.Logf("8 MiB through the client in %s (%.1f MiB/s)",
			took.Round(time.Millisecond), float64(size)/(1024*1024)/took.Seconds())
	})

	// A result set bigger than anything that fits in one packet, or in one
	// comfortable buffer: fifty megabytes in two hundred thousand rows.
	//
	// One big value proves the pipe assembles a message. This proves it can
	// STREAM, for long enough that anything holding on to what it has already
	// carried would be visible. SQL Server is next in this template and is
	// heavier on the socket than MySQL, so the margin is worth having now.
	t.Run("fifty megabytes streams without holding on to it", func(t *testing.T) {
		const rows = 200000
		const filler = 256

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		start := time.Now()
		res, err := conn.Query(ctx,
			"SELECT seq, REPEAT('z', "+strconv.Itoa(filler)+") FROM seq_1_to_"+strconv.Itoa(rows),
			nil, rows)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		took := time.Since(start)
		if len(res.Rows) != rows {
			t.Fatalf("got %d rows, not %d", len(res.Rows), rows)
		}
		for _, row := range res.Rows {
			if len(fmt.Sprint(row[1])) != filler {
				t.Fatalf("a row came back the wrong length")
			}
		}
		carried := float64(rows*filler) / (1024 * 1024)
		t.Logf("%.0f MiB of rows in %s (%.1f MiB/s)", carried, took.Round(time.Millisecond),
			carried/took.Seconds())

		runtime.ReadMemStats(&after)
		// What the pipe kept is the question, not what the driver's result set
		// costs: the rows are still referenced here. TotalAlloc grows by what
		// passed through; HeapInuse is what is still held.
		t.Logf("heap in use %.0f MiB, total allocated during it %.0f MiB",
			float64(after.HeapInuse)/(1024*1024),
			float64(after.TotalAlloc-before.TotalAlloc)/(1024*1024))
	})

	// What the route costs a chatty protocol.
	//
	// Throughput is the easy half. A database conversation is mostly small
	// questions and small answers, and what it feels is the round trip: every
	// one of them now crosses a websocket, a second process and back. SQL Server
	// is next in this template and is chattier than MySQL, so the number worth
	// knowing is the per-query cost against the same database reached directly.
	t.Run("what a round trip costs", func(t *testing.T) {
		const queries = 200

		direct, err := datasource.Open(datasource.Config{
			Driver: "mysql", Host: host, Port: port,
			Database: database, Username: user, Password: password,
			TLS: datasource.TLS{Mode: "disable"},
		})
		if err != nil {
			t.Fatalf("open directly: %v", err)
		}
		defer func() { _ = direct.Close() }()

		// The fastest query, not the average: an average measures the machine as
		// much as the route, and what is being asked is what a round trip costs.
		// Something waiting on a timer raises the floor; load only raises the
		// ceiling.
		measure := func(c *datasource.Conn) time.Duration {
			best := time.Hour
			for i := 0; i < queries; i++ {
				start := time.Now()
				if _, err := c.Query(ctx, "SELECT ?", []any{i}, 1); err != nil {
					t.Fatalf("query %d: %v", i, err)
				}
				if took := time.Since(start); took < best {
					best = took
				}
			}
			return best
		}

		// Direct first and carried second would flatter the second (a warm
		// connection, a warm page cache), so each is measured after a warm-up of
		// its own.
		_ = measure(direct)
		_ = measure(conn)
		straight := measure(direct)
		carried := measure(conn)

		t.Logf("a query costs %s direct, %s through the client (%s added)",
			straight.Round(time.Microsecond), carried.Round(time.Microsecond),
			(carried - straight).Round(time.Microsecond))

		// Not an assertion about a number on somebody's laptop, but a ceiling
		// that says something is wrong rather than slow: a millisecond of added
		// latency per query on loopback would mean a round trip is waiting for
		// something (a delayed acknowledgement, a scheduler, a buffer being
		// flushed on a timer) rather than merely travelling further.
		if added := carried - straight; added > time.Millisecond {
			t.Fatalf("the route adds %s per query, which is a wait rather than a hop", added)
		}
	})

	// And it is still a database connection afterwards: a pipe that half-closed
	// something, or left a frame behind, breaks the NEXT query rather than the
	// big one.
	t.Run("the connection still works after all that", func(t *testing.T) {
		res, err := conn.Query(ctx, "SELECT 'still here' AS state", nil, 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if fmt.Sprint(res.Rows[0][0]) != "still here" {
			t.Fatalf("got %+v", res.Rows)
		}
	})
}

// databaseFromEnv reads a local database to query through the link.
//
// The development database rather than the scratch one: this only ever reads,
// and the scratch database exists only while a suite that creates it is
// running, so pointing at it means a gate that fails on a database that is
// nobody's fault.
func databaseFromEnv(t *testing.T) (host string, port int, user, password, database string) {
	t.Helper()
	dsn := os.Getenv("SAG_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("SAG_TEST_DSN")
	}
	if dsn == "" {
		t.Skip("SAG_DB_DSN not set; skipping the real-database link gate")
	}
	// user:password@tcp(host:port)/database?params
	credentials, rest, ok := strings.Cut(dsn, "@")
	if !ok {
		t.Fatalf("the DSN could not be read: %q", dsn)
	}
	user, password, _ = strings.Cut(credentials, ":")
	address, tail, ok := strings.Cut(rest, ")")
	if !ok || !strings.HasPrefix(address, "tcp(") {
		t.Fatalf("the DSN must use tcp(host:port): %q", dsn)
	}
	host, portText, _ := strings.Cut(strings.TrimPrefix(address, "tcp("), ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the port in the DSN: %v", err)
	}
	database = strings.TrimPrefix(tail, "/")
	if i := strings.Index(database, "?"); i >= 0 {
		database = database[:i]
	}
	// A DSN with no database in it is a DSN for something else: `make ci` uses
	// one, because the suites there create their own scratch databases and
	// never need a default. This test opens a connection and runs statements
	// against it, so without one every query fails with "No database selected",
	// which reads like a fault in the link and is a fault in the environment.
	// Said here, once, rather than diagnosed again.
	if database == "" {
		t.Skipf("the DSN names no database (%q), so there is nothing to query through the link: "+
			"set SAG_DB_DSN to one that does, e.g. user:pass@tcp(127.0.0.1:3306)/flexie_sag_test", dsn)
	}
	return host, port, user, password, database
}
