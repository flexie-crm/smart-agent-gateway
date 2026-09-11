import { useState } from 'react'
import { Loader2, Plus } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { TokenMultiSelect } from '@/components/ui/token-multiselect'
import type { TokenOption } from '@/components/ui/token-multiselect'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { bySource, toolLabel } from '@/lib/tool-display'
import { cn } from '@/lib/utils'
import { api, useResource } from '@/lib/resources'
import type { AgentFormBody, AudioFormBody, FilesFormBody } from '@/lib/resources'

/**
 * The Gateway, and the agents it can hand work to.
 *
 * One workspace has one GATEWAY (the agent keyed `default`): the one the person
 * actually talks to, and the one that is configured. It leads the page on its
 * own, because it is not one of a list. Everything below it is an agent the
 * Gateway may delegate to, and every one of those is optional. The person never
 * picks an agent or a model; they ask, and the Gateway decides whether to
 * answer, call a tool, or hand the work on.
 *
 * Nothing here is compulsory. A workspace with no Gateway still works: the
 * code's own defaults answer. Everything on this screen is an override.
 */
const GATEWAY = 'default'

/**
 * The file types a rule can name, offered as a list rather than typed.
 *
 * Typed extensions are a quiet way to be wrong: "jpeg" and "jpg", a stray dot,
 * a capital letter, and a rule that silently never matches. These are the ones
 * a model can actually be handed, grouped the way somebody thinks about them.
 */
const FILE_TYPES: { value: string; label: string; hint: string }[] = [
  { value: 'png', label: 'PNG', hint: 'Image' },
  { value: 'jpg', label: 'JPEG', hint: 'Image' },
  { value: 'gif', label: 'GIF', hint: 'Image' },
  { value: 'webp', label: 'WebP', hint: 'Image' },
  { value: 'heic', label: 'HEIC', hint: 'Image' },
  { value: 'pdf', label: 'PDF', hint: 'Document' },
  { value: 'docx', label: 'Word', hint: 'Document' },
  { value: 'txt', label: 'Plain text', hint: 'Document' },
  { value: 'md', label: 'Markdown', hint: 'Document' },
  { value: 'rtf', label: 'Rich text', hint: 'Document' },
  { value: 'xlsx', label: 'Excel', hint: 'Spreadsheet' },
  { value: 'csv', label: 'CSV', hint: 'Spreadsheet' },
  { value: 'pptx', label: 'PowerPoint', hint: 'Slides' },
  { value: 'json', label: 'JSON', hint: 'Data' },
  { value: 'xml', label: 'XML', hint: 'Data' },
]

const FILE_TYPE_LABEL = new Map(FILE_TYPES.map((t) => [t.value, t.label]))


