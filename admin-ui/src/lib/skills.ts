import { apiFetch, apiUpload, json, nothing, send } from "./api";
import { SEARCH_LIMIT } from "./search";

/**
 * Skills, as the console sees them.
 *
 * The shapes mirror the API exactly, and the API mirrors the thing: a skill has
 * versions, a version has files, a text file has passages.
 */

export interface SkillVersion {
  id: number;
  skill_id: number;
  /** The version this one was built from: the lineage a rollback follows. */
  parent_version_id: number;
  number: number;
  source: "imported" | "manual" | "learned";
  status: "draft" | "active" | "rejected" | "archived";
  /**
   * Who imported THIS version. The id is 0 once that person is deleted; the
   * name is frozen and survives them, and for a version the system wrote
   * itself it is an agent or a model rather than a person.
   */
  created_by: number;
  created_by_name: string;
  change_summary: string;
  package_sha256: string;
  validated_at: string | null;
  activated_at: string | null;
  created_at: string;
  files: number;
  sections: number;
}

export interface Skill {
  id: number;
  /**
   * The HANDLE: lowercase, hyphenated, and equal to the package's own directory
   * name, because the Agent Skills format requires all three. It is what the
   * agent addresses the skill by, and it is not a name for a person.
   */
  name: string;
  /**
   * The name for a person, empty when the package carried none.
   *
   * Read `label()` rather than this. The format defines no title field, so a
   * package's own comes from `metadata.title` (which the specification says is
   * exactly where a client puts a property it needs) or from the manifest's
   * first heading, and from there it is seeded onto the skill.
   *
   * It is the SKILL's, not the live version's: an administrator can rename a
   * skill to whatever this workspace calls it, and a rollback then leaves the
   * name alone. Until somebody does, it follows whichever version is live.
   */
  title: string;
  description: string;
  /** Who created the skill, which is not who made its newest version. */
  created_by: number;
  created_by_name: string;
  /**
   * Who last changed the skill itself: renamed it, described it, switched it
   * off, or rolled it back. Not who imported the newest version, which is on
   * the version.
   */
  updated_by: number;
  updated_by_name: string;
  active_version_id: number;
  status: "draft" | "active" | "disabled" | "archived";
  created_at: string;
  updated_at: string;
  /** The live version, when the server was asked for it. */
  version: SkillVersion | null;
  versions: number;
  files: number;
  sections: number;
}

export interface SkillFile {
  id: number;
  version_id: number;
  path: string;
  file_type: "skill" | "reference" | "script" | "asset" | "other";
  mime_type: string;
  /** Present only when one file was asked for, and only when it is text. */
  text?: string;
  /**
   * Whether this file is bytes rather than text.
   *
   * It comes from the ROW, so a listing carries it too. That is the difference
   * between offering a file to read and offering it to download, and a screen
   * that guessed from an absent `text` would call every listed file binary.
   */
  binary: boolean;
  size_bytes: number;
  sha256: string;
  /** How many searchable passages this file produced. Zero for an asset. */
  sections: number;
}

export interface SkillSection {
  id: number;
  file_id: number;
  heading: string;
  /** The heading path: "Recovery > Point-in-time recovery". */
  path: string;
  line_start: number;
  line_end: number;
  sequence: number;
  size_bytes: number;
  /** The file it came from. */
  file: string;
}

/** The whole screen, in one answer. */
export interface SkillView {
  skills: Skill[];
  skill_id: number;
  skill: Skill | null;
  versions: SkillVersion[];
  version_id: number;
  files: SkillFile[];
  sections: SkillSection[];
  /**
   * The version's SKILL.md, with its text: what the screen opens by default.
   *
   * It rides on this answer rather than being fetched beside it, because the
   * default state of a screen is part of the screen. Every other file is a new
   * question and costs its own request.
   */
  manifest: SkillFile | null;
}

export interface SkillSelection {
  skill?: number;
  version?: number;
}

/**
 * What one archive's import did.
 *
 * `imported` is a version landing (a new skill, or a new version of one that
 * was already here), `unchanged` is the identical package already being here,
 * and `refused` is an archive that is not a skill package, with the reason.
 */
export type ImportStatus = "imported" | "unchanged" | "refused";

