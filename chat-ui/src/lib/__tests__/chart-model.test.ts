import { describe, expect, it } from 'vitest';
import {
  parseChart,
  scaleFor,
  niceTicks,
  axisLabel,
  areaPath,
  smoothPath,
  barPath,
  valueLabel,
  pieSlices,
} from '../chart-model';

const bar = (data: unknown) =>
  JSON.stringify({ type: 'bar', data: { labels: ['a', 'b'], datasets: [{ label: 'S', data }] } });

describe('reading a chart block', () => {
  it('reads the shape a model writes without being taught', () => {
    const chart = parseChart(bar([1, 2]));
    expect(chart).not.toBeNull();
    expect(chart!.kind).toBe('bar');
    expect(chart!.labels).toEqual(['a', 'b']);
    expect(chart!.series[0]).toMatchObject({ label: 'S', data: [1, 2] });
  });

  it('is not a chart when there are no numbers to draw', () => {
    expect(parseChart('not json')).toBeNull();
    expect(parseChart('{}')).toBeNull();
    expect(parseChart(JSON.stringify({ type: 'bar', data: { datasets: [] } }))).toBeNull();
    // Datasets present but empty of data is still nothing to draw.
    expect(parseChart(bar([]))).toBeNull();
  });

  it('treats a value that is not a number as a HOLE, never as zero', () => {
    // Drawing a gap as zero invents a value nobody supplied, and on a line it
    // invents a cliff.
    const chart = parseChart(bar([1, null, 'x', '3']))!;
    expect(chart.series[0].data[0]).toBe(1);
    expect(Number.isNaN(chart.series[0].data[1])).toBe(true);
    expect(Number.isNaN(chart.series[0].data[2])).toBe(true);
    expect(chart.series[0].data[3]).toBe(3); // a numeric string IS a number
  });

  it('knows the horizontal spellings a model might reach for', () => {
    for (const type of ['horizontalBar', 'bar-horizontal']) {
      expect(parseChart(JSON.stringify({ type, data: { datasets: [{ data: [1] }] } }))!.kind)
        .toBe('hbar');
    }
    const byOption = JSON.stringify({
      type: 'bar', options: { indexAxis: 'y' }, data: { datasets: [{ data: [1] }] },
    });
    expect(parseChart(byOption)!.kind).toBe('hbar');
  });

  it('reads a filled line as an area, which is a different drawing', () => {
    const filled = JSON.stringify({ type: 'line', data: { datasets: [{ data: [1, 2], fill: true }] } });
    expect(parseChart(filled)!.kind).toBe('area');
    const plain = JSON.stringify({ type: 'line', data: { datasets: [{ data: [1, 2] }] } });
    expect(parseChart(plain)!.kind).toBe('line');
  });

  it('draws something rather than nothing when a type is unknown', () => {
    // A model gets an option name wrong far more often than it gets the data
    // wrong; refusing the picture would show nothing where numbers were asked for.
    expect(parseChart(JSON.stringify({ type: 'radar', data: { datasets: [{ data: [1] }] } }))!.kind)
      .toBe('bar');
  });

  it('pads the labels out to the longest series', () => {
    const chart = parseChart(JSON.stringify({
      type: 'bar', data: { labels: ['a'], datasets: [{ data: [1, 2, 3] }] },
    }))!;
    expect(chart.labels).toHaveLength(3);
  });
});

describe('the axis', () => {
  it('always includes zero for bars, because a bar\'s length is its meaning', () => {
    // A cropped baseline makes 4 look like twice 3. That is not a style choice,
    // it is a false statement.
    const scale = scaleFor([980, 1000], true);
    expect(scale.min).toBe(0);
  });

  it('lets a line crop, because a line\'s meaning is its shape', () => {
    const scale = scaleFor([980, 1000], false);
    expect(scale.min).toBeGreaterThan(0);
  });

  it('picks tick values a person would have chosen', () => {
    // Multiples of 1, 2 or 5 times a power of ten, and rounded to the NEAREST
    // of those rather than up: rounding up here gives a step of 500, i.e. three
    // labels on the whole axis.
    expect(niceTicks(0, 1000, 4)).toEqual([0, 200, 400, 600, 800, 1000]);
    expect(niceTicks(0, 9, 4)).toEqual([0, 2, 4, 6, 8, 10]);
    expect(niceTicks(0, 5, 4)).toEqual([0, 1, 2, 3, 4, 5]);
  });

  it('does not print floating-point dust on the axis', () => {
    // Accumulating 0.1 arrives at 0.30000000000000004, and that is what ends up
    // rendered next to a gridline.
    for (const tick of niceTicks(0, 0.5, 4)) {
      expect(String(tick)).not.toMatch(/\d{6,}/);
    }
  });

  it('gives a single value room rather than dividing by zero', () => {
    const scale = scaleFor([7], true);
    expect(scale.max).toBeGreaterThan(scale.min);
    expect(Number.isFinite(scale.fraction(7))).toBe(true);
  });

  it('survives having nothing at all', () => {
    const scale = scaleFor([], true);
    expect(Number.isFinite(scale.fraction(0))).toBe(true);
  });

  it('keeps a label narrower than the chart', () => {
    expect(axisLabel(1500)).toBe('1.5k');
    expect(axisLabel(2_400_000)).toBe('2.4M');
    expect(axisLabel(0)).toBe('0');
    expect(axisLabel(12.5)).toBe('12.5');
  });
});

