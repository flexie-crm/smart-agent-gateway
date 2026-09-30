import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Download,
  FileArchive,
  FileCode2,
  FileText,
  Image as ImageIcon,
  Layers,
  Package,
  Pencil,
  Upload,
  X,
} from "lucide-react";
import { Page } from "@/components/AppShell";
import { Badge } from "@/components/DataTable";
import { Empty, Pane, Row, SearchBand } from "@/components/Pane";
import { CodeEditor, CodeView, warmCodeView } from "@/components/CodeView";
import { Markdown } from "@/components/Markdown";
import { Button } from "@/components/ui/button";
import { CheckboxField } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Field, Modal } from "@/components/ui/modal";
import { NativeSelect } from "@/components/ui/native-select";
import { Textarea } from "@/components/ui/textarea";
import { useAuth } from "@/lib/auth";
import { describeFile } from "@/lib/filekind";
import { describeError, useFormErrors } from "@/lib/form";
import { useNotify } from "@/lib/notify";
import { summarise } from "@/lib/search";
import {
  anythingLanded,
  fileCount,
  importSummary,
  isMarkdown,
  label,
  passageCount,
  readableSize,
  save,
  skills,
  withoutFrontmatter,
} from "@/lib/skills";
import type {
  ImportOutcome,
  Skill,
  SkillFile,
  SkillSelection,
  SkillVersion,
  SkillView,
} from "@/lib/skills";
import { cn } from "@/lib/utils";

/**
 * Skills: a package somebody wrote, imported and kept whole.
 *
 * The screen is what was STORED, not a summary of it. Every file of the version
 * is here with its content, and so is every passage the parser made of it, with
 * the lines it came from. That is deliberate: an import that silently dropped a
 * file or read a code block as four headings would look exactly like a
 * successful one on a screen that only showed a name and a description.
 *
 * One address, one answer, selection as screen state, the same model the brains
 * screen uses (KB/19).
 */
