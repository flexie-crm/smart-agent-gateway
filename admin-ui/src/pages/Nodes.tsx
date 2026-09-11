import { Fragment, useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { Check, ChevronLeft, Copy, Download, Plus, RotateCw, Trash2 } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { SettingsSections } from '@/components/SettingsForm'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type {
  LibraryCredential,
  LibraryHit,
  LibraryModel,
  NodeDetail,
  NodeModel,
  NodePull,
  NodeSummary,
  TemplateSection,
  WorkspaceChoice,
} from '@/lib/resources'

/**
 * The machines that run models we own (KB/35).
 *
 * A machine belongs to the PLATFORM and a model belongs to a workspace, and that
 * is the shape of this whole screen. A box is racked once; every workspace on
 * the deployment may have models on it. So the list is not scoped to anything,
 * and each model carries the workspaces that may route to it.
 *
 * The thing worth saying out loud, because it is the point: **giving a model to
 * a second workspace downloads nothing.** The weights are on one disk, loaded
 * once, answering on one port. What a workspace gets is permission to route
 * there.
 *
 * Nothing here is stored. Every figure is what the MACHINE said when it was
 * asked, so the screen asks whenever it wants to know, and while a download is
 * running it keeps asking. The rest of the console is pushed to over the socket;
 * this is not, because the socket lives on the server the browser dialled while
 * the watching happens on a worker, and a download's progress is a slow ramp
 * where a small poll reads the same number directly and correctly, including
 * after a reload.
 */
export function Nodes() {
  const { machineID } = useParams()
  const navigate = useNavigate()

  // A path segment that is not a number is not a machine, so it falls back to
  // the list rather than asking the server about NaN.
  const id = machineID ? Number(machineID) : null
  const opened = id !== null && Number.isFinite(id) && id > 0 ? id : null

  return opened === null ? (
    <MachineList onOpen={(next) => navigate(`/machines/${next}`)} />
  ) : (
    <MachineScreen machineID={opened} onBack={() => navigate('/machines')} />
  )
}

/** Whether a machine is the computer this is running on. */
function isThisComputer(address: string | undefined): boolean {
  if (!address) return false
  try {
    const host = new URL(address).hostname
    return host === '127.0.0.1' || host === 'localhost' || host === '::1' || host === '[::1]'
  } catch {
    return false
  }
}

const INFERENCE = 'Models that run on hardware we own: this computer, and any machine added to it.'

/* --- the machines ---------------------------------------------------------- */

function MachineList({ onOpen }: { onOpen: (id: number) => void }) {
  const { data, error, reload } = useResource(() => api.nodes.list())
  const machines = data?.nodes ?? null
  const [adding, setAdding] = useState(false)

  // The list is always the screen, whatever is on it.
  //
  // It used to be skipped when there was exactly one machine, on the reasoning
  // that a list of one is a step. That reasoning held while a personal
  // installation could only ever have the computer it runs on; it does not once
  // it can also be given a remote server with a card in it, because then the
  // one machine is a list that is about to have two, and the way to add the
  // second has to be somewhere. Skipping the list also made the whole page swap
  // as the answer landed, which rewrote the heading and the action button in
  // front of the reader.
  //
  // The empty case is still its own answer and not a blank page. That was a real
  // bug: one piece of state stood for both "have not asked yet" and "there is
  // nothing", so a build with no engine showed nothing at all, for ever, with no
  // error and nothing in any log.

  return (
    <Page
      title="Inference"
      description={INFERENCE}
      // Always, and always the same words. The header of this screen does not
      // change once it is drawn: not the heading, not the description, not the
      // action. It used to change all three, because the list was skipped when
      // one machine came back and the machine's own screen took over with its
      // own heading and its own "Add a model".
      actions={
        <Button size="sm" onClick={() => setAdding(true)}>
          <Plus className="size-4" />
          Add a machine
        </Button>
      }
    >
      {error && <p className="text-sm text-destructive">{error}</p>}
      {/* On BOTH screens, because both are places somebody arrives at.
          A deployment lands on this list, so taking it away from here to put it
          on the machine screen removed it from the edition that had it. It is
          one credential for the whole installation either way, so the second
          copy is the same section reading the same answer. */}
      <ModelLibraryCredential />
      <DataTable
        items={machines}
        empty="Nothing to run models with yet. Add a machine to use a server with a graphics card in it."
        remove={{
          run: async (m) => {
            await api.nodes.forget(m.id)
            await reload()
          },
          confirm: (m) =>
            `Remove "${m.name}"? Every workspace using models on it loses them. The weights on the machine are untouched, and it will register again if it is still running.`,
          done: (m) => `${m.name} was removed.`,
        }}
        columns={[
          {
            header: 'Machine',
            // The name opens it. A pencil would say "edit", and this opens a
            // screen: a machine has nothing here to edit, it configures itself.
            cell: (m) => (
              <button
                type="button"
                className="cursor-pointer text-left"
                onClick={() => onOpen(m.id)}
              >
                <div className="font-medium hover:underline">{m.name}</div>
                {/* The address, for a machine that is somewhere else. A
                    loopback port is not an address anybody uses: nobody types
                    it, and it changes with every launch, so under a row already
                    named "This computer" it is a second line saying here. */}
                {!isThisComputer(m.base_url) && (
                  <div className="font-mono text-xs text-muted-foreground">{m.base_url}</div>
                )}
              </button>
            ),
          },
          {
            header: 'State',
            // A machine that is off is a row that says so, never a row missing
            // from the list: somebody looking for one needs to be told it is
            // down, not left wondering whether they imagined adding it.
            cell: (m) =>
              !m.reachable ? (
                <div>
                  <Badge tone="bad">unreachable</Badge>
                  {m.problem && (
                    <div className="mt-1 text-xs text-muted-foreground">{m.problem}</div>
                  )}
                </div>
              ) : m.info?.can_infer ? (
                <div>
                  <Badge tone="good">ready</Badge>
                  {expiring(m) && (
                    <div className="mt-1 text-xs text-amber-600 dark:text-amber-400">
                      {expiring(m)}
                    </div>
                  )}
                </div>
              ) : (
                <Badge tone="warn">cannot run models</Badge>
              ),
          },
          {
            header: 'Models',
            cell: (m) =>
              m.info ? (
                <span className="text-sm">
                  {m.info.models} on disk, <span className="font-medium">{m.info.resident}</span> in
                  memory
                </span>
              ) : (
                <span className="text-muted-foreground">—</span>
              ),
          },
          {
            header: 'Room',
            cell: (m) =>
              m.info ? (
                <span className="text-sm text-muted-foreground">
                  {bytes(m.info.machine.disk_free)} free of {bytes(m.info.machine.disk_total)}
                </span>
              ) : (
                <span className="text-muted-foreground">—</span>
              ),
          },
        ]}
      />

      {adding && <AddMachineModal onClose={() => setAdding(false)} onDone={reload} />}
    </Page>
  )
}

/**
 * Where the installer is fetched from.
 *
 * NOT this console's own origin, which is what it used to be. That works on a
 * deployment and is impossible on a desktop: the personal gateway binds to a
 * free port on loopback, so the command said
 * `curl http://127.0.0.1:55271/inference/download/v1/install.sh`, which the
 * machine being installed cannot resolve, let alone fetch. The binaries are
 * published at one address for everybody; a deployment still serves its own
 * copy, for a closed network that can reach nothing else, and such a deployment
 * can point this elsewhere at build time.
 */
const DOWNLOAD_HOST =
  (import.meta.env.VITE_SAG_DOWNLOAD_HOST as string | undefined) ?? 'https://sag-repo.flexie.io'

/**
 * Whether a machine somewhere else could reach this console's server at all.
 *
 * A loopback origin is not a preference, it is a fact that decides which of the
 * two ways in is even possible: a machine cannot register itself with an address
 * that means "the computer I am already on". The personal edition is always in
 * this position, and so is anyone running a deployment's console over an SSH
 * tunnel, which is why this is read from the origin rather than from the build's
 * posture.
 */
function gatewayIsReachable(): boolean {
  return !/^https?:\/\/(127\.\d+\.\d+\.\d+|localhost|\[::1\])(:|$|\/)/i.test(
    window.location.origin,
  )
}

/**
 * Adding a machine when the gateway cannot be called back.
 *
 * Joining is the better shape and is used wherever the machine can open a
 * connection to us. It never can on the personal edition, whose gateway is on a
 * laptop behind a router, so this is the only way in there.
 *
 * It is one screen and not a wizard, deliberately. A person doing this has a
 * terminal open on the machine and is moving between the two windows: hiding
 * either half behind a step means going back and forth to find what they were
 * copying. So everything is visible at once, in the order it is done.
 */
function ExchangeModal({ onClose, onDone }: { onClose: () => void; onDone: () => Promise<void> }) {
  const { data } = useResource(() => api.nodes.ourCertificate())
  const [name, setName] = useState('')
  const [address, setAddress] = useState('')
  const [certificate, setCertificate] = useState('')
  // Asked, not left to a flag somebody has to notice. A rented GPU machine has
  // a small root disk and a large volume mounted somewhere else, and a model is
  // tens of gigabytes: the default is the one answer most likely to be wrong,
  // and getting it wrong fills the disk the system is running on. So it is a
  // field, prefilled with the default, and it is always in the command.
  const [dataDir, setDataDir] = useState('/var/lib/sag-inference')
  const [busy, setBusy] = useState(false)
  const problems = useFormErrors()

  // Base64, so the certificate is ONE word with no newlines and no quoting.
  //
  // It was inlined as PEM inside `$'...'`, the only shell quoting that survives
  // newlines, and it read as line noise ending in a stray quote: a person
  // cannot tell a complete paste from a truncated one, and a shell without that
  // quoting mangles it silently. Encoded, it is one line whose ends can be
  // checked by eye, and the installer decodes it.
  const install = data
    ? `curl -fsSL ${DOWNLOAD_HOST}/install.sh | sudo sh -s -- \\\n  --data ${dataDir.trim() || '/var/lib/sag-inference'} \\\n  --node-key ${data.key} \\\n  --gateway-cert ${btoa(data.certificate.trim())}`
    : ''

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add a machine"
      wide
      loading={!data}
      submitLabel="Add the machine"
      submitting={busy}
      onSubmit={async () => {
        problems.clear()
        setBusy(true)
        try {
          // The key that went out in the command goes back with the
          // address. Both ends must hold the same one: the machine refuses any
          // caller whose bearer token does not match its own.
          await api.nodes.addByCertificate({ name, address, certificate, key: data!.key })
          await onDone()
          onClose()
        } catch (e) {
          problems.fail(e)
        } finally {
          setBusy(false)
        }
      }}
    >
      <p className="text-sm text-muted-foreground">
        A computer with a graphics card, somewhere else, that will run models for you. The two
        machines swap certificates so that each will talk only to the other.
      </p>

      <div className="border-t border-border pt-4">
        <h3 className="mb-1 text-sm font-medium">1. Install it on that machine</h3>
        <p className="mb-3 text-sm text-muted-foreground">
          Run this on the machine, as root. It carries this gateway certificate, which is what
          makes the machine refuse everybody else.
        </p>
        <div className="mb-4">
          <Field
            label="Where to keep models"
            hint="Models are tens of gigabytes. If that machine has a big disk mounted somewhere, put this on it: downloads land beside them, so both follow this."
          >
            <Input
              value={dataDir}
              onChange={(e) => setDataDir(e.target.value)}
              placeholder="/var/lib/sag-inference"
            />
          </Field>
        </div>
        <Paste text={install} />
        <p className="mt-3 text-sm text-muted-foreground">
          Add either of these to the end of the command if you need them.
        </p>
        {/* The two that decide where things land, and they are not niceties on
            a GPU box: models are tens of gigabytes and the root disk usually is
            not. The old dialog listed these and this one did not, which is how
            somebody fills / with weights. */}
        <dl className="mt-4 grid grid-cols-[max-content_1fr] items-baseline gap-x-6 gap-y-3">
          {[
            [
              '--port <number>',
              'Listen on something other than 19443. Use the same port in the address below.',
            ],
            [
              '--accel cpu',
              'Use the processor rather than a graphics card. This works, and it is a great deal slower.',
            ],
          ].map(([flag, why]) => (
            <Fragment key={flag}>
              <dt className="font-mono text-xs">{flag}</dt>
              <dd className="text-sm text-muted-foreground">{why}</dd>
            </Fragment>
          ))}
        </dl>
      </div>

      <div className="border-t border-border pt-4">
        <h3 className="mb-1 text-sm font-medium">2. Copy back what it prints</h3>
        <p className="mb-3 text-sm text-muted-foreground">
          When it finishes it prints an address and a certificate. Put them here.
        </p>
        {problems.form && <p className="mb-3 text-sm text-destructive">{problems.form}</p>}
        <div className="space-y-5">
          <Field label="Name" error={problems.fields.name} hint="Whatever you will recognise it by.">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="gpu-1" />
          </Field>
          <Field label="Address" error={problems.fields.address}>
            <Input
              value={address}
              onChange={(e) => setAddress(e.target.value)}
              placeholder="https://203.0.113.7:19443"
            />
          </Field>
          <Field label="The machine certificate" error={problems.fields.certificate}>
            <textarea
              value={certificate}
              onChange={(e) => setCertificate(e.target.value)}
              rows={7}
              spellCheck={false}
              placeholder="-----BEGIN CERTIFICATE-----"
              className="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
            />
          </Field>
        </div>
      </div>
    </Modal>
  )
}

