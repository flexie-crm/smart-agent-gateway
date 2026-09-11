/**
 * Reading a chart the assistant asked for, and working out its geometry.
 *
 * The maths lives here rather than in the component for the usual reason: a
 * scale that is off by a pixel is a bug you can only see, and a bug you can only
 * see is a bug nobody can test. Everything below is a pure function of the
 * numbers, so it can be.
 *
 * # The shape on the wire
 *
 * A chart arrives as JSON in a fenced block, in the shape every model already
 * writes without being taught: a type, labels, and datasets. We do NOT invent a
 * format of our own for this. The format is the one thing here we do not get to
 * choose, because the model has to produce it correctly the first time, from a
 * short description, with no chance to iterate.
 *
 * What we do choose is what happens to it: it is drawn as plain SVG, by us, in
 * a few hundred lines. There is no charting library behind this and there does
 * not need to be. A chart in a conversation is a PICTURE of some numbers, shown
 * once, never hovered, never zoomed, never re-plotted. Paying 200KB of
 * JavaScript for interactions nobody performs is a bad trade twice over: it is
 * slow to load and it is one more thing that can be wrong.
 */

/** The kinds we draw. Anything else falls back to bars. */
export type ChartKind = 'bar' | 'hbar' | 'line' | 'area' | 'pie' | 'doughnut'

export interface Series {
  label: string
  data: number[]
  /** An explicit colour, when the chart named one; otherwise the palette. */
  color?: string
}

export interface Chart {
  kind: ChartKind
  labels: string[]
  series: Series[]
  stacked: boolean
}

/**
 * The palette, in the order series are drawn.
 *
 * Fixed hex rather than theme variables, because a chart is read as a set of
 * things that must stay distinguishable from each other, and a colour that
 * shifts with the theme cannot be relied on to stay distinct from its
 * neighbours. Chosen to hold their difference on both a white and a dark
 * background, and to survive the common colour-blindness at least in ORDER.
 */
export const PALETTE = [
  '#2563eb', // blue
  '#e11d48', // rose
  '#16a34a', // green
  '#d97706', // amber
  '#7c3aed', // violet
  '#0891b2', // cyan
  '#db2777', // pink
  '#65a30d', // lime
]

export function colorOf(series: Series, index: number): string {
  return series.color || PALETTE[index % PALETTE.length]
}

/** The colour of one slice or bar within a single-series chart. */
export function sliceColor(index: number): string {
  return PALETTE[index % PALETTE.length]
}

// ─── Reading the block ──────────────────────────────────────────────────────

/**
 * parseChart turns the JSON in the block into something drawable, or null when
 * it is not a chart at all.
 *
 * Deliberately forgiving about everything EXCEPT having numbers to draw. A
 * model writing this by hand gets an option name wrong far more often than it
 * gets the data wrong, and refusing the whole picture over a stray key would
 * mean showing nothing rather than showing the numbers somebody asked for.
 */
export function parseChart(json: string): Chart | null {
  let raw: any
  try {
    raw = JSON.parse(json)
  } catch {
    return null
  }
  if (!raw || typeof raw !== 'object') return null

  const data = raw.data ?? raw
  const rawSeries: any[] = Array.isArray(data?.datasets) ? data.datasets : []
  if (rawSeries.length === 0) return null

  const series: Series[] = rawSeries
    .map((d: any, i: number) => ({
      label: typeof d?.label === 'string' && d.label ? d.label : `Series ${i + 1}`,
      data: numbers(d?.data),
      color: firstColor(d?.backgroundColor) || firstColor(d?.borderColor),
    }))
    .filter((s) => s.data.length > 0)
  if (series.length === 0) return null

  const width = Math.max(...series.map((s) => s.data.length))
  const labels: string[] = []
  for (let i = 0; i < width; i++) {
    const label = Array.isArray(data?.labels) ? data.labels[i] : undefined
    labels.push(label === undefined || label === null ? '' : String(label))
  }

  return {
    kind: kindOf(raw),
    labels,
    series,
    stacked: raw?.options?.scales?.x?.stacked === true || raw?.options?.scales?.y?.stacked === true,
  }
}

function kindOf(raw: any): ChartKind {
  const type = String(raw?.type ?? '').toLowerCase()
  const horizontal = raw?.options?.indexAxis === 'y'
  switch (type) {
    case 'horizontalbar':
    case 'bar-horizontal':
      return 'hbar'
    case 'bar':
      return horizontal ? 'hbar' : 'bar'
    case 'line':
      // A line told to fill under itself is an area chart, which is a different
      // drawing rather than a decoration on the same one.
      return raw?.data?.datasets?.some((d: any) => d?.fill) ? 'area' : 'line'
    case 'area':
      return 'area'
    case 'pie':
      return 'pie'
    case 'doughnut':
    case 'donut':
      return 'doughnut'
    default:
      return 'bar'
  }
}

/** numbers keeps what can be plotted and drops what cannot, in place. */
function numbers(v: unknown): number[] {
  if (!Array.isArray(v)) return []
  return v.map((n) => {
    if (typeof n === 'number' && Number.isFinite(n)) return n
    if (typeof n === 'string' && n.trim() !== '' && Number.isFinite(Number(n))) return Number(n)
    // A point that is not a number is a HOLE, not a zero: drawing a gap as zero
    // invents a value nobody supplied, and on a line chart it invents a cliff.
    return NaN
  })
}

/** A chart may give one colour or one per point; we take the first usable one. */
function firstColor(v: unknown): string | undefined {
  if (typeof v === 'string' && v.trim()) return v
  if (Array.isArray(v)) {
    const found = v.find((c) => typeof c === 'string' && c.trim())
    return typeof found === 'string' ? found : undefined
  }
  return undefined
}

// ─── Scales ─────────────────────────────────────────────────────────────────

export interface Scale {
  min: number
  max: number
  ticks: number[]
  /** Where a value sits, 0 at the axis minimum and 1 at its maximum. */
  fraction(value: number): number
}

/**
 * A scale over the values, rounded out to numbers a person would have chosen.
 *
 * Always INCLUDES ZERO for bars, because a bar's length is its whole meaning: a
 * bar chart with a cropped baseline says 4 is twice 3, and that is not a
 * stylistic choice, it is a false statement. A line chart may crop, because a
 * line's meaning is its shape and forcing zero flattens every real movement out
 * of a series that happens to live between 980 and 1000.
 */
export function scaleFor(values: number[], includeZero: boolean): Scale {
  const finite = values.filter((v) => Number.isFinite(v))
  let min = finite.length ? Math.min(...finite) : 0
  let max = finite.length ? Math.max(...finite) : 1

  if (includeZero) {
    min = Math.min(0, min)
    max = Math.max(0, max)
  }
  if (min === max) {
    // One value, or every value the same. Give it room rather than dividing by
    // zero: a single bar should not fill the frame edge to edge.
    if (min === 0) {
      max = 1
    } else {
      const pad = Math.abs(min) * 0.5
      min -= pad
      max += pad
      if (includeZero) min = Math.min(0, min)
    }
  }

  const ticks = niceTicks(min, max, 4)
  min = Math.min(min, ticks[0])
  max = Math.max(max, ticks[ticks.length - 1])
  const span = max - min

  return {
    min,
    max,
    ticks,
    fraction: (v: number) => (v - min) / span,
  }
}

/**
 * Tick values a person would have chosen: multiples of 1, 2 or 5 times a power
 * of ten, covering the range. The textbook algorithm, and it is the textbook
 * one because axis labels reading 0, 250, 500, 750 are read at a glance and
 * labels reading 0, 233.33, 466.67 are not read at all.
 */
export function niceTicks(min: number, max: number, count: number): number[] {
  if (!Number.isFinite(min) || !Number.isFinite(max) || min === max) return [min, max]
  const step = niceStep((max - min) / Math.max(1, count))
  const first = Math.floor(min / step) * step
  const last = Math.ceil(max / step) * step

  const ticks: number[] = []
  // Counted rather than accumulated: repeated addition of 0.1 arrives at
  // 0.30000000000000004, and that is what ends up printed on the axis.
  const steps = Math.round((last - first) / step)
  for (let i = 0; i <= steps; i++) ticks.push(round(first + i * step))
  return ticks
}

/**
 * The nearest nice step to a rough one, from the 1-2-5 family.
 *
 * The thresholds round to the NEAREST of them rather than up to the next, which
 * matters more than it looks: for a 0..1000 axis wanting four intervals the
 * rough step is 250, and rounding up gives 500, i.e. three labels on the whole
 * axis. Rounding to nearest gives 200 and six labels, which is the axis a person
 * would have drawn.
 */
function niceStep(rough: number): number {
  const magnitude = Math.pow(10, Math.floor(Math.log10(Math.abs(rough) || 1)))
  const normalized = rough / magnitude
  const step = normalized <= 1.5 ? 1 : normalized <= 3 ? 2 : normalized <= 7 ? 5 : 10
  return step * magnitude
}

/** round trims the floating-point dust a division leaves on a tick. */
function round(n: number): number {
  return Math.abs(n) < 1e-10 ? 0 : Number(n.toPrecision(12))
}

/**
 * An axis label as somebody reads it: 1.2k, 3.4M, 0.05. Not a general number
 * formatter, just enough that an axis never becomes wider than its chart.
 */
export function axisLabel(n: number): string {
  const abs = Math.abs(n)
  if (abs >= 1_000_000_000) return trim(n / 1_000_000_000) + 'B'
  if (abs >= 1_000_000) return trim(n / 1_000_000) + 'M'
  if (abs >= 1_000) return trim(n / 1_000) + 'k'
  if (abs === 0) return '0'
  if (abs < 0.01) return n.toPrecision(2)
  return trim(n)
}

function trim(n: number): string {
  return String(Number(n.toFixed(2)))
}

// ─── Shapes ─────────────────────────────────────────────────────────────────

export interface Point {
  x: number
  y: number
}

/**
 * The `d` of a SMOOTH curve through the points, breaking at holes.
 *
 * Monotone cubic interpolation (Fritsch-Carlson), which has the two properties
 * a chart needs and a plain spline does not: it passes through EVERY data
 * point, and it never overshoots between them. Overshoot is not cosmetic — a
 * curve that dips below zero between two positive readings is drawing a value
 * that did not happen, and one that arcs above a local maximum invents a peak.
 *
 * Passing through the points is what lets a hover dot sit exactly on the line.
 * The reference implementation smooths through midpoints instead, which looks
 * the same but leaves the data points slightly off the curve, so its hover has
 * to binary-search the path length to find where the curve actually is.
 */
export function smoothPath(points: (Point | null)[]): string {
  return runs(points)
    .map((run) => (run.length < 3 ? straight(run) : monotone(run)))
    .join(' ')
}

/** The unbroken stretches, so a hole splits the curve rather than bridging it. */
function runs(points: (Point | null)[]): Point[][] {
  const out: Point[][] = []
  let run: Point[] = []
  for (const p of points) {
    if (!p) {
      if (run.length) out.push(run)
      run = []
      continue
    }
    run.push(p)
  }
  if (run.length) out.push(run)
  return out
}

function straight(run: Point[]): string {
  return run.map((p, i) => `${i === 0 ? 'M' : 'L'}${fmt(p.x)},${fmt(p.y)}`).join(' ')
}

function monotone(p: Point[]): string {
  const n = p.length
  // The secant slope of each segment, and the tangent at each point.
  const dx: number[] = []
  const slope: number[] = []
  for (let i = 0; i < n - 1; i++) {
    dx.push(p[i + 1].x - p[i].x)
    slope.push(dx[i] === 0 ? 0 : (p[i + 1].y - p[i].y) / dx[i])
  }

  const m: number[] = [slope[0]]
  for (let i = 1; i < n - 1; i++) {
    // A turning point gets a FLAT tangent. This is the whole trick: it is what
    // stops the curve carrying on past a peak and inventing a higher one.
    m.push(slope[i - 1] * slope[i] <= 0 ? 0 : (slope[i - 1] + slope[i]) / 2)
  }
  m.push(slope[n - 2])

  // Fritsch-Carlson: pull any tangent back inside the circle of radius 3 around
  // its neighbouring secants, which is the condition for staying monotone.
  for (let i = 0; i < n - 1; i++) {
    if (slope[i] === 0) {
      m[i] = 0
      m[i + 1] = 0
      continue
    }
    const a = m[i] / slope[i]
    const b = m[i + 1] / slope[i]
    const h = Math.hypot(a, b)
    if (h > 3) {
      m[i] = ((3 * a) / h) * slope[i]
      m[i + 1] = ((3 * b) / h) * slope[i]
    }
  }

  let d = `M${fmt(p[0].x)},${fmt(p[0].y)}`
  for (let i = 0; i < n - 1; i++) {
    const third = dx[i] / 3
    d +=
      ` C${fmt(p[i].x + third)},${fmt(p[i].y + m[i] * third)}` +
      ` ${fmt(p[i + 1].x - third)},${fmt(p[i + 1].y - m[i + 1] * third)}` +
      ` ${fmt(p[i + 1].x)},${fmt(p[i + 1].y)}`
  }
  return d
}


/**
 * The same curve, closed down to a baseline.
 *
 * It reuses the LINE's path rather than drawing its own, or the fill and the
 * stroke it belongs to would be two different shapes with a sliver of daylight
 * between them wherever the curve bends.
 */
export function areaPath(points: (Point | null)[], baselineY: number): string {
  return runs(points)
    .filter((r) => r.length > 1)
    .map((r) => {
      const curve = r.length < 3 ? straight(r) : monotone(r)
      return `${curve} L${fmt(r[r.length - 1].x)},${fmt(baselineY)} L${fmt(r[0].x)},${fmt(baselineY)} Z`
    })
    .join(' ')
}

/**
 * A bar rounded only on the end AWAY from its axis.
 *
 * A rectangle with `rx` rounds all four corners, so every bar sat on the
 * baseline with two rounded feet and a sliver of daylight under each one. A bar
 * meets its axis flat: that edge is the zero it is measured from, and rounding
 * it away rounds away the thing the bar is claiming.
 *
 * `side` is which end is free: the top of a column, the right of a horizontal
 * bar, and the mirror of each when the value is negative.
 */
export function barPath(
  x: number,
  y: number,
  w: number,
  h: number,
  radius: number,
  side: 'top' | 'bottom' | 'right' | 'left',
): string {
  const r = Math.max(0, Math.min(radius, w / 2, h / 2))
  const L = x
  const R = x + w
  const T = y
  const B = y + h
  switch (side) {
    case 'top':
      return (
        `M${fmt(L)},${fmt(B)} L${fmt(L)},${fmt(T + r)} Q${fmt(L)},${fmt(T)} ${fmt(L + r)},${fmt(T)} ` +
        `L${fmt(R - r)},${fmt(T)} Q${fmt(R)},${fmt(T)} ${fmt(R)},${fmt(T + r)} L${fmt(R)},${fmt(B)} Z`
      )
    case 'bottom':
      return (
        `M${fmt(L)},${fmt(T)} L${fmt(L)},${fmt(B - r)} Q${fmt(L)},${fmt(B)} ${fmt(L + r)},${fmt(B)} ` +
        `L${fmt(R - r)},${fmt(B)} Q${fmt(R)},${fmt(B)} ${fmt(R)},${fmt(B - r)} L${fmt(R)},${fmt(T)} Z`
      )
    case 'right':
      return (
        `M${fmt(L)},${fmt(T)} L${fmt(R - r)},${fmt(T)} Q${fmt(R)},${fmt(T)} ${fmt(R)},${fmt(T + r)} ` +
        `L${fmt(R)},${fmt(B - r)} Q${fmt(R)},${fmt(B)} ${fmt(R - r)},${fmt(B)} L${fmt(L)},${fmt(B)} Z`
      )
    default:
      return (
        `M${fmt(R)},${fmt(T)} L${fmt(L + r)},${fmt(T)} Q${fmt(L)},${fmt(T)} ${fmt(L)},${fmt(T + r)} ` +
        `L${fmt(L)},${fmt(B - r)} Q${fmt(L)},${fmt(B)} ${fmt(L + r)},${fmt(B)} L${fmt(R)},${fmt(B)} Z`
      )
  }
}