export function Skills() {
  const [view, setView] = useState<SkillView | null>(null);
  // What the reader has been TOLD to show, and null until somebody says.
  //
  // Null is not "nothing": it means nobody has chosen, and the default is the
  // manifest, which arrives on the screen's own answer. A file carries its
  // version, so opening another version does not have to "clear" the choice:
  // the file simply is not one of that version's, and the default applies again.
  const [chosen, setChosen] = useState<
    | {
        kind: "file";
        file: SkillFile;
        /** Which lines to mark, when a passage was what opened this file. */
        highlight: { from: number; to: number } | null;
      }
    | { kind: "passages" }
    | null
  >(null);
  // Markdown opens RENDERED: a skill is prose somebody wrote, and landing on its
  // source is landing on the punctuation. Source is a click away, and it is the
  // file exactly as it was stored, frontmatter and all.
  const [rendered, setRendered] = useState(true);
  const [importing, setImporting] = useState(false);
  // The files open in the editor, by path: what is in the box, and what the
  // file said when it was opened.
  //
  // Held on the SCREEN rather than in the reader, for two reasons. Editing two
  // files and saving once is one version, which is what somebody means by a
  // change; and clicking away to another file must not throw away what they
  // typed in this one.
  //
  // `was` is why both are kept. A file that is OPEN in the editor is not a file
  // that has been edited, and without the original there is no way to tell: the
  // bar would announce an unsaved change the moment somebody clicked Edit, and
  // Save would offer to write a version identical to the live one.
  //
  // `against` is the version they were editing when they started. A save is
  // written against it rather than against whatever is live by then.
  const [edits, setEdits] = useState<
    Record<string, { text: string; was: string }>
  >({});
  const [against, setAgainst] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  // Which skill's own form is open. Null is closed; a skill is the one being
  // edited. There is no "new skill" case here, because the only way one comes
  // into existence is by importing a package.
  const [editing, setEditing] = useState<Skill | null>(null);

  // The search. `query` is what has been typed and `hits` is the last answer
  // the server gave; null means there is no answer to narrow by, which is a
  // different thing from an answer that found nothing.
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<Skill[] | null>(null);
  // Whether a search is on is the BOX, not the answer: an empty box is every
  // skill, whatever the last answer happened to say.
  const searching = query.trim() !== "";
  const notify = useNotify();
  const { can } = useAuth();
  // Two rights, and they are different acts: writing a version is what
  // importing one is, and choosing which version the agent uses is what a
  // rollback is (internal/api/skill.go).
  const mayWrite = can("skills:create");
  const mayPublish = can("skills:edit");

  // Only the latest question may paint: click twice quickly and the slower
  // answer must not overwrite the faster one. What is on screen stays until the
  // new answer lands, so the panes never blank between clicks.
  const asked = useRef(0);
  const show = useCallback(async (want: SkillSelection = {}) => {
    const question = ++asked.current;
    const next = await skills.view(want);
    if (question !== asked.current) return;
    setView(next);
    // The reader starts again, because asking for a screen is arriving at one:
    // a different skill is a different thing, and keeping the open file or the
    // passages view would put one skill's name over another's contents.
    //
    // Here rather than at each place a skill is opened, for two reasons. It
    // cannot be forgotten by the next caller. And it lands WITH the answer, so
    // there is no moment where the old view is still on screen and the reader
    // has already reset: that moment is what an earlier test for this was
    // accidentally measuring, and it passed on code that did not hold
    // afterwards.
    setChosen(null);
  }, []);

  // tell turns a failed request into something a person reads. Every click that
  // asks the server goes through it, so a refusal says so instead of looking
  // like a click that did nothing.
  //
  // It is here rather than inside `show` because a screen has exactly one way
  // to report to a person and `show` is also what the first load calls, when
  // there is no screen yet to put a toast in front of.
  const tell = useCallback(
    async (work: Promise<unknown>) => {
      try {
        await work;
      } catch (failure) {
        notify.error(describeError(failure));
      }
    },
    [notify],
  );

  // The first load. A failure is HELD rather than announced: until it answers,
  // "the gateway did not answer" is the whole content of the page.
  const [trouble, setTrouble] = useState("");
  useEffect(() => {
    let live = true;
    void show({})
      .then(() => live && setTrouble(""))
      .catch((failure: unknown) => live && setTrouble(describeError(failure)));
    return () => {
      live = false;
    };
  }, [show]);

  // Search as it is typed, debounced: a keystroke is not a question, a pause
  // is. Only the latest answer may land, the same rule the view follows, since
  // a slower answer to a shorter query would narrow the list by a word the
  // person has already finished typing.
  //
  // An empty box asks nothing and CLEARS nothing. What is held is the last
  // answer, and whether a search is running is read off the box, so clearing it
  // needs no state to be put back.
  const searched = useRef(0);
  useEffect(() => {
    const text = query.trim();
    const question = ++searched.current;
    if (text === "") return;
    const timer = window.setTimeout(() => {
      void (async () => {
        try {
          const found = await skills.search(text);
          if (question === searched.current) setHits(found);
        } catch (failure) {
          // A search that failed narrows NOTHING: the answer is thrown away, so
          // the list holds every skill and the toast says why. An empty one
          // would claim there are no matches, which is the thing we just failed
          // to find out.
          if (question === searched.current) setHits(null);
          notify.error(describeError(failure));
        }
      })();
    }, 250);
    return () => window.clearTimeout(timer);
  }, [query, notify]);

  // Which skill the server says is open. Read off the view rather than the
  // list below, because the effect under it runs before that list is worked
  // out and must not depend on it.
  const openSkillID = view?.skill_id ?? 0;

  // Opening a skill, wherever the instruction came from: a click on its row, or
  // a search that landed on it.
  //
  // One function because it is one action, and the reader reset is part of it:
  // a different skill is a different thing, so it starts again at its manifest
  // rather than keeping whatever file was open. Switching VERSION does not
  // reset it, which is why this does not live in `show`: that is somebody
  // comparing, and they want to stay on what they were comparing.
  // Opening a skill, wherever the instruction came from: a click on its row, or
  // a search that landed on it. `show` resets the reader, so this is the whole
  // of it.
  const openSkill = useCallback(
    (id: number) => {
      // Fetch the highlighter now. Opening a skill is a click or two before its
      // first file is read, so by then the chunk is here and the reader is
      // never briefly empty.
      warmCodeView();
      void tell(show({ skill: id }));
    },
    [show, tell],
  );

  // A search opens its best match when what is on screen is not one of them.
  //
  // The list narrows to what matched, so a selection that is not among the hits
  // would leave the pane highlighting a row it no longer shows. Once per answer,
  // by identity: a view that comes back pointing somewhere else must not become
  // the same question asked forever.
  const opened = useRef<Skill[] | null>(null);
  useEffect(() => {
    if (!searching || hits === null || hits.length === 0) return;
    if (opened.current === hits) return;
    opened.current = hits;
    if (hits.some((hit) => hit.id === openSkillID)) return;
    openSkill(hits[0].id);
  }, [searching, hits, openSkillID, openSkill]);

  // Opening another version of the open skill, keeping the skill selected.
  const openVersion = useCallback(
    (id: number) => void tell(show({ skill: openSkillID, version: id })),
    [show, tell, openSkillID],
  );

  const read = useRef(0);
  const openFile = useCallback(
    async (id: number, lines?: { from: number; to: number }) => {
      const question = ++read.current;
      try {
        const file = await skills.file(id);
        if (question !== read.current) return;
        setChosen({ kind: "file", file, highlight: lines ?? null });
        // Rendered when there is a rendered form, EXCEPT when a passage is what
        // opened the file. A passage cites lines, and lines exist in the source:
        // rendering it would mark nothing and leave the citation pointing at a
        // file the reader cannot count through.
        setRendered(lines === undefined && isMarkdown(file.path));
      } catch (failure) {
        notify.error(describeError(failure));
      }
    },
    [notify],
  );

  // The view we hold IS the selection: the ids come back resolved, so nothing
  // is ever highlighted that the server did not open.
  const loading = view === null;
  const all = view?.skills ?? [];
  // Which skills matched, as a set of ids. The list drawn is the SCREEN's own
  // list narrowed by it, rather than the hits themselves: the screen's list is
  // the one that is up to date, so a skill deleted since the search stops being
  // listed instead of lingering as a row that opens nothing.
  const matched = useMemo(() => {
    if (!searching || hits === null) return null;
    return new Set(hits.map((hit) => hit.id));
  }, [searching, hits]);
  const shownSkills = matched
    ? all.filter((item) => matched.has(item.id))
    : all;
  // What the band says about the answer, and nothing until one lands: a count
  // is a claim about the data, and the data has not spoken yet.
  const summary = searching ? summarise(hits) : "";
  // Whether what the server has open is something the search matched.
  //
  // ONE gate, and everything about the open skill passes through it. A search
  // that matched nothing used to leave the last skill's package and its
  // SKILL.md standing in the two panes on the right while the pane on the left
  // said there was no match: the screen showed a skill and denied having one in
  // the same breath. The brains screen had this exact bug and fixed it; the
  // port left the fix behind.
  //
  // It is a question about the OPEN skill rather than about the list, because a
  // narrowed list that does not contain what is open is exactly the state where
  // the panes are describing something invisible. And it is applied once, here,
  // rather than at each pane: three panes each deciding for themselves is how
  // one of them ends up disagreeing with the other two, which is the bug.
  const open = view?.skill ?? null;
  const shows = open !== null && (matched === null || matched.has(open.id));

  const skill = shows ? open : null;
  const versions = shows ? (view?.versions ?? []) : [];
  const versionID = shows ? (view?.version_id ?? 0) : 0;
  // Memoised because it is the input to the grouping below: a fresh [] on every
  // render would regroup the package on every keystroke anywhere on the page.
  const files = useMemo(() => (shows ? (view?.files ?? []) : []), [view, shows]);
  const sections = shows ? (view?.sections ?? []) : [];
  const version = versions.find((v) => v.id === versionID) ?? null;
  // Which versions may be edited: the live one, and a draft.
  //
  // An ARCHIVED one is deliberately not offered, even though the API would take
  // it. Editing version 1 when version 5 is live produces version 1 plus the
  // change, which silently undoes everything versions 2 to 5 did. That is the
  // correct behaviour of an edit written against a version, and a trap to offer
  // with one click: somebody browsing the history and fixing a typo would
  // publish a rollback without being told.
  //
  // Only two of the three are reachable from here today, because the only ways
  // to open a version for reading are the live one (the default) and Review on
  // the draft bar. The archived case is written down rather than left out so
  // that a screen which later grows a history browser does not have to
  // rediscover the reasoning; it carries no message, because a message nothing
  // can reach is text that cannot be tested.
  const editable =
    version !== null &&
    (version.status === "active" || version.status === "draft");
  // The draft waiting for a decision, if there is one. Newest first from the
  // server, so the first is the newest.
  const pending = versions.find((v) => v.status === "draft") ?? null;
  // Which files are open in the editor, and which of those actually differ.
  const inEditor = Object.keys(edits);
  const changed = inEditor.filter(
    (path) => edits[path].text !== edits[path].was,
  );

  // What the reader shows: what was chosen, or the manifest when nothing has
  // been. A chosen file belongs to a version, so choosing another version falls
  // back to that version's manifest rather than showing a file from the old one.
  const manifest = shows ? (view?.manifest ?? null) : null;
  // What is being read, or the version's manifest when nothing has been chosen.
  // A chosen file also carries the version it came from, so one belonging to a
  // version that is no longer on screen falls back rather than being drawn
  // under another package's heading.
  const standing =
    chosen === null ||
    (chosen.kind === "file" && chosen.file.version_id !== versionID)
      ? manifest
        ? ({ kind: "file", file: manifest, highlight: null } as const)
        : ({ kind: "passages" } as const)
      : chosen;
  const shownFile = standing.kind === "file" ? standing.file : null;
  const highlight = standing.kind === "file" ? standing.highlight : null;

  // Saving what was typed as a new version, which is not live.
  //
  // One request for every file edited, because they are one save: a person who
  // fixed a script and the instructions beside it made ONE change, and two
  // versions for it would be a history of their keystrokes.
  async function saveEdits() {
    if (skill === null || against === null) return;
    setSaving(true);
    try {
      const saved = await skills.draft(skill.id, {
        from: against,
        // Only what differs. A file somebody opened and did not touch is not
        // part of the change, and sending it would ask the server to write a
        // version whose file list it would have to compare anyway.
        edits: changed.map((path) => ({ path, text: edits[path].text })),
      });
      // Cleared only once it is stored: a save that failed must leave what
      // somebody typed exactly where it was.
      setEdits({});
      setAgainst(null);
      notify.success(
        `Saved as version ${saved.version.number}. It is a draft: publish it to let the agent use it.`,
      );
      // Onto the draft, because that is what somebody wants to look at before
      // deciding, and the screen is asked again rather than patched.
      await show({ skill: skill.id, version: saved.version.id });
    } catch (failure) {
      notify.error(describeError(failure));
    } finally {
      setSaving(false);
    }
  }

  // Publishing: the one route that makes a version live, the same one a
  // rollback uses. The skill's own words are sent back unchanged, which is what
  // tells the server nobody renamed anything in this act, so the published
  // package's own title and description apply under the ordinary rule.
  async function publish(draft: SkillVersion) {
    if (skill === null) return;
    const yes = await notify.confirm({
      title: `Publish version ${draft.number}?`,
      body: "Every agent holding this skill will use it from their next turn, and it will be put on the computer it runs on the first time a script is used.",
      confirmLabel: "Publish",
    });
    if (!yes) return;
    try {
      await skills.update(skill.id, {
        title: skill.title,
        description: skill.description,
        status: skill.status === "disabled" ? "disabled" : "active",
        version_id: draft.id,
      });
      notify.success(`Version ${draft.number} is live.`);
      await show({ skill: skill.id, version: draft.id });
    } catch (failure) {
      notify.error(describeError(failure));
    }
  }

  async function discard(draft: SkillVersion) {
    if (skill === null) return;
    const yes = await notify.confirm({
      title: `Discard version ${draft.number}?`,
      body: "The draft is turned down and the live version is untouched. What was in it is not recoverable from here.",
      confirmLabel: "Discard",
      destructive: true,
    });
    if (!yes) return;
    try {
      await skills.discard(skill.id, draft.id);
      notify.success(`Version ${draft.number} was discarded.`);
      await show({ skill: skill.id });
    } catch (failure) {
      notify.error(describeError(failure));
    }
  }

  // Files grouped by their directory, which is how the package was written. The
  // server orders by the path in byte order, so the manifest comes first and
  // each directory arrives together.
  const groups = useMemo(() => {
    const byFolder = new Map<string, SkillFile[]>();
    for (const file of files) {
      const slash = file.path.lastIndexOf("/");
      const folder = slash < 0 ? "" : file.path.slice(0, slash);
      byFolder.set(folder, [...(byFolder.get(folder) ?? []), file]);
    }
    return [...byFolder.entries()];
  }, [files]);

  // Deleting is a row's own action, and the row says what it asks and what it
  // reports (see Row). What is left here is only where the screen should land
  // afterwards: on the default when what was open has gone, and exactly where
  // it was when some other row was deleted.
  async function removeSkill(item: Skill) {
    await skills.remove(item.id);
    await show(item.id === skill?.id ? {} : { skill: skill?.id });
  }

  return (
    <Page
      title="Skills"
      description="Instructions and scripts, imported from a zip file. Every import is kept as its own version, and older versions are never changed."
      flush
      actions={
        <Button size="sm" onClick={() => setImporting(true)}>
          <Upload className="size-4" />
          Import skill
        </Button>
      }
    >
      <div className="flex h-full min-h-0 flex-col">
        {/* Full text search over every skill's handle, name and description.
            Its own band above the panes, because what is typed here narrows the
            list below rather than belonging to any one column. What used to sit
            here was a summary of whichever skill was open, which is what the
            rows and the panes already show. */}
        <SearchBand
          value={query}
          onChange={setQuery}
          placeholder="Search skills"
          label="Search skills"
          summary={summary}
        />

        {/* What is unsaved, and what is waiting to be published. Two bars and
            not one: edits in the browser and a version in the database are
            different things at different stages, and a single bar that meant
            both would have to say which. */}
        {inEditor.length > 0 && (
          <div className="flex shrink-0 items-center gap-3 border-b border-border bg-primary/5 px-4 py-2 text-sm">
            <span className="min-w-0 flex-1 truncate">
              {changed.length === 0
                ? // Open, and the same as it was. Said plainly rather than
                  // announcing an unsaved change nobody has made.
                  `Editing ${inEditor.join(", ")}.`
                : changed.length === 1
                  ? `${changed[0]} is edited and not saved.`
                  : `${changed.length} files are edited and not saved.`}
            </span>
            <Button
              size="sm"
              className="h-7 shrink-0 text-xs"
              // Nothing to save until something differs. A save of an unchanged
              // file would be refused by the server anyway, and a button that
              // asks to be pressed and then explains itself is worse than one
              // that waits.
              disabled={saving || changed.length === 0}
              onClick={() => void saveEdits()}
            >
              {saving ? "Saving…" : "Save as a new version"}
            </Button>
            <Button
              size="sm"
              variant="outline"
              className="h-7 shrink-0 text-xs"
              disabled={saving}
              // The one way out, for every file at once. There was a second
              // button in the reader's header for the file being looked at, and
              // it was one too many: this is the same act, and the bar is where
              // somebody is already looking.
              onClick={() => setEdits({})}
            >
              Cancel
            </Button>
          </div>
        )}
        {pending !== null && (
          <div className="flex shrink-0 items-center gap-3 border-b border-border bg-muted/40 px-4 py-2 text-sm">
            <span className="min-w-0 flex-1 truncate">
              <span className="font-medium">
                Version {pending.number} is a draft
              </span>
              {pending.created_by_name !== "" && ` by ${pending.created_by_name}`}
              {pending.change_summary !== "" && ` — ${pending.change_summary}`}
              . The agent is still using version {version?.number ?? "?"}.
            </span>
            {pending.id !== versionID && (
              <Button
                size="sm"
                variant="outline"
                className="h-7 shrink-0 text-xs"
                onClick={() => void openVersion(pending.id)}
              >
                Review
              </Button>
            )}
            {mayPublish && (
              <Button
                size="sm"
                className="h-7 shrink-0 text-xs"
                onClick={() => void publish(pending)}
              >
                Publish
              </Button>
            )}
            {mayWrite && (
              <Button
                size="sm"
                variant="outline"
                className="h-7 shrink-0 text-xs"
                onClick={() => void discard(pending)}
              >
                Discard
              </Button>
            )}
          </div>
        )}

        <div className="grid min-h-0 flex-1 grid-cols-[minmax(0,17rem)_minmax(0,19rem)_minmax(0,1fr)]">
          {/* The skills */}
          <Pane title="Skills">
            {trouble !== "" ? (
              <Empty>{trouble}</Empty>
            ) : loading ? null : shownSkills.length === 0 ? (
              <Empty>
                {matched ? (
                  // Nothing matched, which is a different statement from having
                  // no skills at all, and a screen that said the second would be
                  // telling somebody their packages had gone.
                  <>No skill matches &ldquo;{query.trim()}&rdquo;.</>
                ) : (
                  <>No skills yet. Import a zip file to add one.</>
                )}
              </Empty>
            ) : (
              shownSkills.map((item) => (
                <Row
                  key={item.id}
                  selected={item.id === skill?.id}
                  onClick={() => openSkill(item.id)}
                  icon={<Package className="size-4 text-primary" />}
                  title={
                    <span className="flex items-center gap-1.5">
                      <span className="min-w-0 truncate">{label(item)}</span>
                      {item.version && (
                        <span className="shrink-0 text-xs font-normal text-muted-foreground">
                          v{item.version.number}
                        </span>
                      )}
                      {item.status !== "active" && (
                        <Badge tone="warn">{item.status}</Badge>
                      )}
                    </span>
                  }
                  subtitle={item.description}
                  meta={`${fileCount(item.files)} · ${passageCount(item.sections)}`}
                  onEdit={() => setEditing(item)}
                  onDelete={() => removeSkill(item)}
                  confirm={`Delete "${label(item)}"?`}
                  confirmBody={`All ${item.versions} of its versions go too, with every file in them.`}
                  deleted={`${label(item)} was deleted, with every version of it.`}
                />
              ))
            )}
          </Pane>

          {/* What is in the version being read */}
          <Pane
            title="Package"
            // Which version these files are and who made it, in the header of
            // the column they belong to. It is one line rather than a control,
            // because reading it is the common case by a long way: CHANGING
            // which version is live is an edit, and edits are in the form.
            note={
              version && (
                <>
                  v{version.number}
                  {version.created_by_name !== "" &&
                    ` · by ${version.created_by_name}`}
                </>
              )
            }
          >
            {loading ? null : !skill ? (
              <Empty>Choose a skill.</Empty>
            ) : (
              <>
                {files.length === 0 ? (
                  <Empty>
                    {versions.length === 0
                      ? "This skill has no versions."
                      : "This version holds no files."}
                  </Empty>
                ) : (
                  <>
                    {/* The passages, as a destination rather than a tab: they are
                    one of the two things a version contains. */}
                    <button
                      type="button"
                      // The passages carry no highlight, so choosing them
                      // takes any mark with them: it belonged to a file.
                      onClick={() => setChosen({ kind: "passages" })}
                      className={cn(
                        "relative flex w-full cursor-pointer items-center gap-2.5 border-b border-border px-4 py-2.5 text-left transition-colors",
                        shownFile === null
                          ? "bg-accent after:absolute after:inset-y-0 after:right-0 after:w-[3px] after:bg-primary"
                          : "hover:bg-accent",
                      )}
                    >
                      <Layers className="size-4 shrink-0 text-muted-foreground" />
                      <span className="flex-1 text-sm">Passages</span>
                      <span className="text-xs text-muted-foreground">
                        {sections.length}
                      </span>
                    </button>

                    {groups.map(([folder, group]) => (
                      <div key={folder || "."}>
                        {folder !== "" && (
                          <p className="border-b border-border/60 bg-muted/30 px-4 py-1 text-xs font-medium text-muted-foreground">
                            {folder}/
                          </p>
                        )}
                        {group.map((file) => (
                          <button
                            key={file.id}
                            type="button"
                            onClick={() => void openFile(file.id)}
                            className={cn(
                              "relative flex w-full cursor-pointer items-center gap-2.5 border-b border-border/60 px-4 py-2.5 text-left transition-colors",
                              shownFile?.id === file.id
                                ? "bg-accent after:absolute after:inset-y-0 after:right-0 after:w-[3px] after:bg-primary"
                                : "hover:bg-accent",
                            )}
                          >
                            <FileIcon file={file} />
                            <span className="min-w-0 flex-1">
                              <span className="block truncate text-sm">
                                {basename(file.path)}
                              </span>
                              <span className="block text-xs text-muted-foreground">
                                {readableSize(file.size_bytes)}
                                {file.sections > 0 &&
                                  ` · ${passageCount(file.sections)}`}
                                {file.binary && " · not indexed"}
                              </span>
                            </span>
                          </button>
                        ))}
                      </div>
                    ))}
                  </>
                )}
              </>
            )}
          </Pane>

          {/* The reader: one file, or the passages of the whole version */}
          <div className="flex min-h-0 flex-col">
            {shownFile ? (
              <FileReader
                file={shownFile}
                highlight={highlight}
                rendered={rendered}
                onRendered={setRendered}
                editing={edits[shownFile.path]?.text ?? null}
                onEdit={(text) =>
                  setEdits((held) => ({
                    ...held,
                    [shownFile.path]: {
                      text,
                      was: held[shownFile.path]?.was ?? shownFile.text ?? "",
                    },
                  }))
                }
                onOpenEditor={() => {
                  setRendered(false);
                  setAgainst(versionID);
                  const was = shownFile.text ?? "";
                  setEdits((held) => ({
                    ...held,
                    [shownFile.path]: { text: was, was },
                  }));
                }}
                mayEdit={mayWrite && editable}
              />
            ) : (
              <Passages
                sections={sections}
                empty={
                  !skill
                    ? "Choose a skill."
                    : version === null
                      ? "This skill has no versions."
                      : "Nothing in this version is text, so there is nothing to search."
                }
                onOpen={(section) =>
                  void openFile(section.file_id, {
                    from: section.line_start,
                    to: section.line_end,
                  })
                }
              />
            )}
          </div>
        </div>
      </div>

      {editing && (
        <SkillForm
          skill={editing}
          // The history, so the form can offer the way back to an earlier
          // version. It is the open skill's, and the form is only ever opened
          // from the open skill's row.
          versions={editing.id === skill?.id ? versions : []}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null);
            // The list, the row and the version strip all read from the screen's
            // one answer, so the screen is asked again rather than patched: a
            // console that edits its own copy eventually shows something the
            // server does not agree with.
            await show({ skill: editing.id, version: versionID });
          }}
        />
      )}

      {importing && (
        <ImportForm
          onClose={() => setImporting(false)}
          // Refreshing the screen and closing the dialog are two things now: a
          // batch where something landed and something was skipped does the
          // first and not the second, so the reasons stay on screen.
          onImported={async (skillID) => {
            await show({ skill: skillID });
          }}
        />
      )}
    </Page>
  );
}

