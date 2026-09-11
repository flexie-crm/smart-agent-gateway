import { useCallback, useEffect, useState } from 'react'
import { json, nothing, send } from './api'

/**
 * The API, as the console sees it.
 *
 * One place, so a screen never invents a URL. The shapes mirror the server's
 * exactly: where the server calls something `model_key`, so does this. Renaming
 * a field on the way in gives you two vocabularies for one thing, and every
 * conversation about it then needs a translation.
 */

/** One thing a model of some vendor may be configured with, as the server
 *  declares it. The row stores only the chosen value; this is what it means. */
export interface SettingSpec {
  key: string
  label: string
  kind: string
  help?: string
  choices?: { value: string; label: string }[]
  default?: string
}

export interface Vendor {
  id: number
  vendor_key: string
  name: string
  base_url?: string
  /** The key itself is never sent back. Only whether there is one. */
  has_credentials: boolean
  status: string
  /** What a model of this vendor may be configured with. It rides on the vendor
   *  because the model form needs it the moment a vendor is picked. */
  settings?: SettingSpec[]
}

export interface Model {
  id: number
  vendor_id: number
  /** The machine this model runs on, when it runs on one of ours. Empty for a
   *  hosted model. It is on the row because the vendor list beside it is
   *  accounts only, and a local model would otherwise show no source. */
  machine?: string
  model_key: string
  type: string
  context_window: number
  /** A free note about this model. */
  description: string
  input_price_per_1m: number
  output_price_per_1m: number
  status: string
}

export interface Tool {
  id: number
  name: string
  kind: string
  /** Where this tool came from: the connection that projected it, or the
   *  product itself. The catalogue is read in groups by it, which is what a
   *  prefix on the name was standing in for. */
  source?: string
  /** `name` without that prefix, for a screen showing the source as a heading:
   *  `update_entity`, not `nli_update_entity`. `name` is still what the model
   *  calls and what every grant records, so it stays the value of anything
   *  selected. */
  short_name?: string
  /** The recipe a native custom tool was made from (e.g. 'query'). Empty otherwise. */
  template?: string
  friendly_name: string
  /** What the MODEL is told this tool does: long, and written for it, not for a person. */
  description: string
  /** What a PERSON is told it is for, in business language. Prefer this where it exists. */
  about?: string
  risk: string
  status: string
  grants: number[]
  /** Set on tools projected from an MCP connection (kind 'mcp'). */
  mcp_server_id?: number
  /** The last sync no longer saw this tool on the remote. */
  remote_missing?: boolean
  /** The remote redefined this tool; approval was re-locked when it did. */
  definition_changed_at?: string
}

/** A single tool with, for a custom tool, everything its edit form prefills. */
export interface ToolDetail extends Tool {
  /**
   * Who this tool CAN be granted to; `grants` is who it IS granted to. Both are
   * the edit form's, so both arrive with it: the screen used to fetch the
   * workspace's groups on the mere possibility that a dialog would open.
   */
  groups: { id: number; name: string }[]
  /** The driver a custom tool uses (e.g. 'mysql'). */
  variant?: string
  /** The driver's human name, e.g. 'MySQL / MariaDB'. */
  variant_label?: string
  /** What this kind of tool is, from its template. */
  about?: string
  /** The parameters the tool takes, so the descriptions can be edited. */
  params?: TemplateParam[]
  /** The driver's form, so the edit view is one request and cannot disagree with itself. */
  sections?: TemplateSection[]
  /** The tool's AI guide. */
  guide?: string
  /** Every field of the form, secrets blanked. A field this tool has no value for is blank. */
  settings?: Record<string, unknown>
  /** Each parameter's current description. */
  param_descriptions?: Record<string, string>
}

/** The form for a tool that does not exist yet, and the values it starts with. */
export interface NewToolForm {
  sections: TemplateSection[]
  values: Record<string, unknown>
}

/** A connection to a third-party tool service (SAG as the MCP client). */
export interface MCPServer {
  id: number
  name: string
  url: string
  auth_type: string
  /** The key itself is never sent back. Only whether there is one. */
  has_api_key: boolean
  /** Set only when this service does not register clients itself and somebody
   *  entered one by hand. Not a secret, so it comes back. */
  oauth_client_id?: string
  /** The secret itself is never sent back. Only whether there is one. */
  has_oauth_client_secret?: boolean
  /** For OAuth connections: the dance has completed and tokens are held. */
  connected: boolean
  status: string
  /** The namespace this connection's tools are stored under. Ours, not the
   *  administrator's: no screen shows it, and it is fixed at creation. */
  tool_prefix: string
  created_at: string
  last_synced_at: string | null
  last_error?: string
}

