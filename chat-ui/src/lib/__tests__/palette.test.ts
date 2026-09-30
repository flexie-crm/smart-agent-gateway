import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// The stylesheet itself, so this checks the colours that actually ship rather
// than a copy of them kept in a test. Read from disk rather than imported: a
// test runner stubs CSS imports to nothing, which would quietly pass everything.
const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')

// "Pleasant" as a number rather than a matter of taste.
//
// The dark theme that ships with this component library is near-black behind
// near-white, which is the most contrast a screen can make: about 18:1. That is
// not the most readable, it is the most tiring. White on black haloes, letters
// bloom at the edges, and it reads as a void rather than a material. What is
// wanted is comfortably past legible and well short of glaring.
//
// So this reads the actual stylesheet and checks the colours, which means the
// palette cannot drift back towards the default without something saying so.

function block(selector: string): Record<string, string> {
  const at = css.indexOf(`${selector} {`)
  const body = css.slice(at, css.indexOf('\n}', at))
  const out: Record<string, string> = {}
  for (const [, name, value] of body.matchAll(/(--[\w-]+):\s*([^;]+);/g)) {
    out[name] = value.trim()
  }
  return out
}

/** The hue angle and the chroma of an oklch colour. */
function hueOf(colour: string): number {
  return Number(parts(colour)[2])
}

/**
 * How much colour it has. Needed alongside the hue, because a hue angle means
 * nothing without it: a grey written as oklch(0.92 0.003 255) has a nominal
 * hue of 255, which is NUMERICALLY close to a blue at 248 while having no
 * colour at all. A rule that compared only hues passed a grey border on a blue
 * card, which is the mismatch it was written to catch.
 */
function chromaOf(colour: string): number {
  return Number(parts(colour)[1])
}

function parts(colour: string): [number, number, number] {
  const m = colour.match(/oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)/)
  if (!m) throw new Error(`not an oklch colour: ${colour}`)
  return [Number(m[1]), Number(m[2]), Number(m[3])]
}

/** oklch(L C H) to linear sRGB, which is what luminance is measured in. */
function linear(colour: string): [number, number, number] {
  const m = colour.match(/oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)/)
  if (!m) throw new Error(`not an oklch colour: ${colour}`)
  const [L, C, Hdeg] = [Number(m[1]), Number(m[2]), Number(m[3])]
  const h = (Hdeg * Math.PI) / 180
  const a = C * Math.cos(h)
  const b = C * Math.sin(h)
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
  const mm = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3
  return [
    4.0767416621 * l - 3.3077115913 * mm + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * mm - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * mm + 1.707614701 * s,
  ]
}

