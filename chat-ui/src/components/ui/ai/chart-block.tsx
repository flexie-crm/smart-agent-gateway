import { useId, useState } from 'react'
import {
  parseChart,
  scaleFor,
  colorOf,
  sliceColor,
  axisLabel,
  valueLabel,
  smoothPath,
  areaPath,
  barPath,
  pieSlices,
  type Chart,
  type Point,
} from '@lib/chart-model'

/**
 * A chart, drawn as SVG.
 *
 * There is no charting library behind this. It used to cost 207KB of
 * JavaScript, lazily loaded and then eagerly preloaded on idle so the lazy
 * loading bought nothing. It is some arithmetic and a few `<path>` elements,
 * and the arithmetic lives in `lib/chart-model.ts` where it can be tested.
 *
 * # It scales by being drawn once
 *
 * Everything is laid out in a fixed coordinate space and shown through a
 * `viewBox`, so the browser scales the picture and nothing recomputes on
 * resize. No measurement, no ResizeObserver, no redraw. It is also what makes
 * the hover cheap: a pointer position is converted to this space by one
 * division, and the tooltip is placed as a PERCENTAGE of the box, so neither
 * has to know how wide the chart ended up on screen.
 *
 * # It is a picture, so it says what it is
 *
 * `role="img"` with a `<title>`, because a screen reader handed a pile of
 * `<path>` elements reads nothing at all.
 */

/** The coordinate space everything is drawn in, before the viewBox scales it. */
const W = 640
const H = 320
const PAD = { top: 16, right: 16, bottom: 34, left: 48 }
const PLOT = { w: W - PAD.left - PAD.right, h: H - PAD.top - PAD.bottom }

/** Where the pointer is, in the drawing's own coordinates. */
interface Hover {
  index: number
  x: number
  y: number
}

export function ChartBlock({ json }: { json: string }) {
  const chart = parseChart(json)
  const [hover, setHover] = useState<Hover | null>(null)

  if (!chart) {
    return <p className="text-sm text-muted-foreground">This chart could not be read.</p>
  }
  const circular = chart.kind === 'pie' || chart.kind === 'doughnut'

  // The pointer, in drawing coordinates. One division, because the drawing is a
  // fixed box the browser scales: nothing here needs to know the real width.
  const track = (e: React.PointerEvent<SVGSVGElement>): { x: number; y: number } => {
    const box = e.currentTarget.getBoundingClientRect()
    return {
      x: ((e.clientX - box.left) / box.width) * W,
      y: ((e.clientY - box.top) / box.height) * H,
    }
  }

  const props = circular
    ? {}
    : {
        onPointerMove: (e: React.PointerEvent<SVGSVGElement>) => setHover(hitTest(chart, track(e))),
        onPointerLeave: () => setHover(null),
      }

  return (
    <figure className="relative m-0">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="h-auto w-full touch-none"
        role="img"
        aria-label={describe(chart)}
        {...props}
      >
        <title>{describe(chart)}</title>
        {circular ? (
          <Circular chart={chart} />
        ) : chart.kind === 'hbar' ? (
          <HorizontalBars chart={chart} hover={hover} />
        ) : chart.kind === 'bar' ? (
          <VerticalBars chart={chart} hover={hover} />
        ) : (
          <Lines chart={chart} hover={hover} />
        )}
      </svg>
      {chart.series.length > 1 && <Legend chart={chart} />}
      {hover && <Tooltip chart={chart} hover={hover} />}
    </figure>
  )
}

/** What a screen reader is told, since the drawing tells it nothing. */
function describe(chart: Chart): string {
  const what =
    chart.kind === 'pie' || chart.kind === 'doughnut'
      ? 'Proportions'
      : chart.kind === 'line' || chart.kind === 'area'
        ? 'Trend'
        : 'Comparison'
  return `${what} of ${chart.series.map((s) => s.label).join(', ')} across ${chart.labels.length} points`
}

// ─── Hover ──────────────────────────────────────────────────────────────────