export function Agents() {
  // The row that was clicked, not the agent: the screen holds rows, and the
  // form fetches the agent it is going to edit.
  const [editing, setEditing] = useState<{ id?: number; gateway?: boolean } | null>(null)
  // Files and audio are configured on their own, not inside the Gateway's form:
  // they are a step BEFORE it rather than a property of it.
  const [files, setFiles] = useState(false)
  const [audio, setAudio] = useState(false)

  // The screen, in one answer, already in the screen's own shape: files, audio,
  // the Gateway, the agents. Nothing here picks a list apart to find out which
  // of its entries is which.
  //
  // And ONE answer is all of it. What each dialog needs, that dialog asks for
  // when it opens (see WithForm below). This page used to hold five resources:
  // the models, the vendors that only labelled them, the tools, the brains and
  // the agent, all fetched the moment ANY of the three dialogs opened, so the
  // one that sets an audio model waited on the tool catalogue.
  const { data: screen, reload } = useResource(() => api.agents.screen())

  // What the Gateway can be handed, in the order somebody would say it. Nothing
  // configured reads as "text only", which is the honest description of a
  // Gateway that cannot be sent a file.
  const loading = screen === null
  // There is exactly one Gateway and it cannot be deleted: a workspace without
  // one has no way to answer anybody. It is set up once and edited thereafter,
  // which is why nothing on this screen offers to remove it.
  const gateway = screen?.gateway ?? null
  const agents = screen?.agents ?? null
  const fileRules = screen?.files.rules ?? []
  const audioSlot = screen?.audio ?? null


  return (
    <Page
      title="Gateway"
      description="What happens when somebody sends a message, in the order it happens."
    >
      {/* The page is the pipeline, top to bottom: what arrives is read, the
          Gateway thinks with it, and work is handed on. Reading it downwards is
          reading what actually happens to a message, which is the only ordering
          somebody has to learn once. */}

      <Step
        n={1}
        first
        title="What people send"
        blurb="People send more than words. A picture, a document or a recording has to be read by a model before the assistant can use it, and you choose which model reads what."
        carries="what those files said, along with the message the person wrote"
      >
        <div className="grid gap-6 sm:grid-cols-2">
          <SlotBlock
            label="Files"
            loading={loading}
            onEdit={gateway ? () => setFiles(true) : undefined}
            empty={
              fileRules.length === 0
                ? 'Nothing can be attached yet. Choose what reads each kind of file.'
                : null
            }
          >
            {/* Each rule against the model that reads it. "Two rules" is a
                number somebody has to open a form to understand; this says what
                will happen to the next file that arrives, which is the thing
                anybody actually wanted to know. */}
            {fileRules.map((rule, i) => (
              <div key={i} className="flex items-baseline justify-between gap-3 py-1.5">
                <span className="text-sm">
                  {rule.types.length === 0
                    ? 'Anything else'
                    : rule.types.map((t) => FILE_TYPE_LABEL.get(t) ?? t).join(', ')}
                </span>
                <span className="shrink-0 font-mono text-xs text-muted-foreground">
                  {rule.model_name || 'a model that is gone'}
                </span>
              </div>
            ))}
          </SlotBlock>
          <SlotBlock
            label="Audio"
            loading={loading}
            onEdit={gateway ? () => setAudio(true) : undefined}
            empty={
              !audioSlot?.model_name
                ? 'Nobody can talk instead of typing until a model is chosen.'
                : null
            }
          >
            <div className="flex items-baseline justify-between gap-3 py-1.5">
              <span className="text-sm">Anything spoken</span>
              <span className="shrink-0 font-mono text-xs text-muted-foreground">
                {audioSlot?.model_name ?? ''}
              </span>
            </div>
          </SlotBlock>
        </div>
      </Step>

      <Step
        n={2}
        title="The Gateway"
        blurb="The assistant people actually talk to. It answers them, uses the tools you have given it, or decides a job is better done by one of the agents below."
        carries="the job, whenever the Gateway decides an agent should take it"
        action={
          gateway ? (
            <Button variant="outline" size="sm" onClick={() => setEditing({ id: gateway.id })}>
              Edit Gateway
            </Button>
          ) : (
            !loading && (
              <Button size="sm" onClick={() => setEditing({ gateway: true })}>
                <Plus className="size-4" />
                Set up the Gateway
              </Button>
            )
          )
        }
      >
        {loading ? (
          <Loader2 className="size-4 animate-spin text-muted-foreground" />
        ) : gateway ? (
          <div className="flex flex-wrap items-center gap-x-10 gap-y-3">
            <div>
              <div className="text-xs text-muted-foreground">Name</div>
              <div className="text-sm font-medium">{gateway.name}</div>
            </div>
            <Fact label="Model" value={gateway.model_name || 'no model'} mono />
            <Fact label="Tools" value={gateway.tools === 0 ? 'none' : String(gateway.tools)} />
            <Fact label="Thinks" value={gateway.reasoning ? 'yes' : 'no'} />
            <Fact label="Status" value={gateway.status === 'active' ? 'active' : 'off'} />
          </div>
        ) : (
          <div className="rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
            <p className="text-sm text-muted-foreground">
              No Gateway is set up yet, so nobody can be answered: the chat tells people to ask
              an administrator. Everything else on this page waits on this one being set up.
            </p>
          </div>
        )}
      </Step>

      <Step
        n={3}
        title="Agents"
        blurb="Helpers for particular jobs. They are optional, the Gateway decides on its own when to use one, and the people using the chat never see this list or choose from it."
        last
        action={
          <Button size="sm" onClick={() => setEditing({ gateway: false })}>
            <Plus className="size-4" />
            New agent
          </Button>
        }
      >
        <DataTable
          items={agents}
          empty="No agents. The Gateway handles everything itself, which is a perfectly good place to stay."
          onEdit={(agent) => setEditing({ id: agent.id })}
          remove={{
            run: async (agent) => {
              await api.agents.remove(agent.id)
              await reload()
            },
            confirm: (a) => `Delete "${a.name}"? The Gateway stops handing work to it.`,
            done: (a) => `${a.name} was deleted.`,
          }}
          columns={[
            {
              header: 'Agent',
              // Only the name: the key is an internal alias, not something the
              // person needs to see.
              cell: (a) => <div className="font-medium">{a.name}</div>,
            },
            {
              header: 'Model',
              cell: (a) => {
                const name = a.model_name
                return name ? (
                  <span className="font-mono text-xs">{name}</span>
                ) : (
                  <span className="text-xs text-muted-foreground">no model</span>
                )
              },
            },
            {
              header: 'Tools',
              cell: (a) => (
                <span className="text-xs">
                  {a.tools === 0 ? (
                    <span className="text-muted-foreground">none</span>
                  ) : (
                    `${a.tools}`
                  )}
                </span>
              ),
            },
            {
              header: 'Thinks',
              cell: (a) => (a.reasoning ? <Badge tone="good">yes</Badge> : <Badge>no</Badge>),
            },
            {
              header: 'Status',
              cell: (a) =>
                a.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>off</Badge>,
            },
          ]}
        />
      </Step>

      {/* Each dialog asks ONE question when it opens, and the answer is that
          whole form: the values it edits and the choices it offers. */}
      {files && gateway && (
        <WithForm load={() => api.agents.filesForm()}>
          {(form) => (
            <FilesForm form={form} onClose={() => setFiles(false)} onSaved={reload} />
          )}
        </WithForm>
      )}

      {audio && gateway && (
        <WithForm load={() => api.agents.audioForm()}>
          {(form) => (
            <AudioForm form={form} onClose={() => setAudio(false)} onSaved={reload} />
          )}
        </WithForm>
      )}

      {editing != null && (
        <WithForm key={editing.id ?? 'new'} load={() => api.agents.form(editing.id)}>
          {(form) => (
            <AgentForm
              form={form}
              gateway={form.agent ? form.agent.key === GATEWAY : Boolean(editing.gateway)}
              onClose={() => setEditing(null)}
              onSaved={async () => {
                setEditing(null)
                await reload()
              }}
            />
          )}
        </WithForm>
      )}
    </Page>
  )
}

