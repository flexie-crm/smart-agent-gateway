// Render one of the icon sources to a PNG at a given size.
//
//   node render.mjs <svg> <out.png> <size>
//
// Chromium does the drawing, because it is the renderer already in the repo
// (the browser gates use it) and it agrees with what a browser would show. The
// alternative was a second image toolchain, installed on every machine, to draw
// two files.
import { chromium } from '@playwright/test'
import { readFileSync, writeFileSync } from 'fs'

const [, , source, out, rawSize] = process.argv
const size = Number(rawSize || 1024)

// The source is authored at 1024. Rendering it at the target size rather than
// scaling a 1024 bitmap down is the whole point: the small sizes are where an
// icon dies, and a resampled curve is muddier than a drawn one.
const svg = readFileSync(source, 'utf8').replace(
  /width="1024" height="1024"/,
  `width="${size}" height="${size}"`,
)

const browser = await chromium.launch()
const page = await browser.newPage({
  viewport: { width: size, height: size },
  deviceScaleFactor: 1,
})
await page.setContent(`<body style="margin:0;background:transparent">${svg}</body>`)
writeFileSync(out, await page.screenshot({ omitBackground: true }))
await browser.close()