/**
 * How a machine is added: it is not, it adds itself.
 *
 * The same shape a node uses to join a swarm. Nobody types an address and nobody
 * chooses a key: the machine mints its own, presents the shared token once, and
 * appears. So this dialog is one command to paste, and what it does is install
 * the node rather than assume somebody has already put it there.
 */
function AddMachineModal(props: { onClose: () => void; onDone: () => Promise<void> }) {
  // Not a choice offered to the person. Which way in is possible is a fact about
  // where this console is being served from, and asking somebody to pick would
  // be asking them to know that.
  return gatewayIsReachable() ? <JoinModal {...props} /> : <ExchangeModal {...props} />
}

function JoinModal({ onClose, onDone }: { onClose: () => void; onDone: () => Promise<void> }) {
  // A FRESH token every time this opens, in ONE request.
  //
  // There used to be a Rotate button, and it asked a question nobody adding a
  // machine has any business answering: they came here to install one, not to
  // manage the credential that lets them. Minting on open removes the choice and
  // the button with it. It was then two calls, a rotate and a read, which was
  // the rotate button in disguise; there is nothing to read back now, so it is
  // the one call it always was.
  //
  // The token is yours, lasts an hour, and the machine that uses it destroys it.
  // So a command copied from an earlier opening stops working, and that is the
  // feature: what somebody leaves in a scrollback is dead. Machines already
  // added are unaffected either way, because each holds a key of its own and
  // checks in with that (KB/35).
  //
  // Minted ONCE per opening, which needs saying because the effect that loads it
  // runs twice on mount (React does that on purpose in development, to catch
  // exactly this). A read run twice is a wasted request; a MINT run twice writes
  // a second token over the first, so the dialog would show one credential while
  // a moment of another one's life had already been spent. Holding the promise
  // makes the second caller wait on the first.
  const minting = useRef<Promise<{ token: string }> | null>(null)
  const { data } = useResource(() => (minting.current ??= api.nodes.mintJoinToken()))

  // Broken where a reader would break it, not where the box runs out.
  //
  // One long line and the browser wraps it wherever the character happens to
  // land, so `sudo sh -s --` came apart into `sudo sh -` and `s -- \`, which is
  // a line that says nothing and a command that looks mistyped. Each line here
  // is a whole thought and fits: fetch it, run it, and the credential.
  const origin = window.location.origin
  const install = data
    ? `curl -fsSL ${DOWNLOAD_HOST}/install.sh | \\\n  sudo sh -s -- --url ${origin} \\\n  --token ${data.token}`
    : ''

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add a machine"
      wide
      loading={!data}
      submitLabel="Done"
      onSubmit={async () => {
        await onDone()
        onClose()
      }}
    >
      <Paste text={install} />

      {/* Sentences, because somebody is reading this while logged into a server
          they are about to run something on as root. It said "Linux with an
          NVIDIA GPU. It appears here in a few seconds. Run it again to upgrade."
          Three fragments with no subject between them: the reader has to work
          out what appears, what to run again, and whether the first line is a
          requirement or a description. */}
      <p className="text-sm text-muted-foreground">
        This installs everything the machine needs, and the machine then adds itself. It should
        appear in this list within a few seconds. To update it later, run the same command again:
        its models and its settings are kept.
      </p>
      <p className="text-sm text-muted-foreground">
        The machine must be running Linux, and needs an NVIDIA graphics card unless you tell it to
        use the processor instead.
      </p>
      {/* Up here and not in the options list, because it is the one number
          somebody has to act on BEFORE the machine will work, and the action is
          on a different screen from this one. It read as a fact about the
          software; it is a thing to go and do. */}
      <p className="text-sm text-muted-foreground">
        The machine listens on port{' '}
        <strong className="font-semibold text-foreground">19443</strong>. Open it on the machine's
        firewall, or this server will not be able to reach it once it is installed.
      </p>

      {/* Two columns, and they line up. These were a disclosure somebody had to
          open, holding rows that wrapped a flag and its reason into one ragged
          line; a reader scans a column far faster than they decide whether to
          open a triangle. */}
      <div className="border-t border-border pt-4">
        <p className="mb-3 text-xs text-muted-foreground">
          Optional. Add to the end of the command only if one of these applies.
        </p>
        <dl className="grid grid-cols-[max-content_1fr] items-baseline gap-x-6 gap-y-3">
          {[
            [
              '--accel cpu',
              'Use the processor instead of a graphics card. This works, and it is slower.',
            ],
            [
              '--data /big/disk',
              'Keep downloaded models somewhere other than the default place. Point this at the big disk.',
            ],
            [
              '--port <number>',
              'Listen on a port other than 19443. Use this if something else on the machine is already using that one.',
            ],
            [
              '--advertise https://gpu1.example.com:9000',
              'The address to reach the machine at. This is worked out on its own, from the address the machine connects from and the port it says it listens on, so it is needed only when neither of those is right: a port forward that changes the port, or a proxy in front of the machine.',
            ],
          ].map(([flag, why]) => (
            <Fragment key={flag}>
              <dt className="font-mono text-xs">{flag}</dt>
              <dd className="text-sm text-muted-foreground">{why}</dd>
            </Fragment>
          ))}
        </dl>
      </div>

      {/* The one question this dialog cannot answer, and where the answer is. */}
      <p className="border-t border-border pt-4 text-sm text-muted-foreground">
        If the machine does not appear here, run{' '}
        <code className="font-mono text-xs">journalctl -u sag-inference -f</code> on it to see what
        went wrong.
      </p>
    </Modal>
  )
}