/**
 * Which point the pointer is nearest, and where that point IS.
 *
 * The geometry is repeated from the renderers on purpose. Threading a hit-test
 * out of each one would mean every drawing function returning a second thing
 * nobody draws, and the layouts here are three lines each.
 */
function hitTest(chart: Chart, at: { x: number; y: number }): Hover | null {
  const n = chart.labels.length
  if (n === 0) return null

  if (chart.kind === 'hbar') {
    const slot = PLOT.h / n
    const index = clamp(Math.floor((at.y - PAD.top) / slot), n)
    return index === null ? null : { index, x: at.x, y: PAD.top + slot * (index + 0.5) }
  }

  if (chart.kind === 'bar') {
    const slot = PLOT.w / n
    const index = clamp(Math.floor((at.x - PAD.left) / slot), n)
    return index === null ? null : { index, x: PAD.left + slot * (index + 0.5), y: at.y }
  }

  // A line's points sit ON the gridline rather than in a band between two, so
  // the nearest one is what the pointer means.
  const step = PLOT.w / Math.max(1, n - 1)
  const index = clamp(Math.round((at.x - PAD.left) / step), n)
  if (index === null) return null
  const scale = lineScale(chart)
  const value = chart.series[0]?.data[index]
  return {
    index,
    x: PAD.left + step * index,
    y: Number.isFinite(value) ? PAD.top + PLOT.h * (1 - scale.fraction(value)) : PAD.top,
  }
}

function clamp(index: number, n: number): number | null {
  return index < 0 || index >= n ? null : index
}

/**
 * The reading, as a card beside the point.
 *
 * HTML rather than SVG text, so the box sizes itself to its contents. Placed as
 * a percentage of the drawing, which is the whole reason the drawing is a fixed
 * box: no measuring, and it stays put at any width.
 *
 * It flips to the other side of the pointer past the halfway mark, so the card
 * never hangs off the edge of the chart it belongs to.
 */
function Tooltip({ chart, hover }: { chart: Chart; hover: Hover }) {
  const left = (hover.x / W) * 100
  const flip = left > 55
  return (
    <div
      className="pointer-events-none absolute z-10 min-w-[7rem] rounded-lg border bg-popover px-2.5 py-2 shadow-md"
      style={{
        left: `${left}%`,
        top: `${(hover.y / H) * 100}%`,
        transform: `translate(${flip ? 'calc(-100% - 14px)' : '14px'}, -50%)`,
      }}
    >
      <div className="mb-1 text-xs font-semibold text-foreground">
        {chart.labels[hover.index] || `#${hover.index + 1}`}
      </div>
      {chart.series.map((series, s) => (
        <div key={s} className="flex items-center gap-2 text-xs leading-5">
          <span
            className="size-2 shrink-0 rounded-full"
            style={{ backgroundColor: chart.series.length === 1 ? sliceColor(hover.index) : colorOf(series, s) }}
          />
          <span className="min-w-0 flex-1 truncate text-muted-foreground">{series.label}</span>
          <span className="font-medium tabular-nums text-foreground">
            {valueLabel(series.data[hover.index])}
          </span>
        </div>
      ))}
    </div>
  )
}

// ─── The furniture ──────────────────────────────────────────────────────────

/**
 * The gridlines and their labels. Horizontal only, and hairline: a grid exists
 * to let somebody read a value off the axis, and the moment it is dark enough
 * to notice it competes with the data it serves.
 */
function Grid({ ticks, y }: { ticks: number[]; y: (v: number) => number }) {
  return (
    <g>
      {ticks.map((tick) => (
        <g key={tick}>
          <line
            x1={PAD.left}
            x2={PAD.left + PLOT.w}
            y1={y(tick)}
            y2={y(tick)}
            className="stroke-border"
            strokeWidth={1}
          />
          <text
            x={PAD.left - 8}
            y={y(tick)}
            textAnchor="end"
            dominantBaseline="middle"
            className="fill-muted-foreground text-[11px]"
          >
            {axisLabel(tick)}
          </text>
        </g>
      ))}
    </g>
  )
}