/**
 * The skill's own form: what it is called, what it is for, and whether it is on.
 *
 * Three fields and no more. A package's contents are the author's and a version
 * is immutable, so there is nothing else here a person could change: this is
 * the whole of what an administrator decides about a skill.
 *
 * There is no "new" case. A skill comes into existence by importing a package
 * and in no other way, so a form that could create one would be a second way to
 * make something the import is the definition of.
 */
function SkillForm({
  skill,
  versions,
  onClose,
  onSaved,
}: {
  skill: Skill;
  versions: SkillVersion[];
  onClose: () => void;
  onSaved: () => void | Promise<void>;
}) {
  const notify = useNotify();
  const [title, setTitle] = useState(skill.title);
  const [description, setDescription] = useState(skill.description);
  const [enabled, setEnabled] = useState(skill.status === "active");
  // Which version should be live. It starts on the one that IS, so opening the
  // form and saving it changes nothing about the version.
  const [versionID, setVersionID] = useState(skill.active_version_id);
  const [busy, setBusy] = useState(false);
  const errors = useFormErrors();
  const rollingBack = versionID !== skill.active_version_id;

  async function save() {
    errors.clear();
    setBusy(true);
    try {
      const saved = await skills.update(skill.id, {
        title: title.trim(),
        description: description.trim(),
        status: enabled ? "active" : "disabled",
        version_id: versionID,
      });
      notify.success(
        rollingBack
          ? `${label(saved)} is now running version ${saved.version?.number ?? ""}.`.trim()
          : `${label(saved)} was saved.`,
      );
      await onSaved();
    } catch (failure) {
      errors.fail(failure);
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Edit skill"
      onSubmit={save}
      submitting={busy}
    >
      <Field
        label="Name"
        hint={`The package calls itself ${skill.name}, which is how the assistant addresses it and cannot be changed. Leave this empty to be called that here too.`}
        error={errors.fields.title}
      >
        <Input
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder={skill.name}
          autoFocus
        />
      </Field>

      <Field
        label="Description"
        hint="What this skill is for. The assistant reads it when it decides whether the skill is worth opening, and it is what a search here matches on."
        error={errors.fields.description}
      >
        <Textarea
          rows={3}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </Field>

      {/* Only when there is more than one, because a picker with a single
          option is a control that cannot do anything. */}
      {versions.length > 1 && (
        <Field
          label="Live version"
          hint="Which version the assistant runs, and the way back to an earlier one. Nothing is deleted by changing it, and you can come back the same way."
          error={errors.fields.version_id}
        >
          <NativeSelect
            // Labelled on the control, because Field's own label is not
            // associated with what it labels (components/ui/modal.tsx): it has
            // no htmlFor and does not wrap the input, so nothing reading the
            // page aloud connects the two. The convention in this file already,
            // and a gap worth closing for every form rather than here.
            aria-label="Live version"
            value={String(versionID)}
            onChange={(event) => setVersionID(Number(event.target.value))}
          >
            {versions.map((v) => (
              <option key={v.id} value={String(v.id)}>
                {versionLabel(v, skill.active_version_id)}
              </option>
            ))}
          </NativeSelect>
        </Field>
      )}

      <CheckboxField
        checked={enabled}
        onChange={setEnabled}
        label="Enabled"
        hint="An enabled skill is available to the assistant. Switch it off to keep it and everything in it without the assistant using it."
      />

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  );
}

/** How a version reads in the picker: its number, and whether it is the live one. */
function versionLabel(version: SkillVersion, live: number): string {
  const when = new Date(version.created_at).toLocaleDateString();
  if (version.id === live) return `v${version.number} · live · ${when}`;
  return `v${version.number} · ${version.status} · ${when}`;
}

function basename(path: string): string {
  return path.split("/").pop() ?? path;
}

/** The icon says what the file is FOR, which is what the package's shape says. */
function FileIcon({ file }: { file: SkillFile }) {
  const className = "size-4 shrink-0 text-muted-foreground";
  if (file.file_type === "script") return <FileCode2 className={className} />;
  if (file.file_type === "asset")
    return file.mime_type.startsWith("image/") ? (
      <ImageIcon className={className} />
    ) : (
      <Package className={className} />
    );
  return <FileText className={className} />;
}

/**
 * How many lines of a file are drawn.
 *
 * A package may carry a generated reference file of a hundred thousand lines,
 * and a row each is a screen that locks the browser rather than shows a file.
 * What is past the cut is SAID, with the whole file a download away: a viewer
 * that silently stops is a viewer that lies about what was stored.
 */
const maxLines = 4000;

function FileReader({
  file,
  highlight,
  rendered,
  onRendered,
  editing,
  onEdit,
  onOpenEditor,
  mayEdit,
}: {
  file: SkillFile;
  highlight: { from: number; to: number } | null;
  rendered: boolean;
  onRendered: (rendered: boolean) => void;
  /** The text being edited, or null when this file is being read. */
  editing: string | null;
  onEdit: (text: string) => void;
  onOpenEditor: () => void;
  mayEdit: boolean;
}) {
  const notify = useNotify();
  const markdown = /\.(md|markdown)$/i.test(file.path);

  async function download() {
    try {
      await save(file);
    } catch (failure) {
      notify.error(describeError(failure));
    }
  }

  return (
    <>
      <header className="flex h-11 shrink-0 items-center gap-3 border-b border-border px-4">
        <span className="min-w-0 flex-1 truncate font-mono text-xs">
          {file.path}
        </span>
        <span className="shrink-0 text-xs text-muted-foreground">
          {describeFile(file.path, file.mime_type)} ·{" "}
          {readableSize(file.size_bytes)}
        </span>
        {markdown && !file.binary && editing === null && (
          <Button
            size="sm"
            variant="outline"
            className="h-7 shrink-0 text-xs"
            onClick={() => onRendered(!rendered)}
          >
            {rendered ? "Source" : "Rendered"}
          </Button>
        )}
        {/* Editing a file. Absent rather than disabled where it cannot be
            done, because a button that can only ever refuse is a promise the
            screen does not keep; the one exception is a reason worth saying,
            which is said in the pane instead. */}
        {/* Editing this file. While it IS being edited there is no button
            here: leaving the editor is Cancel on the bar above, which is one
            act for every open file and is where somebody is already looking. */}
        {!file.binary && mayEdit && editing === null && (
          <Button
            size="sm"
            variant="outline"
            className="h-7 shrink-0 text-xs"
            // Named for what it edits. Every skill ROW carries an Edit too, for
            // the skill's own form, and two buttons called Edit on one screen
            // is one button as far as anything reading the page aloud is
            // concerned.
            aria-label="Edit this file"
            onClick={onOpenEditor}
          >
            <Pencil className="size-3.5" />
            Edit
          </Button>
        )}
        <button
          type="button"
          aria-label="Download this file"
          title="Download this file"
          onClick={() => void download()}
          className="shrink-0 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <Download className="size-4" />
        </button>
      </header>

      {file.binary ? (
        // Bytes. There is nothing honest to show, so what is shown is what it
        // IS: its type, its size, and the hash of exactly what was stored.
        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-8">
          <p className="text-sm">
            This file is stored exactly as it arrived. It is not text, so it is
            not searchable.
          </p>
          <dl className="mt-4 space-y-1 text-xs text-muted-foreground">
            <div>
              <dt className="inline font-medium">Type: </dt>
              <dd className="inline">{file.mime_type}</dd>
            </div>
            <div>
              <dt className="inline font-medium">Size: </dt>
              <dd className="inline">{file.size_bytes} bytes</dd>
            </div>
            <div>
              <dt className="inline font-medium">SHA-256: </dt>
              <dd className="inline break-all font-mono">{file.sha256}</dd>
            </div>
          </dl>
          <Button
            size="sm"
            variant="outline"
            className="mt-5"
            onClick={() => void download()}
          >
            <Download className="size-4" />
            Download
          </Button>
        </div>
      ) : editing !== null ? (
        <CodeEditor
          path={file.path}
          value={editing}
          onChange={onEdit}
          label={`The contents of ${file.path}`}
        />
      ) : rendered ? (
        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
          <Markdown>{withoutFrontmatter(file.text ?? "")}</Markdown>
        </div>
      ) : (
        <CodeView
          path={file.path}
          text={file.text ?? ""}
          highlight={highlight}
          maxLines={maxLines}
        />
      )}
    </>
  );
}

/**
 * The passages of a version: what the parser made of its text files.
 *
 * This is the screen's proof that the import worked. A heading path that reads
 * wrong, a line range that covers half a code block, a file that produced one
 * passage where it should have produced ten: all of it is visible here and
 * invisible anywhere else.
 */
function Passages({
  sections,
  empty,
  onOpen,
}: {
  sections: SkillView["sections"];
  empty: string;
  onOpen: (section: SkillView["sections"][number]) => void;
}) {
  return (
    <>
      <header className="flex h-11 shrink-0 items-center justify-between border-b border-border px-4">
        <h2 className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          Passages
        </h2>
        {sections.length > 0 && (
          <span className="text-xs text-muted-foreground">
            {passageCount(sections.length)}
          </span>
        )}
      </header>

      {sections.length === 0 ? (
        <Empty>{empty}</Empty>
      ) : (
        <div className="min-h-0 flex-1 overflow-auto">
          <table className="w-full text-sm">
            {/* Opaque, and above the rows. A sticky header at 40% lets every
                row scroll THROUGH it, so the column names and somebody's data
                are drawn on top of each other. The tint was only ever safe in
                a header that does not move. */}
            <thead className="sticky top-0 z-10 bg-muted">
              <tr className="border-b border-border text-left">
                <th className="w-10 px-4 py-2.5 text-xs font-medium text-muted-foreground">
                  #
                </th>
                <th className="px-4 py-2.5 text-xs font-medium text-muted-foreground">
                  Heading
                </th>
                <th className="px-4 py-2.5 text-xs font-medium text-muted-foreground">
                  File
                </th>
                <th className="w-24 px-4 py-2.5 text-right text-xs font-medium text-muted-foreground">
                  Lines
                </th>
                <th className="w-20 px-4 py-2.5 text-right text-xs font-medium text-muted-foreground">
                  Size
                </th>
              </tr>
            </thead>
            <tbody>
              {sections.map((section) => (
                <tr
                  key={section.id}
                  onClick={() => onOpen(section)}
                  title="Open the file at these lines"
                  className="cursor-pointer border-b border-border/60 transition-colors hover:bg-accent"
                >
                  <td className="px-4 py-2 text-xs text-muted-foreground">
                    {section.sequence}
                  </td>
                  <td className="px-4 py-2">
                    {/* A passage with no heading is the run of text before the
                        first one: in a manifest that is the frontmatter and the
                        opening paragraph, and it is indexed like any other. */}
                    {section.path === "" ? (
                      <span className="text-muted-foreground">
                        (before the first heading)
                      </span>
                    ) : (
                      section.path
                    )}
                  </td>
                  <td className="px-4 py-2 font-mono text-xs text-muted-foreground">
                    {section.file}
                  </td>
                  <td className="px-4 py-2 text-right font-mono text-xs text-muted-foreground">
                    {section.line_start}–{section.line_end}
                  </td>
                  <td className="px-4 py-2 text-right text-xs text-muted-foreground">
                    {readableSize(section.size_bytes)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/**
 * Importing a package.
 *
 * The refusal is the interesting half. A package can be wrong in a dozen ways
 * and the server names which file is wrong and why, so that answer is put on
 * the field rather than turned into "import failed".
 */
function ImportForm({
  onClose,
  onImported,
}: {
  onClose: () => void;
  onImported: (skillID: number) => Promise<void>;
}) {
  const notify = useNotify();
  const [chosen, setChosen] = useState<File[]>([]);
  const [refused, setRefused] = useState<ImportOutcome[]>([]);
  const [over, setOver] = useState(false);
  const [busy, setBusy] = useState(false);
  const errors = useFormErrors();

  /**
   * Files ACCUMULATE, and a file already chosen is not chosen twice.
   *
   * Dropping a second folder on the zone adds to the selection rather than
   * replacing it, because somebody importing a dozen packages collects them in
   * more than one go, and losing the first eleven to a second drop is the
   * annoyance the whole feature exists to remove. The same file twice is one
   * entry: the import would report the second as unchanged, which is not wrong
   * but is noise.
   */
  function add(more: FileList | null) {
    if (!more || more.length === 0) return;
    setChosen((already) => {
      const seen = new Set(already.map(identity));
      const added = Array.from(more).filter(
        (file) => !seen.has(identity(file)),
      );
      return added.length === 0 ? already : [...already, ...added];
    });
    errors.clear();
    setRefused([]);
  }

  function drop(file: File) {
    setChosen((already) => already.filter((held) => held !== file));
  }

  async function submit() {
    if (chosen.length === 0) {
      errors.reject({ file: "Choose a skill package to import." });
      return;
    }
    errors.clear();
    setRefused([]);
    setBusy(true);
    try {
      const report = await skills.upload(chosen);
      const skipped = report.results.filter(
        (result) => result.status === "refused",
      );

      // ONE package, refused: the reason belongs under the drop zone, beside
      // the file it is about, exactly where it was before importing several
      // was possible. A toast would be the wrong place for the only thing the
      // person needs to read.
      if (report.results.length === 1 && skipped.length === 1) {
        errors.reject({
          file: skipped[0].reason ?? "That is not a skill package.",
        });
        setBusy(false);
        return;
      }

      // What happened, not what was asked for, and in the right tone: an
      // import where everything was skipped is not a success however many
      // words the sentence has.
      const summary = importSummary(report.results);
      if (anythingLanded(report.results)) notify.success(summary);
      else notify.error(summary);

      // Land the screen on something that actually arrived.
      const landed = report.results.find((result) => result.skill);
      if (landed?.skill) await onImported(landed.skill.id);

      if (skipped.length === 0) {
        onClose();
        return;
      }

      // Some landed and some did not. The dialog stays open holding exactly
      // the ones that did not, each with its reason: they are what is left to
      // do, and closing on them would lose the only account of why.
      setRefused(skipped);
      const names = new Set(skipped.map((result) => result.file));
      setChosen((already) => already.filter((file) => names.has(file.name)));
      setBusy(false);
    } catch (failure) {
      errors.fail(failure);
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title="Import skills"
      onSubmit={submit}
      submitting={busy}
      submitLabel={
        chosen.length > 1 ? `Import ${chosen.length} packages` : "Import"
      }
    >
      <Field
        label="Packages"
        hint="One zip per skill, each with SKILL.md inside it."
        required
        error={errors.fields.file}
      >
        <label
          onDragOver={(event) => {
            event.preventDefault();
            setOver(true);
          }}
          onDragLeave={() => setOver(false)}
          onDrop={(event) => {
            event.preventDefault();
            setOver(false);
            add(event.dataTransfer.files);
          }}
          className={cn(
            "flex cursor-pointer flex-col items-center gap-2 rounded-lg border border-dashed px-6 py-8 text-center transition-colors",
            over
              ? "border-primary bg-accent"
              : "border-border hover:bg-accent/50",
          )}
        >
          <Upload className="size-5 text-muted-foreground" />
          <span className="text-sm">
            {chosen.length === 0
              ? "Drop .zip files here, or choose them"
              : "Drop more, or choose more"}
          </span>
          <span className="text-xs text-muted-foreground">
            Importing does not run anything in them
          </span>
          <input
            type="file"
            accept=".zip,application/zip"
            // Several at once: the whole point. A folder of packages is dropped
            // or picked in one go and imported one by one.
            multiple
            // Named, because the label around it is a drop zone with an icon
            // and two lines of prose rather than a word: without this the input
            // has no accessible name at all, so nothing reading the page aloud
            // can say what choosing a file here would do.
            aria-label="Skill packages"
            className="hidden"
            onChange={(event) => {
              add(event.target.files);
              // Cleared, so choosing the same file again after removing it
              // still raises a change event.
              event.target.value = "";
            }}
          />
        </label>
      </Field>

      {chosen.length > 0 && (
        <ul className="divide-y divide-border border-y border-border">
          {chosen.map((file) => {
            const why = refused.find((result) => result.file === file.name);
            return (
              <li
                key={identity(file)}
                className="flex items-start gap-3 py-2 text-sm"
              >
                <FileArchive className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 flex-1">
                  <span className="block truncate">{file.name}</span>
                  <span
                    className={cn(
                      "text-xs",
                      why ? "text-destructive" : "text-muted-foreground",
                    )}
                  >
                    {why ? `Skipped: ${why.reason}` : readableSize(file.size)}
                  </span>
                </span>
                <button
                  type="button"
                  onClick={() => drop(file)}
                  className="cursor-pointer rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
                  aria-label={`Remove ${file.name}`}
                >
                  <X className="size-4" />
                </button>
              </li>
            );
          })}
        </ul>
      )}

      <p className="text-xs text-muted-foreground">
        Each package is imported on its own. An updated version of a skill that
        is already here is added as a new version, and the one running now is
        kept. Anything that is not a skill package is skipped and listed here
        with the reason.
      </p>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  );
}

/**
 * What makes two chosen files the same file.
 *
 * Name, size and modification time: a browser gives no id, and the name alone
 * would treat two different packages that happen to be called skill.zip, from
 * two different folders, as one.
 */
function identity(file: File): string {
  return `${file.name}:${file.size}:${file.lastModified}`;
}