/**
 * Which model reads which file.
 *
 * A list of rules rather than a fixed set of kinds, because how finely to split
 * files is a business question: screenshots to a cheap model and contracts to a
 * careful one, or one rule covering everything. They are tried top to bottom and
 * the first match wins, so a rule with no types is the catch-all and belongs
 * last, which is the one thing the server refuses to accept otherwise.
 */
function FilesForm({
  form,
  onClose,
  onSaved,
}: {
  // The section this form edits and the models it may choose, in one answer.
  // Not the whole agent: a form that changes four file rules has no use for
  // instructions, brains, a confirm list or timestamps. And not the model
  // catalogue plus the vendor catalogue: `label` is already the string the
  // control prints, so there is nothing here to join.
  form: FilesFormBody
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const stored = form.rules
  const readable = form.models
  const [rules, setRules] = useState<{ types: string[]; model_id: number | null }[]>(() =>
    stored.filter((r) => r.types.length > 0).map((r) => ({ types: [...r.types], model_id: r.model_id })),
  )
  // Everything the rules above did not claim. It is kept apart from them because
  // it is not a rule you write, it is the question "and everything else?", which
  // has exactly one answer and always comes last.
  const [catchAll, setCatchAll] = useState<string>(() => {
    const rest = stored.find((r) => r.types.length === 0)
    return rest ? String(rest.model_id) : ''
  })
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  // A rule already using a type does not offer it again: two rules claiming the
  // same type means the second can never match, and nobody writes that on
  // purpose.
  const claimed = (except: number) =>
    new Set(rules.flatMap((r, j) => (j === except ? [] : r.types)))

  async function save() {
    errors.clear()
    setBusy(true)
    try {
      // The catch-all is appended rather than positioned by hand, so the rule
      // that matches everything is last by construction and the server's
      // refusal of a misplaced one can never be triggered from here.
      await api.agents.putFiles([
        ...rules
          .filter((r) => r.model_id != null && r.types.length > 0)
          .map((r) => ({ types: r.types, model_id: r.model_id as number })),
        ...(catchAll ? [{ types: [], model_id: Number(catchAll) }] : []),
      ])
      notify.success('What can be attached has changed.')
      await onSaved()
      onClose()
    } catch (failure) {
      errors.fail(failure)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      wide
      onOpenChange={(next) => !next && onClose()}
      title="Files"
      onSubmit={save}
      submitting={busy}
    >
      <p className="text-xs text-muted-foreground">
        What reads a file somebody attaches, before the Gateway ever sees it. Rules are tried
        from the top and the first match wins, and whatever none of them claims is decided at
        the bottom.
      </p>

      {readable.length === 0 ? (
        <div className="mt-4 rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
          <p className="text-sm text-muted-foreground">
            No model that can read a file is set up yet. Add one first, and files can be
            accepted here.
          </p>
        </div>
      ) : (
        <div className="mt-5 space-y-5">
          {rules.length === 0 && (
            <div className="rounded-md border border-dashed border-border px-4 py-6 text-center">
              <p className="text-sm text-muted-foreground">
                No rules for particular types. Anything accepted will be read by whatever
                &ldquo;everything else&rdquo; says below.
              </p>
            </div>
          )}

          {rules.map((rule, i) => {
            const taken = claimed(i)
            return (
              <div key={i} className="border-t border-border pt-4 first:border-t-0 first:pt-0">
                <div className="mb-2 flex items-center justify-between gap-3">
                  <span className="text-xs font-medium text-muted-foreground">Rule {i + 1}</span>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => setRules((prev) => prev.filter((_, j) => j !== i))}
                  >
                    Remove
                  </Button>
                </div>

                <div className="grid gap-4 sm:grid-cols-2">
                  <label className="block">
                    <span className="mb-1.5 block text-xs font-medium">File types</span>
                    <TokenMultiSelect
                      options={FILE_TYPES.filter((t) => !taken.has(t.value)).map((t) => ({
                        value: t.value,
                        label: t.label,
                        hint: t.hint,
                      }))}
                      value={rule.types}
                      onChange={(types) =>
                        setRules((prev) => prev.map((r, j) => (j === i ? { ...r, types } : r)))
                      }
                      placeholder="Add a file type…"
                      empty="Every type is spoken for by another rule."
                    />
                    <span className="mt-1.5 block text-xs text-muted-foreground">
                      This rule applies to a file of any of these types.
                    </span>
                  </label>

                  <label className="block">
                    <span className="mb-1.5 block text-xs font-medium">Read by</span>
                    <NativeSelect
                      value={rule.model_id ?? ''}
                      onChange={(e) =>
                        setRules((prev) =>
                          prev.map((r, j) =>
                            j === i
                              ? { ...r, model_id: e.target.value ? Number(e.target.value) : null }
                              : r,
                          ),
                        )
                      }
                    >
                      <option value="">Choose a model…</option>
                      {readable.map((m) => (
                        <option key={m.id} value={m.id}>
                          {m.label}
                        </option>
                      ))}
                    </NativeSelect>
                    <span className="mt-1.5 block text-xs text-muted-foreground">
                      It turns the file into text the Gateway can read.
                    </span>
                  </label>
                </div>
              </div>
            )
          })}

          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() =>
              setRules((prev) => [...prev, { types: [], model_id: readable[0]?.id ?? null }])
            }
          >
            <Plus className="size-4" />
            Add rule
          </Button>

          {/* The question the rules above leave open, asked plainly. It is
              deliberately not a rule: there is one of it, it always applies
              last, and its useful setting is often "no". */}
          <div className="border-t border-border pt-4">
            <div className="text-xs font-medium">Everything else</div>
            <p className="mt-0.5 mb-2 text-xs text-muted-foreground">
              Any other file somebody tries to attach.
            </p>
            <NativeSelect value={catchAll} onChange={(e) => setCatchAll(e.target.value)}>
              <option value="">Refuse it</option>
              {readable.map((m) => (
                <option key={m.id} value={m.id}>
                  Read by {m.label}
                </option>
              ))}
            </NativeSelect>
            <p className="mt-1.5 text-xs text-muted-foreground">
              {catchAll
                ? 'Any file can be attached; the rules above pick the model, and this one reads the rest.'
                : 'Only the types named above can be attached. Anything else is turned away before it is uploaded.'}
            </p>
          </div>
        </div>
      )}
    </Modal>
  )
}

