package sqlstore_test

import (
	"context"
	"os"
	"testing"

	"flexie.io/sag/internal/model"

	"flexie.io/sag/internal/store/storetest"
	"flexie.io/sag/internal/testdb"
)

// TestSQLStore runs the shared conformance suite against a real MariaDB.
// Set SAG_TEST_DSN to a scratch database (this suite gets its own copy of
// it, so it never collides with other test packages).
func TestSQLStore(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping SQL store conformance suite")
	}
	st, reset := testdb.Open(t, dsn, dbSuffix)
	storetest.Run(t, st, reset)
}

// A skill with no live version still reads, and is called by its handle.
//
// Not a hypothetical: a version can be rejected, and the pointer from a skill to
// its live version is ON DELETE SET NULL. The state is reached by writing the
// row directly, because no store method produces it, which is why this test is
// here (where the concrete store is in reach) rather than in the interface
// suite.
func TestASkillWithNoLiveVersionStillReads(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set")
	}
	st, reset := testdb.Open(t, dsn, dbSuffix)
	reset(t)
	ctx := context.Background()

	ws := &model.Workspace{Slug: "acme", Name: "acme"}
	if err := st.Workspaces().Create(ctx, ws, model.Nobody()); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	skill, _, err := st.Skills().Import(ctx, ws.ID, &model.SkillPackage{
		Name: "pdf-processing", Title: "PDF Toolkit",
		Description: "Does things.", SHA256: "abc",
		Files: []model.PackageFile{{
			Path: "SKILL.md", FileType: model.SkillFileSkill,
			Text: "---\nname: pdf-processing\n---\n", Size: 28, SHA256: "f1",
		}},
	}, model.Nobody())
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx,
		"UPDATE ai_skills SET active_version_id = NULL WHERE id = ?", skill.ID); err != nil {
		t.Fatalf("unset the live version: %v", err)
	}

	orphaned, err := st.Skills().Skill(ctx, ws.ID, skill.ID)
	if err != nil {
		t.Fatalf("read a skill with no live version: %v", err)
	}
	if orphaned.Version != nil {
		t.Errorf("a version is reported as live: %+v", orphaned.Version)
	}
	// And it is still CALLED what it was called. The words are the skill's own
	// (migration 67), not a reading of whichever version happens to be live, so
	// unsetting the pointer does not make a skill nameless: the row goes on
	// being an honest record of what somebody called this thing.
	//
	// This assertion is the other way round from how it started. It used to
	// demand two empty strings, because the words were reached through the join
	// and there was nothing on the other end of it. The handle fallback it was
	// standing in for has its own test and has not moved
	// (testSkillWithNoTitleIsCalledByItsHandle).
	if orphaned.Title != "PDF Toolkit" || orphaned.Description != "Does things." {
		t.Errorf("a skill with no live version reads %q / %q: it has lost its name",
			orphaned.Title, orphaned.Description)
	}
	if orphaned.Label() != "PDF Toolkit" {
		t.Errorf("label = %q, want what it is called", orphaned.Label())
	}

	// What follows from that, recorded because it is a decision and not an
	// accident: importing into a skill with no live version KEEPS these words
	// rather than taking the new package's. `naming` compares the row against
	// the version that was live, there is none, so the two differ and the words
	// read as somebody's own. Conservative on purpose: the cost is a title that
	// does not refresh in a state only reachable by hand, and the alternative
	// direction overwrites names people chose.
	again, _, err := st.Skills().Import(ctx, ws.ID, &model.SkillPackage{
		Name: "pdf-processing", Title: "Renamed by a package",
		Description: "And redescribed.", SHA256: "def",
		Files: []model.PackageFile{{
			Path: "SKILL.md", FileType: model.SkillFileSkill,
			Text: "---\nname: pdf-processing\n---\n", Size: 28, SHA256: "f2",
		}},
	}, model.Nobody())
	if err != nil {
		t.Fatalf("import into a skill with no live version: %v", err)
	}
	if again.Title != "PDF Toolkit" {
		t.Errorf("title = %q, the import renamed a skill it could not prove was unnamed",
			again.Title)
	}
	// The version it just wrote IS live, and carries its own words.
	if again.Version == nil || again.Version.Title != "Renamed by a package" {
		t.Errorf("the new version reads %+v, want the package's own words", again.Version)
	}
}

// dbSuffix names this package's scratch database. Open makes it, TestMain
// takes it away, and they read it from here so they cannot drift apart.
const dbSuffix = "store"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }
