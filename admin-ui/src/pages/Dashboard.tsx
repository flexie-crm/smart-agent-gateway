import { useEffect, useRef, useState } from "react";
import {
  Activity,
  AlertTriangle,
  Ban,
  Coins,
  Loader2,
  MessagesSquare,
  PauseCircle,
  RefreshCw,
  Users,
  Wrench,
} from "lucide-react";
import { Page } from "@/components/AppShell";
import { Button } from "@/components/ui/button";
import { apiFetch } from "@/lib/api";
import { useLiveDashboard, type LiveDashboard } from "@/lib/ws";
import { cn } from "@/lib/utils";

/**
 * What the gateway is doing, and what it has cost.
 *
 * Two kinds of number, and the dashboard says which is which:
 *
 *   - LIVE: the whole live block (connected, running, waiting) is one snapshot.
 *     It is LOADED once with the page, from /stats, so it is whole on first
 *     paint, and then kept current by the socket: the server pushes a fresh
 *     snapshot the instant a turn starts or ends, an approval is raised or
 *     answered, or a person connects or leaves. One source, one shape (KB/30).
 *   - LOADED: the usage and cost numbers are fetched once with the page and
 *     again when the person presses Refresh. Nothing polls: a dashboard left
 *     open must not drum on the server for numbers nobody is reading.
 *
 * A refusal is shown apart from a failure. A tool a person said no to is the
 * approval system working, and a dashboard that files it under "errors" teaches
 * people to ignore both columns.
 */

interface Stats {
  live: LiveDashboard;
  today: Usage;
  total: Usage;
  models: ModelUse[];
  tools: ToolUse[];
  configured: {
    vendors: number;
    models: number;
    agents: number;
    tools: number;
    workflows: number;
  };
}

interface Usage {
  runs: number;
  input_tokens: number;
  output_tokens: number;
  tool_calls: number;
  failed: number;
  refused: number;
}

interface ModelUse {
  model_id: number;
  name: string;
  vendor: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
}

interface ToolUse {
  name: string;
  friendly_name: string;
  calls: number;
  failed: number;
  refused: number;
}

async function fetchStats(): Promise<Stats> {
  const res = await apiFetch("/v1/stats");
  if (!res.ok) throw new Error("could not load");
  return (await res.json()) as Stats;
}

