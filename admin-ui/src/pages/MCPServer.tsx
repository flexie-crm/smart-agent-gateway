import { useState } from 'react'
import { Page } from '@/components/AppShell'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { MCPScreen } from '@/lib/resources'
import { bySource, toolLabel } from '@/lib/tool-display'

/**
 * Our MCP server: what this workspace exposes to external agents (the CRM's
 * MCP Server settings, workspace-scoped).
 *
 * The exposure list is a kill-switch, never a grant: an exposed tool can
 * still only do what the connecting token's person is allowed to do, and
 * that is re-checked on every call. Unconfigured means the whole catalog is
 * exposed, the CRM's own default; saving once takes explicit control.
 */
export function MCPServer() {
  // ONE request: what may be exposed, and whether each thing IS. The screen
  // used to fetch the tool catalogue, the brains and the saved settings, then
  // walk the catalogue deciding each checkbox from `configured ? saved : true`.
  // That rule is the server's, and it answers it.
  const { data: screen, reload } = useResource(() => api.mcpServer.get())
  // What has been CHANGED since the answer arrived, not a copy of it.
  //
  // Seeding a copy from the response meant an effect writing state on every
  // load, which cascades renders and drifts the moment the two disagree. The
  // answer already says which boxes are ticked; this holds only the edits, and
  // the effective value is one read of both.
  const [toolEdits, setToolEdits] = useState<Record<string, boolean>>({})
  const [brainEdits, setBrainEdits] = useState<Record<number, boolean>>({})
  const [busy, setBusy] = useState(false)
  const notify = useNotify()
  const errors = useFormErrors()

  const exposed = (t: MCPScreen['tools'][number]) => toolEdits[t.name] ?? t.exposed
  const chosen = (b: MCPScreen['brains'][number]) => brainEdits[b.id] ?? b.chosen

  async function save() {
    errors.clear()
    setBusy(true)
    try {
      const tool_config: Record<string, { enabled: boolean }> = {}
      for (const t of screen?.tools ?? []) {
        tool_config[t.name] = { enabled: exposed(t) }
      }
      const brain_config = (screen?.brains ?? []).filter(chosen).map((b) => b.id)
      await api.mcpServer.put({ tool_config, brain_config })
      // Re-read rather than assume: the screen is a view of the server's
      // answer, and the answer has just changed.
      setToolEdits({})
      setBrainEdits({})
      await reload()
      // What was saved, not that a button worked: the count is the thing
      // somebody came here to set.
      const count = Object.values(tool_config).filter((t) => t.enabled).length
      notify.success(
        count === 1
          ? 'One tool is exposed to external agents.'
          : `${count} tools are exposed to external agents.`,
      )
    } catch (failure) {
      errors.fail(failure)
    } finally {
      setBusy(false)
    }
  }

  const tools = screen?.tools ?? []
  const builtin = tools.filter((t) => t.kind !== 'mcp')
  const projected = tools.filter((t) => t.kind === 'mcp')

  return (
    <Page
      title="MCP Server"
      description="What this workspace exposes to external agents connecting over MCP."
      actions={
        <Button size="sm" onClick={() => void save()} disabled={busy || !screen}>
          Save
        </Button>
      }
    >
      <div className="max-w-3xl space-y-8">
        <p className="border-l-2 border-primary bg-muted/40 px-4 py-3 text-sm text-muted-foreground">
          The MCP server lets an external AI agent use this workspace's tools after
          authenticating with an OAuth token. Pick which tools it exposes below. This list is
          independent of your own agents' tool settings, and a person's grants still apply: a
          tool here can only do what the connecting token's user is allowed to do.
          {screen && !screen.configured && (
            <span className="mt-1 block font-medium text-foreground">
              Nothing is configured yet, so every active tool is exposed. Saving takes control.
            </span>
          )}
        </p>

        <Section
          title="Exposed tools"
          hint="Tools the server lists and allows external agents to call."
        >
          <ToolGroups tools={builtin} exposed={exposed} onToggle={setToolEdits} />
        </Section>

        {projected.length > 0 && (
          <Section
            title="Tools from MCP servers"
            hint="Tools this workspace itself consumes from other services. Exposing one relays it."
          >
            <ToolGroups tools={projected} exposed={exposed} onToggle={setToolEdits} />
          </Section>
        )}

        <Section
          title="Knowledge bases"
          // It promised something the server does not do: MCPLoadout passes no
          // brains, because an external caller is not an agent with an
          // assignment (KB/21, KB/32). Saying so is better than a control that
          // quietly does nothing.
          hint="Saved for when external callers can read knowledge. It has no effect yet: a caller over this surface is not an agent with an assignment, so the brain tools reach nothing."
        >
          {(screen?.brains ?? []).length === 0 ? (
            <p className="text-xs text-muted-foreground">No brain exists yet.</p>
          ) : (
            <div className="grid grid-cols-2 gap-1.5">
              {(screen?.brains ?? []).map((brain) => (
                <CheckboxField
                  key={brain.id}
                  checked={chosen(brain)}
                  onChange={(on) => setBrainEdits((prev) => ({ ...prev, [brain.id]: on }))}
                  label={brain.name}
                />
              ))}
            </div>
          )}
        </Section>

        {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
      </div>
    </Page>
  )
}

function Section({
  title,
  hint,
  children,
}: {
  title: string
  hint: string
  children: React.ReactNode
}) {
  return (
    <section>
      <h2 className="text-sm font-medium">{title}</h2>
      <p className="mb-3 mt-0.5 text-xs text-muted-foreground">{hint}</p>
      {children}
    </section>
  )
}

// Grouped by where each tool came from, in the same runs and the same order as
// the tools screen and the agent form. It is what tells two services' `query`
// apart now that a name is read without the prefix that used to do it.
function ToolGroups({
  tools,
  exposed,
  onToggle,
}: {
  tools: MCPScreen['tools']
  exposed: (t: MCPScreen['tools'][number]) => boolean
  onToggle: React.Dispatch<React.SetStateAction<Record<string, boolean>>>
}) {
  if (tools.length === 0) {
    return <p className="text-xs text-muted-foreground">This build ships no tools.</p>
  }
  return (
    <div className="space-y-3">
      {bySource(tools).map((group) => (
        <div key={group.source}>
          <div className="mb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
            {group.source}
          </div>
          <ToolGrid tools={group.items} exposed={exposed} onToggle={onToggle} />
        </div>
      ))}
    </div>
  )
}

function ToolGrid({
  tools,
  exposed,
  onToggle,
}: {
  tools: MCPScreen['tools']
  exposed: (t: MCPScreen['tools'][number]) => boolean
  onToggle: React.Dispatch<React.SetStateAction<Record<string, boolean>>>
}) {
  return (
    <div className="grid grid-cols-2 gap-x-6 gap-y-1.5">
      {tools.map((tool) => (
        <CheckboxField
          key={tool.id}
          checked={exposed(tool)}
          onChange={(on) => onToggle((prev) => ({ ...prev, [tool.name]: on }))}
          label={<span className="block truncate">{toolLabel(tool)}</span>}
        />
      ))}
    </div>
  )
}