/**
 * The category labels along the bottom, thinned until they fit. Rotating them,
 * or shrinking the font until forty fit, produces an unreadable smear; showing
 * every nth keeps the ones shown legible, which is the only usable version.
 */
function CategoryLabels({ labels, x }: { labels: string[]; x: (i: number) => number }) {
  const every = Math.max(1, Math.ceil(labels.length / 12))
  return (
    <g>
      {labels.map((label, i) =>
        i % every === 0 && label ? (
          <text
            key={i}
            x={x(i)}
            y={H - PAD.bottom + 18}
            textAnchor="middle"
            className="fill-muted-foreground text-[11px]"
          >
            {clip(label, 12)}
          </text>
        ) : null,
      )}
    </g>
  )
}

function Legend({ chart }: { chart: Chart }) {
  return (
    <figcaption className="mt-2 flex flex-wrap gap-x-4 gap-y-1">
      {chart.series.map((series, i) => (
        <span key={i} className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
          <span
            className="size-2.5 shrink-0 rounded-full"
            style={{ backgroundColor: colorOf(series, i) }}
          />
          {series.label}
        </span>
      ))}
    </figcaption>
  )
}

function clip(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + '…' : s
}

// ─── Bars ───────────────────────────────────────────────────────────────────

function VerticalBars({ chart, hover }: { chart: Chart; hover: Hover | null }) {
  const scale = scaleFor(
    chart.stacked ? stackedTotals(chart) : chart.series.flatMap((s) => s.data),
    true,
  )
  const y = (v: number) => PAD.top + PLOT.h * (1 - scale.fraction(v))
  const zero = y(0)

  const slot = PLOT.w / Math.max(1, chart.labels.length)
  const groupWidth = slot * 0.7
  const barWidth = chart.stacked ? groupWidth : groupWidth / chart.series.length

  return (
    <g>
      <Grid ticks={scale.ticks} y={y} />
      {/* The band under the pointer, behind the bars: it says which column is
          being read without touching the colour of the bar itself. */}
      {hover !== null && (
        <rect
          x={PAD.left + slot * hover.index}
          y={PAD.top}
          width={slot}
          height={PLOT.h}
          className="fill-muted-foreground/10"
        />
      )}
      {chart.labels.map((_, i) => {
        const left = PAD.left + slot * i + (slot - groupWidth) / 2
        let stackTop = 0
        return (
          <g key={i}>
            {chart.series.map((series, s) => {
              const value = series.data[i]
              if (!Number.isFinite(value)) return null
              const from = chart.stacked ? stackTop : 0
              const to = chart.stacked ? stackTop + value : value
              if (chart.stacked) stackTop = to
              return (
                <path
                  key={s}
                  d={barPath(
                    chart.stacked ? left : left + barWidth * s,
                    Math.min(y(from), y(to)),
                    Math.max(1, barWidth - 2),
                    Math.max(1, Math.abs(y(to) - y(from))),
                    3,
                    // The free end is the one away from the axis, which flips
                    // for a negative value.
                    to >= from ? 'top' : 'bottom',
                  )}
                  fill={chart.series.length === 1 ? sliceColor(i) : colorOf(series, s)}
                />
              )
            })}
          </g>
        )
      })}
      {/* The baseline last, so it sits on top of the bars that meet it. */}
      <line
        x1={PAD.left}
        x2={PAD.left + PLOT.w}
        y1={zero}
        y2={zero}
        className="stroke-muted-foreground/40"
        strokeWidth={1}
      />
      <CategoryLabels labels={chart.labels} x={(i) => PAD.left + slot * (i + 0.5)} />
    </g>
  )
}

/** The height of each stack, which is what a stacked axis has to cover. */
function stackedTotals(chart: Chart): number[] {
  return chart.labels.map((_, i) =>
    chart.series.reduce((sum, s) => {
      const v = s.data[i]
      return sum + (Number.isFinite(v) ? v : 0)
    }, 0),
  )
}

/**
 * Bars along the axis rather than up it: the right chart whenever the
 * categories have names instead of positions, because a name reads left to
 * right and so does the bar beside it.
 */
