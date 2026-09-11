package datasource_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/datasource"
	_ "flexie.io/sag/internal/datasource/postgres"
)

// Reaching a database through an SSH bastion, end to end, against a real
// bastion and a real database.
//
// Everything else about the tunnel is unit-tested: how an auth method is built,
// how a host key is checked. None of that answers the question this does, which
// is whether a driver actually connects and queries down the forwarder.
//
// It is written for a database the host CANNOT reach directly, and it proves
// that first. Without that, a tunnel that quietly did nothing would pass: the
// driver would connect straight to the database and nobody would be any wiser.
//
//	SAG_TEST_TUNNEL='ssh://user:pass@127.0.0.1:2222/db-host:5432'
//	SAG_TEST_TUNNEL_DB='postgres://user:pass@ignored/dbname'
func TestADriverReachesADatabaseItCannotReachDirectly(t *testing.T) {
	spec := os.Getenv("SAG_TEST_TUNNEL")
	dbSpec := os.Getenv("SAG_TEST_TUNNEL_DB")
	if spec == "" || dbSpec == "" {
		t.Skip("SAG_TEST_TUNNEL / SAG_TEST_TUNNEL_DB not set; skipping the live bastion suite")
	}

	ssh, target, targetPort := parseTunnelSpec(t, spec)
	driver, user, password, database := parseTunnelDB(t, dbSpec)

	// The premise. If this fails the rest proves nothing, so it is asserted
	// rather than assumed.
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(target, strconv.Itoa(targetPort)), 2*time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("%s:%d is reachable from here, so this test cannot tell a working tunnel from no tunnel at all",
			target, targetPort)
	}

	cfg := datasource.Config{
		Driver: driver, Host: target, Port: targetPort, Database: database,
		Username: user, Password: password,
		TLS: datasource.TLS{Mode: "disable"},
		SSH: &ssh,
	}

	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatalf("open through the bastion: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping through the bastion: %v", err)
	}

	// A real query, not just a handshake: the forwarder has to carry rows back
	// as well as let a connection open.
	res, err := conn.Query(ctx, "SELECT $1::int + 1 AS n", []any{41}, 0)
	if err != nil {
		t.Fatalf("query through the bastion: %v", err)
	}
	if res.RowCount != 1 || fmt.Sprint(res.Rows[0][0]) != "42" {
		t.Fatalf("rows came back wrong: %+v", res.Rows)
	}

	// The pool opens more than one connection, so the forwarder has to accept
	// more than one too.
	for i := 0; i < 5; i++ {
		if _, err := conn.Query(ctx, "SELECT count(*) FROM information_schema.tables", nil, 0); err != nil {
			t.Fatalf("query %d through the bastion: %v", i, err)
		}
	}

	// Reading the catalog is what a governed tool does before it runs anything,
	// and it is two queries plus a parse of every view definition.
	schema, err := conn.Schema(ctx)
	if err != nil {
		t.Fatalf("read the catalog through the bastion: %v", err)
	}
	if schema.Database != database {
		t.Fatalf("catalog came from %q, want %q", schema.Database, database)
	}
}

// A bastion that will not carry the connection has to fail loudly. A tunnel that
// silently did nothing is the failure mode this whole test exists to rule out.
func TestABastionThatRefusesIsAnError(t *testing.T) {
	spec := os.Getenv("SAG_TEST_TUNNEL")
	if spec == "" {
		t.Skip("SAG_TEST_TUNNEL not set; skipping the live bastion suite")
	}
	ssh, target, targetPort := parseTunnelSpec(t, spec)
	ssh.Password += "-wrong"

	_, err := datasource.Open(datasource.Config{
		Driver: "postgres", Host: target, Port: targetPort, Database: "postgres",
		Username: "x", TLS: datasource.TLS{Mode: "disable"}, SSH: &ssh,
	})
	if err == nil {
		t.Fatal("a bastion that refused the credentials still produced a connection")
	}
}

// ssh://user:password@host:port/target-host:target-port
func parseTunnelSpec(t *testing.T, spec string) (datasource.SSHConfig, string, int) {
	t.Helper()
	rest, ok := strings.CutPrefix(spec, "ssh://")
	if !ok {
		t.Fatalf("SAG_TEST_TUNNEL must look like ssh://user:pass@host:port/target:port")
	}
	credentials, addresses, ok := strings.Cut(rest, "@")
	if !ok {
		t.Fatal("no @ in SAG_TEST_TUNNEL")
	}
	user, password, _ := strings.Cut(credentials, ":")
	bastion, target, ok := strings.Cut(addresses, "/")
	if !ok {
		t.Fatal("no /target:port in SAG_TEST_TUNNEL")
	}
	bastionHost, bastionPortText, _ := strings.Cut(bastion, ":")
	targetHost, targetPortText, _ := strings.Cut(target, ":")
	bastionPort, err := strconv.Atoi(bastionPortText)
	if err != nil {
		t.Fatalf("bastion port: %v", err)
	}
	targetPort, err := strconv.Atoi(targetPortText)
	if err != nil {
		t.Fatalf("target port: %v", err)
	}
	return datasource.SSHConfig{Host: bastionHost, Port: bastionPort, User: user, Password: password}, targetHost, targetPort
}

// driver://user:password@ignored/database
func parseTunnelDB(t *testing.T, spec string) (driver, user, password, database string) {
	t.Helper()
	driver, rest, ok := strings.Cut(spec, "://")
	if !ok {
		t.Fatal("SAG_TEST_TUNNEL_DB must look like postgres://user:pass@ignored/database")
	}
	credentials, address, ok := strings.Cut(rest, "@")
	if !ok {
		t.Fatal("no @ in SAG_TEST_TUNNEL_DB")
	}
	user, password, _ = strings.Cut(credentials, ":")
	_, database, ok = strings.Cut(address, "/")
	if !ok {
		t.Fatal("no /database in SAG_TEST_TUNNEL_DB")
	}
	return driver, user, password, database
}