/** An OAuth client that may connect to our surfaces (CRM parity). */
export interface OAuthClient {
  id: number
  client_id: string
  name: string
  client_type: string
  redirect_uris: string[]
  grant_types: string[]
  scopes: string[]
  is_dcr: boolean
  status: string
  /** Shown exactly once, at mint time: copy it or roll the client. */
  client_secret?: string
  service_token?: string
}

/** Our MCP server's exposure config (CRM parity: mcpToolConfig/mcpBrainConfig). */
export interface MCPServerSettings {
  configured: boolean
  tool_config: Record<string, { enabled: boolean }>
  brain_config: number[]
}

export interface MCPSyncResult {
  offered: number
  added: number
  changed: number
  missing: number
  skipped: number
}

/**
 * One LINE of the Gateway screen: what the table draws, and nothing else.
 *
 * Not the whole agent. Sending the entity put its instructions, brains, confirm
 * list, file rules and timestamps on every row of a table that shows a name, a
 * model, a tool count, whether it reasons and whether it is on. What a form
 * needs, the form asks for by id when it opens.
 */
export interface AgentRow {
  id: number
  key: string
  name: string
  model_name: string
  tools: number
  reasoning: boolean
  /** Chosen values for what this agent may be configured with, most notably
   *  how hard to think. It is the agent's and not the model's because it is a
   *  property of the job: one model serves agents doing different ones. */
  settings?: Record<string, string>
  status: string
}

/** The Gateway screen in one answer: four sections, each with its own shape. */
export interface GatewayScreen {
  /**
   * Each section carries BOTH: the name the table prints, and the id the form
   * edits. The two forms on this screen change exactly these sections, so they
   * need no request of their own.
   */
  files: { rules: { types: string[]; model_id: number; model_name: string }[] }
  /** A null id means nothing is chosen, which the screen says in words. */
  audio: { model_id: number | null; model_name: string }
  /** Null when this workspace has not set a Gateway up yet. */
  gateway: AgentRow | null
  agents: AgentRow[]
}

/** Whether this installation can answer anything yet, and what is missing. */
export interface SetupState {
  ready: boolean
  has_name: boolean
  owner_id?: number
  has_vendor: boolean
  has_model: boolean
  gateway_model?: string
}

/** A kind of vendor the gateway can talk to, for the form that adds one. */
export interface VendorKind {
  key: string
  name: string
  requires_base_url?: boolean
}

/** The MCP server screen: what may be exposed, and whether it is. */
export interface MCPScreen {
  configured: boolean
  tools: {
    id: number
    name: string
    friendly_name: string
    kind: string
    exposed: boolean
    /** The service a relayed tool came from, and its name without the prefix
     *  that heading stands in for. Ours arrive under their own headings. */
    source?: string
    short_name?: string
  }[]
  brains: { id: number; name: string; chosen: boolean }[]
}

export interface Agent {
  id: number
  /**
   * The models this agent refers to, by id, against their names: its own, the
   * one that transcribes audio, and the one each file rule names.
   *
   * It travels WITH the agent. A screen showing model names used to fetch the
   * workspace's whole model catalogue to turn four ids into four strings, which
   * is a second request arriving separately and rewriting the page when it
   * landed. The names belong to the answer.
   */
  model_names?: Record<string, string>
  key: string
  name: string
  instructions: string
  model_id: number | null
  reasoning: boolean
  /** Chosen values for what this agent may be configured with, most notably
   *  how hard to think. It is the agent's and not the model's because it is a
   *  property of the job: one model serves agents doing different ones. */
  settings?: Record<string, string>
  status: string
  /** How the Gateway runs this agent: auto | background | inline. */
  delegation_mode: string
  tools: string[]
  /** The subset of `tools` this agent must stop and confirm before running. */
  confirm_tools: string[]
  /** The ids of the knowledge bases this agent may read. */
  brains: number[]
  /** Which model reads which uploaded file, in the order the rules are tried:
   *  the first whose types match wins, and a rule with no types matches
   *  everything. No rules at all means no file can be uploaded. */
  file_rules: { types: string[]; model_id: number }[]
  /** The model that turns a recording into words, so somebody can talk instead
   *  of typing. Null means the Gateway takes no audio. */
  audio_model_id: number | null
  /** The one unlocked brain the agent keeps as long-term memory, or null. */
  memory_brain_id: number | null
  approval_ttl_seconds: number | null
  /** Bounds the agent's tool loop; null uses the default. */
  max_iterations: number | null
  max_fleet_agents: number | null
  /** Bounds one working leg of a background agent, in seconds; null uses
   * the default. Meaningful only for an agent. */
  background_timeout_seconds: number | null
}

/**
 * A choice a form offers: what it sends back, and the one string somebody reads
 * to pick it.
 *
 * The label is the server's to compose. Building "OpenAI / gpt-4o" on this side
 * meant fetching the whole vendor catalogue to turn one number into one word,
 * on a screen that was only ever going to print it.
 */
