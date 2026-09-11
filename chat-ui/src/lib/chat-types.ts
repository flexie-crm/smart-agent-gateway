// lib/chat-types.ts
// ─── Canonical type definitions for the AI chat system ──────────────────────

// ─── SSE Frame Protocol ─────────────────────────────────────────────────────
// Discriminated union of every SSE frame the backend can send.
// The `type` field is the discriminator.

export type SSEFrame =
  | DeltaFrame
  | ReasoningDeltaFrame
  | ResultFrame
  | ErrorFrame
  | HeartbeatFrame
  | ToolPreparingFrame
  | ToolFrame
  | AgentStartFrame
  | AgentEndFrame
  | ConfirmRequestFrame
  | ConfirmResolvedFrame
  | ClientToolCallFrame
  | ImportResultFrame
  | SaidFrame;

export interface DeltaFrame {
  type: 'delta';
  message: string;
  final?: false;
  is_agent?: boolean;
}

export interface ReasoningDeltaFrame {
  type: 'reasoning_delta';
  message: string;
  final?: false;
}

export interface ResultFrame {
  type: 'result';
  message: string;
  final: true;
}

export interface ErrorFrame {
  type: 'error';
  message: string;
  final: true;
  /**
   * The machine-readable reason, when the server refused rather than failed:
   * `expired`, `invalid_token`. A caller that can DO something about a
   * particular refusal needs to recognise it, and matching on the human
   * sentence would break the first time somebody improved the wording.
   */
  code?: string;
}

export interface HeartbeatFrame {
  type: 'heartbeat';
  message?: string;
  final?: false;
}

// SAG: identity and label are separate. The CRM sent one `tool` string and
// left the UI to guess which it was; a tool id and the words a person reads
// are different things and are now sent as different fields.
export interface ToolPreparingFrame {
  type: 'tool_preparing';
  message: {
    tool_name: string;
    friendly_name?: string;
  };
  final?: false;
}

// SAG: the finished-tool frame. `duration_ms` says its unit, and status is
// the runtime's own vocabulary rather than a UI word, so the client decides
// how to render it instead of the server deciding for it.
export interface ToolFrame {
  type: 'tool';
  message: {
    tool_name: string;
    friendly_name?: string;
    done?: boolean;
    status?: 'completed' | 'failed' | 'rejected';
    duration_ms?: number;
    input?: unknown;
    output?: unknown;
    requested_approval?: boolean;
    /**
     * The stored call, when there is one to open. Absent for a tool that must
     * not be opened (the assistant's own wiring) and for a call with no row,
     * which is the same absence the history answer uses.
     */
    id?: string;
  };
  final?: false;
  is_agent?: boolean;
}

/**
 * The Gateway handing a task to an agent, and getting it back.
 *
 * The payload is the server's AgentMessage and nothing else: which agent, what
 * it is called, and in which mode it was handed the work. It was `any`, and
 * under the `any` this client read a `model` field that no version of the
 * server has ever sent, so the reasoning attribution it fed was always empty.
 * A type is how that stops being invisible.
 */
export interface AgentStartMessage {
  /** The agent's key, which is its stable id. */
  agent: string;
  /** What it is called, for a person to read. */
  name?: string;
  /** terminal, continue, fleet or background (KB/27). */
  mode?: string;
}

export interface AgentStartFrame {
  type: 'agent_start';
  message?: AgentStartMessage;
  final?: false;
}

/** The end of that bracket. It carries nothing: the bracket IS the message. */
export interface AgentEndFrame {
  type: 'agent_end';
  final?: false;
}

// SAG: the card carries the action's REAL risk level and its expiry. The CRM
// flattened risk into three visual weights on the server; deciding how loudly
// to warn is the client's job, and an approval that can expire should say so.
export type ToolRisk =
  | 'read_only'
  | 'internal_write'
  | 'external_communication'
  | 'financial_action'
  | 'destructive_action'
  | 'admin_action';

export interface ConfirmRequestFrame {
  type: 'confirm_request';
  message: {
    token: string;
    title: string;
    description: string;
    severity?: ToolRisk;
    /** The exact arguments the action will run with, so a person can read them. */
    details?: Record<string, unknown>;
    /** Unix seconds. After this the approval can no longer be redeemed. */
    expires_at?: number;
  };
  /** SAG: the card ENDS the stream. Nothing is held open while a person decides. */
  final?: boolean;
}

/**
 * Something the person said WHILE the turn was running, at the point the turn
 * folded it in.
 *
 * A conversation answers one thing at a time, so a message typed mid-answer goes
 * into the running turn rather than starting a second one. This frame is where
 * it landed: the answer so far is finished, these words come after it, and what
 * the model says next is a new answer. Without it the screen keeps appending to
 * one assistant message, the words end up last, and the conversation rearranges
 * itself on the next reload.
 */
export interface SaidFrame {
  type: 'said';
  /** What was said. */
  message: string;
}

export interface ConfirmResolvedFrame {
  type: 'confirm_resolved';
  message: {
    token: string;
    status: 'approved' | 'rejected' | 'expired';
  };
  final?: false;
}