/**
 * What turns a recording into words.
 *
 * Its own form rather than a row among the file rules: reading audio is
 * transcription, a different call to a different kind of model, and it is what
 * lets somebody talk instead of typing rather than attach something.
 */
function AudioForm({
  form,
  onClose,
  onSaved,
}: {
  // The one model id this form changes, and the models that can transcribe.
  // The rest of the agent is none of its business.
  form: AudioFormBody
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const transcribers = form.models
  const [modelID, setModelID] = useState<string>(
    form.model_id != null ? String(form.model_id) : '',
  )
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    errors.clear()
    setBusy(true)
    try {
      await api.agents.putAudio(modelID ? Number(modelID) : null)
      notify.success('How speech is read has changed.')
      await onSaved()
      onClose()
    } catch (failure) {
      errors.fail(failure)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      onOpenChange={(next) => !next && onClose()}
      title="Audio"
      onSubmit={save}
      submitting={busy}
    >
      <p className="text-xs text-muted-foreground">
        What turns a recording into words, so somebody can talk instead of typing.
      </p>
      {transcribers.length === 0 ? (
        <div className="rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
          <p className="text-sm text-muted-foreground">
            No model that transcribes is set up yet. Add one and it can be chosen here.
          </p>
        </div>
      ) : (
        <Field
          label="Transcribed by"
          hint="Leave it unset and the chat offers no microphone: there would be nothing to turn speech into words."
          error={errors.fields.audio_model_id}
        >
          <NativeSelect value={modelID} onChange={(e) => setModelID(e.target.value)}>
            <option value="">Not accepted</option>
            {transcribers.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </NativeSelect>
        </Field>
      )}
    </Modal>
  )
}

/**
 * One step of the pipeline: a numbered marker in a rail down the left, and the
 * step's own content beside it.
 *
 * The rail is the design. A page that reads downwards in the order things
 * actually happen needs no diagram, and the numbers joined by arrows say where a
 * message goes without a sentence explaining it. What the arrow CARRIES is
 * written beside it, because "something moves" is obvious and "what moves" is
 * the part worth knowing.
 *
 * The content sits directly on the page, never in a box: a card around each step
 * would be a container saying what the heading already said.
 */
function Step({
  n,
  title,
  blurb,
  action,
  carries,
  first,
  last,
  children,
}: {
  n: number
  title: string
  blurb: string
  action?: React.ReactNode
  /** What this step hands to the next one. Absent on the last. */
  carries?: string
  /** The first band has a flat top: there is nothing above it to nest into. */
  first?: boolean
  last?: boolean
  children: React.ReactNode
}) {
  return (
    <section className="flex items-stretch gap-5">
      {/* The rail, as the CRM draws a pipeline: each step a band that comes to
          a point and feeds the next, numbered, read downwards.
          
          The bands interlock WITH A SEAM between them, which is the part I got
          wrong twice. Each is cut with a point at the bottom and a notch of the
          same depth at the top; pulling one up by exactly that depth makes them
          fit perfectly, and a perfect fit means the union is a plain rectangle
          and no chevron is visible at all. So the overlap is the depth MINUS a
          few pixels, leaving a thin chevron-shaped gap: the same white seam the
          CRM leaves between two stages, and the only thing that makes the shape
          readable.
          
          Cut out of the band with clip-path rather than drawn beside it: a
          shape cut from the thing itself cannot drift away from it.
          
          The band stretches to its step's height, so which number owns which
          content is a matter of them being the same height rather than of
          reading carefully. */}
      {/* Two elements, because border-radius and clip-path cannot both shape one
          box: a clip-path REPLACES the painted region, so a radius on the same
          element is simply ignored. The outer box rounds the ends of the whole
          rail and clips to it; the inner one carries the fill and the chevron. */}
      <div
        className={cn(
          'w-11 shrink-0 overflow-hidden',
          first && 'rounded-t-xl',
          last && 'rounded-b-xl',
        )}
        style={{
          // Up by the notch depth less the seam, so the point above lands
          // almost in it and the gap that is left traces the chevron.
          marginTop: first ? undefined : -(14 - 4),
        }}
      >
        <div
          className={cn(
            'flex h-full flex-col items-center bg-primary/10',
            first ? 'pt-3' : 'pt-6',
            !last && 'pb-5',
          )}
          style={{
            clipPath: [
              'polygon(',
              first ? '0 0, 100% 0,' : '0 0, 50% 14px, 100% 0,',
              last ? '100% 100%, 0 100%' : '100% calc(100% - 14px), 50% 100%, 0 calc(100% - 14px)',
              ')',
            ].join(' '),
          }}
        >
          <span className="flex size-7 items-center justify-center rounded-full bg-primary text-xs font-semibold text-primary-foreground">
            {n}
          </span>
        </div>
      </div>

      <div className={cn('min-w-0 flex-1', !last && 'pb-8')}>
        <div className="mb-4 flex min-h-7 items-start justify-between gap-4">
          <div>
            <h2 className="text-sm font-semibold leading-7">{title}</h2>
            <p className="mt-1 max-w-2xl text-xs leading-relaxed text-muted-foreground">{blurb}</p>
          </div>
          {action && <div className="shrink-0">{action}</div>}
        </div>
        {children}
        {carries && (
          <p className="mt-6 text-xs text-muted-foreground">
            <span className="text-muted-foreground/70">Then passes along:</span> {carries}
          </p>
        )}
      </div>
    </section>
  )
}

/**
 * One of the two things a person can send: what will happen to it, and a way in.
 *
 * The content is a LIST rather than a summary. A count of rules is a number
 * somebody has to open a form to understand; naming each one against its model
 * says what will happen to the next file that arrives, which is the only thing
 * anybody wanted to know.
 */
/**
 * Fetches a dialog's own answer, and only then renders it.
 *
 * A form's data is the form's. The screen carries ROWS (a name, a model name, a
 * count), which is what a table draws; the values a form edits and the choices
 * it offers are one question, asked when it opens. Keyed at the call site, so
 * opening a different agent is a different fetch rather than a stale one.
 */
function WithForm<T>({ load, children }: { load: () => Promise<T>; children: (form: T) => React.ReactNode }) {
  const { data } = useResource(load)
  if (!data) return null
  return <>{children(data)}</>
}

function SlotBlock({
  label,
  empty,
  loading,
  onEdit,
  children,
}: {
  label: string
  /** What to say when nothing is set up; null when something is. */
  empty: string | null
  /** Nothing is known yet, which is NOT the same as nothing being set up. */
  loading?: boolean
  onEdit?: () => void
  children: React.ReactNode
}) {
  return (
    <div>
      <div className="mb-2 flex h-8 items-center justify-between gap-3">
        <div className="text-xs font-medium text-muted-foreground">{label}</div>
        {onEdit && (
          <Button variant="outline" size="sm" onClick={onEdit}>
            Change
          </Button>
        )}
      </div>
      {/* "Nothing is set up" is an ANSWER, and it was being given before the
          question had been asked: with no data yet, the rules list is empty, so
          this block said "nothing can be attached" and then replaced itself
          with the real rules a moment later. Not knowing yet is its own state. */}
      {loading ? (
        <Loader2 className="size-4 animate-spin text-muted-foreground" />
      ) : empty ? (
        <p className="text-sm text-muted-foreground">{empty}</p>
      ) : (
        <div className="divide-y divide-border/60 border-y border-border/60">{children}</div>
      )}
    </div>
  )
}

function Fact({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={cn('text-sm', mono && 'font-mono text-xs')}>{value}</div>
    </div>
  )
}

function AgentForm({
  form,
  gateway,
  onClose,
  onSaved,
}: {
  // The agent being edited and everything it can be given, in one answer.
  // `agent` is null when one is being created, which is the server saying there
  // is nothing to prefill rather than a form guessing at defaults.
  form: AgentFormBody
  gateway: boolean
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const agent = form.agent
  const models = form.models
  const tools = form.tools
  const brains = form.brains
  const [name, setName] = useState(agent?.name ?? (gateway ? 'Gateway' : ''))
  const [instructions, setInstructions] = useState(agent?.instructions ?? '')
  // No agent runs on "whatever the caller picks": the person never chooses a
  // model, so every agent, main or sub, starts on the first real one.
  const [modelID, setModelID] = useState<number | null>(
    agent?.model_id ?? (models[0]?.id ?? null),
  )
  const [reasoning, setReasoning] = useState(agent?.reasoning ?? false)
  // How hard it thinks, kept as a whole bag so a second setting of this kind
  // needs no state of its own here.
  const [settings, setSettings] = useState<Record<string, string>>(agent?.settings ?? {})
  const [selected, setSelected] = useState<string[]>(agent?.tools ?? [])
  const [confirm, setConfirm] = useState<string[]>(agent?.confirm_tools ?? [])
  const [selectedBrains, setSelectedBrains] = useState<string[]>((agent?.brains ?? []).map(String))
  const [memoryBrain, setMemoryBrain] = useState<string>(
    agent?.memory_brain_id != null ? String(agent.memory_brain_id) : '',
  )
  const [ttl, setTTL] = useState(
    agent?.approval_ttl_seconds ? String(Math.round(agent.approval_ttl_seconds / 60)) : '',
  )
  const [maxIter, setMaxIter] = useState(
    agent?.max_iterations != null ? String(agent.max_iterations) : '',
  )
  const [maxFleet, setMaxFleet] = useState(
    agent?.max_fleet_agents != null ? String(agent.max_fleet_agents) : '',
  )
  const [bgTimeout, setBgTimeout] = useState(
    agent?.background_timeout_seconds ? String(Math.round(agent.background_timeout_seconds / 60)) : '',
  )
  const [status, setStatus] = useState(agent?.status ?? 'active')
  // How the Gateway runs this agent: auto (it decides), background, or
  // inline. Only meaningful for an agent, not the Gateway.
  const [delegationMode, setDelegationMode] = useState(agent?.delegation_mode ?? 'auto')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  // Toggling a tool off also drops it from the confirm set: you cannot pause on
  // a tool the agent can no longer call.
  function toggleTool(name: string, on: boolean) {
    setSelected((prev) => (on ? [...prev, name] : prev.filter((n) => n !== name)))
    if (!on) setConfirm((prev) => prev.filter((n) => n !== name))
  }

  // The confirmation control offers only the tools this agent holds. A tool the
  // code already forces to confirm shows as a locked token: it is in the set by
  // law, not by choice.
  // Ordered by source, so the dropdown reads in the same runs as the checkboxes
  // above it. Two lists of the same tools in two different orders is a form
  // somebody has to search twice.
  const confirmOptions: TokenOption[] = bySource(tools.filter((t) => selected.includes(t.name)))
    .flatMap((group) =>
      // No hint: it used to carry the name the model calls the tool by, which
      // is ours and not the reader's. What identifies a tool here is its own
      // words, under the service it came from.
      group.items.map((t) => ({
        value: t.name,
        label: toolLabel(t),
        fixed: t.approval_locked,
        group: group.source,
      })),
    )

  // Any brain can be READ, so any is choosable as accessible; a locked one is
  // marked. Only an unlocked brain can be the agent's long-term memory, because
  // the agent writes it back.
  const brainOptions: TokenOption[] = brains.map((b) => ({
    value: String(b.id),
    label: b.name,
    hint: b.locked ? 'read only' : undefined,
  }))
  const unlockedBrains = brains.filter((b) => !b.locked)


  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'An agent needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const minutes = Number(ttl)
      // The key is internal, not something a person types: the Gateway is
      // always "default"; a new agent leaves it empty and the server derives
      // one from the name; an edit keeps the key it already has.
      const key = gateway ? GATEWAY : (agent?.key ?? '')
      const body = {
        key,
        name: name.trim(),
        instructions,
        model_id: modelID,
        reasoning,
        settings,
        tools: selected,
        confirm_tools: confirm.filter((n) => selected.includes(n)),
        brains: selectedBrains.map(Number),
        memory_brain_id: memoryBrain ? Number(memoryBrain) : null,
        status,
        delegation_mode: gateway ? 'auto' : delegationMode,
        approval_ttl_seconds: ttl && minutes > 0 ? minutes * 60 : null,
        max_iterations: maxIter && Number(maxIter) > 0 ? Number(maxIter) : null,
        max_fleet_agents: gateway && maxFleet && Number(maxFleet) > 0 ? Number(maxFleet) : null,
        // The background timeout is an agent concept: the Gateway does not
        // run in the background, so it never carries one.
        background_timeout_seconds:
          !gateway && bgTimeout && Number(bgTimeout) > 0 ? Number(bgTimeout) * 60 : null,
      }
      if (agent) await api.agents.update(agent.id, body)
      else await api.agents.create(body)
      notify.success('The agent was saved.')
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      wide
      onOpenChange={(o) => !o && onClose()}
      title={agent ? `Edit ${agent.name}` : gateway ? 'New Gateway' : 'New agent'}
      onSubmit={save}
      submitting={busy}
    >
      <div className="grid grid-cols-2 gap-4">
        <Field label="Name" required error={errors.fields.name}>
          <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </Field>
        <Field label="Status" error={errors.fields.status}>
          <NativeSelect value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </NativeSelect>
        </Field>
      </div>

      <Field
        label="Instructions"
        hint="Added to what the agent is told. Leave empty to keep the built-in prompt."
      >
        <Textarea
          rows={5}
          value={instructions}
          onChange={(e) => setInstructions(e.target.value)}
          placeholder="You are the sales agent. Answer in the customer's language."
        />
      </Field>

      {gateway ? (
        <Field
          label="Model"
          hint="The model the Gateway runs on."
        >
          <NativeSelect
            value={modelID ?? ''}
            onChange={(e) => setModelID(e.target.value ? Number(e.target.value) : null)}
          >
            {models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </NativeSelect>
        </Field>
      ) : (
        <div className="grid grid-cols-2 gap-4">
          {/* An agent runs on a model the administrator sets: the person never
              picks it, so there is no "whatever the caller picks". */}
          <Field label="Model" hint="The model this agent runs on.">
            <NativeSelect
              value={modelID ?? ''}
              onChange={(e) => setModelID(e.target.value ? Number(e.target.value) : null)}
            >
              {models.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field
            label="Preferred mode"
            hint="How the Gateway runs it. Pin one and the Gateway no longer chooses. Fleet means several at once, on the workers, answered when they are all back."
          >
            <NativeSelect value={delegationMode} onChange={(e) => setDelegationMode(e.target.value)}>
              <option value="auto">Let the Gateway decide</option>
              <option value="background">Background</option>
              <option value="inline">Inline</option>
              <option value="fleet">Fleet</option>
            </NativeSelect>
          </Field>
        </div>
      )}

      <CheckboxField
        checked={reasoning}
        onChange={setReasoning}
        label="Reason before answering"
        hint="Let the model think before it answers, which costs time and money and is worth it for work that needs it. Not every model can: one that cannot simply ignores this and answers as it always would."
      />

      {/* Only while it is thinking. Shown from what the server declares rather
          than written out here, so the day a vendor adds a second setting of
          this kind there is nothing to change on this screen.

          Offered whatever model is chosen, on purpose: the choice is the same
          four words on every vendor that has the idea, and a model with no such
          idea does not fail on it. The adapter is refused once, drops the
          parameter and asks again, so the sentence under this field is true and
          not a hope. */}
      {reasoning &&
        form.settings.map((spec) => (
          <Field key={spec.key} label={spec.label} hint={spec.help}>
            <NativeSelect
              value={settings[spec.key] ?? spec.default ?? ''}
              onChange={(e) => setSettings({ ...settings, [spec.key]: e.target.value })}
            >
              {(spec.choices ?? []).map((c) => (
                <option key={c.value} value={c.value}>
                  {c.label}
                </option>
              ))}
            </NativeSelect>
          </Field>
        ))}

      <Field label="Tools" hint="The tools this agent is allowed to use. Anything left unchecked is hidden from it, so the agent never sees it and cannot call it.">
        {tools.length === 0 ? (
          <p className="text-xs text-muted-foreground">This build ships no tools.</p>
        ) : (
          // Grouped by where each tool came from. A connection projecting thirty
          // of them into one alphabetical list is a list nobody can grant from:
          // the only thing separating them was a prefix on the name.
          bySource(tools).map((group) => (
            <div key={group.source} className="mb-3 last:mb-0">
              <div className="mb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                {group.source}
              </div>
              <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
                {group.items.map((tool) => (
                  <CheckboxField
                    key={tool.id}
                    checked={selected.includes(tool.name)}
                    onChange={(on) => toggleTool(tool.name, on)}
                    label={<span className="block truncate">{toolLabel(tool)}</span>}
                  />
                ))}
              </div>
            </div>
          ))
        )}
      </Field>

      <Field
        label="Require confirmation"
        hint="Tools this agent stops and confirms before running. Choose from the tools above; a tool the code already forces to confirm is shown locked."
        error={errors.fields.confirm_tools}
      >
        {confirmOptions.length === 0 && (
          <div className="mb-2 rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
            <p className="text-sm text-muted-foreground">
              Give this agent tools first, then choose which to confirm.
            </p>
          </div>
        )}
        <TokenMultiSelect
          options={confirmOptions}
          value={confirm}
          onChange={setConfirm}
          placeholder="Add a tool to confirm…"
          empty=""
        />
      </Field>

      <Field
        label="Knowledge"
        hint="The brains this agent may read. It navigates and searches only what you assign here."
      >
        <TokenMultiSelect
          options={brainOptions}
          value={selectedBrains}
          onChange={setSelectedBrains}
          placeholder="Add a brain…"
          empty="No brains exist yet."
        />
      </Field>

      <Field
        label="Long-term memory"
        hint="One brain the agent writes back to as it learns, off the conversation. Unlocked brains only. Optional."
      >
        <NativeSelect
          value={memoryBrain}
          onChange={(e) => setMemoryBrain(e.target.value)}
          disabled={unlockedBrains.length === 0}
        >
          <option value="">No long-term memory</option>
          {unlockedBrains.map((b) => (
            <option key={b.id} value={b.id}>
              {b.name}
            </option>
          ))}
        </NativeSelect>
      </Field>

      <div className="border-t border-border pt-4">
        <div className="mb-3 space-y-1 rounded-md border border-primary/30 bg-primary/5 px-4 py-3 text-xs text-muted-foreground">
          <p>
            <span className="font-medium text-foreground">Approval window</span> — minutes a
            confirmation stays answerable. Empty uses the deployment's.
          </p>
          {!gateway && (
            <p>
              <span className="font-medium text-foreground">Background timeout</span> — minutes a
              background task may work before it is stopped and reported as timed out. Empty uses 10.
            </p>
          )}
          <p>
            <span className="font-medium text-foreground">Max iterations</span> — how many tool
            steps this agent may take before it stops. Empty uses 100.
          </p>
          {gateway && (
            <p>
              <span className="font-medium text-foreground">Max agents in a batch</span> — how many
              agents may be started at once when work is split across them. Each one is a separate
              conversation with a model, so this is a cost as much as a limit. Empty uses 20.
            </p>
          )}
        </div>

        <div className="grid grid-cols-3 gap-4">
          <Field label="Approval window (min)" error={errors.fields.approval_ttl_seconds}>
            <Input
              value={ttl}
              onChange={(e) => setTTL(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric"
              placeholder="1440"
            />
          </Field>

          {!gateway && (
            <Field label="Background timeout (min)" error={errors.fields.background_timeout_seconds}>
              <Input
                value={bgTimeout}
                onChange={(e) => setBgTimeout(e.target.value.replace(/\D/g, ''))}
                inputMode="numeric"
                placeholder="10"
              />
            </Field>
          )}

          <Field label="Max iterations" error={errors.fields.max_iterations}>
            <Input
              value={maxIter}
              onChange={(e) => setMaxIter(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric"
              placeholder="100"
            />
          </Field>

          {/* Only the Gateway starts a batch, so only the Gateway is asked how
              big one may be. It takes the slot the background timeout has on an
              agent, so both forms are three across. */}
          {gateway && (
            <Field label="Max agents in a batch" error={errors.fields.max_fleet_agents}>
              <Input
                value={maxFleet}
                onChange={(e) => setMaxFleet(e.target.value.replace(/\D/g, ''))}
                inputMode="numeric"
                placeholder="20"
              />
            </Field>
          )}
        </div>
      </div>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