export interface Choice {
  id: number
  label: string
}

/** The Files dialog, whole: the rules it edits and the models that read a file. */
export interface FilesFormBody {
  rules: { types: string[]; model_id: number }[]
  models: Choice[]
}

/** The Audio dialog, whole: the one model it sets and the models that transcribe. */
export interface AudioFormBody {
  model_id: number | null
  models: Choice[]
}

/** The agent dialog, whole. `agent` is null when one is being created. */
export interface AgentFormBody {
  agent: Agent | null
  models: Choice[]
  tools: {
    id: number
    name: string
    friendly_name: string
    approval_locked: boolean
    source?: string
    short_name?: string
  }[]
  brains: { id: number; name: string; locked: boolean }[]
  /** What this agent may be configured with beyond the named fields: today, how
   *  hard to think. Sent whatever model is chosen, because the choice is the
   *  same everywhere it exists and a model without it ignores it. */
  settings: SettingSpec[]
}

/** The user dialog: this workspace's groups, each saying whether they are in it. */
export interface UserFormBody {
  groups: { id: number; name: string; member: boolean }[]
}

export interface Workflow {
  id: number
  name: string
  status: string
}

export interface WorkflowVersion {
  id: number
  version: number
  definition: unknown
  is_published: boolean
}

export interface Assignment {
  match_type: string
  match_value: string
  priority: number
}

export interface User {
  id: number
  email: string
  name: string
  status: string
  /** The workspaces this person may act in. Empty means they cannot sign in. */
  workspaces: number[]
}

export interface Group {
  id: number
  name: string
}

export interface Role {
  id: number
  name: string
  permissions: string[]
}

/** One entry of the permission catalog: the key the API stores, the words a person reads. */
export interface Permission {
  key: string
  area: string
  label: string
}

export interface Workspace {
  id: number
  /** Derived from the name under the hood; the console never asks for it. */
  slug: string
  name: string
  description: string
  status: string
}

/**
 * What a vendor says it offers.
 *
 * `listed` is the part that matters, and it is not the same question as whether
 * `models` is empty. A vendor that cannot be asked (one with no key yet, an
 * endpoint that is down, or Azure, where you name a deployment you made rather
 * than a model the vendor publishes) offers no list and no opinion, and the form
 * must fall back to trusting the person rather than presenting them with an
 * empty menu and no way to proceed.
 */
export interface VendorCatalog {
  listed: boolean
  models: { id: string; name?: string }[]
}

/** A group of settings under a heading (the connection, the SSH tunnel, ...). */
export interface TemplateSection {
  title: string
  hint?: string
  fields: TemplateField[]
}

/** One input a template's tool takes: identity fixed, description editable. */
export interface TemplateParam {
  key: string
  type: string
  required: boolean
  description: string
}

/** A native tool template (the query template, later others). */
export interface ToolTemplate {
  name: string
  title: string
  description: string
  variants: { key: string; label: string }[]
  params: TemplateParam[]
  default_guide: string
}

/** One settings input a template's form asks for. */
export interface TemplateField {
  key: string
  label: string
  type: 'text' | 'number' | 'password' | 'select' | 'textarea' | 'checkbox'
  required?: boolean
  secret?: boolean
  /** The choices in a select: what is stored, and what a person reads. Two
   *  different things — the value is a name for the code ("denylist"), and
   *  showing it was asking the reader to translate. */
  options?: { value: string; label: string }[]
  default?: string
  help?: string
  span?: number
}

/**
 * A template asking for something before it can finish an operation. The console
 * renders the fields and sends the answers back to `action` with the token; what
 * any of it means is the template's business, not this app's.
 */
export interface ToolActionPrompt {
  action: string
  token: string
  title: string
  hint?: string
  submit?: string
  fields: TemplateField[]
}

/** What running one of a template's operations produced. */
export interface ToolActionResult {
  ok: boolean
  message?: string
  /** Present when the operation is unfinished, not when it failed. */
  prompt?: ToolActionPrompt
  /** The message under its older name, for a failure. */
  error?: string
}

/* --- inference nodes (KB/35) ------------------------------------------------
 *
 * A node is a machine that runs models we own. It has no table: it is a vendor
 * row, asked whether it is one. So everything below is what a MACHINE said,
 * not what a row holds, and it is asked for again whenever it is wanted rather
 * than cached: the same row is a node while the process is running on it and an
 * unreachable address when it is not.
 */

export interface NodeMachine {
  memory_total: number
  memory_free: number
  disk_total: number
  disk_free: number
  processors: number
}

export interface NodeInfo {
  name: string
  /** False for a build that serves the control surface and cannot run a model. */
  can_infer: boolean
  version: string
  uptime_seconds: number
  models: number
  resident: number
  downloads_active: number
  machine: NodeMachine
}