export interface ClientToolCallFrame {
  type: 'client_tool_call';
  message: {
    name: string;
    friendly_name?: string;
    args: Record<string, unknown>;
    requires_confirm?: boolean;
  };
  final?: false;
}

export interface FileProcessingFrame {
  type: 'file_processing';
  message: {
    status: 'start' | 'processing' | 'complete';
    current?: number;
    total?: number;
    file?: string;
    files?: string[];
  };
  final?: false;
}

export interface ImportProgressFrame {
  type: 'import_progress';
  message: {
    entity_type: string;
    current: number;
    total: number;
    created: number;
    merged: number;
    ignored: number;
    percent: number;
  };
  final?: false;
}

export interface ImportResultFrame {
  type: 'import_result';
  message: {
    entity_type: string;
    created: number;
    merged: number;
    ignored: number;
    total: number;
    import_id: string;
    failed?: number;
    fail_reasons?: Array<{ reason: string; count: number }>;
  };
  final?: false;
}

// ─── File Attachments ───────────────────────────────────────────────────────

/**
 * A file somebody attached, as the server describes it.
 *
 * The shape is the upload endpoint's answer, not a shape of our own: `id` is
 * the opaque public id that rides along with the message, and `file_type` is
 * the lower-case extension the Gateway's rules matched on. previewUrl is the
 * one field the client owns, a local blob URL for showing an image back before
 * it has been sent anywhere.
 */
export interface FileAttachment {
  id: string;
  file_name: string;
  file_type: string;
  size_bytes: number;
  previewUrl?: string;
}

// ─── Message Model ──────────────────────────────────────────────────────────

export interface ConfirmationRequest {
  token: string;
  title: string;
  description: string;
  /**
   * The tool's own risk level, exactly as the server declared it.
   *
   * It used to be typed as the reference CRM's `info | warning | danger`, which
   * the server has never sent, so the card's every comparison against it missed
   * and its colour system was decoration. One vocabulary, wire to pixel.
   */
  severity?: ToolRisk;
  /**
   * The arguments the action will run with, exactly as the tool declared them.
   *
   * `unknown` values, not strings, because that is what the wire carries: SAG
   * sends the REAL arguments where the CRM flattened everything to strings
   * (see the protocol table above), so a number is a number and a headers
   * object is an object. This was typed `Record<string, string>` against a
   * frame already typed `Record<string, unknown>` — the same wire-to-model
   * contradiction that made the severity vocabulary drift, and equally
   * invisible with no typechecker running.
   */
  details?: Record<string, unknown>;
  status?: 'pending' | 'approved' | 'rejected' | 'timeout';
}


// ─── Tool timeline ──────────────────────────────────────────────────────────
// One tool the agent ran in this step, for the in-message timeline. Populated
// fully from persisted history (friendly_name + status + duration + approval);
// during a live stream only friendly_name is known (the rest lands on reload).

export interface ToolChip {
  name: string;
  friendly_name: string;
  status?: 'completed' | 'failed' | 'rejected' | 'running' | string | null;
  duration_ms?: number | null;
  requested_approval?: boolean;
  /**
   * The stored call. It is what makes the row openable: with it the chat can
   * ask what the tool was sent and answered, and without it there is nothing
   * to open at all.
   *
   * It arrives BOTH ways: on the frame that says the tool finished, and on a
   * conversation read back from the database. It used to arrive only the second
   * way, so watching a tool run left a row you had to reload the page to open.
   */
  id?: string;
}

// An assistant turn is an ordered sequence of parts — the model's reasoning, the text
// it wrote, and the tools it ran, in the exact order they happened. Rendered as one
// flat, uniformly-spaced timeline. A tool part is appended the moment its tool
// finishes (with duration/status). Position comes from this order, which mirrors the
// persisted `seq` (a tool is never mid-sentence: text then tools, per step).
export type MessagePart =
  | { kind: 'reasoning'; text: string; model?: string }
  | { kind: 'text'; text: string }
  | { kind: 'tool'; tool: ToolChip }
  // What an agent the Gateway delegated to said, kept apart from the Gateway's
  // own answer (a continue-mode delegation, so the Gateway speaks after).
  | { kind: 'agent'; text: string; agent?: string };

export interface ChatMessage {
  id: string;
  content: string;
  role: 'user' | 'assistant' | 'confirm';
  reasoning?: string;
  reasoning_vendor?: string;
  reasoning_model?: string;
  vendor?: string;
  model?: string;
  tools?: ToolChip[];
  parts?: MessagePart[];
  isStreaming?: boolean;
  /**
   * Said while the assistant was still answering, and not yet sent.
   *
   * A conversation answers one thing at a time (the server refuses a second
   * turn outright: "this conversation is already answering"), so something said
   * mid-answer has to wait its turn. It is shown straight away, because it HAS
   * been said, and it is marked so that waiting looks like waiting rather than
   * like nothing happening.
   */
  waiting?: boolean;
  error?: string;
  confirmation?: ConfirmationRequest;
  attachments?: FileAttachment[];
}

