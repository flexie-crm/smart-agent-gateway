package repo

import (
	"html/template"
	"io"
)

// The page.
//
// One page, no framework, no build step: a template and a stylesheet in the
// head. It does two jobs for two different readers. The top is for somebody who
// has been sent a link and does not know what SAG is. The bottom is for an
// administrator standing at a terminal on a GPU box they have just rented, and
// it sits on its own ground so the change of audience is visible.
//
// Written for a buyer, not from the inside out. The first draft was a design
// diary with a download button on it: it explained how the thing was built,
// published its own release status ("waiting on signing") as if that were a
// feature, and headed a three-column comparison "Two editions, one product",
// which is a sentence about the repository. None of that tells anybody why they
// would want it. What does is a question they recognise and the answer coming
// back with their own numbers in it, so that is what the page leads with.
//
// It wears the product's OWN palette, in the same oklch values the chat and the
// console are built from (chat-ui/src/index.css), plus the brand orange, spent
// only where something must be noticed.
//
// The illustrations are hand-drawn SVG, inlined rather than linked, because
// they are painted from CSS custom properties and follow the reader into dark
// mode. An <img> would not: it gets its own document and cannot see ours. On a
// phone they are hidden entirely and the same exchange is rebuilt in real HTML,
// because a 1160-wide drawing in a 342px column is a 0.3 scale factor, which is
// three-pixel type, which is grey noise where the only proof ought to be.

type pageData struct {
	Host     string
	Download string
	Builds   []build
	Mac      *installer
	Win      *installer
	Art      map[string]template.HTML
}

func renderIndex(w io.Writer, d pageData) {
	d.Art = art
	_ = indexTmpl.Execute(w, d)
}