export interface ImportOutcome {
  /** The file's own name, which is how a refusal is matched to a file on disk. */
  file: string;
  status: ImportStatus;
  /** Set on a refusal only. */
  reason?: string;
  /** Set on everything else, carrying the version that is now live. */
  skill?: Skill;
}

/** One answer per archive, in the order they were sent. */
export interface ImportReport {
  results: ImportOutcome[];
}

/**
 * What a skill is called: the title it carried, or its handle when it carried
 * none.
 *
 * ONE name reaches the screen, never both. The handle is an identifier (it has
 * to be lowercase, hyphenated and equal to the package's directory name), so
 * printing it beside a perfectly good title just says the same thing twice in
 * two shapes. Anybody who needs the handle can read it in SKILL.md, which is
 * one click away on the same screen.
 *
 * One function, because the fallback has to be identical everywhere it is
 * applied. A list and a header each writing their own is how one screen ends up
 * calling a skill two different things.
 */
export function label(skill: Pick<Skill, "name" | "title">): string {
  return skill.title === "" ? skill.name : skill.title;
}

/** How a count of files is written, everywhere it is written. */
export function fileCount(n: number): string {
  return `${n} ${n === 1 ? "file" : "files"}`;
}

/**
 * How a count of passages is written.
 *
 * "Passage" and not "section" or "chunk", deliberately, and the same word the
 * KB uses: it is a piece of the skill somebody wrote, and what it is called is
 * what people will say when they talk about the search finding one.
 */
export function passageCount(n: number): string {
  return `${n} ${n === 1 ? "passage" : "passages"}`;
}

/**
 * What to say when an import finishes.
 *
 * One archive gets a sentence about that archive, by name and version, because
 * "1 skill imported" is less than the sentence it would replace. Several get
 * counts, and only of the outcomes that actually happened: "4 skills imported,
 * 2 skipped" and not "4 imported, 0 already here, 2 skipped".
 *
 * It lives here rather than in the dialog so the wording is one thing, tested
 * on its own, and so the tone the toast is raised with can be decided from the
 * same data (nothing landed is not a success, whatever the sentence says).
 */
export function importSummary(results: ImportOutcome[]): string {
  if (results.length === 0) return "Nothing was imported.";

  if (results.length === 1) {
    const only = results[0];
    if (only.skill) {
      const version = only.skill.version?.number ?? 1;
      return only.status === "imported"
        ? `${label(only.skill)} was imported as version ${version}.`
        : `${label(only.skill)} is unchanged: that is already version ${version}.`;
    }
    return only.reason ?? "That is not a skill package.";
  }

  const counts: Record<ImportStatus, number> = {
    imported: 0,
    unchanged: 0,
    refused: 0,
  };
  for (const result of results) counts[result.status]++;

  const said: string[] = [];
  if (counts.imported > 0) {
    said.push(
      `${counts.imported} ${counts.imported === 1 ? "skill" : "skills"} imported`,
    );
  }
  if (counts.unchanged > 0) said.push(`${counts.unchanged} already here`);
  if (counts.refused > 0) said.push(`${counts.refused} skipped`);
  return `${said.join(", ")}.`;
}

/** Whether anything at all landed, which is what the toast's tone turns on. */
export function anythingLanded(results: ImportOutcome[]): boolean {
  return results.some((result) => result.status !== "refused");
}

/** Whether a path is Markdown, and so has a rendered form at all. */
export function isMarkdown(path: string): boolean {
  return /\.(md|markdown)$/i.test(path);
}

/**
 * The document without its frontmatter.
 *
 * The frontmatter is machine-readable metadata (the handle, the description the
 * agent selects on, a licence) and it is the first thing in every SKILL.md, so
 * a rendered view that keeps it opens on a block of YAML instead of on the
 * instructions somebody wrote. It is not hidden: Source shows the file exactly
 * as it was stored, which is the whole point of having the toggle.
 *
 * Only a block at the very TOP is taken, and only one: a `---` further down is a
 * horizontal rule in somebody's prose, and a document that begins with one is
 * not carrying frontmatter.
 */
export function withoutFrontmatter(text: string): string {
  const body = text.startsWith("\ufeff") ? text.slice(1) : text;
  const lines = body.split("\n");
  if (lines.length === 0 || lines[0].trimEnd() !== "---") return body;

  for (let i = 1; i < lines.length; i++) {
    const line = lines[i].trimEnd();
    if (line === "---" || line === "...") {
      // Past the closing marker, and past the blank line that usually follows
      // it, so the rendered document does not open on empty space.
      let start = i + 1;
      while (start < lines.length && lines[start].trim() === "") start++;
      return lines.slice(start).join("\n");
    }
  }
  // Never closed. Nothing is stripped: the file is malformed and showing it as
  // it is says more than showing nothing.
  return body;
}

/** A size a person can read. */
export function readableSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} kB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * What a save answered: the version it wrote, and which paths differ from the
 * one it was written against.
 *
 * `changed` is here because the screen would otherwise have to work it out
 * again, and the server has just done it.
 */
export type DraftSaved = {
  version: SkillVersion;
  changed: string[];
};

export const skills = {
  /** One address, one answer: the list, the selection, and what is in it. */
  view: (want: SkillSelection = {}) => {
    const params = new URLSearchParams();
    if (want.skill) params.set("skill", String(want.skill));
    if (want.version) params.set("version", String(want.version));
    const query = params.toString();
    return json<SkillView>(`/v1/skills/view${query ? `?${query}` : ""}`);
  },

  /** Import packages. The answer says what happened to each, in order. */
  upload: (files: File[]) => apiUpload<ImportReport>("/v1/skills", files),

  /**
   * Save the skill's own form: what it is called, what it is for, and whether
   * it is on. One request, because they are one dialog: a save that lands in
   * two writes can half land.
   */
  update: (
    id: number,
    s: {
      title: string;
      description: string;
      status: "active" | "disabled";
      /** Which version should be live. Naming the one that already is does nothing. */
      version_id: number;
    },
  ) => json<Skill>(`/v1/skills/${id}`, send("PUT", s)),

  /**
   * Which skills match what was typed, over the handle, the name and the
   * description, most relevant first.
   *
   * Whole skills rather than hits, because what the screen does with them is
   * draw them as its list. The limit is asked for rather than left to the
   * server, so the screen can tell a complete answer from a page of one.
   */
  search: (query: string, limit: number = SEARCH_LIMIT) => {
    const params = new URLSearchParams({ q: query, limit: String(limit) });
    return json<Skill[]>(`/v1/skills/search?${params}`);
  },

  remove: (id: number) => nothing(`/v1/skills/${id}`, { method: "DELETE" }),

  /**
   * Save edits as a new version that is NOT live.
   *
   * `from` is the version the edits were written against, sent rather than
   * assumed: a save written against version 2 must not land on version 5
   * because somebody published one while this was being typed.
   *
   * One request for however many files were edited, because they are one save:
   * three files edited in a sitting is one version, not three.
   */
  draft: (
    id: number,
    edit: { from: number; edits: { path: string; text: string }[] },
  ) => json<DraftSaved>(`/v1/skills/${id}/versions`, send("POST", edit)),

  /** Turn a draft down. Only a draft: the live version is not reachable here. */
  discard: (id: number, versionID: number) =>
    nothing(`/v1/skills/${id}/versions/${versionID}`, { method: "DELETE" }),

  /** One file, with its text. A binary file comes back without content. */
  file: (id: number) => json<SkillFile>(`/v1/skill-files/${id}`),
};

/**
 * Save a file out of a package.
 *
 * It cannot be a plain link, and that is not a style choice. Every /v1 route
 * wants a bearer token on the request, and a link carries cookies instead: the
 * refresh cookie is path-scoped to the auth endpoints, so a browser following
 * an <a href> arrives with nothing and is refused. So the bytes are fetched
 * with the session on them and handed to the browser as something it already
 * holds.
 */
export async function save(file: SkillFile): Promise<void> {
  const res = await apiFetch(`/v1/skill-files/${file.id}/download`);
  if (!res.ok) throw new Error("That file could not be read.");
  const url = URL.createObjectURL(await res.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = file.path.split("/").pop() || "file";
  link.click();
  URL.revokeObjectURL(url);
}
