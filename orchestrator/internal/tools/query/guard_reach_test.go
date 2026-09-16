package query

import (
	"context"
	"net"
	"testing"

	"flexie.io/sag/internal/datasource"
)

// Two people whose own computers both answer to localhost:3306 are not reaching
// the same database, so they must not share a snapshot of one. Before the far
// side had an identity, these three produced one key.
func TestAGuardIsNotSharedAcrossComputers(t *testing.T) {
	base := Settings{Connection: datasource.Config{
		Driver: "mysql", Host: "localhost", Port: 3306, Database: "app", Username: "u",
	}}
	alice, bob := base, base
	alice.Connection.Reach = &datasource.Reach{Describe: "the chat application", Via: "1/7/laptop-a",
		Dial: func(context.Context, string, int) (net.Conn, error) { return nil, nil }}
	bob.Connection.Reach = &datasource.Reach{Describe: "the chat application", Via: "1/9/laptop-b",
		Dial: func(context.Context, string, int) (net.Conn, error) { return nil, nil }}

	t.Logf("no reach : %q", cacheKey(base))
	t.Logf("alice    : %q", cacheKey(alice))
	t.Logf("bob      : %q", cacheKey(bob))
	if cacheKey(alice) == cacheKey(bob) {
		t.Errorf("two people reaching localhost:3306 on their OWN computers share one guard")
	}
	if cacheKey(alice) == cacheKey(base) {
		t.Errorf("reaching through a computer is the same key as reaching directly")
	}
}