// The drawings, read once out of the binary. They are ours, so they are trusted
// HTML; nothing from a request reaches them.
var art = func() map[string]template.HTML {
	out := map[string]template.HTML{}
	for _, name := range []string{"chat", "console"} {
		if b, err := assets.ReadFile("assets/art/" + name + ".svg"); err == nil {
			out[name] = template.HTML(b)
		}
	}
	return out
}()

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Flexie SAG, the Smart Agent Gateway - private AI connected to your own data</title>
<meta name="description" content="Flexie SAG, the Smart Agent Gateway: an AI assistant your company runs itself. Ask it about your own business and it goes and checks: your databases, your servers, your files. You choose the models, you choose what it may reach, and it asks before it changes anything.">
<meta property="og:title" content="Flexie SAG">
<meta property="og:description" content="An AI assistant that works with your databases, your servers and your files. Your models, your rules.">
<link rel="icon" type="image/png" sizes="32x32" href="/assets/favicon-32.png">
<link rel="icon" type="image/png" sizes="192x192" href="/assets/favicon-192.png">
<style>
  :root {
    --bg:       oklch(0.988 0.0015 255);
    --surface:  oklch(1 0 0);
    --sidebar:  oklch(0.985 0.002 255);
    --soft:     oklch(0.965 0.003 255);
    --sel:      oklch(0.945 0.005 255);
    --fg:       oklch(0.25 0.008 255);
    --muted:    oklch(0.5 0.012 255);
    --line:     oklch(0.912 0.004 255);
    --ink:      oklch(0.25 0.008 255);
    --on-ink:   oklch(0.985 0 0);
    /* The tool panel is dark in BOTH themes, the way the chat draws one. */
    --panel:    oklch(0.25 0.008 255);
    --on-panel: oklch(0.985 0 0);
    --ok:       oklch(0.62 0.14 150);
    --ok-fg:    oklch(0.45 0.13 150);
    /* The brand orange, spent only where something must be noticed. */
    --accent:   #D9660A;
    --accent-ink: #FFFFFF;
    /* A placeholder is not muted text: it sits a step lighter again, or it
       reads as something somebody typed. */
    --ghost:    oklch(0.68 0.01 255);
    /* A second accent, from the mark's own periwinkle. One colour plus grey
       reads as a template; two related ones read as a palette. */
    --accent2:  #5B6BC0;
    --tint:     rgba(217,102,10,.07);
    --tint2:    rgba(91,107,192,.07);
    --glow:     rgba(217,102,10,.09);
    --shadow:   0 1px 2px rgba(16,20,28,.05), 0 12px 28px -8px rgba(16,20,28,.10), 0 40px 80px -32px rgba(16,20,28,.14);
    --band:     oklch(0.25 0.008 255);
    --on-band:  oklch(0.985 0 0);
    --ui: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg:       oklch(0.235 0.008 255);
      --surface:  oklch(0.262 0.008 255);
      --sidebar:  oklch(0.248 0.008 255);
      --soft:     oklch(0.305 0.01 255);
      --sel:      oklch(0.325 0.01 255);
      --fg:       oklch(0.87 0.006 255);
      --muted:    oklch(0.745 0.014 255);
      --line:     oklch(1 0 0 / 11%);
      --ink:      oklch(0.85 0.012 255);
      --on-ink:   oklch(0.24 0.01 255);
      --panel:    oklch(0.185 0.008 255);
      --on-panel: oklch(0.9 0.006 255);
      --ok:       oklch(0.72 0.13 150);
      --ok-fg:    oklch(0.8 0.12 150);
      --accent:   #FF9A2E;
      --accent-ink: #241300;
      --ghost:    oklch(0.56 0.012 255);
      --accent2:  #94A3E8;
      --tint:     rgba(255,154,46,.10);
      --tint2:    rgba(148,163,232,.10);
      --glow:     rgba(255,154,46,.10);
      --shadow:   0 1px 2px rgba(0,0,0,.30), 0 16px 40px -10px rgba(0,0,0,.45);
      --band:     oklch(0.19 0.008 255);
      --on-band:  oklch(0.9 0.006 255);
    }
  }
  * { box-sizing: border-box; }
  html { scroll-behavior: smooth; }
  body {
    margin: 0; color: var(--fg);
    font: 17px/1.6 var(--ui); -webkit-font-smoothing: antialiased;
    /* A tint behind the top of the page rather than flat paper, so the hero
       has somewhere to sit. It fades out by the second screen and never
       competes with the product shots. */
    background:
      radial-gradient(1100px 640px at 82% -10%, var(--tint), transparent 64%),
      radial-gradient(900px 560px at 4% -6%, var(--tint2), transparent 60%),
      radial-gradient(700px 420px at 50% 42%, var(--tint2), transparent 70%),
      var(--bg);
    background-repeat: no-repeat;
  }
  /* One measure. The first draft had two -- a 1160 wrap and a 720 prose cap --
     so some blocks stopped 390px short of others and the right edge of the page
     changed from section to section with no system behind it. */
  .wrap { max-width: 1200px; margin: 0 auto; padding: 0 24px; }
  a { color: var(--fg); text-underline-offset: 3px; }
  a:hover { color: var(--accent); }

  /* ------------------------------------------------------------- masthead */
  .top { display: flex; align-items: center; gap: 11px; padding: 20px 0; }
  .top img { width: 28px; height: 28px; }
  .top b { font-size: 15px; font-weight: 600; letter-spacing: -0.01em; white-space: nowrap; }
  .top .expand {
    font-size: 13.5px; color: var(--muted); white-space: nowrap;
    padding-left: 11px; margin-left: 1px; border-left: 1px solid var(--line);
  }
  @media (max-width: 760px) { .top .expand { display: none; } }
  .top nav { margin-left: auto; display: flex; align-items: center; gap: 26px; font-size: 15px; }
  .top nav a { color: var(--muted); text-decoration: none; font-weight: 500; }
  .top nav a:hover { color: var(--fg); }
  .top .src { display: inline-flex; align-items: center; gap: 7px; }
  .top .src svg { flex: none; }
  @media (max-width: 1060px) { .top .src { display: none; } }
  .top .mini {
    background: var(--accent); color: var(--accent-ink); border-radius: 100px;
    padding: 9px 18px; font-weight: 650; font-size: 14.5px;
  }
  .top .mini:hover { color: var(--accent-ink); filter: brightness(1.06); }
  @media (max-width: 860px) { .top nav a:not(.mini) { display: none; } }

  /* ------------------------------------------------------------- hero */
  /* Split, not centred. Centred text in a 1120 column leaves the right half of
     a laptop screen empty and puts the one thing that proves the product below
     the fold. Beside it, both are above it. */
  .hero { display: grid; grid-template-columns: minmax(0,0.86fr) minmax(0,1.14fr);
          gap: 48px; align-items: center; padding: 36px 0 24px; }
  @media (max-width: 1000px) { .hero { grid-template-columns: 1fr; gap: 36px; padding-top: 24px; } }
  h1 {
    font-size: clamp(33px, 3.9vw, 46px); line-height: 1.08; margin: 0 0 18px;
    font-weight: 680; letter-spacing: -0.032em;
  }
  h1 .hl { color: var(--accent); }
  .lede { font-size: 18px; line-height: 1.6; color: var(--muted); margin: 0 0 28px; }
  .get { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; }
  .btn {
    display: inline-flex; align-items: center; justify-content: center; gap: 10px;
    text-decoration: none; background: var(--accent); color: var(--accent-ink);
    font-size: 15px; font-weight: 650; padding: 0 24px; height: 48px;
    border-radius: 100px; border: 1px solid transparent; white-space: nowrap;
    box-shadow: 0 1px 2px rgba(16,20,28,.08), 0 8px 20px -6px var(--glow);
    transition: filter .15s, transform .15s;
  }
  .btn:hover { color: var(--accent-ink); filter: brightness(1.07); transform: translateY(-1px); }
  .btn svg { flex: none; }
  .btn.ghost {
    background: var(--surface); color: var(--fg); border-color: var(--line);
    box-shadow: 0 1px 2px rgba(16,20,28,.04);
  }
  .btn.ghost:hover { border-color: var(--muted); color: var(--fg); filter: none; }
  /* The way to say "not yet" without a dead grey button: a small flag ON the
     thing, so the button still reads as a button. */
  .badged { position: relative; }
  .badged i {
    position: absolute; top: -11px; right: 10px; font-style: normal;
    font-size: 11px; font-weight: 650; letter-spacing: .02em;
    background: var(--surface); color: var(--accent);
    border: 1px solid var(--line); border-radius: 100px; padding: 2px 9px;
  }
  .under { font-size: 14px; color: var(--muted); margin: 18px 0 0; }

  /* What a buyer wants to know before they click, in three lines. */
  .trust { list-style: none; margin: 26px 0 0; padding: 0; display: grid; gap: 11px; }
  .trust li { display: flex; align-items: center; gap: 11px; font-size: 15.5px; color: var(--fg); }
  .trust a { color: var(--accent2); text-decoration-thickness: 1px; }
  .trust svg { flex: none; color: var(--accent); }

  /* The three things somebody actually types, which is the clearest thing this
     page can say about what it is. */
  .asks { display: flex; justify-content: center; flex-wrap: wrap; gap: 10px; margin: 40px 0 0; }
  .ask {
    font-size: 14.5px; color: var(--muted); background: var(--surface);
    border-radius: 100px; padding: 9px 18px; border: 1px solid var(--line);
    box-shadow: 0 1px 2px rgba(16,20,28,.04);
  }
  .ask:nth-child(1) { border-color: color-mix(in oklab, var(--accent) 34%, var(--line)); }
  .ask:nth-child(2) { border-color: color-mix(in oklab, var(--accent2) 34%, var(--line)); }
  .ask:nth-child(3) { border-color: color-mix(in oklab, var(--accent) 24%, var(--line)); }
  .ask b { color: var(--fg); font-weight: 500; }


  /* ---------------------------------------------------------- the mock */
  /* Real HTML, not a drawing. It was hand-written SVG, which meant every
     measurement was a number I typed and every string of text was positioned
     by hand rather than laid out; it also renders at whatever scale the column
     happens to be, so its type was never the size it claimed. This is the same
     window built the way the window is built, so the type is type. */
  .mock {
    background: var(--surface); border: 1px solid var(--line); border-radius: 14px;
    box-shadow: var(--shadow); overflow: hidden; font-size: 14px; line-height: 1.55;
  }
  .mock-bar {
    display: flex; align-items: center; gap: 8px; padding: 11px 14px;
    border-bottom: 1px solid var(--line); background: var(--sidebar);
  }
  .mock-bar i { width: 11px; height: 11px; border-radius: 50%; display: block; }
  .mock-bar i:nth-child(1) { background: #FF5F57; }
  .mock-bar i:nth-child(2) { background: #FEBC2E; }
  .mock-bar i:nth-child(3) { background: #28C840; }
  .mock-bar b { flex: 1; text-align: center; font-size: 13px; font-weight: 600; margin-right: 44px; }
  .mock-body { padding: 20px 22px 18px; }
  /* The paragraph rule below is (0,1,1) and this was (0,1,0), so its shorthand
     margin won and took the auto with it: the person's message sat on the left,
     where the assistant's answers are. Matched on two classes so it outranks. */
  .mock-body > .mock-you {
    background: var(--soft); border-radius: 14px; padding: 12px 16px;
    margin: 0 0 20px auto; max-width: 76%; width: fit-content;
  }
  .mock-think { color: var(--muted); font-size: 13px; margin-bottom: 14px; display: flex; gap: 9px; align-items: center; }
  .mock-tool { background: var(--panel); color: var(--on-panel); border-radius: 12px; padding: 14px 16px; margin-bottom: 18px; }
  .mock-tool-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding-bottom: 11px; }
  .mock-tool-head span { font-weight: 500; }
  .mock-pill {
    display: inline-flex; align-items: center; gap: 6px; font-size: 12.5px; font-weight: 400;
    background: rgba(255,255,255,.13); border-radius: 100px; padding: 4px 12px; opacity: .82;
  }
  .mock-sql {
    font-family: var(--mono); font-size: 12.5px; line-height: 1.7; opacity: .58; margin: 0;
    padding: 11px 0; border-top: 1px solid rgba(255,255,255,.13); border-bottom: 1px solid rgba(255,255,255,.13);
    white-space: pre; overflow-x: auto;
  }
  .mock-row { display: flex; align-items: baseline; gap: 16px; padding-top: 9px; opacity: .78; }
  .mock-row span:first-child { flex: 1; }
  .mock-row b { font-weight: 500; }
  .mock-row span:last-child { width: 58px; text-align: right; opacity: .8; }
  .mock-body > p { margin: 0 0 14px; }
  .mock-composer { border: 1px solid var(--line); border-radius: 14px; padding: 13px 15px 11px; margin-top: 20px; }
  .mock-ph { color: var(--ghost); display: block; margin-bottom: 16px; }
  .mock-acts { display: flex; align-items: center; gap: 18px; color: var(--muted); font-size: 13.5px; }
  /* Each action is its own flex row. Left inline, the icon sits on the text
     baseline, which puts it a couple of pixels high against the cap height. */
  .mock-acts > span { display: inline-flex; align-items: center; gap: 7px; }
  .mock-acts svg { flex: none; }
  .mock-send {
    margin-left: auto; width: 30px; height: 30px; border-radius: 50%;
    background: var(--ink); color: var(--on-ink);
    display: flex; align-items: center; justify-content: center;
  }
  .mock-foot { text-align: center; color: var(--muted); font-size: 12.5px; margin: 16px 0 2px; }

  /* ------------------------------------------------------------- art */
  figure { margin: 40px 0 0; }
  .shotlabel { font-size: 14.5px; font-weight: 650; color: var(--accent); text-align: center; margin: 0 0 14px; }
  .hero figure { margin: 0; }
  /* Whole. It was cropped with a fade at the foot to clear the fold, and that
     read as a half-erased picture rather than a deliberate cut: the chart
     dissolved mid-line and the window lost its own bottom edge. Showing all of
     it costs nothing where it matters, because the fold falls at the same place
     either way -- what changes is only whether the part below it looks broken. */
  /* Lifted off the page. A 1px hairline round a screenshot reads as a diagram;
     a shadow reads as a window. */
  .shot {
    border-radius: 14px; overflow: hidden; box-shadow: var(--shadow);
    border: 1px solid var(--line); background: var(--surface);
  }
  .shot svg { width: 100%; height: auto; display: block; }
  figcaption { font-size: 14px; color: var(--muted); margin-top: 14px; text-align: center; }

  /* ------------------------------------------------------------- sections */
  section { padding: 88px 0 0; }
  h2 { font-size: 34px; font-weight: 600; letter-spacing: -0.025em; margin: 0 0 14px; line-height: 1.15; }
  .kicker {
    font-size: 13px; text-transform: uppercase; letter-spacing: 0.1em;
    color: var(--accent); font-weight: 650; margin: 0 0 12px;
    display: inline-flex; align-items: center; gap: 9px;
  }
  .kicker::before {
    content: ""; width: 22px; height: 2px; border-radius: 2px;
    background: currentColor; opacity: .5;
  }
  .alt .kicker { color: var(--accent2); }
  p { margin: 0 0 16px; }
  .intro { font-size: 19px; color: var(--muted); }

  /* Kicker and heading on the left, the sentence that explains them on the
     right, so a section head uses the full measure instead of leaving a third
     of it blank. */
  .head { display: grid; grid-template-columns: 420px minmax(0,1fr); gap: 64px; align-items: start; }
  @media (max-width: 900px) { .head { grid-template-columns: 1fr; gap: 16px; } }

  .grid { display: grid; grid-template-columns: repeat(3, minmax(0,1fr)); gap: 22px; margin-top: 52px; }
  @media (max-width: 900px) { .grid { grid-template-columns: repeat(2, minmax(0,1fr)); gap: 40px; } }
  @media (max-width: 620px) { .grid { grid-template-columns: 1fr; gap: 34px; } }
  .grid > div {
    position: relative; overflow: hidden;
    background: var(--surface); border: 1px solid var(--line); border-radius: 16px;
    padding: 28px 26px 28px; box-shadow: 0 1px 2px rgba(16,20,28,.04);
    transition: box-shadow .18s, transform .18s;
  }
  /* A hairline of colour along the top edge, alternating between the two
     accents, so a row of six cards has rhythm instead of being one grey field. */
  .grid > div::before {
    content: ""; position: absolute; inset: 0 0 auto; height: 3px;
    background: linear-gradient(90deg, var(--accent), color-mix(in oklab, var(--accent) 30%, transparent));
  }
  .grid > div:nth-child(even)::before {
    background: linear-gradient(90deg, var(--accent2), color-mix(in oklab, var(--accent2) 30%, transparent));
  }
  .grid > div:hover { box-shadow: 0 2px 4px rgba(16,20,28,.05), 0 18px 40px -16px rgba(16,20,28,.18); transform: translateY(-2px); }
  .grid h3 { font-size: 18px; font-weight: 650; margin: 0 0 9px; letter-spacing: -0.015em; }
  .grid .claim { font-size: 15.5px; color: var(--fg); margin: 0 0 7px; line-height: 1.5; }
  .grid .qual { font-size: 14.5px; color: var(--muted); margin: 0; line-height: 1.55; }

  /* ------------------------------------------------------------- the band */
  .band {
    position: relative; overflow: hidden;
    background:
      radial-gradient(680px 300px at 22% 0%, rgba(217,102,10,.30), transparent 68%),
      radial-gradient(620px 320px at 84% 108%, rgba(91,107,192,.34), transparent 66%),
      var(--band);
    color: var(--on-band); border-radius: 20px;
    padding: 60px 48px; margin-top: 88px; text-align: center;
    box-shadow: var(--shadow);
  }
  .band p { font-size: clamp(24px, 3.2vw, 34px); font-weight: 600; letter-spacing: -0.02em; margin: 0 0 12px; line-height: 1.2; }
  .band span { font-size: 16.5px; opacity: .72; display: block; max-width: 60ch; margin: 0 auto; }

  /* ------------------------------------------------------------- editions */
  /* Cards, like the features above them. Bare columns split by hairlines next
     to a row of cards reads as two designs on one page. */
  .eds { display: grid; grid-template-columns: repeat(3, minmax(0,1fr)); gap: 22px; margin-top: 52px; }
  @media (max-width: 860px) { .eds { grid-template-columns: 1fr; } }
  .ed {
    background: var(--surface); border: 1px solid var(--line); border-radius: 16px;
    padding: 28px 28px 30px; box-shadow: 0 1px 2px rgba(16,20,28,.04);
    display: grid; grid-template-rows: auto auto 1fr auto; row-gap: 14px;
  }
  /* The one somebody can actually have. */
  .ed.now {
    border-color: color-mix(in oklab, var(--accent) 55%, var(--line));
    background: linear-gradient(180deg, var(--tint), transparent 42%), var(--surface);
    box-shadow: 0 1px 2px rgba(16,20,28,.05), 0 14px 36px -14px var(--glow);
  }
  .ed h3 { font-size: 21px; margin: 0; font-weight: 600; letter-spacing: -0.015em; }
  .ed .who { font-size: 15px; color: var(--muted); margin: 0; }
  .ed ul { margin: 0; padding-left: 18px; font-size: 15px; color: var(--muted); }
  .ed li { margin-bottom: 6px; }
  .ed .foot { align-self: end; }
  .ed .foot .under { margin-top: 10px; }

  /* ------------------------------------------------------------- the phone demo */
  /* Real HTML, shown only where the drawings cannot be read. */
  .tiny { display: none; margin-top: 40px; }
  .tiny .q { background: var(--soft); border-radius: 12px; padding: 13px 15px; font-size: 15px; margin-bottom: 14px; }
  .tiny .panel { background: var(--panel); color: var(--on-panel); border-radius: 12px; padding: 14px 15px; font-size: 13px; }
  .tiny .panel .t { font-size: 14px; margin-bottom: 10px; }
  .tiny .panel code { display: block; font-family: var(--mono); font-size: 11.5px; opacity: .62; line-height: 1.5; }
  .tiny .panel .row { display: flex; justify-content: space-between; opacity: .78; margin-top: 8px; padding-top: 8px; border-top: 1px solid rgba(255,255,255,.14); }
  .tiny .a { font-size: 15px; margin-top: 14px; }
  @media (max-width: 720px) {
    figure { display: none; }
    .tiny { display: block; }
  }

  /* ------------------------------------------------------------- the admin half */
  #models { margin-top: 96px; background: var(--soft); }
  #models .inner { max-width: 1200px; margin: 0 auto; padding: 72px 24px 80px; }
  /* ONE measure, and everything in it: prose, headings, the command and the
     table. It had three -- a 1120 wrap, a 68ch doc and a 62ch intro -- while
     pre and table spanned the lot, so a paragraph stopped 400px short of the
     code block under it. That is the exact fault the marketing half was
     rebuilt to remove, left standing in the half that was not. */
  #models .col { max-width: none; }
  #models h3 { font-size: 21px; font-weight: 600; margin: 40px 0 12px; letter-spacing: -0.015em; }
  code, pre { font-family: var(--mono); font-size: 14px; }
  pre {
    background: var(--panel); color: var(--on-panel); border-radius: 10px;
    padding: 16px 18px; overflow-x: auto; margin: 0 0 16px; line-height: 1.6;
    font-size: 13.5px;
  }
  code.inline { background: var(--sel); padding: 1px 6px; border-radius: 4px; font-size: 14px; }
  table { width: 100%; border-collapse: collapse; margin: 0 0 16px; font-size: 15px; }
  th, td { text-align: left; padding: 11px 12px 11px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { font-weight: 600; color: var(--muted); font-size: 12.5px; text-transform: uppercase; letter-spacing: 0.06em; }
  td.file { white-space: nowrap; }
  td.size { text-align: right; color: var(--muted); white-space: nowrap; }
  .scroll { overflow-x: auto; }
  /* On a phone the install command is cut off at about 40% inside a scroller
     nobody can see, and it is the only actionable thing in this half. It
     already carries backslash continuations, so it wraps honestly. */
  @media (max-width: 720px) {
    pre { white-space: pre-wrap; word-break: break-word; font-size: 12.5px; }
  }
  ul.plain { margin: 0 0 16px; padding-left: 20px; }
  ul.plain li { margin-bottom: 7px; }
  .note { border-left: 3px solid var(--accent); padding: 4px 0 4px 16px; font-size: 16px; margin: 0 0 16px; color: var(--muted); }
  .note strong { color: var(--fg); }

    /* The version lives here rather than under the download button. An
     administrator wants to know which build they are getting; a stranger
     deciding whether to try this reads "0.1.5" as "not finished yet", and the
     first draft said it twice above the fold. */
  footer {
    padding: 28px 0 56px; color: var(--muted); font-size: 15px;
    display: flex; gap: 20px; flex-wrap: wrap; align-items: center;
  }
  footer a { color: var(--muted); }
</style>
</head>
<body>
<div class="wrap">

<div class="top">
  <img src="/assets/logo.svg" alt="">
  <b>Flexie SAG</b><span class="expand">Smart Agent Gateway</span>
  <nav>
    <a href="#what">What it does</a>
    <a href="#get">Get it</a>
    <a href="#models">Your own models</a>
    <a class="src" href="https://github.com/flexie-crm/smart-agent-gateway"><svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 .2a8 8 0 0 0-2.5 15.6c.4.07.55-.17.55-.38v-1.34C3.84 14.4 3.4 13 3.4 13c-.36-.9-.87-1.15-.87-1.15-.7-.48.06-.47.06-.47.78.05 1.2.8 1.2.8.7 1.2 1.83.85 2.28.65.07-.5.27-.85.5-1.05-1.74-.2-3.56-.87-3.56-3.88 0-.86.3-1.56.8-2.11-.08-.2-.35-1 .08-2.07 0 0 .66-.21 2.15.8a7.5 7.5 0 0 1 3.92 0c1.5-1.01 2.15-.8 2.15-.8.43 1.08.16 1.88.08 2.07.5.55.8 1.25.8 2.11 0 3.02-1.83 3.68-3.57 3.87.28.24.53.72.53 1.45v2.15c0 .21.14.46.55.38A8 8 0 0 0 8 .2Z"/></svg> Source</a>
  </nav>
</div>

<div class="hero">
  <div>
    <h1>Private AI for your company, <span class="hl">connected to your own data.</span></h1>
    <p class="lede">
      SAG &mdash; the Smart Agent Gateway &mdash; connects to the systems you already run:
      your databases, your servers, the files on your own computer. Ask it something and it
      goes and finds out, does the work, and checks with you before it changes anything.
    </p>
    <div class="get">
      {{if .Mac}}
      <a class="btn" href="{{.Mac.URL}}">
        <svg class="pf" width="25" height="25" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M16.37 12.76c.02 2.66 2.33 3.54 2.36 3.56-.02.06-.37 1.27-1.22 2.51-.73 1.08-1.5 2.15-2.7 2.17-1.18.02-1.56-.7-2.9-.7-1.35 0-1.77.68-2.88.72-1.17.04-2.06-1.16-2.8-2.23-1.5-2.19-2.66-6.19-1.11-8.89.77-1.34 2.14-2.19 3.63-2.21 1.13-.02 2.2.77 2.9.77.69 0 1.99-.95 3.36-.81.57.02 2.18.23 3.2 1.75-.08.05-1.91 1.12-1.89 3.36M14.2 4.6c.62-.75 1.03-1.79.92-2.83-.89.04-1.97.6-2.61 1.35-.57.66-1.07 1.72-.94 2.74.99.08 2-.51 2.63-1.26"/></svg>
        Download for Mac
      </a>
      {{end}}
      {{if .Win}}
      <a class="btn" href="{{.Win.URL}}">
        <svg class="pf" width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M3 5.6l7.2-1v7.1H3V5.6m0 12.8l7.2 1v-7H3v6M11.2 4.4L21 3v8.7h-9.8V4.4m0 15.2L21 21v-8.6h-9.8v7.2"/></svg>
        Download for Windows
      </a>
      {{else}}
      <span class="btn ghost badged">Download for Windows<i>Soon</i></span>
      {{end}}
    </div>
    {{if .Mac}}
    <p class="under">Free to use on your own computer &middot; no account needed &middot; {{.Mac.Size}}</p>
    {{end}}
    <ul class="trust">
      <li>
        <svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 10.5l4 4 8-9"/></svg>
        Runs on your own machine or your own server
      </li>
      <li>
        <svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 10.5l4 4 8-9"/></svg>
        Use the AI providers you already pay for, or your own models
      </li>
      <li>
        <svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 10.5l4 4 8-9"/></svg>
        You choose what it may reach, and what it must ask about
      </li>
      <li>
        <svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 10.5l4 4 8-9"/></svg>
        Fully open source &mdash; <a href="https://github.com/flexie-crm/smart-agent-gateway">read every line of it</a>
      </li>
    </ul>
  </div>

  <figure>
    <p class="shotlabel">Ask it about your customers. It goes and checks.</p>
    <div class="mock">
      <div class="mock-bar"><i></i><i></i><i></i><b>SAG Personal</b></div>
      <div class="mock-body">
        <p class="mock-you">Which customers slipped this month, and draft a follow-up for the three worth the most.</p>
        <div class="mock-think">
          <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6l4 4 4-4"/></svg>
          Thought for 6 seconds
        </div>
        <div class="mock-tool">
          <div class="mock-tool-head">
            <span>Asked the customer database</span>
            <span class="mock-pill">
              <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 8.5l3.5 3.5L13 4.5"/></svg>
              answered
            </span>
          </div>
          <p class="mock-sql">select name, mrr, last_seen from customers
where last_seen &lt; now() - interval 30 day</p>
          <div class="mock-row"><span>Northwind Ltd</span><b>&pound;4,180</b><span>41 days</span></div>
          <div class="mock-row"><span>Halcyon Group</span><b>&pound;3,050</b><span>37 days</span></div>
        </div>
        <p>Six accounts have gone quiet past thirty days. Three carry most of it, together &pound;9,400 a month.</p>
        <p>I have drafted the follow-ups. Say the word and I will send them.</p>
        <div class="mock-composer">
          <span class="mock-ph">How can I help you today?</span>
          <div class="mock-acts">
            <span><svg width="15" height="15" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M13 5.5l-6.2 6.2a2.5 2.5 0 0 0 3.5 3.5l6.2-6.2a4.1 4.1 0 0 0-5.8-5.8L4.5 9.4a5.7 5.7 0 0 0 8 8l4.4-4.4"/></svg> Attach files</span>
            <span><svg width="15" height="15" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 2.5l6 2.3v5.1c0 3.4-2.4 6.5-6 7.6-3.6-1.1-6-4.2-6-7.6V4.8z"/></svg> Ask to approve</span>
            <span class="mock-send"><svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 13V3M4 7l4-4 4 4"/></svg></span>
          </div>
        </div>
        <p class="mock-foot">AI can make mistakes. Check your AI vendor's terms &amp; conditions.</p>
      </div>
    </div>
  </figure>
</div>

<div class="asks">
  <span class="ask"><b>&ldquo;Which customers slipped this month?&rdquo;</b></span>
  <span class="ask"><b>&ldquo;Why is the Tuesday job late?&rdquo;</b></span>
  <span class="ask"><b>&ldquo;Reconcile the March invoices.&rdquo;</b></span>
</div>

<div class="tiny">
  <div class="q">Which customers slipped this month, and draft a follow-up for the three worth the most.</div>
  <div class="panel">
    <div class="t">Asked the customer database</div>
    <code>select name, mrr, last_seen from customers<br>where last_seen &lt; now() - interval 30 day</code>
    <div class="row"><span>Northwind Ltd</span><span>&pound;4,180</span><span>41 days</span></div>
    <div class="row"><span>Halcyon Group</span><span>&pound;3,050</span><span>37 days</span></div>
  </div>
  <p class="a">Six accounts have gone quiet past thirty days. Three carry most of it, together &pound;9,400 a month. I have drafted the follow-ups.</p>
</div>

<section id="what">
  <div class="head">
    <div>
      <p class="kicker">What it does</p>
      <h2>It can reach the work, not just talk about it.</h2>
    </div>
    <p class="intro">
      It works inside the systems you already run, and everything it can reach is something
      you switched on, for the people you chose.
    </p>
  </div>

  <div class="grid">
    <div>
      <h3>Answers from your own data</h3>
      <p class="claim">Connect a database and ask questions of it in plain language.</p>
      <p class="qual">You decide which tables it may see and which fields come back empty. Every query is checked against those rules before it runs.</p>
    </div>
    <div>
      <h3>It asks before it acts</h3>
      <p class="claim">Anything that changes something stops and shows you exactly what it is about to do.</p>
      <p class="qual">You approve it or you do not. Nothing happens in between, and approving never fails afterwards.</p>
    </div>
    <div>
      <h3>Your files and your servers</h3>
      <p class="claim">It works on the computer you are sitting at, and on machines only you can reach.</p>
      <p class="qual">Read and edit files, search a project, run a command. Each of those is a permission you grant or withhold.</p>
    </div>
    <div>
      <h3>Knowledge it can add to</h3>
      <p class="claim">Give it your policies, your product notes, your way of doing things.</p>
      <p class="qual">It finds the one page it needs instead of re-reading everything, and can write back what it learns.</p>
    </div>
    <div>
      <h3>Any model, including yours</h3>
      <p class="claim">Use the providers you already pay for, or run models on your own hardware.</p>
      <p class="qual">Both look the same to everyone using it, so work that should not leave the building can run on your own machines without anybody changing how they work.</p>
    </div>
    <div>
      <h3>You decide who gets what</h3>
      <p class="claim">People, groups and teams, each with their own models, tools and knowledge.</p>
      <p class="qual">Permissions are checked every single time, so taking something away takes effect at once.</p>
    </div>
  </div>

  <div class="band">
    <p>You decide what it sees, and who gets to see it.</p>
    <span>
      Use the AI providers you already have an agreement with, or run models on your own
      machines so the work stays in the building. Either way the rules are yours: which
      systems it may reach, which fields come back empty, and what it has to ask you about
      first.
    </span>
  </div>
</section>

<section class="alt">
  <div class="head">
    <div>
      <p class="kicker">Running it</p>
      <h2>One place to decide what it may do.</h2>
    </div>
    <p class="intro">
      Add people, connect the model providers you already use, switch capabilities on one at
      a time, and see what it is doing right now.
    </p>
  </div>
  <figure>
    <div class="shot whole">{{.Art.console}}</div>
  </figure>
</section>

<section id="get">
  <div class="head">
    <div>
      <p class="kicker">Get it</p>
      <h2>Start on your own computer.</h2>
    </div>
    <p class="intro">
      Download it and it works: no server to set up, no account to make, nothing to
      configure before you can ask it something.
    </p>
  </div>

  <div class="eds">
    <div class="ed now">
      <h3>For you</h3>
      <p class="who">Everything on one computer, answering only to you.</p>
      <ul>
        <li>Ready to use in about a minute</li>
        <li>Your conversations stay on your disk</li>
        <li>Reaches your own files and folders</li>
        <li>Keeps itself up to date</li>
      </ul>
      <div class="foot">
        {{if .Mac}}
        <a class="btn" href="{{.Mac.URL}}"><svg class="pf" width="25" height="25" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M16.37 12.76c.02 2.66 2.33 3.54 2.36 3.56-.02.06-.37 1.27-1.22 2.51-.73 1.08-1.5 2.15-2.7 2.17-1.18.02-1.56-.7-2.9-.7-1.35 0-1.77.68-2.88.72-1.17.04-2.06-1.16-2.8-2.23-1.5-2.19-2.66-6.19-1.11-8.89.77-1.34 2.14-2.19 3.63-2.21 1.13-.02 2.2.77 2.9.77.69 0 1.99-.95 3.36-.81.57.02 2.18.23 3.2 1.75-.08.05-1.91 1.12-1.89 3.36M14.2 4.6c.62-.75 1.03-1.79.92-2.83-.89.04-1.97.6-2.61 1.35-.57.66-1.07 1.72-.94 2.74.99.08 2-.51 2.63-1.26"/></svg>Download for Mac</a>
        <p class="under">{{.Mac.Size}} &middot; Apple silicon and Intel</p>
        {{else}}
        <a class="btn ghost" href="https://flexie.io/">Tell me when it is ready</a>
        {{end}}
      </div>
    </div>
    <div class="ed">
      <h3>For Windows</h3>
      <p class="who">The same application, for a Windows machine.</p>
      <ul>
        <li>Ready to use in about a minute</li>
        <li>Your conversations stay on your disk</li>
        <li>Reaches your own files and folders</li>
      </ul>
      <div class="foot">
          {{if .Win}}
          <a class="btn" href="{{.Win.URL}}"><svg class="pf" width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M3 5.6l7.2-1v7.1H3V5.6m0 12.8l7.2 1v-7H3v6M11.2 4.4L21 3v8.7h-9.8V4.4m0 15.2L21 21v-8.6h-9.8v7.2"/></svg>Download for Windows</a>
          <p class="under">{{.Win.Size}} &middot; 64-bit</p>
          {{else}}
        <a class="btn ghost" href="https://flexie.io/">Tell me when it is ready</a>
        <p class="under">In testing now</p>
          {{end}}
      </div>
    </div>
    <div class="ed">
      <h3>For your company</h3>
      <p class="who">One installation your whole team works in.</p>
      <ul>
        <li>People, teams and who may do what</li>
        <li>Shared models, tools and knowledge</li>
        <li>Runs on your own servers</li>
      </ul>
      <div class="foot">
        <a class="btn ghost" href="https://flexie.io/">Talk to us</a>
        <p class="under">Taking early customers</p>
      </div>
    </div>
  </div>
</section>

</div>

<section id="models">
  <div class="inner">
  <div class="col">
    <p class="kicker">For whoever sets it up</p>
    <h2>Run the models on a machine you rent or own.</h2>
    <p class="intro">
      Models can come from a provider's API, or on hardware you control. This is the second
      kind: a service you install on a machine with an NVIDIA graphics card. Once it has
      joined, those models appear in SAG like any other.
    </p>

    <h3>Installing</h3>
    <p>
      Run this on the GPU machine. The token comes from the console, under Inference, and it
      stands for an hour.
    </p>
<pre>curl -fsSL https://{{.Host}}/install.sh | sudo sh -s -- \
    --url https://your-sag-server.example.com \
    --token &lt;token&gt;</pre>
    <p>
      It reads the graphics card, downloads the matching build and starts a service. Nothing
      is compiled on your machine. If your card is one we do not publish for, it says so and
      writes nothing.
    </p>

    <h3>Which graphics cards work</h3>
    <p>
      Any NVIDIA card from the <strong>Ampere generation onwards</strong>, which means 2020
      and later. The engine needs bfloat16 in hardware, and cards older than that do not
      have it.
    </p>
    <p class="note">
      <strong>Not supported:</strong> Tesla T4, V100, and the RTX 20 series or older. These
      are common on the cheapest cloud instances, so it is worth checking before you rent
      one. The least expensive cards that do work well are the <strong>L4</strong> and the
      <strong>A10</strong>.
    </p>

    <h3>Builds</h3>
    <p>
      The installer picks the right one by itself. They are listed here so you can fetch one
      by hand for a machine with no route to the internet, and pass it with
      <code class="inline">--from</code>.
    </p>
    <div class="scroll">
      <table>
        <thead>
          <tr><th>Build</th><th>For</th><th>CUDA</th><th style="text-align:right">Size</th></tr>
        </thead>
        <tbody>
        {{range .Builds}}
          <tr>
            <td class="file"><a href="{{$.Download}}/{{.File}}">{{.Name}}</a></td>
            <td>{{if .Cards}}{{.Cards}}{{else}}&mdash;{{end}}</td>
            <td>{{if .CUDA}}{{.CUDA}}{{else}}&mdash;{{end}}</td>
            <td class="size">{{.Size}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
    </div>
    <p>
      Every file has a <code class="inline">.sha256</code> beside it.
      <a href="{{.Download}}/builds.txt">builds.txt</a> lists what is published.
    </p>

    <h3>Checking a machine can be reached</h3>
    <p>
      A node listens on <code class="inline">19443</code>, and SAG connects <em>to</em> it. The
      machine cannot tell you whether that works, because a firewall, a security group or a
      missing port forward all look like a healthy install from the inside. So ask from out
      here instead:
    </p>
<pre># the address the internet sees you at
curl https://{{.Host}}/v1/ip

# and whether your node answers on it
curl https://{{.Host}}/v1/reachable?port=19443</pre>
    <p>
      The installer does this for you at the end and prints the result. It only ever connects
      back to the address the request came from, so it cannot be aimed at anything else.
    </p>

    <h3>Adding it to SAG</h3>
    <ul class="plain">
      <li>The machine registers itself when the installer runs, using the token.</li>
      <li>It appears in the console under <strong>Inference</strong>.</li>
      <li>Download a model onto it there, then give that model to a team.</li>
    </ul>
  </div>
  </div>
</section>

<div class="wrap">
<footer>
  <span>Flexie SAG</span>
  <a href="https://flexie.io/">flexie.io</a>
  <a href="https://github.com/flexie-crm/smart-agent-gateway">Source</a>
  {{if .Mac}}<a href="{{.Mac.URL}}">Download for Mac</a>
  {{if .Mac.Version}}<span>Personal {{.Mac.Version}}</span>{{end}}{{end}}
</footer>
</div>

</body>
</html>
`))