export function Dashboard() {
  const [stats, setStats] = useState<Stats | null>(null);
  // The live block: seeded from the /stats snapshot on load, then kept current
  // by the socket. connected, running, and waiting all update themselves the
  // instant they change; the usage numbers below are loaded, not pushed.
  const live = useLiveDashboard(stats?.live ?? null);
  const [error, setError] = useState("");
  // The Refresh button has two independent timers, decoupled from the fetch:
  // the icon spins for 2s (feedback even when the reply is instant), and the
  // button stays disabled for 5s (a rate limit, so it cannot be machine-gunned).
  const [spinning, setSpinning] = useState(false);
  const [cooling, setCooling] = useState(false);
  const refreshTimers = useRef<number[]>([]);
  useEffect(
    () => () => refreshTimers.current.forEach((id) => window.clearTimeout(id)),
    [],
  );

  // Loaded once with the page; a reply landing after the page is gone has
  // nobody to tell.
  useEffect(() => {
    let current = true;
    fetchStats()
      .then((body) => {
        if (!current) return;
        setStats(body);
        setError("");
      })
      .catch(() => {
        if (current) setError("The gateway did not answer.");
      });
    return () => {
      current = false;
    };
  }, []);

  // The Refresh button; the initial load already has the page loader. The spin
  // and the cooldown run on their own timers, not on the fetch: a fast reply
  // still spins for 2s, and the button cannot be clicked again for 5s. The fetch
  // itself just updates the numbers whenever it lands.
  const refresh = () => {
    if (cooling) return;
    setSpinning(true);
    setCooling(true);
    // The previous pair has already fired (cooldown is longer than the spin), so
    // replacing is safe; the cleanup clears whichever is still pending on unmount.
    refreshTimers.current = [
      window.setTimeout(() => setSpinning(false), 2000),
      window.setTimeout(() => setCooling(false), 5000),
    ];
    fetchStats()
      .then((body) => {
        setStats(body);
        setError("");
      })
      .catch(() => setError("The gateway did not answer."));
  };

  const tokensToday = stats
    ? stats.today.input_tokens + stats.today.output_tokens
    : 0;
  // Average work per answer: a useful read on how heavy today's are, and a
  // guard against dividing by a day with nothing in it yet.
  const tokensPerTurn =
    stats && stats.today.runs > 0
      ? Math.round(tokensToday / stats.today.runs)
      : 0;
  const nothingConfigured =
    !!stats &&
    (stats.configured.vendors === 0 || stats.configured.models === 0);

  // ONE Page, always, and only its body waits for the numbers.
  //
  // There used to be three: a loading one with just the title, an error one the
  // same, and the real one with the description and the Refresh button. So
  // arriving on this screen painted a header, then swapped it for a taller one
  // the moment the fetch landed, and the subtitle and the button appeared out of
  // nowhere a beat after the page did. The title and the description are facts
  // about the SCREEN, known before anything is fetched; only the numbers are
  // loading, so only the numbers say so.
  return (
    <Page
      title="Dashboard"
      description="What the gateway is doing right now."
      actions={
        <Button
          size="sm"
          variant="outline"
          disabled={cooling || !stats}
          onClick={refresh}
        >
          <RefreshCw className={cn("size-4", spinning && "animate-spin")} />
          Refresh
        </Button>
      }
    >
      {error && !stats && <p className="text-sm text-destructive">{error}</p>}
      {!error && !stats && (
        <Loader2 className="size-4 animate-spin text-muted-foreground" />
      )}
      {stats && (
        <>
          {nothingConfigured && (
            <NothingConfigured configured={stats.configured} />
          )}

          <section className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {/* The three live tiles are one snapshot: seeded from /stats, then kept
            current by the socket (KB/30). All three update themselves. */}
            <Tile
              icon={Users}
              label="Connected now"
              value={live?.connected ?? 0}
              hint={`${live?.sessions ?? 0} open session${(live?.sessions ?? 0) === 1 ? "" : "s"}`}
              live
            />
            <Tile
              icon={Activity}
              label="Answering now"
              value={live?.running ?? 0}
              hint="Being written right now"
              live
            />
            <Tile
              icon={PauseCircle}
              label="Waiting on a person"
              value={live?.waiting_approval ?? 0}
              hint="Approvals not yet answered"
              live
            />
            <Tile
              icon={MessagesSquare}
              label="Answers today"
              value={stats.today.runs}
              hint={`${format(tokensPerTurn)} tokens per answer`}
            />
            <Tile
              icon={Wrench}
              label="Tool calls today"
              value={stats.today.tool_calls}
              hint="Tools the agent ran"
            />
            <Tile
              icon={Coins}
              label="Tokens today"
              value={tokensToday}
              hint={`${format(stats.today.input_tokens)} in / ${format(stats.today.output_tokens)} out`}
            />
            <Tile
              icon={AlertTriangle}
              label="Failed today"
              value={stats.today.failed}
              hint="Answers that could not be finished"
              alert
            />
            <Tile
              icon={Ban}
              label="Refused today"
              value={stats.today.refused}
              hint="Approvals a person declined"
            />
          </section>

          <div className="mt-8 grid gap-6 lg:grid-cols-2">
            <Panel
              title="Models"
              hint="Everything configured, whether or not it has run."
            >
              {stats.models.length === 0 ? (
                <Empty>No model is configured.</Empty>
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border text-left text-xs text-muted-foreground">
                      <th className="pb-2 font-medium">Model</th>
                      <th className="pb-2 text-right font-medium">Calls</th>
                      <th className="pb-2 text-right font-medium">Tokens</th>
                    </tr>
                  </thead>
                  <tbody>
                    {stats.models.map((m) => (
                      <tr
                        key={m.model_id}
                        className="border-b border-border/50 last:border-0"
                      >
                        <td className="py-2">
                          <span className="font-medium">{m.name}</span>
                          <span className="ml-2 text-xs text-muted-foreground">
                            {m.vendor}
                          </span>
                        </td>
                        <td className="py-2 text-right tabular-nums">
                          {format(m.calls)}
                        </td>
                        <td className="py-2 text-right tabular-nums text-muted-foreground">
                          {format(m.input_tokens + m.output_tokens)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Panel>

            <Panel
              title="Tools"
              hint="A refusal is a person saying no, not a failure."
            >
              {stats.tools.length === 0 ? (
                <Empty>No tool has been called yet.</Empty>
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border text-left text-xs text-muted-foreground">
                      <th className="pb-2 font-medium">Tool</th>
                      <th className="pb-2 text-right font-medium">Calls</th>
                      <th className="pb-2 text-right font-medium">Failed</th>
                      <th className="pb-2 text-right font-medium">Refused</th>
                    </tr>
                  </thead>
                  <tbody>
                    {stats.tools.map((t) => (
                      <tr
                        key={t.name}
                        className="border-b border-border/50 last:border-0"
                      >
                        <td className="py-2">{t.friendly_name || t.name}</td>
                        <td className="py-2 text-right tabular-nums">
                          {format(t.calls)}
                        </td>
                        <td
                          className={cn(
                            "py-2 text-right tabular-nums",
                            t.failed > 0
                              ? "text-destructive"
                              : "text-muted-foreground",
                          )}
                        >
                          {format(t.failed)}
                        </td>
                        <td className="py-2 text-right tabular-nums text-muted-foreground">
                          {format(t.refused)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Panel>
          </div>

          <p className="mt-8 text-xs text-muted-foreground">
            {format(stats.total.runs)} answers and{" "}
            {format(stats.total.input_tokens + stats.total.output_tokens)}{" "}
            tokens since this workspace was created.
          </p>
        </>
      )}
    </Page>
  );
}

function Tile({
  icon: Icon,
  label,
  value,
  hint,
  live,
  alert,
}: {
  icon: typeof Activity;
  label: string;
  value: number;
  hint: string;
  live?: boolean;
  // alert tints the number when it is a problem worth noticing (failed turns).
  // A refusal is not a problem, so it is never alerted.
  alert?: boolean;
}) {
  const alarmed = alert && value > 0;
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <div className="flex items-center justify-between">
        <p className="text-xs font-medium text-muted-foreground">{label}</p>
        <Icon
          className={cn(
            "size-4 text-muted-foreground",
            alarmed && "text-destructive",
          )}
        />
      </div>
      <p
        className={cn(
          "mt-2 flex items-baseline gap-2 text-2xl font-semibold tabular-nums",
          alarmed && "text-destructive",
        )}
      >
        {format(value)}
        {live && value > 0 && (
          <span className="relative flex size-2">
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary opacity-60" />
            <span className="relative inline-flex size-2 rounded-full bg-primary" />
          </span>
        )}
      </p>
      <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
    </div>
  );
}

function Panel({
  title,
  hint,
  children,
}: {
  title: string;
  hint: string;
  children: React.ReactNode;
}) {
  return (
    <section className="rounded-lg border border-border bg-card p-4">
      <div className="mb-3">
        <h2 className="text-sm font-semibold">{title}</h2>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </div>
      {children}
    </section>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <p className="py-6 text-center text-sm text-muted-foreground">{children}</p>
  );
}

/**
 * NothingConfigured says WHY the dashboard is empty, and what to do about it. A
 * blank screen that does not explain itself leaves somebody hunting through
 * menus for the reason.
 *
 * Two messages, not one sentence with a noun dropped into it. The template
 * version read "There is no a vendor configured, so no turn can run", which is
 * two mistakes in nine words: an article that survived the substitution, and
 * "turn", which is our word for a unit of work and means nothing to the person
 * reading it. What is missing at each step is a different thing to say, so it
 * is said differently.
 */
function NothingConfigured({
  configured,
}: {
  configured: Stats["configured"];
}) {
  const noVendor = configured.vendors === 0;
  return (
    <div className="mb-6 rounded-lg border border-border bg-muted/40 p-4">
      <p className="text-sm font-medium">
        {noVendor
          ? "Nothing is connected yet, so this workspace cannot answer anything."
          : "No model is switched on, so this workspace cannot answer anything."}
      </p>
      <p className="mt-1 text-sm text-muted-foreground">
        {noVendor
          ? "Connect a vendor under Vendors, then switch on the models you want to use. This page fills in as soon as people start asking things."
          : "A vendor is connected, but none of its models is available to use. Switch one on under Models and this page will start filling in."}
      </p>
    </div>
  );
}

const formatter = new Intl.NumberFormat();
function format(value: number): string {
  return formatter.format(value);
}