export interface NodeSummary {
  id: number
  /** Minted on the machine's own disk. How one that comes back is recognised. */
  node_id: string
  name: string
  base_url: string
  version: string
  /** False when the machine did not answer; `problem` says what happened. */
  reachable: boolean
  problem?: string
  info?: NodeInfo
  /** When the certificate this machine serves with runs out. It renews itself
   *  by checking back in, so this is here to make a machine that has STOPPED
   *  checking in visible before the day it goes silent. */
  cert_expires_at?: string
}

/** One workspace that has been given a model, and the row that lets it route. */
export interface ModelWorkspace {
  workspace_id: number
  name: string
  model_id: number
}

export interface WorkspaceChoice {
  id: number
  name: string
}

/** One model on a machine, as the add-a-local-model dialog sees it: nothing to
 *  type, because a name, a kind and a context length are properties of the
 *  weights and the price of hardware you own is zero. */
export interface MachineModelChoice {
  uid: string
  handle: string
  kind: string
  context_length?: number
  size_bytes: number
  /** This workspace already routes to it. */
  here: boolean
}

export interface MachineChoice {
  id: number
  name: string
  reachable: boolean
  problem?: string
  models: MachineModelChoice[]
}

export interface NodeFacts {
  architecture?: string
  parameters?: number
  context_length?: number
  license?: string
  published_format?: string
  files: number
  size_bytes: number
}

/** Where a model is right now, in the node's own vocabulary. */
export type Residency = 'resident' | 'released' | 'waking' | 'absent'

export interface NodeModel {
  uid: string
  repo: string
  revision: string
  name: string
  /** What the gateway asks for, and what an `ai_models` row stores as its key. */
  handle: string
  kind: string
  facts: NodeFacts
  settings: Record<string, string>
  /** What an administrator decided. `residency` is where the model actually is,
   *  and the two disagree while a machine is coming back up. */
  resident: boolean
  residency: Residency
  /** Why the last attempt to bring it into memory failed, when one did.
   *
   *  `resident` without `residency` is the honest picture of a failed load and
   *  a picture with no caption: it cannot tell "there was no room" from "this
   *  engine cannot load this kind of model at all". This is the caption. */
  load_error?: string
  added_at: string
  /** Which workspaces may route to this model. The weights sit on one disk and
   *  are loaded once; what varies is who is allowed to use them, and this is the
   *  only place that is visible. Empty means nothing can use it yet. */
  workspaces: ModelWorkspace[]
}

export type PullState = 'downloading' | 'verifying' | 'ready' | 'failed' | 'cancelled'

export interface NodePull {
  id: string
  repo: string
  revision: string
  state: PullState
  bytes_done: number
  bytes_total: number
  files_done: number
  files_total: number
  error?: string
  model_uid?: string
  started_at: string
  updated_at: string
}

export interface NodeDetail extends NodeSummary {
  models: NodeModel[]
  pulls: NodePull[]
}

/** One machine's screen in ONE answer: the machine, and the workspaces a model
 *  on it can be given to. The screen cannot draw the one without the other. */
export interface MachineScreen {
  machine: NodeDetail
  workspaces: WorkspaceChoice[]
}

export interface LibraryHit {
  repo: string
  name: string
  kind: string
  downloads: number
  likes: number
}

export interface LibraryFile {
  path: string
  size: number
}

/** Whether this installation holds a credential for the model library, and who
 *  put it there. Never the credential itself: no route returns it. */
export interface LibraryCredential {
  configured: boolean
  updated_by?: number
  updated_at?: string
}

/** Whether something can be done here, and why not when it cannot. */
export interface Verdict {
  ok: boolean
  reason?: string
}

/** A library model resolved to ONE commit: what would be fetched, what it
 *  weighs, and whether this machine can take it. This is what a confirmation is
 *  drawn from, so that approving it means it will work. */
export interface LibraryModel {
  repo: string
  name: string
  revision: string
  kind: string
  license?: string
  gated: boolean
  files: LibraryFile[]
  size_bytes: number
  /** Every weights file on offer, so a refusal for ambiguity can say what to
   *  choose between. */
  choices: LibraryFile[]
  storage: Verdict
  memory: Verdict
  access: Verdict
  /** Whether anything on this machine could load these weights once they
   *  arrived. The only one of the four about the model rather than the
   *  machine's resources, and the one that stops an hour of downloading
   *  something nothing here can execute. */
  runtime: Verdict
  /** What the weights declare themselves to be, which is what `runtime` was
   *  decided on. Shown so a refusal says what it read. */
  architecture?: string
  already_here?: string
}

