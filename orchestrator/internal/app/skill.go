package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
	"flexie.io/sag/internal/store"
)

// Skills, as a screen and as an import.
//
// This is the seam: the API layer knows nothing about zip archives or the
// package format, and the store knows nothing about where a package came from.
// What joins them is here.

// SkillUpload is one archive offered for import.
//
// Name is what the sender called the file. It exists to report WHICH archive
// was refused, since a refused one has no skill name to be known by: it could
// not be read. It never reaches a path or a row; the name a skill is stored
// under comes from the manifest inside it.
type SkillUpload struct {
	Name    string
	Archive io.ReaderAt
	Size    int64
}

// What one archive's import did. Three outcomes, and a person reading a
// summary needs all three to be told apart: a version landed, the identical
// package was already here, or the archive is not a skill package.
const (
	SkillImported  = "imported"
	SkillUnchanged = "unchanged"
	SkillRefused   = "refused"
)

// SkillImport is what happened to one archive.
//
// Reason is set only on a refusal, and it is the sender's to act on ("scripts/
// run.sh is a symbolic link"). Skill is set on everything else, and carries the
// version, so "imported as version 3" can be said without another request.
type SkillImport struct {
	File   string       `json:"file"`
	Status string       `json:"status"`
	Reason string       `json:"reason,omitempty"`
	Skill  *model.Skill `json:"skill,omitempty"`
}

// ImportSkills reads the uploaded packages and stores each as a version.
//
// The order matters and is the whole design: a package is validated and parsed
// COMPLETELY before its transaction opens, so a bad archive costs one refusal
// and no rows, and a good one is written in a single transaction that either
// lands whole or leaves nothing. Nothing in an archive is executed, and nothing
// is written to a path an archive named.
//
// A batch is that, in a loop, and the loop is the point: one archive that is
// not a skill package must not cost the nine good ones beside it. Every error
// the reader can produce is a Rejection (it has no other error constructor), so
// a refusal is always recorded and the loop always continues.
//
// A STORE failure is different and stops the batch. It is ours, not the
// sender's, it is not describable to them, and if the database has gone then
// every remaining archive would fail the same way. Whatever landed before it
// stays landed: each import is its own transaction, and the screen shows them
// on the next read.
func (a *App) ImportSkills(ctx context.Context, workspaceID int64, uploads []SkillUpload, by model.Actor) ([]SkillImport, error) {
	results := make([]SkillImport, 0, len(uploads))
	for _, upload := range uploads {
		pkg, err := skill.Read(upload.Archive, upload.Size)
		if err != nil {
			var rejection *skill.Rejection
			if errors.As(err, &rejection) {
				results = append(results, SkillImport{
					File:   upload.Name,
					Status: SkillRefused,
					Reason: rejection.Reason,
				})
				continue
			}
			return nil, fmt.Errorf("read %s: %w", upload.Name, err)
		}

		stored, added, err := a.Store.Skills().Import(ctx, workspaceID, pkg, by)
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", upload.Name, err)
		}
		status := SkillUnchanged
		if added {
			status = SkillImported
		}
		results = append(results, SkillImport{File: upload.Name, Status: status, Skill: stored})
	}
	return results, nil
}

// SkillEdit is one save: which version was being edited, and the new contents
// of the files somebody changed.
type SkillEdit struct {
	// From is the version the edits were written against. Sent by the client
	// rather than assumed to be the live one, because a person may open an
	// older version and edit that, and because it is what makes a save safe: an
	// edit written against version 2 must not be quietly applied to version 5
	// if somebody published one in the meantime.
	From  int64
	Edits []skill.Edit
}

// DraftSkillVersion saves an edit as a new version that is not live.
//
// The two halves: skill.Rewrite turns the version's files plus the edits into a
// package, holding it to every rule an imported one is held to, and the store
// writes it as a draft. What comes back is the version and whether anything was
// written, which is the difference between "saved as version 4" and "that is
// what version 2 already says".
//
// A refusal from the format reader comes back as a Rejection, the same type an
// import answers with, so the screen tells somebody what is wrong with their
// edit in the same words it tells them what is wrong with their zip.
func (a *App) DraftSkillVersion(ctx context.Context, workspaceID, skillID int64,
	in SkillEdit, by model.Actor,
) (*model.SkillVersion, []string, error) {
	held, err := a.Store.Skills().Skill(ctx, workspaceID, skillID)
	if err != nil {
		return nil, nil, err
	}
	from := in.From
	if from == 0 {
		// Nothing said, so the live one, which is what a client that has only
		// ever shown the live version means.
		from = held.ActiveVersionID
	}

	// Every file of that version, contents included: a new version holds all of
	// them, and the ones nobody edited are carried forward as they are.
	listed, err := a.Store.Skills().Files(ctx, workspaceID, from)
	if err != nil {
		return nil, nil, fmt.Errorf("list the version's files: %w", err)
	}
	if len(listed) == 0 {
		return nil, nil, store.ErrNotFound
	}
	current := make([]model.PackageFile, 0, len(listed))
	for _, f := range listed {
		// A LIST carries no content (a package may hold a twenty megabyte
		// template), so each file is read for its own bytes.
		whole, err := a.Store.Skills().File(ctx, workspaceID, f.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f.Path, err)
		}
		current = append(current, model.PackageFile{
			Path: f.Path, FileType: f.FileType, MIMEType: f.MIMEType,
			Text: whole.Text, Bytes: whole.Bytes, Size: f.SizeBytes, SHA256: f.SHA256,
		})
	}

	pkg, changed, err := skill.Rewrite(held.Name, current, in.Edits)
	if err != nil {
		return nil, nil, err
	}
	if len(changed) == 0 {
		// Said here rather than left to the store's idempotency, because the two
		// are different answers: the store would hand back the version that
		// already holds these bytes, which for an unchanged save is the live one,
		// and a screen would report "saved as version 2" about a save that did
		// nothing.
		return nil, nil, &skill.Rejection{Reason: "nothing changed, so there is nothing to save"}
	}

	version, added, err := a.Store.Skills().Draft(ctx, workspaceID, skillID, from, pkg,
		summarise(changed), by)
	if err != nil {
		return nil, nil, err
	}
	if !added {
		// The bytes are a version this skill already holds: somebody has edited
		// their way back to one that exists, which is not a new version of
		// anything and is worth saying plainly.
		return version, changed, &skill.Rejection{Reason: fmt.Sprintf(
			"that is what version %d already says, so nothing was saved", version.Number)}
	}
	return version, changed, nil
}

