import { useState } from 'react'
import { Plus, X } from 'lucide-react'
import { Field, Modal } from '@/components/ui/modal'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { CheckboxField } from '@/components/ui/checkbox'
import { NativeSelect } from '@/components/ui/native-select'
import { brains } from '@/lib/brains'
import { useResource } from '@/lib/resources'
import { useNotify } from '@/lib/notify'
import type { Brain, Category, Document } from '@/lib/brains'
import { useFormErrors } from '@/lib/form'

/**
 * The forms that curate a brain.
 *
 * A document's relations are picked as (category, document) pairs, because that
 * is how a person finds a document they half remember: they know roughly where
 * it lives before they know its title.
 */

export function BrainForm({
  brain,
  onClose,
  onSaved,
}: {
  brain?: Brain
  onClose: () => void
  onSaved: () => void | Promise<void>
}) {
  const notify = useNotify()
  const [name, setName] = useState(brain?.name ?? '')
  const [description, setDescription] = useState(brain?.description ?? '')
  const [locked, setLocked] = useState(brain?.locked ?? false)
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A brain needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = { name: name.trim(), description, locked }
      if (brain) await brains.update(brain.id, body)
      else await brains.create(body)
      notify.success('The knowledge base was saved.')
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={brain ? 'Edit brain' : 'New brain'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field
        label="Description"
        hint="The agent reads this when it decides which brain is worth opening. Say what is in it."
        error={errors.fields.description}
      >
        <Textarea
          rows={3}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </Field>

      <CheckboxField
        checked={locked}
        onChange={setLocked}
        label="Locked memory"
        hint="When locked, agents can read this brain but cannot add, edit or remove its memory. Leave it unlocked to let agents update the memory as they work."
      />

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

export function CategoryForm({
  brain,
  brains: allBrains,
  category,
  onClose,
  onSaved,
}: {
  brain: Brain
  brains: Brain[]
  category?: Category
  onClose: () => void
  onSaved: () => void | Promise<void>
}) {
  const notify = useNotify()
  const [name, setName] = useState(category?.name ?? '')
  const [description, setDescription] = useState(category?.description ?? '')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  const owner = allBrains.find((b) => b.id === (category?.brain_id ?? brain.id)) ?? brain

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A category needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = { name: name.trim(), description }
      if (category) await brains.updateCategory(category.id, body)
      else await brains.createCategory(brain.id, body)
      notify.success(`${name.trim()} was saved.`)
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={category ? 'Edit category' : 'New category'}
      onSubmit={save}
      submitting={busy}
    >
      {/* The brain is shown, not chosen: a category belongs to the brain you are
          standing in, and moving one between brains would orphan every document
          in it from the graph it is part of. */}
      <Field label="Brain">
        <Input value={owner.name} disabled readOnly />
      </Field>

      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field label="Description" error={errors.fields.description}>
        <Textarea rows={3} value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

interface Relation {
  categoryID: number | null
  documentID: number | null
}

export function DocumentForm({
  brain,
  categories,
  categoryID,
  document,
  onClose,
  onSaved,
}: {
  brain: Brain
  categories: Category[]
  categoryID: number
  document?: Document
  onClose: () => void
  onSaved: (saved: Document) => void | Promise<void>
}) {
  const notify = useNotify()
  const [category, setCategory] = useState(document?.category_id ?? categoryID)
  const [title, setTitle] = useState(document?.title ?? '')
  const [content, setContent] = useState(document?.content ?? '')
  /**
   * The existing relations, as (category, document) pairs, straight from the
   * document.
   *
   * This used to be a fan-out: one request PER CATEGORY in the brain, all of
   * their documents pulled down, then a search through the lot to work out
   * which category each link lived in. A brain with twenty categories cost
   * twenty requests to open one document. The link now says where it lives, so
   * there is nothing to search and nothing to fetch.
   */
  const [relations, setRelations] = useState<Relation[]>(() =>
    (document?.related ?? []).map((link) => ({
      categoryID: link.category_id,
      documentID: link.id,
    })),
  )
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  // Documents to choose from, per category. A category's are fetched as it is
  // PICKED, because a brain may hold thousands and nobody is choosing from
  // thousands anyway.
  const [picked, setPicked] = useState<Record<number, Document[]>>({})

  async function loadChoices(id: number) {
    const docs = await brains.documents(id)
    setPicked((prev) => ({ ...prev, [id]: docs }))
  }

  // The categories the existing relations already point at, so their pickers can
  // be changed and not only read. A native select cannot fill itself as it
  // opens, so these are asked for when the form does: at most one request per
  // category actually linked to, where it was one per category in the brain.
  const linked = [...new Set((document?.related ?? []).map((l) => l.category_id))]
  const { data: alreadyLinked } = useResource(
    async () => Object.fromEntries(await Promise.all(linked.map(async (id) => [id, await brains.documents(id)] as const))),
    linked.length > 0,
  )

  // And the link itself is the picker's first answer, so a relation shows its
  // document's title on the first paint rather than after a round trip.
  const fromLinks: Record<number, Document[]> = {}
  for (const link of document?.related ?? []) {
    fromLinks[link.category_id] = [
      ...(fromLinks[link.category_id] ?? []),
      { id: link.id, title: link.title } as Document,
    ]
  }
  const choicesIn = (categoryID: number | null) =>
    categoryID === null
      ? []
      : picked[categoryID] ?? alreadyLinked?.[categoryID] ?? fromLinks[categoryID] ?? []

  async function save() {
    if (!title.trim()) {
      errors.reject({ title: 'A document needs a title.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const related = relations
        .map((r) => r.documentID)
        .filter((id): id is number => id !== null && id !== document?.id)

      const saved = document
        ? await brains.updateDocument(document.id, {
            title: title.trim(),
            content,
            category_id: category,
            related,
          })
        : await brains.createDocument(category, { title: title.trim(), content, related })
      notify.success(`${saved.title} was saved.`)
      await onSaved(saved)
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
      title={document ? 'Edit document' : 'New document'}
      onSubmit={save}
      submitting={busy}
    >
      <div className="grid grid-cols-2 gap-4">
        <Field label="Brain">
          <Input value={brain.name} disabled readOnly />
        </Field>
        <Field label="Category" error={errors.fields.category_id}>
          <Select
            value={category}
            onChange={(v) => setCategory(v)}
            options={categories.map((c) => ({ value: c.id, label: c.name }))}
          />
        </Field>
      </div>

      <Field label="Title" required error={errors.fields.title}>
        <Input value={title} onChange={(e) => setTitle(e.target.value)} autoFocus />
      </Field>

      <Field label="Content (Markdown)" error={errors.fields.content}>
        <Textarea
          rows={14}
          value={content}
          onChange={(e) => setContent(e.target.value)}
          className="font-mono text-xs"
          placeholder="# A heading&#10;&#10;Markdown. It is rendered when the document is read."
        />
      </Field>

      <div className="space-y-2">
        <p className="text-sm font-medium">Related documents</p>
        <p className="text-xs text-muted-foreground">
          Relations go both ways: this document will appear on theirs too. That is what makes the
          brain something an agent can walk through rather than a list it has to read.
        </p>

        {relations.map((relation, index) => (
          <div key={index} className="flex items-center gap-2">
            <Select
              value={relation.categoryID}
              placeholder="Select category"
              onChange={(v) => {
                void loadChoices(v)
                setRelations((prev) =>
                  prev.map((r, i) => (i === index ? { categoryID: v, documentID: null } : r)),
                )
              }}
              options={categories.map((c) => ({ value: c.id, label: c.name }))}
            />
            <Select
              value={relation.documentID}
              placeholder="Select document"
              disabled={!relation.categoryID}
              onChange={(v) =>
                setRelations((prev) =>
                  prev.map((r, i) => (i === index ? { ...r, documentID: v } : r)),
                )
              }
              options={choicesIn(relation.categoryID)
                .filter((d) => d.id !== document?.id)
                .map((d) => ({ value: d.id, label: d.title }))}
            />
            <button
              type="button"
              aria-label="Remove relation"
              onClick={() => setRelations((prev) => prev.filter((_, i) => i !== index))}
              className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              <X className="size-4" />
            </button>
          </div>
        ))}

        <button
          type="button"
          onClick={() => setRelations((prev) => [...prev, { categoryID: null, documentID: null }])}
          className="flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
        >
          <Plus className="size-4" />
          Add relation
        </button>
      </div>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

/** A plain select. Radix's would be prettier and would not survive a form reset. */
function Select({
  value,
  onChange,
  options,
  placeholder,
  disabled,
}: {
  value: number | null
  onChange: (value: number) => void
  options: { value: number; label: string }[]
  placeholder?: string
  disabled?: boolean
}) {
  return (
    // In a relation row the two selects split the width evenly (the CRM's
    // layout): a select sized to its content makes every row a different shape.
    <div className="min-w-0 flex-1">
      <NativeSelect
        value={value ?? ''}
        disabled={disabled}
        onChange={(e) => onChange(Number(e.target.value))}
      >
        {placeholder && <option value="">{placeholder}</option>}
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </NativeSelect>
    </div>
  )
}
