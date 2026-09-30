package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// Skills.
//
// A version is immutable and one version is live. Everything in this file exists
// to make those two things true rather than intended:
//
//   - nothing here updates a version's files or its sections. There is no
//     statement in this file that can, which is a stronger guarantee than a rule
//     somebody has to remember;
//   - `activate` is the one place either record of "which version is live" is
//     written, and it writes both. A second writer is how a pointer and a status
//     start disagreeing, and then nobody can say which one is right.

type skillStore struct{ db *sqldb.DB }

// --- importing -------------------------------------------------------------------

func (s *skillStore) Import(ctx context.Context, workspaceID int64, pkg *model.SkillPackage, by model.Actor) (*model.Skill, bool, error) {
	if pkg == nil || pkg.Name == "" || pkg.SHA256 == "" {
		return nil, false, fmt.Errorf("import skill: the package has not been read")
	}

	var (
		id    int64
		added bool
	)
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// Reset per attempt: db.Tx runs the body again when it lost a race, and
		// a flag left over from the attempt that rolled back would report a
		// version that no longer exists.
		added = false
		now := time.Now().UTC()

		skill, err := findOrCreate(ctx, tx, workspaceID, pkg, by, now)
		if err != nil {
			return err
		}
		id = skill.ID

		// The same bytes as a version this skill already holds? Then it IS that
		// version. Nothing is written and nothing is activated: somebody who
		// uploads the same file twice has not changed anything, and a version
		// history that grows on every double click is a history of clicks.
		var existing int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM ai_skill_versions WHERE skill_id = ? AND package_sha256 = ?`,
			skill.ID, pkg.SHA256).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look for the package: %w", err)
		}

		versionID, err := insertVersion(ctx, tx, skill, pkg, origin{
			source: model.SkillSourceImported,
			parent: skill.ActiveVersionID,
		}, by, now)
		if err != nil {
			return err
		}
		if err := insertFiles(ctx, tx, skill.ID, versionID, pkg, now); err != nil {
			return err
		}
		if err := activate(ctx, tx, skill, versionID, by, now); err != nil {
			return err
		}
		added = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	skill, err := s.Skill(ctx, workspaceID, id)
	if err != nil {
		return nil, false, err
	}
	return skill, added, nil
}

// findOrCreate is the skill's identity: one row per name per workspace.
//
// The read takes a lock, so two imports of the same skill queue rather than
// race. The duplicate-key path is still needed and is not belt-and-braces: a
// row that does not exist yet cannot be locked, so two imports of a NEW skill
// both read nothing, and one of them loses the insert.
func findOrCreate(ctx context.Context, tx *sqldb.Tx, workspaceID int64, pkg *model.SkillPackage, by model.Actor, now time.Time) (*model.Skill, error) {
	skill, err := lockedSkill(ctx, tx,
		`SELECT `+lockedSkillColumns+` FROM ai_skills
		  WHERE workspace_id = ? AND name = ? FOR UPDATE`, workspaceID, pkg.Name)
	switch {
	case err == nil:
		// Nothing to write HERE. The identity, the status and who CREATED it are
		// none of this package's business: a skill IS (workspace, name),
		// re-importing must not re-enable one somebody switched off, and the
		// creator is whoever made the skill rather than whoever updated it. Who
		// imported THIS package goes on the version.
		//
		// The title and the description are not written here either, and they
		// are not left alone: `activate` writes them, under the rule in
		// `naming`, because what a skill is called follows from which version is
		// live and the two must be written together. That is also why the words
		// this read carries matter: the rule is decided against them.
		return skill, nil

	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO ai_skills
		   (workspace_id, name, created_by, created_by_name, status,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		workspaceID, pkg.Name, nullID(by.UserID), by.Name, model.SkillActive, now, now)
	if err != nil {
		if errors.Is(wrapWriteErr("insert skill", err), store.ErrConflict) {
			// Somebody else created it between the read and the write. Their row
			// is the one to use: a skill IS (workspace, name).
			return lockedSkill(ctx, tx,
				`SELECT `+lockedSkillColumns+` FROM ai_skills
				  WHERE workspace_id = ? AND name = ? FOR UPDATE`, workspaceID, pkg.Name)
		}
		return nil, wrapWriteErr("insert skill", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("insert skill: %w", err)
	}
	created := &model.Skill{
		ID: id, WorkspaceID: workspaceID, Name: pkg.Name, Status: model.SkillActive,
	}
	created.Made(by)
	return created, nil
}

// lockedSkill reads what a write needs to know about a skill: which row it is,
// which version is live, whether it is switched on, and what it is CALLED.
//
// The last is here because the naming rule is decided against it (see `naming`):
// a write has to know whether the words on the row are still the live version's
// or somebody's own, and it cannot ask that without holding them.
const lockedSkillColumns = `id, active_version_id, status, title, description`

func lockedSkill(ctx context.Context, tx *sqldb.Tx, query sqldb.Statement, args ...any) (*model.Skill, error) {
	skill := &model.Skill{}
	var active sql.NullInt64
	err := tx.QueryRowContext(ctx, query, args...).Scan(
		&skill.ID, &active, &skill.Status, &skill.Title, &skill.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read skill: %w", err)
	}
	skill.ActiveVersionID = active.Int64
	return skill, nil
}

// Draft writes an edited package as a new version, live nothing.
//
// Everything Import does except the last step. The skill must already exist
// (there is no findOrCreate here: a skill comes into existence by importing a
// package and in no other way), and nothing is activated, so the agent goes on
// using the version it was using until somebody publishes this one.
func (s *skillStore) Draft(ctx context.Context, workspaceID, skillID, from int64,
	pkg *model.SkillPackage, summary string, by model.Actor,
) (*model.SkillVersion, bool, error) {
	if pkg == nil || pkg.Name == "" || pkg.SHA256 == "" {
		return nil, false, fmt.Errorf("draft skill version: the package has not been built")
	}

	var (
		versionID int64
		added     bool
	)
	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		// Reset per attempt: db.Tx runs the body again when it lost a race.
		added = false
		now := time.Now().UTC()

		skill, err := lockedSkill(ctx, tx,
			`SELECT `+lockedSkillColumns+` FROM ai_skills
			  WHERE id = ? AND workspace_id = ? FOR UPDATE`, skillID, workspaceID)
		if err != nil {
			return err
		}
		// The parent must be this skill's own version, for the same reason
		// Update checks it: a lineage that leaves the skill it belongs to is
		// worse than a refusal.
		if err := requireExists(ctx, tx, "draft skill version",
			`SELECT 1 FROM ai_skill_versions WHERE id = ? AND skill_id = ?`,
			from, skillID); err != nil {
			return err
		}

		// The same bytes as a version this skill already holds? Then it IS that
		// version, and there is nothing to save. Somebody who edited a file back
		// to what it used to say has not made a new version of anything.
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM ai_skill_versions WHERE skill_id = ? AND package_sha256 = ?`,
			skillID, pkg.SHA256).Scan(&versionID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look for the package: %w", err)
		}

		versionID, err = insertVersion(ctx, tx, skill, pkg, origin{
			source:  model.SkillSourceManual,
			parent:  from,
			summary: summary,
		}, by, now)
		if err != nil {
			return err
		}
		if err := insertFiles(ctx, tx, skillID, versionID, pkg, now); err != nil {
			return err
		}
		// The skill row is touched so the list shows somebody has been here,
		// and NOT renamed: what this package calls itself becomes the skill's
		// words when it is published, under the ordinary rule, and not before.
		if _, err := tx.ExecContext(ctx,
			`UPDATE ai_skills SET updated_by = ?, updated_by_name = ?, updated_at = ?
			  WHERE id = ? AND workspace_id = ?`,
			nullID(by.UserID), by.Name, now, skillID, workspaceID); err != nil {
			return wrapWriteErr("touch the skill", err)
		}
		added = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	version, err := s.version(ctx, workspaceID, skillID, versionID)
	if err != nil {
		return nil, false, err
	}
	return version, added, nil
}

