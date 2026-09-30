import { useEffect, useMemo, useState } from 'react'
import { Plus } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Markdown } from '@/components/Markdown'
import { bySource, toolLabel } from '@/lib/tool-display'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { SettingField, SettingsSections, fieldSpan } from '@/components/SettingsForm'
import { CheckboxField } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { TokenMultiSelect } from '@/components/ui/token-multiselect'
import type { TokenOption } from '@/components/ui/token-multiselect'
import { Textarea } from '@/components/ui/textarea'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { POSTURE } from '@/lib/api'
import { api, useResource } from '@/lib/resources'
import type {
  SkillChoice,
  TemplateParam,
  TemplateSection,
  Tool,
  ToolActionPrompt,
  ToolActionResult,
  ToolDetail,
  ToolTemplate,
} from '@/lib/resources'

/**
 * Tools: what the agents can do.
 *
 * A built-in tool is NOT created here. It arrives with a deploy, because it is
 * code. What an administrator decides is whether it is on, whether it needs a
 * human, and who may reach it. A NATIVE custom tool is the exception: it is
 * instantiated from a template ("Add tool"), and each instance is its own
 * grantable tool.
 */
export function Tools() {
  const { data: tools, reload } = useResource(() => api.tools.list())
  const [editing, setEditing] = useState<Tool | null>(null)
  const [adding, setAdding] = useState(false)

  const risk = (level: string) =>
    level === 'read_only' ? (
      <Badge>reads only</Badge>
    ) : level === 'internal_write' ? (
      <Badge tone="warn">writes</Badge>
    ) : (
      <Badge tone="bad">{level.replace(/_/g, ' ')}</Badge>
    )

  const groups = bySource(tools ?? [])

  return (
    <Page
      title="Tools"
      description="What the agents can do."
      actions={
        <Button size="sm" onClick={() => setAdding(true)}>
          <Plus className="size-4" />
          Add tool
        </Button>
      }
    >
      {/* One table per source, rather than one list with a prefix to decode.
          Where a tool came from is the first thing somebody sorts by when a
          connection projects thirty of them, and it was only ever legible as
          `nli_` on the front of a name. */}
      {groups.length === 0 && (
        <p className="text-sm text-muted-foreground">This build ships no tools.</p>
      )}
      {groups.map((group) => (
      <div key={group.source} className="mb-8 last:mb-0">
        <div className="mb-2 flex items-baseline gap-2">
          <h2 className="text-sm font-medium">{group.source}</h2>
          <span className="text-xs text-muted-foreground">
            {group.items.length} {group.items.length === 1 ? 'tool' : 'tools'}
          </span>
        </div>
      <DataTable
        items={group.items}
        empty="This build ships no tools."
        onEdit={(tool) => setEditing(tool)}
        columns={[
          {
            // The tool's name and what is true of it. The name the model calls
            // it by is an internal identifier and is not shown: it told a
            // reader nothing they were not already reading in the row.
            header: 'Tool',
            cell: (t) => (
              <div className="flex items-center gap-1.5 font-medium">
                {toolLabel(t)}
                {t.kind === 'custom' && <Badge>custom</Badge>}
                {t.remote_missing && <Badge tone="bad">gone from service</Badge>}
                {!t.remote_missing && t.definition_changed_at && (
                  <Badge tone="warn" title={`Changed ${new Date(t.definition_changed_at).toLocaleString()}`}>
                    changed
                  </Badge>
                )}
              </div>
            ),
          },
          { header: 'Risk', cell: (t) => risk(t.risk) },
          // A grant names a GROUP of people, and a personal installation has one
          // person in it. The column would read "everyone" on every row for
          // ever: a heading with only one possible answer is noise dressed as
          // information, and it sends somebody looking for the screen that
          // changes it, which is not there either.
          ...(POSTURE.single_user
            ? []
            : [
                {
                  header: 'Who may use it',
                  cell: (t: Tool) =>
                    t.grants.length === 0 ? (
                      <span className="text-xs text-muted-foreground">everyone</span>
                    ) : (
                      <span className="text-xs">
                        {t.grants.length} {t.grants.length === 1 ? 'group' : 'groups'}
                      </span>
                    ),
                },
              ]),
          {
            header: 'Status',
            cell: (t) =>
              t.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>off</Badge>,
          },
        ]}
      />
      </div>
      ))}

      {/* A native custom tool gets its full form (settings, guide, params); a
          built-in or MCP tool has only the administrator's switches to set. */}
      {editing &&
        (editing.kind === 'custom' ? (
          <EditCustomToolModal
            tool={editing}
            onClose={() => setEditing(null)}
            onSaved={async () => {
              setEditing(null)
              await reload()
            }}
          />
        ) : (
          <ToolForm
            tool={editing}
            onClose={() => setEditing(null)}
            onSaved={async () => {
              setEditing(null)
              await reload()
            }}
          />
        ))}

      {adding && (
        <AddToolModal
          onClose={() => setAdding(false)}
          onRefresh={reload}
          onCreated={async () => {
            setAdding(false)
            await reload()
          }}
        />
      )}
    </Page>
  )
}