function HorizontalBars({ chart, hover }: { chart: Chart; hover: Hover | null }) {
  const scale = scaleFor(chart.series.flatMap((s) => s.data), true)
  const labelWidth = 96
  const plotLeft = PAD.left + labelWidth
  const plotWidth = W - PAD.right - plotLeft - 52 // and room for the value
  const x = (v: number) => plotLeft + plotWidth * scale.fraction(v)
  const zero = x(0)

  const slot = PLOT.h / Math.max(1, chart.labels.length)
  const groupHeight = slot * 0.62
  const barHeight = groupHeight / chart.series.length

  return (
    <g>
      {hover !== null && (
        <rect
          x={PAD.left}
          y={PAD.top + slot * hover.index}
          width={W - PAD.left - PAD.right}
          height={slot}
          rx={4}
          className="fill-muted-foreground/10"
        />
      )}
      {chart.labels.map((label, i) => {
        const top = PAD.top + slot * i + (slot - groupHeight) / 2
        return (
          <g key={i}>
            <text
              x={plotLeft - 10}
              y={PAD.top + slot * (i + 0.5)}
              textAnchor="end"
              dominantBaseline="middle"
              className="fill-foreground text-[12px]"
            >
              {clip(label, 14)}
            </text>
            {chart.series.map((series, s) => {
              const value = series.data[i]
              if (!Number.isFinite(value)) return null
              const end = x(value)
              return (
                <g key={s}>
                  <path
                    d={barPath(
                      Math.min(zero, end),
                      top + barHeight * s,
                      Math.max(1, Math.abs(end - zero)),
                      Math.max(1, barHeight - 2),
                      3,
                      value >= 0 ? 'right' : 'left',
                    )}
                    fill={chart.series.length === 1 ? sliceColor(i) : colorOf(series, s)}
                  />
                  {/* The number beside its own bar: a value you can read beats a
                      bar you have to measure against a gridline. */}
                  {chart.series.length === 1 && (
                    <text
                      x={Math.max(zero, end) + 8}
                      y={top + barHeight * s + barHeight / 2}
                      dominantBaseline="middle"
                      className="fill-muted-foreground text-[11px] tabular-nums"
                    >
                      {axisLabel(value)}
                    </text>
                  )}
                </g>
              )
            })}
          </g>
        )
      })}
      <line
        x1={zero}
        x2={zero}
        y1={PAD.top}
        y2={H - PAD.bottom}
        className="stroke-muted-foreground/40"
        strokeWidth={1}
      />
    </g>
  )
}

// ─── Lines ──────────────────────────────────────────────────────────────────

/**
 * A line may crop its axis: its meaning is its SHAPE, and forcing zero flattens
 * every real movement out of a series living between 980 and 1000. An area may
 * not, because the thing it fills is measured from the baseline.
 */
function lineScale(chart: Chart) {
  return scaleFor(chart.series.flatMap((s) => s.data), chart.kind === 'area')
}