const luminance = (colour: string) => {
  const [r, g, b] = linear(colour)
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

const lightnessOf = (colour: string) => Number(colour.match(/oklch\(([\d.]+)/)![1])

describe('the night palette', () => {
  const night = block('.dark')

  it('is not black behind white', () => {
    // The two mistakes that make a dark theme hurt. A page at L 0.145 under text
    // at L 0.985 is the default this replaces.
    expect(lightnessOf(night['--background'])).toBeGreaterThan(0.17)
    expect(lightnessOf(night['--background'])).toBeLessThan(0.26)
    expect(lightnessOf(night['--foreground'])).toBeLessThan(0.94)
  })

  it('reads comfortably: past legible, short of glaring', () => {
    const body = contrast(night['--foreground'], night['--background'])
    // 4.5 is the floor for readable text anywhere; under about 14 is where the
    // haloing stops. The default sits near 18.
    expect(body).toBeGreaterThan(7)
    expect(body).toBeLessThan(14)
  })

it('keeps quiet text readable at the small sizes an interface uses it at', () => {
    // Most of what an interface says is in quiet text: a column heading, a hint,
    // a timestamp, and a great deal of it at text-xs. The floor for text of ANY
    // size is 4.5:1, and that is what this was, which is far too little when the
    // text is small. 7:1 is the line for small text.
    expect(contrast(night['--muted-foreground'], night['--background'])).toBeGreaterThan(7)
  })

  it('shows what the person said without shining a lamp at them', () => {
    // A white card every few lines is the single brightest thing in a dark
    // conversation, and it is the thing that repeats most.
    expect(contrast(night['--said-foreground'], night['--said'])).toBeGreaterThan(7)
    expect(luminance(night['--said'])).toBeLessThan(luminance(night['--foreground']))

    // Measured against the page rather than compared in lightness, and the
    // rule changed with the colour. It used to require the surface to sit
    // within 0.1 of the background's lightness, which is the right rule for a
    // GREY card: lightness is the only thing that can separate one from the
    // page, so a grey that is much lighter is just a glare.
    //
    // This surface is blue, so hue separates it and lightness does not have to.
    // What still has to hold is that it is a surface and not a light, and the
    // quantity for that is luminance against the page: the blue is 1.58:1,
    // where the grey it replaced was 1.20:1 and a white card would be 16.7:1.
    // So the ceiling is on the ratio, well below a card, and there is a floor
    // too, because the grey was close enough to the page that a short message
    // read as part of the answer above it.
    const againstThePage = contrast(night['--said'], night['--background'])
    expect(againstThePage).toBeGreaterThan(1.25)
    expect(againstThePage).toBeLessThan(2.5)
  })

  it('carries a hint of colour, so it is a material rather than grey plastic', () => {
    // Below the threshold of being seen as blue, above the threshold of feeling
    // dead. Zero chroma is what the default has.
    for (const token of ['--background', '--foreground', '--card', '--muted']) {
      const chroma = Number(night[token].match(/oklch\([\d.]+\s+([\d.]+)/)![1])
      expect(chroma).toBeGreaterThan(0)
      expect(chroma).toBeLessThan(0.02)
    }
  })

  it('pulls accents back, because a saturated colour on a dark ground shouts', () => {
    const daylight = block(':root')
    const chromaOf = (c: string) => Number(c.match(/oklch\([\d.]+\s+([\d.]+)/)![1])
    for (const token of ['--code-keyword', '--code-string', '--code-name']) {
      expect(chromaOf(night[token])).toBeLessThanOrEqual(chromaOf(daylight[token]) + 0.001)
    }
  })
})

describe('the daylight palette', () => {
  const day = block(':root')

it('is not white behind black either', () => {
    // The daylight version of the same mistake: pure white under near-black is
    // 19.8:1, the top of the scale. It reads as clinical and it glares.
    expect(lightnessOf(day['--background'])).toBeLessThan(0.995)
    const body = contrast(day['--foreground'], day['--background'])
    expect(body).toBeGreaterThan(12)
    expect(body).toBeLessThan(17)
  })

  it('keeps quiet text readable at small sizes', () => {
    expect(contrast(day['--muted-foreground'], day['--background'])).toBeGreaterThan(5.5)
  })

  it('still reads', () => {
    expect(contrast(day['--foreground'], day['--background'])).toBeGreaterThan(7)
    expect(contrast(day['--muted-foreground'], day['--background'])).toBeGreaterThan(4)
    expect(contrast(day['--said-foreground'], day['--said'])).toBeGreaterThan(7)
  })

  it('shows what the person said as a card, not as the page', () => {
    // The daylight half of the night rule above, and the same quantity, so the
    // two are comparable: how far the surface is from the page it sits on.
    //
    // A near-white card is the failure here, the mirror of a lamp at night: a
    // short message reads as part of the answer above it because nothing tells
    // them apart. The pale blue is 1.0865:1 against the page where the grey it
    // replaced was 1.0599:1, so the floor sits between them. The ceiling keeps
    // it a card rather than a slab: a saturated blue at the same lightness
    // measures 1.52:1.
    //
    // The page is oklch(0.988 ...) and not white, which matters: the first
    // version of these bounds was computed against pure white and put the
    // floor above the value it was meant to admit. The numbers here are the
    // ones this file's own arithmetic produces.
    const againstThePage = contrast(day['--said'], day['--background'])
    expect(againstThePage).toBeGreaterThan(1.07)
    expect(againstThePage).toBeLessThan(1.3)

    // And the edge around it belongs to the card. A grey hairline on a blue
    // card is the mismatch this catches, and it takes BOTH parts: the border
    // has to carry as much colour as the fill, and carry it at the same hue.
    // Hue alone passed the grey, whose nominal 255 sits 7 degrees from the
    // fill's 248 while being colourless.
    expect(chromaOf(day['--said-border'])).toBeGreaterThan(chromaOf(day['--said']) * 0.5)
    expect(Math.abs(hueOf(day['--said-border']) - hueOf(day['--said']))).toBeLessThan(20)
  })
})