// Discard turns a draft down, and only a draft.
func (s *skillStore) Discard(ctx context.Context, workspaceID, skillID, versionID int64, by model.Actor) error {
	return s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		now := time.Now().UTC()
		// Scoped by workspace through the skill, and by status in the same
		// statement rather than read-then-write: the row count is what says
		// whether a DRAFT of THIS skill in THIS workspace was turned down, so
		// two people discarding at once cannot both be told they did it.
		res, err := tx.ExecContext(ctx,
			`UPDATE ai_skill_versions v
			   JOIN ai_skills s ON s.id = v.skill_id
			    SET v.status = ?
			  WHERE v.id = ? AND v.skill_id = ? AND s.workspace_id = ? AND v.status = ?`,
			model.SkillVersionRejected, versionID, skillID, workspaceID, model.SkillVersionDraft)
		if err != nil {
			return wrapWriteErr("discard the draft", err)
		}
		gone, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("discard the draft: %w", err)
		}
		if gone == 0 {
			return store.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE ai_skills SET updated_by = ?, updated_by_name = ?, updated_at = ?
			  WHERE id = ? AND workspace_id = ?`,
			nullID(by.UserID), by.Name, now, skillID, workspaceID); err != nil {
			return wrapWriteErr("touch the skill", err)
		}
		return nil
	})
}

// origin is what kind of version is being written and where it came from.
//
// Passed in rather than decided here, because there are two routes now and the
// row must be written by ONE function: an imported package and a package edited
// on the screen differ in three columns and in nothing else, and a second
// INSERT for the second route is how two versions of a version come to exist.
type origin struct {
	// source is model.SkillSourceImported or model.SkillSourceManual.
	source string
	// parent is the version this one was built from: the live one on an import,
	// and the one that was being READ on an edit, which are not always the same
	// version (somebody can open an old one and edit it).
	parent int64
	// summary is what changed, for a person reading the history later. Empty on
	// an import, where the package speaks for itself.
	summary string
}

func insertVersion(ctx context.Context, tx *sqldb.Tx, skill *model.Skill, pkg *model.SkillPackage, from origin, by model.Actor, now time.Time) (int64, error) {
	// The next number is read inside the transaction, and the unique key on
	// (skill_id, version_number) is what actually enforces it: two imports that
	// somehow read the same number cannot both write it.
	var number int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version_number), 0) + 1 FROM ai_skill_versions WHERE skill_id = ?`,
		skill.ID).Scan(&number); err != nil {
		return 0, fmt.Errorf("next version number: %w", err)
	}

	// Validated, now, because validation here is the format check this package
	// has just passed, and both routes pass the same one: skill.Read for an
	// archive and skill.Rewrite for an edit, which runs the same frontmatter,
	// naming and classification rules over the result. What neither needs is
	// execution evidence: that is asked of a version the system wrote itself.
	//
	// The parent is the version this one was built from, which is the lineage a
	// rollback follows, and NULL for a first import.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO ai_skill_versions
		   (skill_id, parent_version_id, version_number, source, status,
		    created_by, created_by_name, title, description, change_summary,
		    package_sha256, validated_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		skill.ID, nullID(from.parent), number,
		from.source, model.SkillVersionDraft,
		nullID(by.UserID), by.Name,
		pkg.Title, pkg.Description, nullString(from.summary),
		pkg.SHA256, now, now)
	if err != nil {
		return 0, wrapWriteErr("insert skill version", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert skill version: %w", err)
	}
	return id, nil
}