function Lines({ chart, hover }: { chart: Chart; hover: Hover | null }) {
  // Unique per chart on the page: two charts sharing a gradient id means the
  // second one silently paints with the first one's colour.
  const gradient = useId().replace(/:/g, '')
  const scale = lineScale(chart)
  const y = (v: number) => PAD.top + PLOT.h * (1 - scale.fraction(v))
  const count = Math.max(1, chart.labels.length - 1)
  const x = (i: number) => PAD.left + (PLOT.w * i) / count

  return (
    <g>
      <Grid ticks={scale.ticks} y={y} />
      {/* The crosshair, under the lines so it never cuts across one. Dashed,
          because it is a pointer rather than a reading. */}
      {hover !== null && (
        <line
          x1={hover.x}
          x2={hover.x}
          y1={PAD.top}
          y2={PAD.top + PLOT.h}
          className="stroke-muted-foreground/50"
          strokeWidth={1}
          strokeDasharray="4 4"
        />
      )}
      {chart.series.map((series, s) => {
        const points: (Point | null)[] = series.data.map((v, i) =>
          Number.isFinite(v) ? { x: x(i), y: y(v) } : null,
        )
        const color = colorOf(series, s)
        return (
          <g key={s}>
            {/* Every line gets a wash under it, not only an area chart: a 2.5px
                stroke alone on a wide chart reads as a hairline, and the wash
                gives it a body to be the edge of.

                It FADES to nothing at the baseline rather than sitting as a
                flat block of tint. A flat fill draws a hard horizontal edge
                along the bottom of the plot that competes with the axis; a
                gradient puts the ink where the line is and lets it go. */}
            <defs>
              <linearGradient id={`${gradient}-${s}`} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={color} stopOpacity={chart.kind === 'area' ? 0.32 : 0.18} />
                <stop offset="100%" stopColor={color} stopOpacity={0} />
              </linearGradient>
            </defs>
            <path d={areaPath(points, y(scale.min))} fill={`url(#${gradient}-${s})`} />
            <path
              d={smoothPath(points)}
              fill="none"
              stroke={color}
              strokeWidth={2.5}
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            {/* Markers only on a short series. Past that they bead the line
                rather than marking it, and the hover dot is what points at a
                reading anyway. */}
            {points.length <= 12 &&
              points.map((p, i) =>
                p ? (
                  <circle
                    key={i}
                    cx={p.x}
                    cy={p.y}
                    r={3}
                    fill={color}
                    className="stroke-background"
                    strokeWidth={1.5}
                  />
                ) : null,
              )}
            {/* The reading under the pointer, ringed in the background colour so
                it stays visible wherever the line happens to be. */}
            {hover !== null && Number.isFinite(series.data[hover.index]) && (
              <circle
                cx={x(hover.index)}
                cy={y(series.data[hover.index])}
                r={4.5}
                fill={color}
                className="stroke-background"
                strokeWidth={2}
              />
            )}
          </g>
        )
      })}
      <CategoryLabels labels={chart.labels} x={x} />
    </g>
  )
}

// ─── Pie and doughnut ───────────────────────────────────────────────────────

function Circular({ chart }: { chart: Chart }) {
  const values = chart.series[0]?.data ?? []
  const radius = Math.min(PLOT.h, PLOT.w) / 2 - 8
  const cx = PAD.left + radius + 8
  const cy = PAD.top + PLOT.h / 2
  const slices = pieSlices(values, cx, cy, radius, chart.kind === 'doughnut' ? radius * 0.58 : 0)

  if (slices.length === 0) {
    return (
      <text x={W / 2} y={H / 2} textAnchor="middle" className="fill-muted-foreground text-[12px]">
        Nothing to show
      </text>
    )
  }

  // The key sits beside the pie rather than under it, so a wedge and its name
  // are readable in one glance. Slices are not labelled in place because a thin
  // one has nowhere to put the text.
  const keyLeft = cx + radius + 28
  const lineHeight = Math.min(22, PLOT.h / Math.max(1, slices.length))
  const keyTop = cy - (slices.length - 1) * lineHeight * 0.5

  return (
    <g>
      {slices.map((slice) => (
        <path
          key={slice.index}
          d={slice.path}
          fill={sliceColor(slice.index)}
          className="stroke-background"
          strokeWidth={1.5}
        />
      ))}
      {slices.map((slice, i) => (
        <g key={slice.index} transform={`translate(${keyLeft}, ${keyTop + i * lineHeight})`}>
          <rect x={0} y={-5} width={10} height={10} rx={2} fill={sliceColor(slice.index)} />
          <text x={16} y={0} dominantBaseline="middle" className="fill-foreground text-[12px]">
            {clip(chart.labels[slice.index] || `Item ${slice.index + 1}`, 16)}
          </text>
          <text
            x={W - PAD.right - keyLeft}
            y={0}
            textAnchor="end"
            dominantBaseline="middle"
            className="fill-muted-foreground text-[11px] tabular-nums"
          >
            {(slice.share * 100).toFixed(slice.share < 0.1 ? 1 : 0)}%
          </text>
        </g>
      ))}
    </g>
  )
}