// ─── Type Guards ────────────────────────────────────────────────────────────

export function isConfirmMessage(msg: ChatMessage): msg is ChatMessage & { role: 'confirm'; confirmation: ConfirmationRequest } {
  return msg.role === 'confirm' && !!msg.confirmation;
}

export function isAssistantMessage(msg: ChatMessage): msg is ChatMessage & { role: 'assistant' } {
  return msg.role === 'assistant';
}

export function isUserMessage(msg: ChatMessage): msg is ChatMessage & { role: 'user' } {
  return msg.role === 'user';
}

// ─── Agent Activity State ───────────────────────────────────────────────────
// Single discriminated union representing what the agent is currently doing.
// Replaces the scattered `activeTool` + boolean condition approach.

export type AgentActivity =
  | { kind: 'idle' }
  | { kind: 'running' }
  | { kind: 'reasoning' }
  | { kind: 'tool'; name: string }
  | { kind: 'responding' }
  | { kind: 'confirming' }
  | { kind: 'uploading'; detail: string };

// ─── Inline Form Types (for FormBlock) ──────────────────────────────────────

export type InlineFormFieldType =
  | 'text'
  | 'textarea'
  | 'email'
  | 'tel'
  | 'number'
  | 'date'
  | 'select'
  | 'multiselect'
  | 'checkbox'
  | 'multicheckbox'
  | 'radio'
  | 'info'
  | 'hidden';

export type InlineFormOption = {
  value: string;
  label: string;
};

export type InlineForm = {
  kind: 'inline_form';
  form_id: string;
  purpose: string;
  title?: string;
  submit_label?: string;
  fields: Array<{
    name: string;
    label: string;
    type: InlineFormFieldType;
    required?: boolean;
    placeholder?: string;
    options?: InlineFormOption[];
    default_value?: string | boolean | string[];
    variant?: 'info' | 'warning' | 'danger' | 'success' | 'muted';
    content?: string;
    html?: boolean;
  }>;
};

// ─── Duplicate Table (for DupMergeBlock) ────────────────────────────────────
//
// Mirror of the JSON envelope returned by CoreBundle/Helper/AI/Tools/
// DuplicateMerge/Handler.php. Both bulk-scan and by-id scopes share the same
// `kind` discriminator so response.tsx can detect via shape sniff (parsed.kind
// === 'duplicate_table') without needing a fenced-block language tag.

export type DuplicateTableRecord = {
  id: number;
  name: string;
  email?: string | null;
  phone?: string | null;
  url?: string | null;
};

export type DuplicateTableCrossEntity = {
  type: string;
  count: number;
  rows: DuplicateTableRecord[];
};

export type DuplicateTableChatAction = {
  type: string;            // 'open_merge_modal'
  entity_type: string;
  ids: number[];
  gateway_id: number;
  label: string;
};

export type DuplicateTableRow = {
  primary: DuplicateTableRecord;
  duplicates_same_entity: DuplicateTableRecord[];
  cross_entity: DuplicateTableCrossEntity | null;
  merge_url: string | null;
  chat_action: DuplicateTableChatAction | null;
};

export type DuplicateTableBulk = {
  kind: 'duplicate_table';
  mode: 'scan';
  scope: 'bulk';
  entity_type: string;
  group_count: number;
  truncated: boolean;
  rows: DuplicateTableRow[];
  summary: string;
  next_action?: string;
};

export type DuplicateTableById = {
  kind: 'duplicate_table';
  mode: 'scan';
  scope: 'by_id';
  entity_type: string;
  entity_id: number;
  primary: DuplicateTableRecord;
  duplicates_same_entity: DuplicateTableRecord[];
  cross_entity: DuplicateTableCrossEntity | null;
  advice: string[];
  merge_url: string | null;
  chat_action: DuplicateTableChatAction | null;
  summary: string;
  next_action?: string;
};

export type DuplicateTable = DuplicateTableBulk | DuplicateTableById;

// ─── Legacy types for backward-compat ───────────────────────────────────────

export type UIMessage = {
  id: string;
  content: string;
  role: 'user' | 'assistant';
  createdAt?: Date;
  form?: InlineForm;
};

export type ChatStatus =
  | 'idle'
  | 'loading'
  | 'streaming'
  | 'submitted'
  | 'error'
  | 'ready'
  | 'submitting';

export type UseChatOptions = {
  api?: string;
  id?: string;
  initialInput?: string;
  initialMessages?: UIMessage[];
  onResponse?: (response: Response) => void;
  onFinish?: (message: UIMessage) => void;
  onError?: (error: Error) => void;
};

export type UseChatHelpers = {
  messages: UIMessage[];
  input: string;
  handleSubmit: (e: React.SyntheticEvent<HTMLFormElement>, options?: any) => void;
  handleInputChange: (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  isLoading: boolean;
  stop: () => void;
  reload: () => void;
  append: (message: UIMessage) => void;
  setInput: (input: string) => void;
};