/** A block of text whose whole purpose is to be copied. */
function Paste({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)

  // The button is ABOVE the block, not floating on it. It used to be absolutely
  // positioned in the corner of a box that scrolls sideways, so a long command,
  // which is the only kind anybody pastes, ran underneath it and the two were
  // unreadable together.
  //
  // And the command wraps rather than scrolls: somebody about to run a thing as
  // root on their server should be able to see all of it first. It wraps at
  // words (`break-words`) and not at any character (`break-all`), so the only
  // thing ever broken mid-word is the token, which has no words in it.
  return (
    <div className="rounded-md border border-border">
      <div className="flex items-center justify-between border-b border-border px-3 py-1.5">
        <span className="text-xs text-muted-foreground">Run this on the machine you are adding, as root</span>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={async () => {
            await navigator.clipboard.writeText(text)
            setCopied(true)
            window.setTimeout(() => setCopied(false), 2000)
          }}
        >
          {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </div>
      <pre className="whitespace-pre-wrap break-words px-3 py-2.5 font-mono text-xs leading-relaxed">
        {text}
      </pre>
    </div>
  )
}

/* --- one machine ----------------------------------------------------------- */

function MachineScreen({ machineID, onBack }: { machineID: number; onBack: (() => void) | null }) {
  const { data, error, reload } = useResource(() => api.nodes.get(machineID))
  const machine = data?.machine
  const [adding, setAdding] = useState(false)
  const [settings, setSettings] = useState<NodeModel | null>(null)
  const [sharing, setSharing] = useState<NodeModel | null>(null)
  const notify = useNotify()

  const running = (machine?.pulls ?? []).some(
    (p) => p.state === 'downloading' || p.state === 'verifying',
  )
  const waking = (machine?.models ?? []).some((m) => m.residency === 'waking')
  // Kept asking while something is moving, and left alone when nothing is. A
  // screen that polls forever is a screen that keeps a laptop awake.
  usePolling(reload, running || waking)

  return (
    <Page
      // The SAME title as the list, and it never changes.
      //
      // It used to be the machine's name, which made the header rewrite itself
      // twice on the way in: the list renders while it is asking, headed
      // "Inference"; the answer comes back with one machine, so the list is
      // skipped and this screen takes over, headed "This computer". Two
      // different words in the same place, half a second apart, and the eye
      // reads a page that changed its mind.
      //
      // Which machine is CONTENT, and it is below. It is also the wrong thing
      // to put in the title on a personal installation, where the answer is
      // always "this one".
      title="Inference"
      // One line from the first render, for the same reason. The header is a
      // fixed height with its contents centred, so a description that arrives
      // late takes the block from one line to two and pushes the title up.
      description={INFERENCE}
      actions={
        <div className="flex gap-2">
          {onBack && (
            <Button size="sm" variant="outline" onClick={onBack}>
              <ChevronLeft className="size-4" />
              All machines
            </Button>
          )}
          {/* Gated on being able to ASK it, not merely on it being reachable:
              a machine whose model list could not be read cannot answer a
              search either, so the button opened a dialog that then failed. */}
          <Button
            size="sm"
            onClick={() => setAdding(true)}
            disabled={!machine?.reachable || !machine?.models}
          >
            <Plus className="size-4" />
            Add a model
          </Button>
        </div>
      }
    >
      {error && <p className="text-sm text-destructive">{error}</p>}
      {/* Which machine, and where, for a machine that is somewhere else.
          Nothing at all for the computer this is running on. Its name is a
          label we wrote and its address is a loopback port that changes with
          every launch: two lines saying "here", above a screen that is about
          here. Decided from the ADDRESS rather than from the edition, because
          it is a fact about the machine: a deployment can be pointed at its own
          loopback too, and would want the same silence. */}
      {machine && !isThisComputer(machine.base_url) && (
        <div className="flex items-baseline gap-2 text-sm">
          <span className="font-medium">{machine.name}</span>
          <span className="text-xs text-muted-foreground">{machine.base_url}</span>
        </div>
      )}
      {/* The credential for gated weights, HERE rather than on the list.
          It is where the problem is met: somebody searches from this screen, is
          told a model is gated, and the way out has to be within reach of that
          sentence. It was on the list, which a personal installation never sees,
          because a list of one machine is skipped: so the one edition most
          likely to want a Meta model had no way to enter a token at all. */}
      <ModelLibraryCredential />
      {/* Waiting is a state, and it has to look like one.
          This screen begins by asking the machine whether it is there, and a
          machine that has gone away does not refuse, it says nothing: the
          question runs to its ceiling. Rendering nothing meanwhile is what
          reads as a blank page and gets reported as a broken screen, when what
          is actually happening is a wait for a box that is never going to
          answer. */}
      {!machine && !error && (
        <p className="text-sm text-muted-foreground">Asking this machine how it is…</p>
      )}
      {/* Whenever there IS one, not only when the machine is unreachable.
          A machine can answer the first question and fail the next: /node
          replies, /node/models does not, and the answer then carries
          reachable=true WITH a problem set. Printing it only in the unreachable
          branch dropped the reason on the floor, so the machine read as ready
          while its model list could not be fetched, which is an ordinary state
          while an installation is still coming up. */}
      {machine && (!machine.reachable || machine.problem) && (
        <p className="text-sm text-destructive">{machine.problem || 'This machine did not answer.'}</p>
      )}

      {machine?.info && <MachineFacts machine={machine} />}

      {/* Only what is still in flight or still needs a decision.
          A download that FINISHED has become a row in the table below, and
          showing it here as well says the same thing twice. Worse, the node
          keeps a pull for an hour after it ends (KB/35, deliberately, so a
          screen reopened later still shows what happened), so a `ready` line
          outlived the model it produced: delete the model and the download sat
          there claiming to be ready, describing something that no longer
          existed, with no button on it to dismiss it because a finished pull
          has none. Failed and cancelled ones stay: those have Resume and
          Delete, and they are the ones somebody still has to do something
          about. */}
      {unfinished(machine?.pulls).length > 0 && (
        <Downloads
          pulls={unfinished(machine?.pulls)}
          onCancel={async (pull) => {
            await api.nodes.cancelPull(machineID, pull.id)
            await reload()
          }}
          onResume={async (pull) => {
            await api.nodes.resumePull(machineID, pull.id)
            await reload()
          }}
          onDelete={async (pull) => {
            await api.nodes.deletePull(machineID, pull.id)
            await reload()
          }}
        />
      )}

      <DataTable
        // Keyed by the uid the machine minted, which is the one thing about a
        // model that is unique and never changes.
        //
        // `models?.map` and not `models.map`. The optional chain used to stop at
        // `machine`, and a machine that is DOWN is exactly the case where the
        // answer arrives with `models` null: we could not ask it what it holds.
        // So the one screen somebody opens to find out why a machine is down
        // threw, React unmounted the tree, and they got a blank page instead of
        // the sentence saying it did not answer.
        //
        // And `?? []` and not `?? null` once the machine itself has arrived.
        // Null means ONE thing to DataTable, "still loading", and it drew a
        // spinner that never stopped: the person saw "This machine did not
        // answer" with something turning underneath it for ever. That is the
        // same collapse of two states into one value as the blank page a
        // function above, and the empty line below says which of them this is.
        items={machine ? (machine.models?.map((m) => ({ ...m, id: m.uid })) ?? []) : null}
        empty={
          machine && !machine.models
            ? 'This machine could not be asked what it holds.'
            : 'Nothing on this machine yet.'
        }
        onEdit={(m) => setSettings(m)}
        remove={{
          run: async (m) => {
            await api.nodes.remove(machineID, m.uid)
            await reload()
          },
          confirm: (m) =>
            `Delete "${m.handle}"? Its ${bytes(m.facts.size_bytes)} of weights go from this machine, and every workspace using it loses it.`,
          done: (m) => `${m.handle} was deleted.`,
        }}
        columns={[
          {
            header: 'Model',
            cell: (m) => (
              <div>
                <div className="font-medium">{m.handle}</div>
                <div className="font-mono text-xs text-muted-foreground">{m.repo}</div>
              </div>
            ),
          },
          { header: 'Kind', cell: (m) => <Badge>{m.kind}</Badge> },
          {
            header: 'Size',
            cell: (m) => (
              <span className="text-sm text-muted-foreground">
                {bytes(m.facts.size_bytes)}
                {m.facts.context_length
                  ? ` · ${m.facts.context_length.toLocaleString()} tokens`
                  : ''}
              </span>
            ),
          },
          {
            header: 'In memory',
            cell: (m) => (
              <ResidencySwitch
                model={m}
                onChange={async (want) => {
                  const call = want ? api.nodes.load : api.nodes.unload
                  await call(machineID, m.uid)
                  await reload()
                  notify.success(want ? `${m.handle} is loading.` : `${m.handle} was released.`)
                }}
              />
            ),
          },
          {
            header: 'Used by',
            // The weights sit on one disk; what varies is who may route to them,
            // and there is nowhere else this is visible.
            cell: (m) => (
              <button
                type="button"
                className="cursor-pointer text-left"
                onClick={() => setSharing(m)}
              >
                {(m.workspaces?.length ?? 0) === 0 ? (
                  <Badge tone="warn">no workspace yet</Badge>
                ) : (
                  <span className="text-sm hover:underline">
                    {(m.workspaces ?? []).map((w) => w.name).join(', ')}
                  </span>
                )}
              </button>
            ),
          },
        ]}
      />

      {adding && machine && (
        <AddModelModal
          machineID={machineID}
          onClose={() => setAdding(false)}
          onStarted={async () => {
            setAdding(false)
            await reload()
          }}
        />
      )}
      {settings && (
        <ModelSettingsModal
          machineID={machineID}
          model={settings}
          onClose={() => setSettings(null)}
          onSaved={async () => {
            setSettings(null)
            await reload()
          }}
        />
      )}
      {sharing && (
        <ShareModal
          machineID={machineID}
          model={sharing}
          workspaces={data?.workspaces ?? []}
          onClose={() => setSharing(null)}
          onSaved={async () => {
            setSharing(null)
            await reload()
          }}
        />
      )}
    </Page>
  )
}

/** What the machine is, read off the machine. Facts, so they are flat text. */
function MachineFacts({ machine }: { machine: NodeDetail }) {
  const m = machine.info!.machine
  const facts: [string, string][] = [
    ['Memory', `${bytes(m.memory_free)} free of ${bytes(m.memory_total)}`],
    ['Disk', `${bytes(m.disk_free)} free of ${bytes(m.disk_total)}`],
    ['Processors', String(m.processors)],
    ['Up', duration(machine.info!.uptime_seconds)],
  ]
  return (
    <dl className="grid grid-cols-2 gap-x-8 gap-y-2 pt-2 pb-4 sm:grid-cols-4">
      {facts.map(([label, value]) => (
        <div key={label}>
          <dt className="text-xs text-muted-foreground">{label}</dt>
          <dd className="text-sm">{value}</dd>
        </div>
      ))}
    </dl>
  )
}

/**
 * The downloads worth showing: everything except the ones that finished well.
 *
 * `ready` is the one state with nothing left to do and nothing to press, so it
 * is the one state that does not belong in a list of downloads.
 */
function unfinished(pulls: NodePull[] | null | undefined): NodePull[] {
  return (pulls ?? []).filter((p) => p.state !== 'ready')
}

/** Anything arriving, with how far it has got. */
function Downloads({
  pulls,
  onCancel,
  onResume,
  onDelete,
}: {
  pulls: NodePull[]
  onCancel: (p: NodePull) => Promise<void>
  onResume: (p: NodePull) => Promise<void>
  onDelete: (p: NodePull) => Promise<void>
}) {
  return (
    <div className="space-y-3 pb-4">
      {pulls.map((pull) => {
        const done = pull.bytes_total > 0 ? Math.min(1, pull.bytes_done / pull.bytes_total) : 0
        const moving = pull.state === 'downloading' || pull.state === 'verifying'
        return (
          <div key={pull.id}>
            <div className="flex items-baseline justify-between gap-4">
              <span className="text-sm font-medium">{pull.repo}</span>
              <span className="text-xs text-muted-foreground">
                {pull.state === 'downloading'
                  ? `${bytes(pull.bytes_done)} of ${bytes(pull.bytes_total)} · ${pull.files_done}/${pull.files_total} files`
                  : pull.state === 'verifying'
                    ? 'checking what arrived'
                    : pull.state === 'ready'
                      ? 'ready'
                      : (pull.error ?? pull.state)}
              </span>
              {moving ? (
                <Button type="button" variant="ghost" size="sm" onClick={() => void onCancel(pull)}>
                  Stop
                </Button>
              ) : (
                // A stopped download still has its bytes, so there is something
                // to carry on with. Deleting is what throws them away, and is
                // the only thing here that does.
                pull.state !== 'ready' && (
                  <span className="flex shrink-0 gap-1">
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() => void onResume(pull)}
                    >
                      <RotateCw className="size-4" />
                      Resume
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() => void onDelete(pull)}
                    >
                      <Trash2 className="size-4" />
                      Delete
                    </Button>
                  </span>
                )
              )}
            </div>
            <div className="mt-1 h-1 w-full overflow-hidden rounded-full bg-muted">
              <div
                className={
                  pull.state === 'failed'
                    ? 'h-full bg-destructive'
                    : 'h-full bg-primary transition-[width]'
                }
                style={{ width: `${Math.round((pull.state === 'ready' ? 1 : done) * 100)}%` }}
              />
            </div>
          </div>
        )
      })}
    </div>
  )
}