/** A value as somebody reads it in a tooltip: the whole number, grouped. */
export function valueLabel(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return Number.isInteger(n) ? n.toLocaleString() : n.toLocaleString(undefined, { maximumFractionDigits: 2 })
}

export interface Slice {
  /** The path of this wedge, ready for `d`. */
  path: string
  /** Its share of the whole, 0..1, for the legend. */
  share: number
  index: number
}

/**
 * The wedges of a pie, clockwise from twelve o'clock.
 *
 * `innerRadius` above zero makes it a doughnut, which is the same drawing with
 * a hole, not a different chart.
 */
export function pieSlices(
  values: number[],
  cx: number,
  cy: number,
  radius: number,
  innerRadius = 0,
): Slice[] {
  const usable = values.map((v) => (Number.isFinite(v) && v > 0 ? v : 0))
  const total = usable.reduce((a, b) => a + b, 0)
  if (total <= 0) return []

  const slices: Slice[] = []
  let start = -Math.PI / 2 // twelve o'clock
  usable.forEach((value, index) => {
    if (value <= 0) return
    const share = value / total
    const end = start + share * Math.PI * 2

    // A single slice covering the whole circle cannot be drawn as one arc: its
    // start and end points are the same, and the renderer draws nothing at all.
    // Two half-arcs are the standard way round it.
    const sweep = end - start
    slices.push({
      path:
        sweep >= Math.PI * 2 - 1e-9
          ? ring(cx, cy, radius, innerRadius)
          : wedge(cx, cy, radius, innerRadius, start, end),
      share,
      index,
    })
    start = end
  })
  return slices
}

function wedge(
  cx: number,
  cy: number,
  r: number,
  inner: number,
  start: number,
  end: number,
): string {
  const large = end - start > Math.PI ? 1 : 0
  const p = (radius: number, angle: number) =>
    `${fmt(cx + radius * Math.cos(angle))},${fmt(cy + radius * Math.sin(angle))}`

  if (inner <= 0) {
    return `M${fmt(cx)},${fmt(cy)} L${p(r, start)} A${fmt(r)},${fmt(r)} 0 ${large} 1 ${p(r, end)} Z`
  }
  return (
    `M${p(r, start)} A${fmt(r)},${fmt(r)} 0 ${large} 1 ${p(r, end)} ` +
    `L${p(inner, end)} A${fmt(inner)},${fmt(inner)} 0 ${large} 0 ${p(inner, start)} Z`
  )
}

function ring(cx: number, cy: number, r: number, inner: number): string {
  const circle = (radius: number, sweep: number) =>
    `M${fmt(cx - radius)},${fmt(cy)} ` +
    `A${fmt(radius)},${fmt(radius)} 0 1 ${sweep} ${fmt(cx + radius)},${fmt(cy)} ` +
    `A${fmt(radius)},${fmt(radius)} 0 1 ${sweep} ${fmt(cx - radius)},${fmt(cy)} Z`
  return inner > 0 ? `${circle(r, 1)} ${circle(inner, 0)}` : circle(r, 1)
}

/** fmt keeps the path data short; sub-pixel precision is not visible. */
function fmt(n: number): string {
  return String(Math.round(n * 100) / 100)
}
