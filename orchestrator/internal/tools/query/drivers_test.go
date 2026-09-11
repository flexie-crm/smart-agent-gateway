package query

// The databases these tests can reach, and the SQL they can read.
//
// A driver lives in its own package and registers itself from there, so it is in
// a binary only if something imports it. In the server that is the wiring
// (internal/app); here it is this file, because a test binary is its own program
// and nothing else in it would have pulled the driver in.
import (
	_ "flexie.io/sag/internal/datasource/mysql"
	_ "flexie.io/sag/internal/datasource/postgres"
	_ "flexie.io/sag/internal/sqlguard/mysql"
	_ "flexie.io/sag/internal/sqlguard/postgres"
)
