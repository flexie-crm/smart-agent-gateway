/**
 * A form somebody else declared.
 *
 * The console does not know what any of these fields ARE. A server hands it a
 * list of sections, each with a list of fields carrying a key, a label, a type
 * and a width, and this renders them in order and hands back the values. That
 * is the whole contract.
 *
 * It is here rather than in a page because this is the THIRD thing that
 * declares a form: a custom tool's driver (KB/31), a datasource driver, and now
 * a model on an inference node (KB/35). One primitive, one renderer, one thing
 * to learn. Anything specific to what a particular form is for belongs in the
 * page that uses it, never here.
 */
import { CheckboxField } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Field } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import type { TemplateField, TemplateSection } from '@/lib/resources'
import { cn } from '@/lib/utils'

const spanClass: Record<number, string> = {
  1: 'col-span-1',
  2: 'col-span-2',
  3: 'col-span-3',
  4: 'col-span-4',
  5: 'col-span-5',
  6: 'col-span-6',
}

// fieldSpan is where a field sits in the six-column row: what the template
// declared, a full row for a multi-line field, or a sensible half otherwise.
/** SectionHint lays a template's own explanation out so it can be read. */
export function SectionHint({ text }: { text: string }) {
  const blocks = text.split(/\n\n+/).map((b) => b.trim()).filter(Boolean)
  return (
    <div className="mt-2 space-y-2 text-xs leading-relaxed text-muted-foreground">
      {blocks.map((block, i) => {
        const points = block.split('\n').filter((line) => line.trimStart().startsWith('- '))
        if (points.length > 0) {
          return (
            <ul key={i} className="list-disc space-y-1 pl-4">
              {points.map((point, j) => (
                <li key={j}>{point.trimStart().slice(2)}</li>
              ))}
            </ul>
          )
        }
        return <p key={i}>{block}</p>
      })}
    </div>
  )
}

export function fieldSpan(field: TemplateField): string {
  if (field.type === 'textarea') return spanClass[6]
  const span = field.span ?? 0
  return spanClass[span >= 1 && span <= 6 ? span : 3]
}

/**
 * The connection settings as tabs, one per server-defined section. The console
 * knows nothing about what a section is: it renders the sections it is handed,
 * in order, laying each field out on a six-column row at the width the template
 * declared, so the form flows (a narrow port beside a wide host).
 */
export function SettingsSections({
  sections,
  active,
  onSelect,
  values,
  onChange,
}: {
  sections: TemplateSection[]
  active: number
  onSelect: (i: number) => void
  values: Record<string, string>
  onChange: (key: string, v: string) => void
}) {
  const current = sections[active] ?? sections[0]
  return (
    <div>
      <div className="flex gap-1 border-b border-border">
        {sections.map((section, i) => (
          <button
            key={section.title}
            type="button"
            onClick={() => onSelect(i)}
            className={cn(
              '-mb-px cursor-pointer border-b-2 px-3 py-2 text-sm',
              i === active
                ? 'border-primary font-medium'
                : 'border-transparent text-muted-foreground hover:text-foreground',
            )}
          >
            {/* A bold ghost reserves the active width, so switching to bold does
                not shift the row. */}
            <span className="grid">
              <span className="col-start-1 row-start-1 invisible font-medium">{section.title}</span>
              <span className={cn('col-start-1 row-start-1', i === active && 'font-medium')}>{section.title}</span>
            </span>
          </button>
        ))}
      </div>
      {/* A section's own words, in paragraphs. A template writes what somebody
          needs to know before filling a form in, and some of it is genuinely
          several thoughts; run together into one block nobody reads any of it.
          A blank line is a paragraph, and a line starting with "- " is a point. */}
      {current?.hint && <SectionHint text={current.hint} />}
      <div className="mt-3 grid grid-cols-6 items-start gap-x-4 gap-y-4">
        {current?.fields.map((field) => (
          <div key={field.key} className={fieldSpan(field)}>
            <SettingField field={field} value={values[field.key] ?? ''} onChange={(v) => onChange(field.key, v)} />
          </div>
        ))}
      </div>
    </div>
  )
}

/** One template settings input, rendered by its declared type. */
export function SettingField({
  field,
  value,
  onChange,
}: {
  field: TemplateField
  value: string
  onChange: (v: string) => void
}) {
  // A checkbox is its own label: the words go beside the box, not above an
  // empty one. Its value travels as a string like every other declared field,
  // because the form is one shape for all of them.
  if (field.type === 'checkbox') {
    return (
      <CheckboxField
        checked={truthy(value)}
        onChange={(on) => onChange(on ? 'true' : '')}
        label={field.label}
        hint={field.help}
      />
    )
  }
  return (
    <Field label={field.label} required={field.required} hint={field.help}>
      {field.type === 'select' ? (
        <NativeSelect value={value} onChange={(e) => onChange(e.target.value)}>
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </NativeSelect>
      ) : field.type === 'textarea' ? (
        <Textarea value={value} onChange={(e) => onChange(e.target.value)} rows={3} />
      ) : (
        <Input
          type={field.type === 'password' ? 'password' : field.type === 'number' ? 'number' : 'text'}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={field.secret ? '••••••••' : undefined}
        />
      )}
    </Field>
  )
}

/** What a stored checkbox value means. Anything a form can send for "on" is on. */
function truthy(value: string): boolean {
  return ['true', '1', 'yes', 'on'].includes(value.trim().toLowerCase())
}