export const api = {
  /** The machines that run models we own. PLATFORM scope: a box is racked once
   *  and every workspace may have models on it. */
  /** Adding a model that is already on one of our machines. */
  localModels: {
    /** Every machine and what is on it, in ONE request, with what this
     *  workspace already has marked. */
    machines: () => json<{ machines: MachineChoice[] }>('/v1/models/machines'),
    /** Writes a route. Downloads nothing: the weights are on the machine. */
    add: (machineID: number, uid: string) =>
      json<Model>('/v1/models/local', send('POST', { machine_id: machineID, uid })),
  },

  nodes: {
    /** The list screen: every machine, asked in parallel whether it is up. */
    list: () => json<{ nodes: NodeSummary[] }>('/v1/nodes'),
    /** One machine in ONE answer: what it is, what is on its disk, who may use
     *  each of those, and anything downloading. */
    get: (id: number) => json<MachineScreen>(`/v1/nodes/${id}`),
    /** Removes the record and every workspace's route. The weights stay where
     *  they are: the machine may simply no longer be ours. */
    forget: (id: number) => json<{ removed: boolean }>(`/v1/nodes/${id}`, send('DELETE', {})),

    /** The token a machine is started with. The same one comes back until it is
     *  rotated, because it is meant to be pasted onto machine after machine. */
    /**
     * A token to start the next machine with, minted on the spot.
     *
     * One call, not a rotate-then-read pair: there is nothing to read back. A
     * token belongs to whoever asked for it, stands for an hour, and the machine
     * that uses it destroys it, so the only useful answer is a fresh one.
     */
    mintJoinToken: () => json<{ token: string }>('/v1/nodes/join-token', send('POST', {})),
    // Adding a machine that cannot call us back, which is two halves of one
    // exchange: our certificate goes out so the machine will accept only this
    // gateway, and the machine's own comes back so we accept only it.
    //
    // A GET for the outgoing half, unlike every other credential here, because
    // it is a public certificate: it carries no private key and its whole
    // purpose is to be copied onto machines.
    ourCertificate: () => json<{ certificate: string; key: string }>('/v1/nodes/certificate'),
    addByCertificate: (body: {
      name: string
      address: string
      certificate: string
      key: string
    }) =>
      json<{ id: number; node_id: string; name: string; base_url: string }>(
        '/v1/nodes/by-certificate',
        send('POST', body),
      ),

    /**
     * The one credential for weights published under terms somebody accepted.
     *
     * Read tells you only WHETHER there is one and who set it. There is no way
     * to read the credential back, deliberately: a credential that can be
     * fetched is one mistake away from a log or a browser history, which is the
     * same rule the join token follows.
     */
    libraryCredential: () => json<LibraryCredential>('/v1/nodes/library-credential'),
    setLibraryCredential: (token: string) =>
      json<LibraryCredential>('/v1/nodes/library-credential', send('PUT', { token })),
    clearLibraryCredential: () =>
      json<LibraryCredential>('/v1/nodes/library-credential', send('DELETE', {})),

    search: (id: number, q: string) =>
      json<{ models: LibraryHit[] }>(`/v1/nodes/${id}/library/search?q=${encodeURIComponent(q)}`),
    /** The promise: one commit, the exact files, and three verdicts. */
    describe: (id: number, repo: string, file?: string) =>
      json<LibraryModel>(
        `/v1/nodes/${id}/library/describe?repo=${encodeURIComponent(repo)}` +
          (file ? `&file=${encodeURIComponent(file)}` : ''),
      ),

    pull: (id: number, body: { repo: string; revision?: string; file?: string }) =>
      json<NodePull>(`/v1/nodes/${id}/pulls`, send('POST', body)),
    /** Stops it and KEEPS what it fetched, so it can be resumed. */
    cancelPull: (id: number, pullID: string) =>
      json<NodePull>(`/v1/nodes/${id}/pulls/${pullID}/cancel`, send('POST', {})),
    /** Carries on from where it stopped: whole files skipped, a partial one
     *  continued. Not a retry. */
    resumePull: (id: number, pullID: string) =>
      json<NodePull>(`/v1/nodes/${id}/pulls/${pullID}/resume`, send('POST', {})),
    /** Forgets it and removes the bytes. The only one that throws work away. */
    deletePull: (id: number, pullID: string) =>
      json<{ removed: boolean }>(`/v1/nodes/${id}/pulls/${pullID}`, send('DELETE', {})),

    /** The form AND its values, in one answer: they are one question. */
    form: (id: number, uid: string) =>
      json<{ sections: TemplateSection[]; values: Record<string, string> }>(
        `/v1/nodes/${id}/models/${uid}/form`,
      ),
    saveSettings: (id: number, uid: string, values: Record<string, string>) =>
      json<NodeModel>(`/v1/nodes/${id}/models/${uid}/settings`, send('PUT', { values })),
    /** Returns as soon as the machine has STARTED loading. The model reads
     *  `waking` until it is in. */
    load: (id: number, uid: string) =>
      json<NodeModel>(`/v1/nodes/${id}/models/${uid}/load`, send('POST', {})),
    unload: (id: number, uid: string) =>
      json<NodeModel>(`/v1/nodes/${id}/models/${uid}/unload`, send('POST', {})),
    /** Who may route to this model. The WHOLE list, not a change to it. Sharing
     *  downloads nothing: the weights are already on the machine. */
    share: (id: number, uid: string, workspaces: number[]) =>
      json<{ workspaces: number }>(
        `/v1/nodes/${id}/models/${uid}/workspaces`,
        send('PUT', { workspaces }),
      ),
    remove: (id: number, uid: string) =>
      json<{ uid: string; name: string; freed_bytes: number }>(
        `/v1/nodes/${id}/models/${uid}`,
        send('DELETE', {}),
      ),
  },

  /**
   * Whether this installation can answer anything yet.
   *
   * Runtime, unlike the posture: what kind of build this is never changes,
   * whereas whether setup is finished changes the moment somebody finishes it.
   */
  setup: {
    state: () => json<SetupState>('/v1/setup'),
  },

  vendors: {
    /**
     * The vendors screen in ONE answer: the vendors configured, and the kinds
     * one can be. The catalogue is not a second question — the screen cannot
     * offer to add a vendor without knowing what kinds exist, and it is a
     * constant, so asking for it separately was a request that could never
     * return anything different.
     */
    list: () => json<{ vendors: Vendor[]; kinds: VendorKind[] }>('/v1/vendors'),
    catalog: (id: number) => json<VendorCatalog>(`/v1/vendors/${id}/catalog`),
    create: (v: Partial<Vendor> & { credentials?: string }) =>
      json<Vendor>('/v1/vendors', send('POST', v)),
    update: (id: number, v: Partial<Vendor> & { credentials?: string }) =>
      json<Vendor>(`/v1/vendors/${id}`, send('PUT', v)),
    remove: (id: number) => nothing(`/v1/vendors/${id}`, { method: 'DELETE' }),
    clearCredentials: (id: number) => nothing(`/v1/vendors/${id}/credentials`, { method: 'DELETE' }),
  },

  models: {
    /**
     * The models screen in ONE answer: the models and the vendors they belong
     * to. The page shows each model's vendor name, asks whether a vendor is
     * openai-compatible to decide a field, and needs to know whether any vendor
     * exists before it can offer to add a model — the last of which is a fact
     * about an empty list and cannot ride on a row. It was a second request
     * that arrived separately and rewrote the screen when it landed.
     */
    list: () => json<{ models: Model[]; vendors: Vendor[] }>('/v1/models'),
    create: (m: Partial<Model>) => json<Model>('/v1/models', send('POST', m)),
    update: (id: number, m: Partial<Model>) => json<Model>(`/v1/models/${id}`, send('PUT', m)),
    remove: (id: number) => nothing(`/v1/models/${id}`, { method: 'DELETE' }),
  },

  tools: {
    list: () => json<Tool[]>('/v1/tools'),
    get: (id: number) => json<ToolDetail>(`/v1/tools/${id}`),
    templates: () => json<ToolTemplate[]>('/v1/tools/templates'),
    /** The form for a NEW tool: its fields and the values it starts from, together. */
    newForm: (name: string, variant: string) =>
      json<NewToolForm>(`/v1/tools/templates/${name}/fields?variant=${encodeURIComponent(variant)}`),
    createCustom: (body: {
      template: string
      variant: string
      alias: string
      display_name?: string
      description?: string
      guide?: string
      param_descriptions?: Record<string, string>
      settings: Record<string, unknown>
    }) => json<Tool>('/v1/tools/custom', send('POST', body)),
    // Running one of the template's own operations. Testing the connection is the
    // default; a template that cannot finish without something more (a
    // verification code, a choice) answers with a prompt describing the fields to
    // collect, which the console renders without knowing what they mean.
    testCustom: (body: {
      template: string
      variant: string
      settings: Record<string, unknown>
      action?: string
      token?: string
      values?: Record<string, string>
    }) => json<ToolActionResult>('/v1/tools/custom/test', send('POST', body)),
    // Testing an edit tests against the stored tool, so a secret left blank is
    // carried forward from the sealed value instead of sent empty.
    testCustomEdit: (
      id: number,
      body: {
        settings: Record<string, unknown>
        action?: string
        token?: string
        values?: Record<string, string>
      },
    ) => json<ToolActionResult>(`/v1/tools/custom/${id}/test`, send('POST', body)),
    updateCustom: (
      id: number,
      body: {
        settings: Record<string, unknown>
        display_name?: string
        description?: string
        guide?: string
        param_descriptions?: Record<string, string>
      },
    ) => json<Tool>(`/v1/tools/custom/${id}`, send('PUT', body)),
    removeCustom: (id: number) => nothing(`/v1/tools/${id}`, { method: 'DELETE' }),
    update: (id: number, t: { status: string; grants: number[]; settings?: Record<string, string> }) =>
      json<Tool>(`/v1/tools/${id}`, send('PUT', t)),
  },

  agents: {
    /**
     * The Gateway SCREEN, shaped like the screen: what arrives is read (files,
     * audio), the Gateway thinks with it, and work goes to the agents.
     *
     * Not the agents collection. That was a flat list the client picked apart —
     * find the one keyed `default`, call it the Gateway, call the rest the
     * agents, then reach inside it for its file rules and its audio model and
     * present those as two more sections. That is the screen's own structure,
     * rebuilt on this side of the wire from a list flattened to send it.
     */
    screen: () => json<GatewayScreen>('/v1/gateway'),
    /**
     * Each section of the screen is written on its own.
     *
     * A form that changes four file rules used to send the whole agent back,
     * which meant fetching the whole agent first to have something to echo: its
     * instructions, its brains, its confirm list and its timestamps, all
     * round-tripped so one field could move.
     */
    putFiles: (rules: { types: string[]; model_id: number }[]) =>
      nothing('/v1/gateway/files', send('PUT', { rules })),
    putAudio: (model_id: number | null) =>
      nothing('/v1/gateway/audio', send('PUT', { model_id })),
    /**
     * And each form is READ on its own, when it opens.
     *
     * This screen had three dialogs and five requests, every one of them fired
     * the moment ANY dialog opened: the models, the vendors (to label the
     * models), the tools, the brains and the agent. The dialog that sets one
     * audio model was waiting on the tool catalogue.
     */
    filesForm: () => json<FilesFormBody>('/v1/gateway/files/form'),
    audioForm: () => json<AudioFormBody>('/v1/gateway/audio/form'),
    /** The agent dialog. No id is the form for an agent that does not exist yet. */
    form: (id?: number) => json<AgentFormBody>(id ? `/v1/agents/${id}/form` : '/v1/agents/form'),
    create: (a: Partial<Agent>) => json<Agent>('/v1/agents', send('POST', a)),
    update: (id: number, a: Partial<Agent>) => json<Agent>(`/v1/agents/${id}`, send('PUT', a)),
    remove: (id: number) => nothing(`/v1/agents/${id}`, { method: 'DELETE' }),
  },

  workflows: {
    list: () => json<Workflow[]>('/v1/workflows'),
    create: (w: { name: string }) => json<Workflow>('/v1/workflows', send('POST', w)),
    update: (id: number, w: { name: string; status: string }) =>
      json<Workflow>(`/v1/workflows/${id}`, send('PUT', w)),
    remove: (id: number) => nothing(`/v1/workflows/${id}`, { method: 'DELETE' }),
    versions: (id: number) => json<WorkflowVersion[]>(`/v1/workflows/${id}/versions`),
    createVersion: (id: number, definition: unknown) =>
      json<WorkflowVersion>(`/v1/workflows/${id}/versions`, send('POST', { definition })),
    publish: (id: number, versionID: number) =>
      nothing(`/v1/workflows/${id}/publish`, send('POST', { version_id: versionID })),
    assignments: (id: number) => json<Assignment[]>(`/v1/workflows/${id}/assignments`),
    setAssignments: (id: number, assignments: Assignment[]) =>
      nothing(`/v1/workflows/${id}/assignments`, send('PUT', assignments)),
  },

  users: {
    list: () => json<User[]>('/v1/users'),
    create: (u: { email: string; name: string; password: string; workspaces: number[] }) =>
      json<User>('/v1/users', send('POST', u)),
    update: (id: number, u: { email: string; name: string; status: string; workspaces: number[] }) =>
      json<User>(`/v1/users/${id}`, send('PUT', u)),
    remove: (id: number) => nothing(`/v1/users/${id}`, { method: 'DELETE' }),
    /**
     * The dialog, in one answer: this workspace's groups with this person's
     * memberships already marked.
     *
     * It was two requests and a join — every group of the workspace, and every
     * group this person belongs to anywhere, kept where the two overlapped.
     * That intersection IS the workspace scoping rule, and the rule is the
     * server's, not something a console re-derives from two lists.
     */
    form: (id?: number) => json<UserFormBody>(id ? `/v1/users/${id}/form` : '/v1/users/form'),
    /** Replace their memberships among the current workspace's groups. */
    setGroups: (id: number, groups: number[]) =>
      nothing(`/v1/users/${id}/groups`, send('PUT', { groups })),
  },

  mcpServers: {
    list: () => json<MCPServer[]>('/v1/mcp-servers'),
    create: (m: { name: string; url: string; auth_type: string; api_key?: string }) =>
      json<MCPServer>('/v1/mcp-servers', send('POST', m)),
    update: (id: number, m: { name: string; url: string; auth_type: string; api_key?: string; status: string }) =>
      json<MCPServer>(`/v1/mcp-servers/${id}`, send('PUT', m)),
    remove: (id: number) => nothing(`/v1/mcp-servers/${id}`, { method: 'DELETE' }),
    sync: (id: number) => json<MCPSyncResult>(`/v1/mcp-servers/${id}/sync`, { method: 'POST' }),
    connect: (id: number) =>
      json<{ authorize_url: string }>(`/v1/mcp-servers/${id}/connect`, { method: 'POST' }),
  },

  mcpServer: {
    /**
     * The MCP server screen in ONE answer: what may be exposed, and whether
     * each thing IS.
     *
     * It was three requests and a client-side join — the tool catalogue, the
     * brains, the saved settings — then walking the catalogue deciding each
     * checkbox from `configured ? saved : true`. That rule is the server's, so
     * a checkbox now arrives already knowing whether it is ticked.
     */
    get: () => json<MCPScreen>('/v1/mcp-server'),
    put: (settings: { tool_config: Record<string, { enabled: boolean }>; brain_config: number[] }) =>
      json<MCPServerSettings>('/v1/mcp-server', send('PUT', settings)),
  },

  oauthClients: {
    list: () => json<OAuthClient[]>('/v1/oauth-clients'),
    create: (c: { name: string; client_type: string; redirect_uris: string[] }) =>
      json<OAuthClient>('/v1/oauth-clients', send('POST', c)),
    update: (id: number, c: { name: string; client_type: string; redirect_uris: string[]; status: string }) =>
      json<OAuthClient>(`/v1/oauth-clients/${id}`, send('PUT', c)),
    remove: (id: number) => nothing(`/v1/oauth-clients/${id}`, { method: 'DELETE' }),
  },

  workspaces: {
    list: () => json<Workspace[]>('/v1/workspaces'),
    create: (w: { name: string; description: string }) =>
      json<Workspace>('/v1/workspaces', send('POST', w)),
    update: (id: number, w: { name: string; description: string; status: string }) =>
      json<Workspace>(`/v1/workspaces/${id}`, send('PUT', w)),
    remove: (id: number) => nothing(`/v1/workspaces/${id}`, { method: 'DELETE' }),
  },

  groups: {
    list: () => json<Group[]>('/v1/groups'),
    create: (g: { name: string }) => json<Group>('/v1/groups', send('POST', g)),
    update: (id: number, g: { name: string }) => json<Group>(`/v1/groups/${id}`, send('PUT', g)),
    remove: (id: number) => nothing(`/v1/groups/${id}`, { method: 'DELETE' }),
  },

  roles: {
    /**
     * The roles screen in ONE answer: the roles, and the permissions a role can
     * hold. The catalogue is a constant the screen cannot draw a form without,
     * so asking for it separately was a request that could never differ.
     */
    list: () => json<{ roles: Role[]; permissions: Permission[] }>('/v1/roles'),
    create: (r: { name: string; permissions: string[] }) => json<Role>('/v1/roles', send('POST', r)),
    update: (id: number, r: { name: string; permissions: string[] }) =>
      json<Role>(`/v1/roles/${id}`, send('PUT', r)),
    remove: (id: number) => nothing(`/v1/roles/${id}`, { method: 'DELETE' }),
  },
}

/**
 * useResource loads a list and reloads it after a write.
 *
 * It always re-asks the server rather than patching what it holds. A console
 * that edits its own copy after a save eventually shows something the server
 * does not agree with, and the person looking at it has no way to tell.
 */
/**
 * `when` gates the fetch. A resource a screen needs only inside a dialog should
 * not be fetched on every view of that screen: the Gateway page was asking for
 * tools, brains and vendors on load, for three forms nobody had opened. Pass
 * `false` and nothing is requested until it turns true, then it loads once.
 */
export function useResource<T>(load: () => Promise<T>, when = true) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')

  const reload = useCallback(async () => {
    try {
      setData(await load())
      setError('')
    } catch {
      setError('The gateway did not answer.')
    }
    // The loader is defined inline by every caller, so depending on it would
    // reload forever. The caller's own dependencies decide when to re-fetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    // Not wanted yet, or already here: a gate that turns true a second time
    // must not re-ask for what it is already holding.
    if (!when || data !== null) return
    let current = true
    void load()
      .then((value) => {
        if (current) {
          setData(value)
          setError('')
        }
      })
      .catch(() => {
        if (current) setError('The gateway did not answer.')
      })
    return () => {
      current = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [when])

  return { data, error, reload }
}
