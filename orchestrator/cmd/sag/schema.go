package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/schemasync"
)

// sag schema [--dump-sql | --update] [--allow-destructive]
//
//	--dump-sql   print the SQL that would bring this database to the declared
//	             schema, and change nothing (the default)
//	--update     run it
//
// The schema is declared in schema/: one CREATE TABLE per file, and the file is
// the definition. This command works out the difference between it and the
// database in SAG_DB_DSN, from whatever state that database is in, an empty one
// included.
//
// Dropping a table or a column destroys data, and --update refuses to do it
// unless --allow-destructive says so. A tool that silently drops a column
// because somebody deleted a line in a file is a tool that will one day delete
// production.
const schemaDir = "schema"

func runSchema(ctx context.Context, cfg *config.Config, args []string) error {
	flags := flag.NewFlagSet("schema", flag.ContinueOnError)
	dumpSQL := flags.Bool("dump-sql", false, "print the SQL and change nothing")
	update := flags.Bool("update", false, "run the SQL against the database")
	destructive := flags.Bool("allow-destructive", false, "permit statements that lose data")
	pull := flags.Bool("pull", false, "rewrite schema/ from the database, instead of the database from schema/")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *pull {
		if err := schemasync.Dump(ctx, cfg.DBDSN, schemaDir); err != nil {
			return err
		}
		fmt.Println("schema: schema/ now describes this database.")
		return nil
	}

	plan, err := schemasync.Compute(ctx, cfg.DBDSN, schemaDir)
	if err != nil {
		return err
	}
	if plan.InSync() {
		fmt.Println("-- The database already matches the declared schema. Nothing to do.")
		return nil
	}

	// Printing is the default, because a schema change nobody read is exactly
	// the change you regret. --update is the one that means it.
	if !*update || *dumpSQL {
		for _, statement := range plan.Statements {
			fmt.Println(statement)
		}
		if !*update {
			if len(plan.Destructive) > 0 {
				fmt.Fprintf(os.Stderr, "\n-- %d of these DESTROY DATA. --update will refuse them without --allow-destructive.\n",
					len(plan.Destructive))
			}
			return nil
		}
		fmt.Println()
	}

	if err := schemasync.Apply(ctx, cfg.DBDSN, plan, *destructive); err != nil {
		return err
	}
	fmt.Printf("schema: applied %d statement(s). The database is what you declared.\n", len(plan.Statements))
	return nil
}