/**
 * AddToolModal: the "Add tool" flow, disclosed step by step in one form.
 *
 *   Type (Native | Workflow) → the native template → its driver → the settings.
 *
 * Workflow-backed custom tools are a later thing, so that choice is shown but
 * disabled. Everything after a step appears once the step is answered, so the
 * form is never a wall of empty inputs.
 */
/**
 * How a skill is offered in a picker: ONE name, which is the rule everywhere a
 * skill is printed (lib/skills.ts). The handle is an identifier, so showing it
 * beside a perfectly good title says the same thing twice in two shapes, and a
 * package that carried no title is already called by its handle.
 *
 * The status still shows. A skill that is switched off is choosable and says
 * which it is, rather than vanishing from a list somebody is trying to find it
 * in. It will not appear in the tool's guide while it is off, which is the
 * same rule an agent's assigned skills follow.
 */
function skillOptionsFrom(choices: SkillChoice[]): TokenOption[] {
  return choices.map((sk) => ({
    value: String(sk.id),
    label: sk.name,
    hint: sk.status === 'active' ? undefined : sk.status,
  }))
}

function AddToolModal({
  onClose,
  onCreated,
  onRefresh,
}: {
  onClose: () => void
  onCreated: () => Promise<void>
  onRefresh: () => Promise<void>
}) {
  const notify = useNotify()
  const errors = useFormErrors()
  const [templates, setTemplates] = useState<ToolTemplate[]>([])
  // Set once the tool exists and still needs somebody to sign in at the service.
  const [offer, setOffer] = useState<{ url?: string; reason?: string } | null>(null)
  const [template, setTemplate] = useState<ToolTemplate | null>(null)
  const [variant, setVariant] = useState('')
  const [sections, setSections] = useState<TemplateSection[]>([])
  const [tab, setTab] = useState(0)
  // A test result belongs to the settings it was run against, so changing any of
  // them retires it rather than leaving a stale "Connected" beside edited values.
  const [testVersion, setTestVersion] = useState(0)
  const invalidateTest = () => setTestVersion((v) => v + 1)
  const [displayName, setDisplayName] = useState('')
  const [description, setDescription] = useState('')
  const [guide, setGuide] = useState('')
  const [skillChoices, setSkillChoices] = useState<SkillChoice[]>([])
  const [selectedSkills, setSelectedSkills] = useState<string[]>([])
  const skillOptions = useMemo(() => skillOptionsFrom(skillChoices), [skillChoices])
  const [paramDescriptions, setParamDescriptions] = useState<Record<string, string>>({})
  const [values, setValues] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    void api.tools.templates().then(setTemplates)
  }, [])

  // When a driver is chosen, load its connection form from the server, grouped
  // into sections. The console renders whatever sections it is given and never
  // decides for itself what a field belongs to.
  useEffect(() => {
    if (!template || !variant) return
    let cancelled = false
    void api.tools.newForm(template.name, variant).then((form) => {
      if (cancelled) return
      setSections(form.sections)
      // The skills ride on the same answer, because they are part of this
      // form: a second request to fill one picker is the thing the one
      // request per dialog rule exists to stop.
      setSkillChoices(form.skills ?? [])
      // The values come with the form. What a new tool starts from is the
      // server's decision, not something assembled here out of field defaults.
      setValues((prev) => {
        const next = { ...prev }
        for (const [key, value] of Object.entries(form.values)) {
          if (next[key] === undefined) next[key] = value == null ? '' : String(value)
        }
        return next
      })
    })
    return () => {
      cancelled = true
    }
  }, [template, variant])

  // Choosing a template prefills the editable presentation (description, guide,
  // and each parameter's description) from the template's own, so the person
  // starts from a working tool and refines it.
  function chooseTemplate(name: string) {
    const t = templates.find((x) => x.name === name) ?? null
    setTemplate(t)
    setVariant('')
    setSections([])
    setTab(0)
    invalidateTest()
    // default_description, NOT description. The second is the paragraph in the
    // callout above, written for the person choosing a tool; putting it here
    // filled the model's instructions with copy written to sell it one.
    //
    // Cleared out entirely, Build still writes an instance-specific one naming
    // the address, the engine or the server, so an emptied field is a sensible
    // tool rather than a nameless one.
    setDescription(t?.default_description ?? '')
    setGuide(t?.default_guide ?? '')
    setParamDescriptions(Object.fromEntries((t?.params ?? []).map((p) => [p.key, p.description])))
  }

  const settings = (): Record<string, unknown> => {
    const out: Record<string, unknown> = {}
    for (const section of sections)
      for (const field of section.fields) {
        // A callback field is read-only: the deployment's own address, shown to
        // be pasted elsewhere. Sending it back would store a fact about us as
        // though somebody had configured it.
        if (field.type === 'callback') continue
        const raw = values[field.key] ?? ''
        if (field.type === 'number') out[field.key] = raw === '' ? undefined : Number(raw)
        else out[field.key] = raw
      }
    return out
  }

  async function create() {
    if (!template) return
    errors.clear()
    setBusy(true)
    try {
      const created = await api.tools.createCustom({
        template: template.name,
        variant,
        display_name: displayName,
        description,
        guide,
        param_descriptions: paramDescriptions,
        settings: settings(),
        skills: selectedSkills.map(Number),
      })
      // A tool that signs in to a service is not finished when it is saved, so
      // the dialog stays and offers the sign-in rather than closing and leaving
      // somebody to find the tool again.
      if (created.connect_url || created.connect_error) {
        setOffer({ url: created.connect_url, reason: created.connect_error })
        setBusy(false)
        await onRefresh()
        return
      }
      notify.success('The tool was created. Grant it to an agent to use it.')
      await onCreated()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  const setValue = (key: string, v: string) => {
    setValues((prev) => ({ ...prev, [key]: v }))
    invalidateTest()
  }

  // The tool exists and the service has still to be told who is asking. A step
  // of its own rather than a line under the form, because it leaves the
  // console: it opens the service's own consent page in another tab.
  if (offer) {
    return (
      <Modal
        open
        onOpenChange={(o) => !o && void onCreated()}
        title="Sign in to finish"
        onSubmit={onCreated}
        submitLabel="Done"
        hideCancel
      >
        {offer.url ? (
          <div className="space-y-3">
            {/* Answers the question somebody asks here, which is why this step
                exists at all when they just typed everything in. */}
            <p className="text-sm text-muted-foreground">
              The tool is saved, but it has no access yet. The client ID and secret you entered identify
              the <strong>application</strong>, not the person it acts as. Only a person can grant that,
              and only the service can issue the token it hands back, so there is nothing to type here.
            </p>
            <p className="text-sm text-muted-foreground">
              Sign in once and it is remembered and renewed on its own. Close that tab when it says it is
              connected.
            </p>
            <Button asChild size="sm">
              <a href={offer.url} target="_blank" rel="noreferrer">
                Sign in to the service
              </a>
            </Button>
          </div>
        ) : (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              The tool is saved, but the sign-in could not be started, so it has no access yet:
            </p>
            <p className="text-sm text-destructive">{offer.reason}</p>
            <p className="text-sm text-muted-foreground">
              Open the tool, check the addresses and the client it was given, then press Test to try again.
            </p>
          </div>
        )}
      </Modal>
    )
  }

  return (
    <Modal open onOpenChange={(o) => !o && onClose()} title="Add tool" onSubmit={create} submitting={busy} submitLabel="Create tool" wide>
      <Field label="Type">
        <div className="flex gap-2">
          <Segment active label="Native" hint="Built from a template" onClick={() => undefined} selected />
          <Segment active={false} label="Workflow" hint="Coming soon" onClick={() => undefined} />
        </div>
      </Field>

      {/* What is being made and what it is called, together, because they are
          one decision. Both required, and enforced by the browser rather than
          by a check in here: each empty option has an empty value, so the form
          cannot be submitted without them. They had the same hole before, where
          create() returned early and the button did nothing and said nothing.

          ONE name. The identifier the assistant calls it by is derived from
          this on the server, so nobody types a second name or learns that an
          identifier has rules. */}
      <div className="grid grid-cols-2 items-start gap-x-4 gap-y-4">
        <Field label="Tool" required>
          <NativeSelect required value={template?.name ?? ''} onChange={(e) => chooseTemplate(e.target.value)}>
            <option value="">Choose a tool…</option>
            {templates.map((t) => (
              <option key={t.name} value={t.name}>
                {t.title}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <Field label="Name" required hint="What people call it.">
          <Input
            required
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
            placeholder="Production orders"
          />
        </Field>
      </div>

      {template && (
        <>
          {/* What this kind of tool is, in plain words, so the person knows what
              they are making before they configure it. A tinted callout, so it
              reads as guidance and not another field. */}
          <div className="rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
            <p className="text-sm text-muted-foreground">{template.description}</p>
          </div>

          <Field
            label="AI Description"
            hint="Teach the AI when to use this tool. Write it for the model, not for people. Leave it empty and the tool describes itself from the settings you gave it."
          >
            <Textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
          </Field>

          {/* Required, and enforced by the browser rather than by a check here:
              the empty option has an empty value, so a form submitted without a
              driver never reaches the server. Nothing below this can be filled
              in until one is chosen anyway, because the settings ARE the
              driver's. */}
          <Field label="Driver" required>
            <NativeSelect
              required
              value={variant}
              onChange={(e) => {
                setVariant(e.target.value)
                setSections([])
                setTab(0)
                invalidateTest()
              }}
            >
              <option value="">Choose a driver…</option>
              {template.variants.map((v) => (
                <option key={v.key} value={v.key}>
                  {v.label}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </>
      )}

      {template && variant && sections.length > 0 && (
        <>
          {/* The connection groups (server-defined) as tabs: cleaner than one
              long list, and the console knows nothing about what a group is. */}
          <SettingsSections sections={sections} active={tab} onSelect={setTab} values={values} onChange={setValue} />

          <ConnectionTest
            key={testVersion}
            run={(answer) =>
              api.tools.testCustom({
                template: template.name,
                variant,
                settings: settings(),
                ...(answer ?? {}),
              })
            }
          />
        </>
      )}

      {/* A clear line between the tool's own settings above and the AI-facing
          inputs below. */}
      {template && <hr className="border-border" />}

      {template && (
        <ParamsEditor
          params={template.params}
          values={paramDescriptions}
          onChange={(key, v) => setParamDescriptions((prev) => ({ ...prev, [key]: v }))}
        />
      )}

      {template && (
        <Field
          label="AI Guide"
          hint="Detailed usage the AI fetches only when it needs it, so it does not weigh down every request."
        >
          <Textarea value={guide} onChange={(e) => setGuide(e.target.value)} rows={4} />
        </Field>
      )}

      {template && (
        <Field
          label="Skills for using this tool"
          hint="Written procedures that document THIS tool: an API's paths, what its codes mean, the order things go in. The assistant is told they exist when it reads the guide above, and opens one only if it needs to. An agent holding the tool can read these whether or not the skill was assigned to it."
        >
          <TokenMultiSelect
            options={skillOptions}
            value={selectedSkills}
            onChange={setSelectedSkills}
            placeholder="Add a skill…"
            empty="No skills exist yet. Import one on the Skills screen."
          />
        </Field>
      )}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

/**
 * EditCustomToolModal: the full form for an existing native custom tool. Its
 * identity (template, name, driver) is fixed; everything else, the connection
 * settings, the presentation, the guide, plus the administrator's switches, is
 * editable. A secret left blank is kept: the server carries the sealed value
 * forward, so changing a host never means re-entering a password.
 */
function EditCustomToolModal({
  tool,
  onClose,
  onSaved,
}: {
  tool: Tool
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const errors = useFormErrors()
  const [tool_, setTool] = useState<ToolDetail | null>(null)
  // Who this tool may be granted to arrives with the tool itself; `tool.grants`
  // is who it IS granted to. The workspace's group list used to be a separate
  // request, fired on every view of the tools table.
  const groups = tool_?.groups ?? []
  const [variant, setVariant] = useState('')
  const [sections, setSections] = useState<TemplateSection[]>([])
  const [tab, setTab] = useState(0)
  // A test result belongs to the settings it was run against, so changing any of
  // them retires it rather than leaving a stale "Connected" beside edited values.
  const [testVersion, setTestVersion] = useState(0)
  const invalidateTest = () => setTestVersion((v) => v + 1)
  const [displayName, setDisplayName] = useState(tool.friendly_name)
  const [description, setDescription] = useState(tool.description)
  const [guide, setGuide] = useState('')
  const [paramDescriptions, setParamDescriptions] = useState<Record<string, string>>({})
  const [values, setValues] = useState<Record<string, string>>({})
  const [enabled, setEnabled] = useState(tool.status === 'active')
  const [grants, setGrants] = useState<number[]>(tool.grants)
  const [skillChoices, setSkillChoices] = useState<SkillChoice[]>([])
  const [selectedSkills, setSelectedSkills] = useState<string[]>([])
  const skillOptions = useMemo(() => skillOptionsFrom(skillChoices), [skillChoices])
  // The tool's sign-in, when it has one. Null for every other kind of tool.
  const [connection, setConnection] = useState<ToolDetail['connection']>(undefined)
  const [signingIn, setSigningIn] = useState(false)
  // Set while a sign-in is happening in another tab, which is what says this
  // form has something to wait for.
  const [awaitingSignIn, setAwaitingSignIn] = useState(false)
  const [busy, setBusy] = useState(false)

  // ONE request. It answers with the tool, the form its driver makes up, the
  // values that fill it, and the three things the template contributes (what
  // this kind of tool is, what the driver is called, which parameters it takes).
  // Nothing here fetches a catalogue to find out about one tool.
  useEffect(() => {
    let cancelled = false
    void api.tools.get(tool.id).then((detail: ToolDetail) => {
      if (cancelled) return
      setTool(detail)
      setVariant(detail.variant ?? '')
      setGuide(detail.guide ?? '')
      setParamDescriptions(detail.param_descriptions ?? {})
      // What it could be documented by, and what it is, both out of this one
      // answer.
      setSkillChoices(detail.skill_choices ?? [])
      setSelectedSkills((detail.skills ?? []).map(String))
      setConnection(detail.connection)
      // The form and the values that fill it arrive together, from the one
      // request that asked for this tool. Nothing is merged or defaulted here:
      // an existing tool is what is stored, and a field it has no value for
      // comes back blank, which is the server saying so rather than this view
      // deciding it.
      setSections(detail.sections ?? [])
      const seed: Record<string, string> = {}
      for (const [key, value] of Object.entries(detail.settings ?? {})) seed[key] = value == null ? '' : String(value)
      setValues(seed)
    })
    return () => {
      cancelled = true
    }
  }, [tool])

  // A sign-in happens in ANOTHER TAB, so this form has to find out that it
  // finished. Two ways, because either can be the one that happens: the
  // callback page posts back to the window that opened it, and somebody who
  // closes that tab by hand instead lands back here with the form focused.
  //
  // Without this the form went on saying "not connected" until the whole page
  // was reloaded and the tool opened again, which is how it read as broken.
  useEffect(() => {
    if (!awaitingSignIn) return
    let cancelled = false

    const recheck = async () => {
      const detail = await api.tools.get(tool.id)
      if (cancelled) return
      setConnection(detail.connection)
      if (detail.connection?.connected) {
        setAwaitingSignIn(false)
        notify.success('Connected. The tool can now act at the service.')
      }
    }

    const heard = (event: MessageEvent) => {
      // Same origin only, and only our own message: anything else arriving on
      // this channel is somebody else's business.
      if (event.origin !== window.location.origin) return
      const message = event.data as { source?: string; kind?: string } | null
      if (message?.source !== 'sag' || message.kind !== 'tool-connect') return
      void recheck()
    }
    const returned = () => {
      if (document.visibilityState === 'visible') void recheck()
    }

    window.addEventListener('message', heard)
    window.addEventListener('focus', returned)
    document.addEventListener('visibilitychange', returned)
    return () => {
      cancelled = true
      window.removeEventListener('message', heard)
      window.removeEventListener('focus', returned)
      document.removeEventListener('visibilitychange', returned)
    }
  }, [awaitingSignIn, tool.id, notify])

  const settings = (): Record<string, unknown> => {
    const out: Record<string, unknown> = {}
    for (const section of sections)
      for (const field of section.fields) {
        // A callback field is read-only: the deployment's own address, shown to
        // be pasted elsewhere. Sending it back would store a fact about us as
        // though somebody had configured it.
        if (field.type === 'callback') continue
        const raw = values[field.key] ?? ''
        if (field.type === 'number') out[field.key] = raw === '' ? undefined : Number(raw)
        else out[field.key] = raw
      }
    return out
  }

  const setValue = (key: string, v: string) => {
    setValues((prev) => ({ ...prev, [key]: v }))
    invalidateTest()
  }

  async function save() {
    errors.clear()
    setBusy(true)
    try {
      await api.tools.updateCustom(tool.id, {
        settings: settings(),
        display_name: displayName,
        description,
        guide,
        param_descriptions: paramDescriptions,
        skills: selectedSkills.map(Number),
      })
      await api.tools.update(tool.id, { status: enabled ? 'active' : 'disabled', grants })
      notify.success('The tool was updated.')
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  async function remove() {
    if (!(await notify.confirm({ title: `Delete ${toolLabel(tool)}?`, confirmLabel: 'Delete', destructive: true })))
      return
    setBusy(true)
    try {
      await api.tools.removeCustom(tool.id)
      notify.success(`${toolLabel(tool)} was deleted.`)
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  const driverLabel = tool_?.variant_label ?? variant

  // On an edit a secret is shown blank and kept if left so. That is this form's
  // knowledge, not the field renderer's, so note it on the secret fields here
  // and hand the generic tabs ordinary fields.
  const connectionSections = useMemo(
    () =>
      sections.map((section) => ({
        ...section,
        fields: section.fields.map((field) =>
          field.secret ? { ...field, help: [field.help, 'Leave blank to keep it unchanged.'].filter(Boolean).join(' ') } : field,
        ),
      })),
    [sections],
  )

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={toolLabel(tool)}
      onSubmit={save}
      submitting={busy}
      loading={!tool_}
      submitLabel="Save changes"
      wide
      footerStart={
        <Button type="button" variant="destructive" size="sm" onClick={remove} disabled={busy}>
          Delete tool
        </Button>
      }
    >
      {tool_?.about && (
        <div className="rounded-md border border-primary/30 bg-primary/5 px-4 py-3">
          <p className="text-sm text-muted-foreground">{tool_.about}</p>
        </div>
      )}

      <div className="grid grid-cols-2 items-start gap-x-4 gap-y-4">
        <Field label="Name" hint="How the AI calls it. Fixed once created.">
          <Input value={tool.name} readOnly disabled />
        </Field>
        <Field label="Name" hint="What people call it. The assistant's own name for it was fixed when the tool was made.">
          <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder={tool.name} />
        </Field>
      </div>

      <Field
        label="AI Description"
        hint="Teach the AI when to use this tool. Write it for the model, not for people."
      >
        <Textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
      </Field>

      {driverLabel && <Field label="Driver" hint="Fixed once created.">{<Input value={driverLabel} readOnly disabled />}</Field>}

      {/* A tool that signs in to a service is not finished until somebody has.
          Shown on opening, because it used to appear only as a side effect of
          pressing Test, so a tool that had never been connected looked
          complete and offered no way to connect it. */}
      {connection && (
        <Field
          label="Connection"
          hint={
            connection.connected
              ? 'The assistant acts as the person who signed in. Disconnect to revoke that without deleting the tool.'
              : 'This tool cannot reach the service until somebody signs in. The settings are kept either way.'
          }
        >
          <div className="flex items-center gap-3">
            {connection.connected ? (
              <>
                <Badge tone="good">
                  {connection.connected_by ? `Connected by ${connection.connected_by}` : 'Connected'}
                </Badge>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={signingIn}
                  onClick={async () => {
                    setSigningIn(true)
                    try {
                      await api.tools.disconnect(tool.id)
                      setConnection({ connected: false })
                      notify.success('The sign-in was removed. The tool and its settings are kept.')
                    } catch (failure) {
                      errors.fail(failure)
                    } finally {
                      setSigningIn(false)
                    }
                  }}
                >
                  Disconnect
                </Button>
              </>
            ) : (
              <>
                <Badge tone="warn">Not connected</Badge>
                <Button
                  type="button"
                  size="sm"
                  disabled={signingIn}
                  onClick={async () => {
                    setSigningIn(true)
                    try {
                      const { authorize_url } = await api.tools.connect(tool.id)
                      // Not noopener: the callback page tells the window that
                      // opened it when the sign-in finished, and a window with
                      // no opener cannot.
                      window.open(authorize_url, '_blank')
                      setAwaitingSignIn(true)
                    } catch (failure) {
                      errors.fail(failure)
                    } finally {
                      setSigningIn(false)
                    }
                  }}
                >
                  {signingIn ? 'Starting…' : 'Sign in to the service'}
                </Button>
                <span className="text-sm text-muted-foreground">
                  {awaitingSignIn
                    ? 'Waiting for the sign-in in the other tab. This updates by itself.'
                    : 'Opens in a new tab.'}
                </span>
              </>
            )}
          </div>
        </Field>
      )}

      {sections.length > 0 && (
        <>
          <SettingsSections sections={connectionSections} active={tab} onSelect={setTab} values={values} onChange={setValue} />
          <ConnectionTest
            key={testVersion}
            run={(answer) => api.tools.testCustomEdit(tool.id, { settings: settings(), ...(answer ?? {}) })}
          />
        </>
      )}

      <hr className="border-border" />

      {tool_?.params && (
        <ParamsEditor params={tool_.params} values={paramDescriptions} onChange={(key, v) => setParamDescriptions((prev) => ({ ...prev, [key]: v }))} />
      )}

      <Field label="AI Guide" hint="Detailed usage the AI fetches only when it needs it, so it does not weigh down every request.">
        <Textarea value={guide} onChange={(e) => setGuide(e.target.value)} rows={4} />
      </Field>

      <Field
        label="Skills for using this tool"
        hint="Written procedures that document THIS tool: an API's paths, what its codes mean, the order things go in. The assistant is told they exist when it reads the guide above, and opens one only if it needs to. An agent holding the tool can read these whether or not the skill was assigned to it."
      >
        <TokenMultiSelect
          options={skillOptions}
          value={selectedSkills}
          onChange={setSelectedSkills}
          placeholder="Add a skill…"
          empty="No skills exist yet. Import one on the Skills screen."
        />
      </Field>

      <hr className="border-border" />

      <CheckboxField
        checked={enabled}
        onChange={setEnabled}
        label="Available"
        hint="Switched off, the model is never told this tool exists. That is the only refusal it cannot argue with."
      />
      {/* Same reason as the column: there is nobody to grant to. Worse here,
          because it renders "No group exists yet." and so reads as something
          waiting to be set up, on an installation where it never will be. */}
      {!POSTURE.single_user && (
        <Field label="Who may use it" hint="Grant nobody and it is open to everyone in the workspace. The first grant is what makes the list exclusive.">
          <div className="space-y-1.5">
            {groups.length === 0 ? (
              <p className="text-xs text-muted-foreground">No group exists yet.</p>
            ) : (
              groups.map((group) => (
                <CheckboxField
                  key={group.id}
                  checked={grants.includes(group.id)}
                  onChange={(on) => setGrants((prev) => (on ? [...prev, group.id] : prev.filter((id) => id !== group.id)))}
                  label={group.name}
                />
              ))
            )}
          </div>
        </Field>
      )}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

function Segment({
  label,
  hint,
  active,
  selected,
  onClick,
}: {
  label: string
  hint: string
  active: boolean
  selected?: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      disabled={!active}
      onClick={onClick}
      className={
        'flex-1 rounded-md border px-3 py-2 text-left ' +
        (selected ? 'border-primary bg-primary/5' : 'border-border') +
        (active ? ' cursor-pointer' : ' cursor-not-allowed opacity-50')
      }
    >
      <div className="text-sm font-medium">{label}</div>
      <div className="text-xs text-muted-foreground">{hint}</div>
    </button>
  )
}

/**
 * The tool's parameters: the template fixes their identity (name, type,
 * required); only each description is editable, to add context like which tables
 * a database has. Shared by the add and edit forms.
 */
function ParamsEditor({
  params,
  values,
  onChange,
}: {
  params: TemplateParam[]
  values: Record<string, string>
  onChange: (key: string, v: string) => void
}) {
  if (params.length === 0) return null
  return (
    <div>
      <div className="text-sm font-medium">AI Parameters</div>
      <p className="text-xs text-muted-foreground">
        The inputs the AI provides when it calls this tool. Fixed by the template; edit the descriptions for better
        context.
      </p>
      <div className="mt-2 space-y-2.5">
        {params.map((p) => (
          <div key={p.key} className="rounded-md border border-border p-3">
            <div className="flex items-center gap-2 text-sm">
              <span className="font-mono">{p.key}</span>
              <Badge>{p.type}</Badge>
              {p.required && <Badge tone="warn">required</Badge>}
            </div>
            <Textarea
              className="mt-2"
              rows={2}
              value={values[p.key] ?? ''}
              onChange={(e) => onChange(p.key, e.target.value)}
              placeholder="What the AI should put here"
            />
          </div>
        ))}
      </div>
    </div>
  )
}

// Static so Tailwind keeps the classes: a field's declared width in the row.
/**
 * Testing a connection, including anything the far end wants before it will
 * accept one.
 *
 * The console does not know what a template's operations are. It runs the one
 * every template has, shows what came back, and when the answer carries a prompt
 * it renders those fields and sends the values to the action named in it. A
 * server asking for a verification code and a template asking anything else look
 * identical from here.
 */
function ConnectionTest({
  run,
}: {
  run: (answer: { action: string; token: string; values: Record<string, string> } | null) => Promise<ToolActionResult>
}) {
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<ToolActionResult | null>(null)
  const [answers, setAnswers] = useState<Record<string, string>>({})

  async function go(prompt: ToolActionPrompt | null) {
    setBusy(true)
    try {
      const next = await run(prompt ? { action: prompt.action, token: prompt.token, values: answers } : null)
      setResult(next)
      if (!next.prompt) setAnswers({})
    } catch (failure) {
      setResult({ ok: false, message: failure instanceof Error ? failure.message : 'The test could not run.' })
    } finally {
      setBusy(false)
    }
  }

  const prompt = result?.prompt
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => {
            setAnswers({})
            setResult(null)
            void go(null)
          }}
          disabled={busy}
        >
          {busy && !prompt ? 'Testing…' : 'Test connection'}
        </Button>
        {result?.ok && (
          <span className="text-sm text-emerald-600 dark:text-emerald-400">{result.message || 'Connected.'}</span>
        )}
        {result && !result.ok && !prompt && (
          <span className="text-sm text-destructive">{result.message || result.error || 'Could not connect.'}</span>
        )}
      </div>

      {/* Somewhere the administrator has to go for this to finish. Rendered
          without knowing what is there, exactly as a prompt's fields are: a new
          tab, because coming back is a redirect to this gateway and taking the
          console with them would lose the form. */}
      <div>
        {result?.visit && (
          <div className="flex items-center gap-3 border-t border-border pt-3">
            <Button type="button" size="sm" asChild>
              <a href={result.visit} target="_blank" rel="noreferrer">
                {result.visiting || 'Continue'}
              </a>
            </Button>
            <span className="text-sm text-muted-foreground">
              Opens in a new tab. Come back here when it is done.
            </span>
          </div>
        )}
      </div>


      {prompt && (
        <div className="space-y-3 border-t border-border pt-3">
          <div>
            <p className="text-sm font-medium">{prompt.title}</p>
            {prompt.hint && <p className="text-sm text-muted-foreground">{prompt.hint}</p>}
          </div>
          <div className="grid grid-cols-6 gap-4">
            {prompt.fields.map((field) => (
              <div key={field.key} className={fieldSpan(field)}>
                <SettingField
                  field={field}
                  value={answers[field.key] ?? ''}
                  onChange={(v) => setAnswers((prev) => ({ ...prev, [field.key]: v }))}
                />
              </div>
            ))}
          </div>
          <Button type="button" size="sm" onClick={() => void go(prompt)} disabled={busy}>
            {busy ? 'Working…' : prompt.submit || 'Continue'}
          </Button>
        </div>
      )}
    </div>
  )
}

function ToolForm({
  tool,
  onClose,
  onSaved,
}: {
  tool: Tool
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  // ONE request, and it is this dialog's. Who a tool may be granted to arrives
  // with the tool: the screen used to fetch the workspace's whole group list on
  // every view of the tools table, on the chance somebody opened a dialog.
  const { data: detail } = useResource(() => api.tools.get(tool.id))
  const groups = detail?.groups ?? []
  const [enabled, setEnabled] = useState(tool.status === 'active')
  const [grants, setGrants] = useState<number[]>(tool.grants)
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  // A native tool's own settings, for the ones that have any: the terminal's
  // list of what it may run. The form is declared by the code that ships the
  // tool and drawn by the component every other declared form uses, so a second
  // tool with settings needs no change here.
  const sections = detail?.sections ?? []
  const [tab, setTab] = useState(0)
  // What is stored, and what has been typed over it, kept apart and merged
  // where they are read.
  //
  // Not an effect that copies the stored values into state when they arrive.
  // That is state derived from a prop written the long way round, and it has a
  // real failure in it as well as a lint rule: the settings land asynchronously,
  // so an effect that overwrites everything on arrival throws away whatever
  // somebody had already typed. Merged this way, what they typed wins and the
  // rest is whatever the server said.
  const stored = useMemo(() => {
    const seed: Record<string, string> = {}
    for (const [key, value] of Object.entries(detail?.settings ?? {})) {
      seed[key] = value == null ? '' : String(value)
    }
    return seed
  }, [detail])
  const [edits, setEdits] = useState<Record<string, string>>({})
  const values = useMemo(() => ({ ...stored, ...edits }), [stored, edits])

  async function save() {
    errors.clear()
    setBusy(true)
    try {
      await api.tools.update(tool.id, {
        status: enabled ? 'active' : 'disabled',
        grants,
        ...(sections.length > 0 ? { settings: values } : {}),
      })
      notify.success(`${toolLabel(tool)} was saved.`)
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  return (
    <Modal open onOpenChange={(o) => !o && onClose()} title={toolLabel(tool)} onSubmit={save} submitting={busy} wide>
      {/* What this tool is for, in the words of the business. `about` is written
          for whoever is deciding whether a team should have this ability;
          `description` is the model's own copy and reads like one, so it is the
          fallback and not the first choice.

          Rendered as Markdown, because it is Markdown: a description written for
          a model is full of `backticks`, bold and lists, and a connected service
          writes its own. Splitting on blank lines put the asterisks and the
          bullets on the screen as characters. The renderer is the hardened one
          the brains screen uses, which is the point here: this text comes from
          somebody else's server. */}
      <Markdown>{tool.about || tool.description}</Markdown>
      {/* Where it came from, said in the words the catalogue groups by. The
          dialog opens away from that heading, so it is repeated here; the name
          the model calls it is not, because it is ours and not the reader's. */}
      {tool.source && <div className="text-xs text-muted-foreground">{tool.source}</div>}

      <CheckboxField
        checked={enabled}
        onChange={setEnabled}
        label="Available"
        hint="Switched off, the model is never told this tool exists. That is the only refusal it cannot argue with."
      />

      {sections.length > 0 && (
        <SettingsSections
          sections={sections}
          active={tab}
          onSelect={setTab}
          values={values}
          onChange={(key, value) => setEdits((prev) => ({ ...prev, [key]: value }))}
        />
      )}

      <Field
        label="Who may use it"
        hint="Grant nobody and it is open to everyone in the workspace. The first grant is what makes the list exclusive."
      >
        <div className="space-y-1.5">
          {groups.length === 0 ? (
            <p className="text-xs text-muted-foreground">No group exists yet.</p>
          ) : (
            groups.map((group) => (
              <CheckboxField
                key={group.id}
                checked={grants.includes(group.id)}
                onChange={(on) =>
                  setGrants((prev) => (on ? [...prev, group.id] : prev.filter((id) => id !== group.id)))
                }
                label={group.name}
              />
            ))
          )}
        </div>
      </Field>

      {/* Nothing here deletes: this dialog is only ever shown for a BUILT-IN or
          an MCP tool, and neither is data we own. A built-in arrives with a
          deploy and an MCP tool belongs to its connection; a custom tool is
          routed to its own dialog, which is where deleting lives. */}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