describe('the shapes', () => {
  const p = (x: number, y: number) => ({ x, y });

  it('closes an area down to the baseline, once per run', () => {
    const d = areaPath([p(0, 0), p(1, 1), null, p(3, 3), p(4, 4)], 10);
    expect(d.match(/Z/g)).toHaveLength(2);
    expect(d).toContain('10');
  });

  it('does not draw an area for a run of one point', () => {
    expect(areaPath([p(0, 0), null, p(2, 2)], 10)).toBe('');
  });

  it('splits a pie into shares that add up', () => {
    const slices = pieSlices([1, 1, 2], 0, 0, 10);
    expect(slices).toHaveLength(3);
    expect(slices.reduce((sum, s) => sum + s.share, 0)).toBeCloseTo(1);
    expect(slices[2].share).toBeCloseTo(0.5);
  });

  it('draws a single slice as a ring, not an invisible arc', () => {
    // A wedge covering the whole circle has the same start and end point, and
    // renders as nothing at all.
    const [only] = pieSlices([5], 0, 0, 10);
    expect(only.share).toBe(1);
    expect(only.path).not.toBe('');
    expect(only.path.match(/A/g)!.length).toBeGreaterThan(1);
  });

  it('has nothing to draw when nothing is positive', () => {
    expect(pieSlices([0, 0], 0, 0, 10)).toEqual([]);
    expect(pieSlices([-1, -2], 0, 0, 10)).toEqual([]);
  });

  it('leaves a hole in a doughnut', () => {
    const [slice] = pieSlices([1, 1], 0, 0, 10, 5);
    // Two arcs (outer and inner) rather than a wedge from the centre.
    expect(slice.path.match(/A/g)).toHaveLength(2);
    expect(slice.path).not.toContain('M0,0 L');
  });
});

describe('the curve', () => {
  const at = (xs: number[], ys: number[]) => xs.map((x, i) => ({ x, y: ys[i] }));

  it('passes through every point it is given', () => {
    // This is what lets a hover dot sit exactly ON the line rather than near it.
    // A midpoint spline looks the same and does not, which is why an
    // implementation using one has to binary-search the path to find the curve.
    const points = at([0, 10, 20, 30], [10, 5, 8, 2]);
    const d = smoothPath(points);
    for (const p of points) {
      expect(d).toContain(`${p.x},${p.y}`);
    }
  });

  it('never overshoots between two points', () => {
    // A curve that dips below zero between two positive readings draws a value
    // that did not happen; one that arcs above a local maximum invents a peak.
    // Monotone interpolation flattens the tangent at every turning point, so
    // the control points stay inside the values they sit between.
    const d = smoothPath(at([0, 10, 20], [100, 0, 100]));
    for (const y of controlYs(d)) {
      expect(y).toBeGreaterThanOrEqual(0);
      expect(y).toBeLessThanOrEqual(100);
    }
  });

  it('stays inside a rising series too', () => {
    const d = smoothPath(at([0, 10, 20, 30], [0, 1, 2, 30]));
    for (const y of controlYs(d)) {
      expect(y).toBeGreaterThanOrEqual(0);
      expect(y).toBeLessThanOrEqual(30);
    }
  });

  it('breaks at a hole rather than curving through it', () => {
    const d = smoothPath([{ x: 0, y: 0 }, { x: 1, y: 1 }, null, { x: 3, y: 3 }, { x: 4, y: 4 }]);
    expect(d.match(/M/g)).toHaveLength(2);
  });

  it('draws a straight segment when there is nothing to curve', () => {
    expect(smoothPath([{ x: 0, y: 0 }, { x: 1, y: 1 }])).toBe('M0,0 L1,1');
    expect(smoothPath([])).toBe('');
  });
});

/** Every y in a path's numbers, control points included. */
function controlYs(d: string): number[] {
  const pairs = d.match(/-?[\d.]+,-?[\d.]+/g) ?? [];
  return pairs.map((pair) => Number(pair.split(',')[1]));
}

describe('a bar meets its axis flat', () => {
  it('rounds only the end away from the axis', () => {
    // `rx` on a rect rounds all four corners, so every column sat on the
    // baseline with two rounded feet and a sliver of daylight under each.
    const up = barPath(0, 0, 20, 100, 3, 'top');
    // The two bottom corners are exact, and only the top has curves.
    expect(up).toContain('0,100');
    expect(up).toContain('20,100');
    expect(up.match(/Q/g)).toHaveLength(2);
  });

  it('flips the free end for a negative value', () => {
    const down = barPath(0, 0, 20, 100, 3, 'bottom');
    expect(down).toContain('M0,0');       // square at the axis, on top
    expect(down.match(/Q/g)).toHaveLength(2);
  });

  it('rounds the far end of a horizontal bar', () => {
    const right = barPath(0, 0, 100, 20, 3, 'right');
    expect(right).toContain('M0,0');      // square where it meets the axis
    expect(right).toContain('0,20');
    expect(right.match(/Q/g)).toHaveLength(2);
  });

  it('never rounds more than the bar can hold', () => {
    // A one-pixel bar with a three-pixel radius would turn inside out.
    const tiny = barPath(0, 0, 2, 1, 8, 'top');
    expect(tiny).not.toContain('NaN');
    expect(tiny.match(/Q/g)).toHaveLength(2);
  });
});

describe('a value in a tooltip', () => {
  it('is the whole number, grouped, not the axis abbreviation', () => {
    // An axis says 1.5k because it has no room; a tooltip is the place somebody
    // goes to find out that it was 1,543.
    expect(valueLabel(1543)).toBe((1543).toLocaleString());
    expect(valueLabel(0.5)).toBe((0.5).toLocaleString());
    expect(valueLabel(NaN)).toBe('—');
  });
});