func insertFiles(ctx context.Context, tx *sqldb.Tx, skillID, versionID int64, pkg *model.SkillPackage, now time.Time) error {
	const insert sqldb.Statement = `INSERT INTO ai_skill_files
	   (skill_id, version_id, path, file_type, mime_type, text_content,
	    binary_content, size_bytes, sha256, created_at)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	// One sequence across the whole version, not per file, because the column is
	// unique per version: it is the order a reader walks the passages in.
	sequence := 0
	for _, f := range pkg.Files {
		// Exactly one of the two content columns holds anything. A nil `any`
		// binds as NULL, so the unused column is genuinely empty rather than an
		// empty string pretending to be absent.
		var asText, asBytes any
		if f.Bytes != nil {
			asBytes = f.Bytes
		} else {
			asText = f.Text
		}

		res, err := tx.ExecContext(ctx, insert,
			skillID, versionID, f.Path, f.FileType, nullString(f.MIMEType),
			asText, asBytes, f.Size, f.SHA256, now)
		if err != nil {
			return wrapWriteErr("insert skill file", err)
		}
		fileID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert skill file: %w", err)
		}
		if err := insertSections(ctx, tx, skillID, versionID, fileID, f.Sections, &sequence, now); err != nil {
			return err
		}
	}
	return nil
}

func insertSections(ctx context.Context, tx *sqldb.Tx, skillID, versionID, fileID int64, sections []model.PackageSection, sequence *int, now time.Time) error {
	if len(sections) == 0 {
		return nil
	}
	// One statement per hundred passages rather than one per passage: a package
	// of reference material runs to thousands, and a round trip each would make
	// importing a skill slower than reading it.
	const insert sqldb.Statement = `INSERT INTO ai_skill_sections
	   (skill_id, version_id, file_id, heading, section_path, body,
	    line_start, line_end, sequence_no, sha256, created_at)
	 VALUES `
	const cols = 11

	for start := 0; start < len(sections); start += sqldb.RowChunk {
		end := start + sqldb.RowChunk
		if end > len(sections) {
			end = len(sections)
		}
		chunk := sections[start:end]

		args := make([]any, 0, len(chunk)*cols)
		for _, section := range chunk {
			*sequence++
			args = append(args, skillID, versionID, fileID,
				nullString(section.Heading), nullString(section.Path), section.Body,
				section.LineStart, section.LineEnd, *sequence, section.SHA256, now)
		}
		if _, err := tx.ExecContext(ctx, insert.Rows(len(chunk), cols), args...); err != nil {
			return wrapWriteErr("insert skill sections", err)
		}
	}
	return nil
}

// activate is the ONLY writer of which version is live.
//
// Three statements, one transaction: the version that was live is archived, the
// new one becomes active, and the skill's pointer moves. They are here together
// because they are one fact, and a second place that wrote any one of them would
// be a way for the pointer and the statuses to disagree.
//
// The skill's own title and description move with the pointer, under the rule in
// `naming`, and they are written by the same statement for the same reason: what
// a skill is called follows from which version is live, so the two cannot be
// allowed to be written apart.
func activate(ctx context.Context, tx *sqldb.Tx, skill *model.Skill, versionID int64, by model.Actor, now time.Time) error {
	title, description, err := naming(ctx, tx, skill, versionID)
	if err != nil {
		return err
	}
	if skill.ActiveVersionID != 0 && skill.ActiveVersionID != versionID {
		if _, err := tx.ExecContext(ctx,
			`UPDATE ai_skill_versions SET status = ? WHERE id = ? AND skill_id = ?`,
			model.SkillVersionArchived, skill.ActiveVersionID, skill.ID); err != nil {
			return wrapWriteErr("archive the version that was live", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_skill_versions SET status = ?, activated_at = ? WHERE id = ? AND skill_id = ?`,
		model.SkillVersionActive, now, versionID, skill.ID); err != nil {
		return wrapWriteErr("make the version live", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_skills
		    SET active_version_id = ?, title = ?, description = ?,
		        updated_by = ?, updated_by_name = ?, updated_at = ?
		  WHERE id = ?`,
		versionID, title, description,
		nullID(by.UserID), by.Name, now, skill.ID); err != nil {
		return wrapWriteErr("point the skill at its version", err)
	}
	return nil
}

// naming is what a skill should be CALLED once versionID is the live one.
//
// The rule, in one sentence: a skill's title and description follow the live
// version until somebody edits them, and after that they are theirs.
//
// Which of the two it is needs no column, because THE VERSION THAT WAS LIVE
// answers it. If the row still says exactly what that version said, nobody has
// been here; if it does not, somebody wrote those words on purpose and neither
// an import nor a rollback may take them away. A `title_edited` flag would say
// the same thing and would be a second fact to keep true.
//
// A new skill falls out of the same rule rather than being a case of its own:
// nothing was live, so what it was named after is two empty strings, which is
// exactly what its row holds, so the first version's words are taken.
func naming(ctx context.Context, tx *sqldb.Tx, skill *model.Skill, versionID int64) (string, string, error) {
	wasTitle, wasDescription, err := versionNaming(ctx, tx, skill.ActiveVersionID)
	if err != nil {
		return "", "", err
	}
	if skill.Title != wasTitle || skill.Description != wasDescription {
		return skill.Title, skill.Description, nil
	}
	return versionNaming(ctx, tx, versionID)
}

// versionNaming reads what one version calls itself.
//
// Zero, and a row that has gone, both answer with empty strings rather than an
// error, and that is the safe direction: an unreadable previous version makes
// the comparison above see a difference, so the words already on the skill are
// KEPT. Losing the chance to refresh a title is much better than overwriting
// one somebody chose.
func versionNaming(ctx context.Context, tx *sqldb.Tx, versionID int64) (string, string, error) {
	if versionID == 0 {
		return "", "", nil
	}
	var title, description string
	err := tx.QueryRowContext(ctx,
		`SELECT title, description FROM ai_skill_versions WHERE id = ?`, versionID).
		Scan(&title, &description)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read what a skill version calls itself: %w", err)
	}
	return title, description, nil
}

// --- reading ---------------------------------------------------------------------

// The active version rides on the skill, because a list of skills with no
// version on them is a list that cannot say what any of them is: which version,
// how many files, imported when.
//
// The title and the description are read TWICE and they are two different
// facts. The skill's own are what a person sees and searches on; the version's
// are what that package said of itself, which is what the version history is
// for. They are equal until somebody renames the skill, and the whole point of
// the rename is that they are then allowed to differ.
const selectSkills sqldb.Statement = `
	SELECT s.id, s.workspace_id, s.name, s.title, s.description,
	       COALESCE(s.created_by, 0), s.created_by_name,
	       COALESCE(s.updated_by, 0), s.updated_by_name,
	       COALESCE(s.active_version_id, 0), s.status, s.created_at, s.updated_at,
	       (SELECT COUNT(*) FROM ai_skill_versions n WHERE n.skill_id = s.id),
	       (SELECT COUNT(*) FROM ai_skill_files f WHERE f.version_id = s.active_version_id),
	       (SELECT COUNT(*) FROM ai_skill_sections x WHERE x.version_id = s.active_version_id),
	       v.id, COALESCE(v.parent_version_id, 0), v.version_number, v.source, v.status,
	       COALESCE(v.created_by, 0), COALESCE(v.created_by_name, ''),
	       COALESCE(v.title, ''), COALESCE(v.description, ''),
	       v.change_summary, v.package_sha256, v.validated_at, v.activated_at, v.created_at
	  FROM ai_skills s
	  LEFT JOIN ai_skill_versions v ON v.id = s.active_version_id
	`

func (s *skillStore) Skills(ctx context.Context, workspaceID int64) ([]*model.Skill, error) {
	rows, err := s.db.QueryContext(ctx,
		selectSkills+` WHERE s.workspace_id = ? ORDER BY s.name`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	skills := []*model.Skill{}
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		skills = append(skills, skill)
	}
	return skills, rows.Err()
}

// Search finds skills by what they are called and what they are for.
//
// The full-text index (`ft_skill`) covers the handle, the title and the
// description, so all three are one question: somebody who read a SKILL.md will
// type the handle, and somebody who did not will type a word out of the
// description.
//
// It answers with whole skills rather than with hits, because the console draws
// the matches as the skills pane, and a hit type would be the same row with
// fewer columns on it. Ordered by relevance, and by name where relevance ties,
// so the order is stable rather than the database's.
//
// It does NOT reach the passages of a package. `ai_skill_sections` has its own
// full-text index for that, and finding a skill by its name is a different
// question from finding a paragraph by what it says.
func (s *skillStore) Search(ctx context.Context, workspaceID int64, query string, limit int) ([]*model.Skill, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	term := booleanTerm(query)
	if term == "" {
		// Everything typed was below the index's token size. Nothing, rather
		// than everything, which is what an empty MATCH would return.
		return []*model.Skill{}, nil
	}

	rows, err := s.db.QueryContext(ctx,
		selectSkills+` WHERE s.workspace_id = ?
		    AND MATCH(s.name, s.title, s.description) AGAINST (? IN BOOLEAN MODE)
		  ORDER BY MATCH(s.name, s.title, s.description) AGAINST (? IN BOOLEAN MODE) DESC,
		           s.name
		  LIMIT ?`, workspaceID, term, term, limit)
	if err != nil {
		return nil, fmt.Errorf("search skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	skills := []*model.Skill{}
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		skills = append(skills, skill)
	}
	return skills, rows.Err()
}

func (s *skillStore) Skill(ctx context.Context, workspaceID, id int64) (*model.Skill, error) {
	row := s.db.QueryRowContext(ctx,
		selectSkills+` WHERE s.workspace_id = ? AND s.id = ?`, workspaceID, id)
	skill, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return skill, err
}

// One scan serves the list and the single read (through the package's own
// `scanner`, a row from either), so the two cannot drift apart.
func scanSkill(row scanner) (*model.Skill, error) {
	skill := &model.Skill{}
	var (
		versionID, parent, number     sql.NullInt64
		author                        sql.NullInt64
		authorName                    sql.NullString
		source, status, hash, summary sql.NullString
		versionTitle, versionAbout    sql.NullString
		validated, activated, created sql.NullTime
	)
	if err := row.Scan(
		&skill.ID, &skill.WorkspaceID, &skill.Name, &skill.Title, &skill.Description,
		&skill.CreatedBy, &skill.CreatedByName,
		&skill.UpdatedBy, &skill.UpdatedByName,
		&skill.ActiveVersionID, &skill.Status, &skill.CreatedAt, &skill.UpdatedAt,
		&skill.Versions, &skill.Files, &skill.Sections,
		&versionID, &parent, &number, &source, &status, &author, &authorName,
		&versionTitle, &versionAbout,
		&summary, &hash, &validated, &activated, &created,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan skill: %w", err)
	}
	// No active version is a real state, not a missing row: a skill whose only
	// version was rejected has an identity and nothing to run.
	if versionID.Valid {
		skill.Version = &model.SkillVersion{
			ID:              versionID.Int64,
			SkillID:         skill.ID,
			ParentVersionID: parent.Int64,
			Number:          int(number.Int64),
			Source:          source.String,
			Status:          status.String,
			// The VERSION's own words, not the skill's. A renamed skill running
			// this version must still show what the package called itself.
			Title:         versionTitle.String,
			Description:   versionAbout.String,
			ChangeSummary: summary.String,
			PackageSHA256: hash.String,
			ValidatedAt:   when(validated),
			ActivatedAt:   when(activated),
			CreatedAt:     created.Time,
			Files:         skill.Files,
			Sections:      skill.Sections,
		}
		skill.Version.Made(model.Actor{UserID: author.Int64, Name: authorName.String})
	}
	return skill, nil
}

// One definition of what a version row IS, used by the list and by the single
// read. Two copies of a column list and a scan is how one of them comes to be
// missing a column that was added to the other.
const versionColumns sqldb.Statement = `
	    v.id, v.skill_id, COALESCE(v.parent_version_id, 0), v.version_number,
	    v.source, v.status, COALESCE(v.created_by, 0), v.created_by_name,
	    v.title, v.description, v.change_summary, v.package_sha256,
	    v.validated_at, v.activated_at, v.created_at,
	    (SELECT COUNT(*) FROM ai_skill_files f WHERE f.version_id = v.id),
	    (SELECT COUNT(*) FROM ai_skill_sections x WHERE x.version_id = v.id)
	  FROM ai_skill_versions v
	  JOIN ai_skills s ON s.id = v.skill_id
	`

// scanner (jobs.go) is whatever a row came from, so one scan serves Query and
// QueryRow.
func scanVersion(row scanner) (*model.SkillVersion, error) {
	v := &model.SkillVersion{}
	var (
		summary              sql.NullString
		validated, activated sql.NullTime
	)
	if err := row.Scan(&v.ID, &v.SkillID, &v.ParentVersionID, &v.Number,
		&v.Source, &v.Status, &v.CreatedBy, &v.CreatedByName,
		&v.Title, &v.Description, &summary, &v.PackageSHA256,
		&validated, &activated, &v.CreatedAt, &v.Files, &v.Sections); err != nil {
		return nil, err
	}
	v.ChangeSummary = summary.String
	v.ValidatedAt, v.ActivatedAt = when(validated), when(activated)
	return v, nil
}

func (s *skillStore) Versions(ctx context.Context, workspaceID, skillID int64) ([]*model.SkillVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+versionColumns+
			` WHERE s.workspace_id = ? AND v.skill_id = ?
		  ORDER BY v.version_number DESC`, workspaceID, skillID)
	if err != nil {
		return nil, fmt.Errorf("list skill versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	versions := []*model.SkillVersion{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill version: %w", err)
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// version is one of them, scoped the same way.
func (s *skillStore) version(ctx context.Context, workspaceID, skillID, id int64) (*model.SkillVersion, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx,
		`SELECT `+versionColumns+
			` WHERE s.workspace_id = ? AND v.skill_id = ? AND v.id = ?`,
		workspaceID, skillID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read skill version: %w", err)
	}
	return v, nil
}

// Every file read is scoped through its version to its skill to the workspace.
// A file id is not a capability: it is only reachable by somebody whose token
// speaks for the workspace the skill is in.
const fromSkillFiles sqldb.Statement = `
	  FROM ai_skill_files f
	  JOIN ai_skill_versions v ON v.id = f.version_id
	  JOIN ai_skills s ON s.id = v.skill_id
	`

// Files is ordered by the path in BYTE order, not the column's.
//
// The column's collation is case-insensitive, which leaves two files differing
// only in case with no defined order between them, and sorts SKILL.md after
// every lowercase directory. Byte order is total, and it is the same on any
// server whatever collation it was created with.
func (s *skillStore) Files(ctx context.Context, workspaceID, versionID int64) ([]*model.SkillFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.id, f.skill_id, f.version_id, f.path, f.file_type, COALESCE(f.mime_type, ''),
		        f.size_bytes, f.sha256, f.created_at,
		        f.binary_content IS NOT NULL,
		        (SELECT COUNT(*) FROM ai_skill_sections x WHERE x.file_id = f.id)`+
			fromSkillFiles+
			` WHERE s.workspace_id = ? AND f.version_id = ? ORDER BY BINARY f.path`,
		workspaceID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list skill files: %w", err)
	}
	defer func() { _ = rows.Close() }()

	files := []*model.SkillFile{}
	for rows.Next() {
		f := &model.SkillFile{}
		if err := rows.Scan(&f.ID, &f.SkillID, &f.VersionID, &f.Path, &f.FileType, &f.MIMEType,
			&f.SizeBytes, &f.SHA256, &f.CreatedAt, &f.Binary, &f.Sections); err != nil {
			return nil, fmt.Errorf("scan skill file: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *skillStore) File(ctx context.Context, workspaceID, fileID int64) (*model.SkillFile, error) {
	f := &model.SkillFile{}
	var text sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT f.id, f.skill_id, f.version_id, f.path, f.file_type, COALESCE(f.mime_type, ''),
		        f.text_content, f.binary_content, f.size_bytes, f.sha256, f.created_at,
		        f.binary_content IS NOT NULL,
		        (SELECT COUNT(*) FROM ai_skill_sections x WHERE x.file_id = f.id)`+
			fromSkillFiles+
			` WHERE s.workspace_id = ? AND f.id = ?`,
		workspaceID, fileID).Scan(&f.ID, &f.SkillID, &f.VersionID, &f.Path, &f.FileType, &f.MIMEType,
		&text, &f.Bytes, &f.SizeBytes, &f.SHA256, &f.CreatedAt, &f.Binary, &f.Sections)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	f.Text = text.String
	return f, nil
}

func (s *skillStore) Sections(ctx context.Context, workspaceID, versionID int64) ([]*model.SkillSection, error) {
	// LENGTH rather than the body itself. A version's passages are a list
	// somebody is scanning to see what the parser made of the package; reading
	// one means opening the file at the lines it names.
	rows, err := s.db.QueryContext(ctx,
		`SELECT x.id, x.skill_id, x.version_id, x.file_id, COALESCE(x.heading, ''),
		        COALESCE(x.section_path, ''), COALESCE(x.line_start, 0),
		        COALESCE(x.line_end, 0), x.sequence_no, LENGTH(x.body),
		        x.sha256, x.created_at, f.path
		   FROM ai_skill_sections x
		   JOIN ai_skill_files f ON f.id = x.file_id
		   JOIN ai_skill_versions v ON v.id = x.version_id
		   JOIN ai_skills s ON s.id = v.skill_id
		  WHERE s.workspace_id = ? AND x.version_id = ?
		  ORDER BY x.sequence_no`, workspaceID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list skill sections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sections := []*model.SkillSection{}
	for rows.Next() {
		section := &model.SkillSection{}
		if err := rows.Scan(&section.ID, &section.SkillID, &section.VersionID, &section.FileID,
			&section.Heading, &section.Path, &section.LineStart, &section.LineEnd,
			&section.Sequence, &section.SizeBytes, &section.SHA256,
			&section.CreatedAt, &section.File); err != nil {
			return nil, fmt.Errorf("scan skill section: %w", err)
		}
		sections = append(sections, section)
	}
	return sections, rows.Err()
}

// --- administering ---------------------------------------------------------------

// Update is everything about a skill a person decides: what it is called, what
// it is for, whether it is switched on, and which version is live.
//
// One method and ONE TRANSACTION, because they are one form. Saving a rename
// and a rollback as two writes is how a dialog half lands, and these two halves
// are a bad pair to lose: a skill left running a version nobody asked for under
// a name nobody chose.
//
// The rollback goes through `activate`, which is still the only writer of which
// version is live. That matters here rather than being tidiness: `activate` also
// moves the skill's words to follow the new version, and the statement after it
// writes what the FORM said, so a person who typed a name keeps it and one who
// did not gets the version's. The order is the rule.
//
// What it CANNOT change is the handle (the package's, and the agent addresses
// the skill by it), any version's files, or any version's own words.
func (s *skillStore) Update(ctx context.Context, workspaceID, id int64, in store.SkillUpdate, by model.Actor) (*model.Skill, error) {
	switch in.Status {
	case model.SkillDraft, model.SkillActive, model.SkillDisabled, model.SkillArchived:
	default:
		// Refused here and not left to the column: an enum rejecting a value
		// reads as a database error, and this is a caller passing something that
		// is not a status.
		return nil, fmt.Errorf("update skill: %q is not a status", in.Status)
	}

	err := s.db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		skill, err := lockedSkill(ctx, tx,
			`SELECT `+lockedSkillColumns+` FROM ai_skills
			  WHERE id = ? AND workspace_id = ? FOR UPDATE`, id, workspaceID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		title, description := in.Title, in.Description

		if in.VersionID != 0 && in.VersionID != skill.ActiveVersionID {
			// The version must be this skill's own. Without this check a version
			// id belonging to another skill (or another workspace) would be made
			// live here, and the pointer would leave the skill it points from.
			if err := requireExists(ctx, tx, "make skill version live",
				`SELECT 1 FROM ai_skill_versions WHERE id = ? AND skill_id = ?`,
				in.VersionID, id); err != nil {
				return err
			}

			// A form ALWAYS sends every field, including the ones nobody
			// touched, so "the form said this title" is not the same statement
			// as "somebody chose this title". They are told apart the same way
			// `naming` tells them apart on the row: if what arrived still equals
			// what is stored, nobody typed in that box, and the rollback decides
			// the words under the ordinary rule.
			//
			// Without this the rule would be dead in practice rather than in
			// principle: every rollback comes from a form, every form echoes the
			// current name back, and so every rollback would pin the name and no
			// skill would ever follow its live version again.
			if in.Title == skill.Title && in.Description == skill.Description {
				title, description, err = naming(ctx, tx, skill, in.VersionID)
				if err != nil {
					return err
				}
			}
			if err := activate(ctx, tx, skill, in.VersionID, by, now); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE ai_skills
			    SET title = ?, description = ?, status = ?,
			        updated_by = ?, updated_by_name = ?, updated_at = ?
			  WHERE id = ? AND workspace_id = ?`,
			title, description, in.Status,
			nullID(by.UserID), by.Name, now, id, workspaceID); err != nil {
			return wrapWriteErr("update skill", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Skill(ctx, workspaceID, id)
}

// DeleteSkill takes its versions, their files and their sections with it. The
// database enforces that, not this function (KB/14).
func (s *skillStore) DeleteSkill(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_skills WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	return requireAffected(res, "delete skill")
}

// when turns a nullable timestamp into a pointer, so "never validated" and
// "validated at the zero time" are not the same thing.
func when(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	moment := t.Time
	return &moment
}