// DiscardSkillVersion turns a draft down.
func (a *App) DiscardSkillVersion(ctx context.Context, workspaceID, skillID, versionID int64, by model.Actor) error {
	return a.Store.Skills().Discard(ctx, workspaceID, skillID, versionID, by)
}

// summarise is what a person reading the history later is told changed.
//
// The paths, because that is the fact. A field for somebody to type a message
// into was considered and left out: it is one more thing to fill in on every
// save, it is empty most of the time, and what it would say is already here.
func summarise(changed []string) string {
	switch len(changed) {
	case 1:
		return "edited " + changed[0]
	case 2:
		return "edited " + changed[0] + " and " + changed[1]
	default:
		return fmt.Sprintf("edited %s and %d more", changed[0], len(changed)-1)
	}
}

// SkillSelection is what the caller says they are looking at. Either may be
// zero, which means "you decide".
type SkillSelection struct {
	SkillID   int64
	VersionID int64
}

// SkillOverview is the whole screen in one answer: the skills, the one selected,
// its version history, and the files and passages of the version being read.
//
// One request for the same reason the brains screen has one (KB/19): a client
// that asks which skills exist, then which versions, then which files, then
// which passages has four round trips to paint one page and three empty panes
// while it waits.
type SkillOverview struct {
	Skills  []*model.Skill `json:"skills"`
	SkillID int64          `json:"skill_id"`
	Skill   *model.Skill   `json:"skill"`
	// Versions is the history, newest first. Which one is live is on the rows.
	Versions  []*model.SkillVersion `json:"versions"`
	VersionID int64                 `json:"version_id"`
	// Files and Sections belong to VersionID, which is not always the live one:
	// reading an old version is how you see what changed.
	Files    []*model.SkillFile    `json:"files"`
	Sections []*model.SkillSection `json:"sections"`
	// Manifest is the version's SKILL.md, WITH its text, because it is what the
	// screen opens by default: a skill is its instructions, and landing on a
	// list of files means everybody's first click is the same one.
	//
	// It rides on the screen's own answer rather than being fetched beside it,
	// which is the rule this endpoint already follows (KB/19): the default state
	// of a screen is part of the screen. Every OTHER file is a new question and
	// costs its own request.
	Manifest *model.SkillFile `json:"manifest"`
}

// SkillView resolves a selection into everything needed to draw it.
//
// A selection naming something gone is not an error: it is a stale link or a
// back button, and it lands on the default the way asking for nothing does.
func (a *App) SkillView(ctx context.Context, workspaceID int64, want SkillSelection) (*SkillOverview, error) {
	skills, err := a.Store.Skills().Skills(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	view := &SkillOverview{
		Skills:   skills,
		Versions: []*model.SkillVersion{},
		Files:    []*model.SkillFile{},
		Sections: []*model.SkillSection{},
	}
	if len(skills) == 0 {
		return view, nil
	}

	view.SkillID = firstOr(want.SkillID, skills, func(s *model.Skill) int64 { return s.ID })
	for _, s := range skills {
		if s.ID == view.SkillID {
			view.Skill = s
		}
	}

	versions, err := a.Store.Skills().Versions(ctx, workspaceID, view.SkillID)
	if err != nil {
		return nil, fmt.Errorf("list skill versions: %w", err)
	}
	view.Versions = versions
	if len(versions) == 0 {
		return view, nil
	}

	// The default is the LIVE version, not the newest, and the difference shows
	// after a rollback: the skill is running version 2 while version 5 sits at
	// the top of the list, and opening the screen must show what is running.
	view.VersionID = view.Skill.ActiveVersionID
	for _, v := range versions {
		if v.ID == want.VersionID {
			view.VersionID = want.VersionID
		}
	}
	if view.VersionID == 0 {
		view.VersionID = versions[0].ID
	}

	if view.Files, err = a.Store.Skills().Files(ctx, workspaceID, view.VersionID); err != nil {
		return nil, fmt.Errorf("list skill files: %w", err)
	}
	if view.Sections, err = a.Store.Skills().Sections(ctx, workspaceID, view.VersionID); err != nil {
		return nil, fmt.Errorf("list skill sections: %w", err)
	}

	// The manifest, read in full. A version without one cannot exist (the import
	// refuses a package that has none), but an empty answer is still handled
	// rather than assumed: this is a read of a row, and rows can be deleted.
	for _, file := range view.Files {
		if file.Path != skill.Manifest {
			continue
		}
		manifest, err := a.Store.Skills().File(ctx, workspaceID, file.ID)
		if err != nil {
			return nil, fmt.Errorf("read the manifest: %w", err)
		}
		view.Manifest = manifest
		break
	}
	return view, nil
}

// SkillFile is one file of one version, with its content, scoped to the
// workspace by the store.
func (a *App) SkillFile(ctx context.Context, workspaceID, fileID int64) (*model.SkillFile, error) {
	file, err := a.Store.Skills().File(ctx, workspaceID, fileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	return file, nil
}