/**
 * The one credential for weights published under terms somebody accepted.
 *
 * It lives HERE, on the machines screen, because this is where a person meets
 * the problem: they search for a model, are told it is gated, and the way out
 * has to be within reach of that sentence rather than in a settings page
 * somebody has to be told exists. The refusal used to name a screen that had
 * not been built, which is worse than saying nothing.
 *
 * It is one line until it is needed. Most installations only ever fetch open
 * weights and should not have a credential form in their face; the summary says
 * whether there is one, and the form is a click away.
 */
/**
 * What the library credential last answered, kept outside the component.
 *
 * The section is on two screens, and `useResource` holds its answer in the
 * component: every mount starts at nothing and asks again. So walking into a
 * machine and back out asked twice more and blanked the section each time,
 * which is a part of the page rebuilding itself for a value that had not
 * changed and belongs to the whole installation rather than to either screen.
 *
 * One value for the process, so the second mount draws what the first learned
 * and the request behind it is only ever a revalidation.
 */
let lastKnownCredential: LibraryCredential | null = null

function ModelLibraryCredential() {
  const notify = useNotify()
  const { data, error, reload } = useResource(() => api.nodes.libraryCredential())
  const [open, setOpen] = useState(false)
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)

  // A machine screen that cannot read this is still a usable machine screen:
  // nothing here is required to list or run a model, so a failure reads as
  // "there is none" rather than taking the section away or reporting an error
  // about a credential most installations do not have.
  //
  // The shared hook rather than an effect of our own, which is what this was:
  // an effect whose body calls setState is a cascading render the linter is
  // right to refuse, and `useResource` already does this correctly once.
  // Rendered from the first frame, ASKED or not.
  //
  // It used to return null until its own request answered and then insert
  // ninety pixels, which pushed the table on one screen and the machine's own
  // figures on the other: a page that settles by moving everything down. Both
  // states of it are the same shape now, so what arrives changes the words and
  // not the layout.
  useEffect(() => {
    if (data) lastKnownCredential = data
  }, [data])

  const state: LibraryCredential | null =
    data ?? lastKnownCredential ?? (error ? { configured: false } : null)

  async function save() {
    setBusy(true)
    try {
      await api.nodes.setLibraryCredential(token)
      // What the next mount will draw, updated with it: the remembered answer
      // is what makes walking away and back instant, so it has to be right.
      lastKnownCredential = { configured: true }
      await reload()
      setToken('')
      setOpen(false)
      notify.success('The model library credential was saved.')
    } catch (failure) {
      notify.error(failure instanceof Error ? failure.message : 'That did not work.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mb-4 border-b border-border pt-3 pb-4 text-sm">
      <div className="flex items-center justify-between gap-4">
        <div>
          <div className="font-medium">Model library</div>
          {/* Two lines high whichever sentence is in it, because the shorter of
              them arriving would otherwise pull the page up by one line. */}
          <p className="min-h-10 text-muted-foreground">
            {!state
              ? null
              : state.configured
                ? 'An access token is saved, so models published under a licence can be downloaded.'
                : 'Some models can only be downloaded by someone who has accepted their licence. Save an access token here and every machine can fetch them.'}
          </p>
        </div>
        {state && (
          <Button size="sm" variant="outline" onClick={() => setOpen((o) => !o)}>
            {state.configured ? 'Replace' : 'Add a token'}
          </Button>
        )}
      </div>

      {open && (
        <div className="mt-3 space-y-2">
          {/* The two steps, in order, with the addresses. A person who has not
              done this before cannot be expected to know either of them, and
              the refusal that sends them here says the same thing. */}
          <ol className="list-decimal space-y-1 pl-5 text-muted-foreground">
            <li>
              Open the model&apos;s page on{' '}
              <a
                className="underline"
                href="https://huggingface.co"
                target="_blank"
                rel="noreferrer"
              >
                huggingface.co
              </a>{' '}
              and accept its licence, signed in with your own account. Nobody can accept it for
              you.
            </li>
            <li>
              Create an access token at{' '}
              <a
                className="underline"
                href="https://huggingface.co/settings/tokens"
                target="_blank"
                rel="noreferrer"
              >
                huggingface.co/settings/tokens
              </a>{' '}
              and paste it below.
            </li>
          </ol>
          <div className="flex items-center gap-2">
            <Input
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="Access token"
              className="max-w-md"
            />
            <Button size="sm" onClick={() => void save()} disabled={busy || !token.trim()}>
              Save
            </Button>
            {state?.configured && (
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                onClick={async () => {
                  await api.nodes.clearLibraryCredential()
                  await reload()
                  notify.success('The model library credential was removed.')
                }}
              >
                Remove
              </Button>
            )}
          </div>
          {/* Said out loud, because a credential people cannot see the fate of
              is a credential they are right to be wary of typing. */}
          <p className="text-xs text-muted-foreground">
            Stored encrypted. It is never shown again and no screen can read it back.
          </p>
        </div>
      )}
    </div>
  )
}

/**
 * Whether a model is kept in memory.
 *
 * Three states, not two, because loading is minutes and a control that flipped
 * straight to "in memory" would be lying for all of them. What it shows is where
 * the model IS, not what was clicked.
 */
function ResidencySwitch({
  model,
  onChange,
}: {
  model: NodeModel
  onChange: (want: boolean) => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const notify = useNotify()

  if (model.residency === 'waking') {
    return <Badge tone="warn">loading…</Badge>
  }
  const resident = model.residency === 'resident'

  return (
    <div className="flex items-center gap-2">
      <Button
        type="button"
        size="sm"
        variant={resident ? 'outline' : 'ghost'}
        disabled={busy}
        onClick={async () => {
          setBusy(true)
          try {
            await onChange(!resident)
          } catch (failure) {
            // Said out loud, and the control stays where it was. Somebody who
            // released a model and was not told it failed believes the memory
            // is free.
            notify.error(failure instanceof Error ? failure.message : 'That did not work.')
          } finally {
            setBusy(false)
          }
        }}
      >
        {resident ? 'Release' : 'Load'}
      </Button>
      {/* The decision and the reality disagree while a machine is coming back
          up, or when a load failed. Showing only one of them hides the fault.

          And WHY, when the machine said why. The badge alone is the same red
          chip whether the machine ran out of memory or the engine cannot load
          that architecture at all, and those are the same picture with
          completely different answers: wait, versus this will never work.
          Somebody lost an evening to that. */}
      {model.resident && !resident && (
        <span className="flex items-center gap-2">
          <Badge tone="bad">wanted, not loaded</Badge>
          {model.load_error && (
            <span className="text-xs text-muted-foreground">{model.load_error}</span>
          )}
        </span>
      )}
      {resident && <Badge tone="good">in memory</Badge>}
    </div>
  )
}

/* --- who may use a model ---------------------------------------------------- */

/**
 * Which workspaces may route to a model.
 *
 * The whole list is sent, not a change to it: a set is easier to be sure about
 * than a pair of add and remove calls that can arrive in either order.
 */
function ShareModal({
  machineID,
  model,
  workspaces,
  onClose,
  onSaved,
}: {
  machineID: number
  model: NodeModel
  workspaces: WorkspaceChoice[]
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [chosen, setChosen] = useState<number[]>((model.workspaces ?? []).map((w) => w.workspace_id))
  const [busy, setBusy] = useState(false)
  const notify = useNotify()

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Who may use ${model.handle}`}
      submitLabel="Save"
      submitting={busy}
      onSubmit={async () => {
        setBusy(true)
        try {
          await api.nodes.share(machineID, model.uid, chosen)
          notify.success(
            chosen.length === 0
              ? `${model.handle} is no longer available to any workspace.`
              : `${model.handle} is available to ${chosen.length} workspace${chosen.length === 1 ? '' : 's'}.`,
          )
          await onSaved()
        } catch (failure) {
          notify.error(failure instanceof Error ? failure.message : 'That did not save.')
        } finally {
          setBusy(false)
        }
      }}
    >
      <p className="text-sm text-muted-foreground">
        The weights stay where they are. A workspace added here gets a route to the copy already on
        this machine, and nothing is downloaded again.
      </p>
      <div className="space-y-1">
        {workspaces.map((ws) => (
          <CheckboxField
            key={ws.id}
            label={ws.name}
            checked={chosen.includes(ws.id)}
            onChange={(on) =>
              setChosen((prev) => (on ? [...prev, ws.id] : prev.filter((id) => id !== ws.id)))
            }
          />
        ))}
      </div>
    </Modal>
  )
}

/* --- adding a model -------------------------------------------------------- */

/**
 * Search, then confirm, then start.
 *
 * The confirmation is drawn from what the MACHINE said about the exact commit:
 * the files it would fetch, what they weigh, and whether there is room, memory
 * and permission for them. Approval must equal success (CLAUDE.md), so nothing
 * can be started from the search results alone.
 */
function AddModelModal({
  machineID,
  onClose,
  onStarted,
}: {
  machineID: number
  onClose: () => void
  onStarted: () => Promise<void>
}) {
  const [query, setQuery] = useState('')
  const [hits, setHits] = useState<LibraryHit[] | null>(null)
  const [chosen, setChosen] = useState<LibraryModel | null>(null)
  const [problem, setProblem] = useState('')
  const [busy, setBusy] = useState(false)
  const notify = useNotify()

  async function run<T>(work: () => Promise<T>): Promise<T | null> {
    setBusy(true)
    setProblem('')
    try {
      return await work()
    } catch (failure) {
      setProblem(failure instanceof Error ? failure.message : 'That did not work.')
      return null
    } finally {
      setBusy(false)
    }
  }

  const blocked =
    chosen &&
    (!chosen.storage.ok || !chosen.access.ok || !chosen.runtime.ok || !!chosen.already_here)

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add a model"
      wide
      submitLabel={chosen ? 'Download it' : 'Search'}
      submitting={busy}
      onSubmit={async () => {
        // Before a model is chosen the form's action IS the search, so pressing
        // return in the box does the obvious thing rather than nothing.
        if (!chosen) {
          await run(async () => setHits((await api.nodes.search(machineID, query)).models))
          return
        }
        const started = await run(() =>
          api.nodes.pull(machineID, { repo: chosen.repo, revision: chosen.revision }),
        )
        if (started) {
          notify.success(`${chosen.name} is downloading.`)
          await onStarted()
        }
      }}
      submitDisabled={chosen ? !!blocked : !query.trim()}
    >
      {problem && <p className="text-sm text-destructive">{problem}</p>}

      {!chosen ? (
        <>
          {/* No button beside the box. The dialog already has one action and
              before a model is chosen that action IS the search. */}
          <Field
            label="Find a model"
            hint="Searched in the public model library, by the machine itself."
          >
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Qwen3-0.6B"
            />
          </Field>

          {hits?.length === 0 && <p className="text-sm text-muted-foreground">Nothing matched that.</p>}
          {hits && hits.length > 0 && (
            <ul className="-mx-5 -mb-4 mt-0! divide-y divide-border">
              {hits.map((hit) => (
                <li key={hit.repo}>
                  <button
                    type="button"
                    className="flex w-full cursor-pointer items-center justify-between gap-4 px-5 py-2.5 text-left leading-5 hover:bg-muted/50"
                    onClick={() =>
                      void run(async () => setChosen(await api.nodes.describe(machineID, hit.repo)))
                    }
                  >
                    <span>
                      <span className="text-sm/5 font-medium">{hit.name}</span>
                      <span className="ml-2 font-mono text-xs/5 text-muted-foreground">
                        {hit.repo}
                      </span>
                    </span>
                    <span className="shrink-0 text-xs/5 text-muted-foreground">
                      {hit.kind} · {hit.downloads.toLocaleString()} downloads
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        <Confirmation
          model={chosen}
          onBack={() => setChosen(null)}
          onChoose={(file) =>
            void run(async () => setChosen(await api.nodes.describe(machineID, chosen.repo, file)))
          }
        />
      )}
    </Modal>
  )
}

/** Exactly what would happen, before anybody agrees to it. */
function Confirmation({
  model,
  onBack,
  onChoose,
}: {
  model: LibraryModel
  onBack: () => void
  onChoose: (file: string) => void
}) {
  const facts: [string, string][] = [
    ['From', model.repo],
    ['Version', model.revision.slice(0, 12)],
    ['Kind', model.kind],
    ['Size', `${bytes(model.size_bytes)} in ${model.files.length} files`],
  ]
  if (model.license) facts.push(['Licence', model.license])
  // Only when it is the thing being refused on. It is the name the weights give
  // themselves, which is precise and is not language anybody speaks, so it earns
  // a row exactly when somebody has been told no and wants to know what was
  // read, and clutters the panel every other time.
  if (!model.runtime.ok && model.architecture) facts.push(['Built as', model.architecture])

  // Every reason this cannot go ahead, in the machine's own words: it is the
  // only side that knows how much room it has.
  //
  // Runtime comes first of them, because it is the only one that cannot be
  // resolved: room can be made and credentials can be added, but a model this
  // machine has no way to execute is not going to become loadable by clearing a
  // disk, and saying so first stops somebody acting on the other two.
  const refusals = [
    // Nothing to fetch means the machine answered with an ambiguity rather than
    // a plan: several compression levels and none named. The list of them is
    // below, so this says what to do rather than only what is wrong.
    model.files.length === 0 &&
      `This model is published at ${model.choices.length} compression levels. Choose one below.`,
    model.already_here && 'This machine already has this model at this version.',
    !model.runtime.ok && model.runtime.reason,
    !model.access.ok && model.access.reason,
    !model.storage.ok && model.storage.reason,
  ].filter(Boolean) as string[]

  return (
    <>
      <Button type="button" variant="ghost" size="sm" onClick={onBack} className="self-start">
        <ChevronLeft className="size-4" />
        Search again
      </Button>

      <dl className="grid grid-cols-2 gap-x-8 gap-y-2">
        {facts.map(([label, value]) => (
          <div key={label}>
            <dt className="text-xs text-muted-foreground">{label}</dt>
            <dd className="font-mono text-sm">{value}</dd>
          </div>
        ))}
      </dl>

      {refusals.map((reason) => (
        <p key={reason} className="text-sm text-destructive">
          {reason}
        </p>
      ))}
      {/* Memory is a warning and not a refusal: how much a model really needs
          depends on how it is compressed on load, which nothing knows yet. */}
      {refusals.length === 0 && !model.memory.ok && (
        <p className="text-sm text-amber-600 dark:text-amber-500">
          {model.memory.reason} It will download, and it may not load.
        </p>
      )}
      {/* The choice, offered, rather than a sentence about a choice.
          A repository routinely publishes one model at twenty or thirty
          compression levels, and the machine will not pick for somebody: how
          much quality to trade for how much memory is their decision. It used
          to say exactly that and stop there, with nothing to click, so a whole
          class of model was a wall with an explanation on it. The sizes are
          what the decision is actually made on. */}
      {model.choices.length > 1 && (
        <div className="space-y-1.5">
          <p className="text-sm text-muted-foreground">
            Published at {model.choices.length} compression levels. Smaller is faster and needs less
            memory; larger keeps more of the model.
          </p>
          <ul className="max-h-56 divide-y divide-border overflow-y-auto rounded-md border border-border">
            {model.choices.map((choice) => (
              <li key={choice.path}>
                <button
                  type="button"
                  className="flex w-full cursor-pointer items-center justify-between gap-4 px-3 py-2 text-left hover:bg-muted/50"
                  onClick={() => onChoose(choice.path)}
                >
                  <span className="font-mono text-xs">{choice.path}</span>
                  <span className="shrink-0 text-xs text-muted-foreground">
                    {bytes(choice.size)}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
      <p className="flex items-center gap-2 text-sm text-muted-foreground">
        <Download className="size-4" />
        The download runs on the machine. Closing this does not stop it.
      </p>
    </>
  )
}

/* --- a model's settings ---------------------------------------------------- */

/**
 * The settings a machine declares for one of its models.
 *
 * The console does not know what any of them are: it asks for the form, draws
 * whatever it is handed, and sends the values back. Third thing in this product
 * to do that, which is why the renderer is shared.
 */
function ModelSettingsModal({
  machineID,
  model,
  onClose,
  onSaved,
}: {
  machineID: number
  model: NodeModel
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [tab, setTab] = useState(0)
  const [busy, setBusy] = useState(false)
  const notify = useNotify()

  // ONE request when it opens: the form and what is in it are one question.
  const { data } = useResource(() => api.nodes.form(machineID, model.uid))

  // Only the EDITS are state. What arrived is what arrived, so copying it into
  // state in an effect would be a second copy of a thing that cannot change.
  const [edits, setEdits] = useState<Record<string, string>>({})
  const sections: TemplateSection[] | null = data?.sections ?? null
  const values = { ...(data?.values ?? {}), ...edits }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={model.handle}
      wide
      loading={!sections}
      submitLabel="Save"
      submitting={busy}
      onSubmit={async () => {
        setBusy(true)
        try {
          await api.nodes.saveSettings(machineID, model.uid, values)
          notify.success(
            model.residency === 'resident'
              ? `${model.handle} was saved. Release and load it for the change to take effect.`
              : `${model.handle} was saved.`,
          )
          await onSaved()
        } catch (failure) {
          notify.error(failure instanceof Error ? failure.message : 'That did not save.')
        } finally {
          setBusy(false)
        }
      }}
    >
      <Facts model={model} />
      {sections && (
        <SettingsSections
          sections={sections}
          active={tab}
          onSelect={setTab}
          values={values}
          onChange={(key, v) => setEdits((prev) => ({ ...prev, [key]: v }))}
        />
      )}
    </Modal>
  )
}

/**
 * What was read off the weights, shown and not editable.
 *
 * A model's architecture, size and licence are properties of what was
 * downloaded. An input around one invites somebody to change something that
 * cannot be changed.
 */
function Facts({ model }: { model: NodeModel }) {
  const facts: [string, string][] = [
    ['From', model.repo],
    ['Version', model.revision.slice(0, 12)],
  ]
  if (model.facts.architecture) facts.push(['Architecture', model.facts.architecture])
  if (model.facts.context_length)
    facts.push(['Context', `${model.facts.context_length.toLocaleString()} tokens`])
  if (model.facts.license) facts.push(['Licence', model.facts.license])
  facts.push(['Size', bytes(model.facts.size_bytes)])

  return (
    <dl className="grid grid-cols-3 gap-x-8 gap-y-2 border-b border-border pb-4">
      {facts.map(([label, value]) => (
        <div key={label}>
          <dt className="text-xs text-muted-foreground">{label}</dt>
          <dd className="truncate font-mono text-sm" title={value}>
            {value}
          </dd>
        </div>
      ))}
    </dl>
  )
}

/* --- helpers --------------------------------------------------------------- */

/** How often the screen asks again while something is moving. */
const POLL_MS = 2000

/**
 * Ask again on an interval, but only while there is something to see.
 *
 * The callback is held in a ref so that changing it does not restart the clock:
 * `reload` is rebuilt on every render, and depending on it directly would clear
 * and re-arm the timer forever without it ever firing.
 */
function usePolling(reload: () => Promise<void>, active: boolean) {
  const latest = useRef(reload)
  useEffect(() => {
    latest.current = reload
  })

  useEffect(() => {
    if (!active) return
    const timer = setInterval(() => void latest.current(), POLL_MS)
    return () => clearInterval(timer)
  }, [active])
}

/**
 * A machine whose certificate is running out.
 *
 * It should never happen: a machine renews by checking back in, daily, and one
 * that is answering is one that is checking in. So this is not a countdown to
 * put on the screen, it is the symptom of a machine that has stopped renewing
 * while still answering, and it is shown only once there is something to do
 * about it.
 */
function expiring(m: NodeSummary): string | null {
  if (!m.cert_expires_at) return null
  // Rounded up, the way a deadline is read: something that runs out in twenty
  // hours has "a day", not "zero days".
  const days = Math.ceil((Date.parse(m.cert_expires_at) - Date.now()) / 86_400_000)
  if (days > 14) return null
  if (days <= 0) return 'its certificate has expired'
  return `its certificate expires in ${days} ${days === 1 ? 'day' : 'days'}`
}

/** Bytes as somebody would say them. */
function bytes(n: number): string {
  const units: [string, number][] = [
    ['TB', 1e12],
    ['GB', 1e9],
    ['MB', 1e6],
    ['kB', 1e3],
  ]
  for (const [unit, scale] of units) {
    if (n >= scale) return `${(n / scale).toFixed(1)} ${unit}`
  }
  return `${n} bytes`
}

function duration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}
