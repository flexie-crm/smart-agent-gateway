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
    font-size: clamp(30px, 3.4vw, 41px); line-height: 1.1; margin: 0 0 18px;
    font-weight: 680; letter-spacing: -0.032em;
  }
  /* Two rows, and the same two rows at every width: the highlighted half is the
     second line by construction rather than by whatever the column width does. */
  h1 .hl { color: var(--accent); display: block; }
  .lede { font-size: 18px; line-height: 1.6; color: var(--muted); margin: 0 0 20px; }
  /* Who makes this. It sits above the headline rather than only in the footer,
     because somebody arriving from a search has no other way to know. */
  .by { font-size: 14px; color: var(--muted); margin: 0 0 14px; letter-spacing: 0.01em; }
  .by a { color: var(--accent2); text-decoration-thickness: 1px; }
  /* There are two products and the page must say so before the download button,
     or a visitor reads the whole page as being about whichever one they guessed. */
  .eds-line { font-size: 15.5px; line-height: 1.6; color: var(--fg); margin: 0 0 28px; }
  .eds-line strong { font-weight: 600; }
  /* The Personal card carries two platform buttons side by side. */
  .ed .get { display: flex; flex-wrap: wrap; gap: 10px; }
  .ed .foot .under a { color: var(--accent2); }
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
  /* What it can reach, one chip per thing. They are the tools that exist, so
     the row is the honest answer to "what does it actually connect to". */
  .capslead {
    text-align: center; margin: 44px 0 14px; font-size: 13px; letter-spacing: .07em;
    text-transform: uppercase; color: var(--muted); font-weight: 600;
  }
  /* Wraps to as many rows as it needs: this list is meant to grow. */
  .caps { display: flex; justify-content: center; flex-wrap: wrap; gap: 10px; margin: 0; }
  .cap {
    display: inline-flex; align-items: center; gap: 8px;
    font-size: 14px; font-weight: 500; color: var(--fg); background: var(--surface);
    border: 1px solid var(--line); border-radius: 100px; padding: 8px 16px 8px 13px;
    box-shadow: 0 1px 2px rgba(16,20,28,.04);
  }
  .cap svg { width: 20px; height: 20px; flex: none; color: var(--accent); }
  .cap svg.brand { width: 22px; height: 22px; }
  .cap:nth-child(even) svg { color: var(--accent2); }
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
  .eds { display: grid; grid-template-columns: repeat(2, minmax(0,1fr)); gap: 22px; margin-top: 52px; }
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
    <p class="by">A product by <a href="https://flexie.io/">Flexie CRM</a></p>
    <h1>AI agents that work on <span class="hl">systems you already run.</span></h1>
    <p class="lede">
      Your databases, your servers, the files on your own computer. A CRM or an ERP records
      what happened; these act on it, and check with you before they change anything. They
      run on your own computer or your own server, so the work stays where you are.
    </p>
    <p class="eds-line">
      Two editions: <strong>Personal</strong>, free on your own computer, and
      <strong>Enterprise</strong>, one installation your whole company works in.
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

<p class="capslead">What it can reach</p>
<div class="caps">
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(0.405 1.739) scale(0.96628)"><path d="M12 5.5a3 3 0 0 0-5.7-1.3A2.8 2.8 0 0 0 4 7a2.9 2.9 0 0 0 .7 1.9A3 3 0 0 0 5 14.5a3 3 0 0 0 3 3 2.7 2.7 0 0 0 4 .6Z"/><path d="M12 5.5a3 3 0 0 1 5.7-1.3A2.8 2.8 0 0 1 20 7a2.9 2.9 0 0 1-.7 1.9A3 3 0 0 1 19 14.5a3 3 0 0 1-3 3 2.7 2.7 0 0 1-4 .6Z"/><path d="M12 5.5v12.6"/></g></svg>Knowledge</span>
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(-0.914 0.305) scale(0.97462)"><path d="M13 3H6.5A1.5 1.5 0 0 0 5 4.5v15A1.5 1.5 0 0 0 6.5 21h8"/><path d="M13 3l5 5v3"/><path d="M13 3v5h5"/><circle cx="17" cy="16" r="3.2"/><path d="m19.4 18.4 2.1 2.1"/></g></svg>Files &amp; search</span>
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(0.305 0.305) scale(0.97462)"><rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="m7.5 10.5 2.5 2-2.5 2"/><path d="M13 14.5h4"/></g></svg>Terminal</span>
  <span class="cap"><svg class="brand" viewBox="0 0 128 128" aria-hidden="true"><g transform="translate(11.124 11.124) scale(0.82619)"><linearGradient id="a" x1="96.306" x2="25.454" y1="35.144" y2="98.431" gradientTransform="matrix(1 0 0 -1 0 128)" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="#a9c8ff"/><stop offset="1" stop-color="#c7e6ff"/></linearGradient><path fill="url(#a)" fill-rule="evenodd" d="M7.2 110.5c-1.7 0-3.1-.7-4.1-1.9-1-1.2-1.3-2.9-.9-4.6l18.6-80.5c.8-3.4 4-6 7.4-6h92.6c1.7 0 3.1.7 4.1 1.9 1 1.2 1.3 2.9.9 4.6l-18.6 80.5c-.8 3.4-4 6-7.4 6H7.2z" clip-rule="evenodd" opacity=".8"/><linearGradient id="b" x1="25.336" x2="94.569" y1="98.33" y2="36.847" gradientTransform="matrix(1 0 0 -1 0 128)" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="#2d4664"/><stop offset=".169" stop-color="#29405b"/><stop offset=".445" stop-color="#1e2f43"/><stop offset=".79" stop-color="#0c131b"/><stop offset="1"/></linearGradient><path fill="url(#b)" fill-rule="evenodd" d="M120.3 18.5H28.5c-2.9 0-5.7 2.3-6.4 5.2L3.7 104.3c-.7 2.9 1.1 5.2 4 5.2h91.8c2.9 0 5.7-2.3 6.4-5.2l18.4-80.5c.7-2.9-1.1-5.3-4-5.3z" clip-rule="evenodd"/><path fill="#2C5591" fill-rule="evenodd" d="M64.2 88.3h22.3c2.6 0 4.7 2.2 4.7 4.9s-2.1 4.9-4.7 4.9H64.2c-2.6 0-4.7-2.2-4.7-4.9s2.1-4.9 4.7-4.9zM78.7 66.5c-.4.8-1.2 1.6-2.6 2.6L34.6 98.9c-2.3 1.6-5.5 1-7.3-1.4-1.7-2.4-1.3-5.7.9-7.3l37.4-27.1v-.6l-23.5-25c-1.9-2-1.7-5.3.4-7.4 2.2-2 5.5-2 7.4 0l28.2 30c1.7 1.9 1.8 4.5.6 6.4z" clip-rule="evenodd"/><path fill="#FFF" fill-rule="evenodd" d="M77.6 65.5c-.4.8-1.2 1.6-2.6 2.6L33.6 97.9c-2.3 1.6-5.5 1-7.3-1.4-1.7-2.4-1.3-5.7.9-7.3l37.4-27.1v-.6l-23.5-25c-1.9-2-1.7-5.3.4-7.4 2.2-2 5.5-2 7.4 0l28.2 30c1.7 1.8 1.8 4.4.5 6.4zM63.5 87.8h22.3c2.6 0 4.7 2.1 4.7 4.6 0 2.6-2.1 4.6-4.7 4.6H63.5c-2.6 0-4.7-2.1-4.7-4.6 0-2.6 2.1-4.6 4.7-4.6z" clip-rule="evenodd"/></g></svg>PowerShell</span>
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(0.364 0.364) scale(0.9697)"><path d="M3 9h13"/><path d="m12.5 5.5 3.5 3.5-3.5 3.5"/><path d="M21 15H8"/><path d="m11.5 11.5-3.5 3.5 3.5 3.5"/></g></svg>API requests</span>
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(-1.566 -1.509) scale(1.1497)"><circle cx="8.2" cy="8.2" r="3.9"/><path d="m11 11 8.2 8.2"/><path d="m14.6 14.6 2.2-2.2"/><path d="m17.1 17.1 2.2-2.2"/></g></svg>SSH Client</span>
  <span class="cap"><svg class="brand" viewBox="0 0 128 128" aria-hidden="true"><g transform="translate(12.801 12.8) scale(0.80128)"><path fill="#00618A" d="M117.688 98.242c-6.973-.191-12.297.461-16.852 2.379-1.293.547-3.355.559-3.566 2.18.711.746.82 1.859 1.387 2.777 1.086 1.754 2.922 4.113 4.559 5.352 1.789 1.348 3.633 2.793 5.551 3.961 3.414 2.082 7.223 3.27 10.504 5.352 1.938 1.23 3.859 2.777 5.75 4.164.934.684 1.563 1.75 2.773 2.18v-.195c-.637-.812-.801-1.93-1.387-2.777l-2.578-2.578c-2.52-3.344-5.719-6.281-9.117-8.719-2.711-1.949-8.781-4.578-9.91-7.73l-.199-.199c1.922-.219 4.172-.914 5.949-1.391 2.98-.797 5.645-.59 8.719-1.387l4.164-1.187v-.793c-1.555-1.594-2.664-3.707-4.359-5.152-4.441-3.781-9.285-7.555-14.273-10.703-2.766-1.746-6.184-2.883-9.117-4.363-.988-.496-2.719-.758-3.371-1.586-1.539-1.961-2.379-4.449-3.566-6.738-2.488-4.793-4.93-10.023-7.137-15.066-1.504-3.437-2.484-6.828-4.359-9.91-9-14.797-18.687-23.73-33.695-32.508-3.195-1.867-7.039-2.605-11.102-3.57l-6.543-.395c-1.332-.555-2.715-2.184-3.965-2.977C16.977 3.52 4.223-3.312.539 5.672-1.785 11.34 4.016 16.871 6.09 19.746c1.457 2.012 3.32 4.273 4.359 6.539.688 1.492.805 2.984 1.391 4.559 1.438 3.883 2.695 8.109 4.559 11.695.941 1.816 1.98 3.727 3.172 5.352.727.996 1.98 1.438 2.18 2.973-1.227 1.715-1.297 4.375-1.984 6.543-3.098 9.77-1.926 21.91 2.578 29.137 1.383 2.223 4.641 6.98 9.117 5.156 3.918-1.598 3.043-6.539 4.164-10.902.254-.988.098-1.715.594-2.379v.199l3.57 7.133c2.641 4.254 7.324 8.699 11.297 11.699 2.059 1.555 3.68 4.242 6.344 5.152v-.199h-.199c-.516-.805-1.324-1.137-1.98-1.781-1.551-1.523-3.277-3.414-4.559-5.156-3.613-4.902-6.805-10.27-9.711-15.855-1.391-2.668-2.598-5.609-3.77-8.324-.453-1.047-.445-2.633-1.387-3.172-1.281 1.988-3.172 3.598-4.164 5.945-1.582 3.754-1.789 8.336-2.375 13.082-.348.125-.195.039-.398.199-2.762-.668-3.73-3.508-4.758-5.949-2.594-6.164-3.078-16.09-.793-23.191.59-1.836 3.262-7.617 2.18-9.316-.516-1.691-2.219-2.672-3.172-3.965-1.18-1.598-2.355-3.703-3.172-5.551-2.125-4.805-3.113-10.203-5.352-15.062-1.07-2.324-2.875-4.676-4.359-6.738-1.645-2.289-3.484-3.977-4.758-6.742-.453-.984-1.066-2.559-.398-3.566.215-.684.516-.969 1.191-1.191 1.148-.887 4.352.297 5.547.793 3.18 1.32 5.832 2.578 8.527 4.363 1.289.855 2.598 2.512 4.16 2.973h1.785c2.789.641 5.914.195 8.523.988 4.609 1.402 8.738 3.582 12.488 5.949 11.422 7.215 20.766 17.48 27.156 29.734 1.027 1.973 1.473 3.852 2.379 5.945 1.824 4.219 4.125 8.559 5.941 12.688 1.816 4.113 3.582 8.27 6.148 11.695 1.348 1.801 6.551 2.766 8.918 3.766 1.66.699 4.379 1.43 5.949 2.379 3 1.809 5.906 3.965 8.723 5.945 1.402.992 5.73 3.168 5.945 4.957zm-88.605-75.52c-1.453-.027-2.48.156-3.566.395v.199h.195c.695 1.422 1.918 2.34 2.777 3.566l1.98 4.164.199-.195c1.227-.867 1.789-2.25 1.781-4.363-.492-.52-.562-1.164-.992-1.785-.562-.824-1.66-1.289-2.375-1.98zm0 0"/></g></svg>MySQL</span>
  <span class="cap"><svg class="brand" viewBox="0 0 128 128" aria-hidden="true"><g transform="translate(10.285 10.506) scale(0.83584)"><path d="M93.809 92.112c.785-6.533.55-7.492 5.416-6.433l1.235.108c3.742.17 8.637-.602 11.513-1.938 6.191-2.873 9.861-7.668 3.758-6.409-13.924 2.873-14.881-1.842-14.881-1.842 14.703-21.815 20.849-49.508 15.543-56.287-14.47-18.489-39.517-9.746-39.936-9.52l-.134.025c-2.751-.571-5.83-.912-9.289-.968-6.301-.104-11.082 1.652-14.709 4.402 0 0-44.683-18.409-42.604 23.151.442 8.841 12.672 66.898 27.26 49.362 5.332-6.412 10.484-11.834 10.484-11.834 2.558 1.699 5.622 2.567 8.834 2.255l.249-.212c-.078.796-.044 1.575.099 2.497-3.757 4.199-2.653 4.936-10.166 6.482-7.602 1.566-3.136 4.355-.221 5.084 3.535.884 11.712 2.136 17.238-5.598l-.22.882c1.474 1.18 1.375 8.477 1.583 13.69.209 5.214.558 10.079 1.621 12.948 1.063 2.868 2.317 10.256 12.191 8.14 8.252-1.764 14.561-4.309 15.136-27.985"/><path d="M75.458 125.256c-4.367 0-7.211-1.689-8.938-3.32-2.607-2.46-3.641-5.629-4.259-7.522l-.267-.79c-1.244-3.358-1.666-8.193-1.916-14.419-.038-.935-.064-1.898-.093-2.919-.021-.747-.047-1.684-.085-2.664a18.8 18.8 0 01-4.962 1.568c-3.079.526-6.389.356-9.84-.507-2.435-.609-4.965-1.871-6.407-3.82-4.203 3.681-8.212 3.182-10.396 2.453-3.853-1.285-7.301-4.896-10.542-11.037-2.309-4.375-4.542-10.075-6.638-16.943-3.65-11.96-5.969-24.557-6.175-28.693C4.292 23.698 7.777 14.44 15.296 9.129 27.157.751 45.128 5.678 51.68 7.915c4.402-2.653 9.581-3.944 15.433-3.851 3.143.051 6.136.327 8.916.823 2.9-.912 8.628-2.221 15.185-2.139 12.081.144 22.092 4.852 28.949 13.615 4.894 6.252 2.474 19.381.597 26.651-2.642 10.226-7.271 21.102-12.957 30.57 1.544.011 3.781-.174 6.961-.831 6.274-1.295 8.109 2.069 8.607 3.575 1.995 6.042-6.677 10.608-9.382 11.864-3.466 1.609-9.117 2.589-13.745 2.377l-.202-.013-1.216-.107-.12 1.014-.116.991c-.311 11.999-2.025 19.598-5.552 24.619-3.697 5.264-8.835 6.739-13.361 7.709-1.544.33-2.947.474-4.219.474zm-9.19-43.671c2.819 2.256 3.066 6.501 3.287 14.434.028.99.054 1.927.089 2.802.106 2.65.355 8.855 1.327 11.477.137.371.26.747.39 1.146 1.083 3.316 1.626 4.979 6.309 3.978 3.931-.843 5.952-1.599 7.534-3.851 2.299-3.274 3.585-9.86 3.821-19.575l4.783.116-4.75-.57.14-1.186c.455-3.91.783-6.734 3.396-8.602 2.097-1.498 4.486-1.353 6.389-1.01-2.091-1.58-2.669-3.433-2.823-4.193l-.399-1.965 1.121-1.663c6.457-9.58 11.781-21.354 14.609-32.304 2.906-11.251 2.02-17.226 1.134-18.356-11.729-14.987-32.068-8.799-34.192-8.097l-.359.194-1.8.335-.922-.191c-2.542-.528-5.366-.82-8.393-.869-4.756-.08-8.593 1.044-11.739 3.431l-2.183 1.655-2.533-1.043c-5.412-2.213-21.308-6.662-29.696-.721-4.656 3.298-6.777 9.76-6.305 19.207.156 3.119 2.275 14.926 5.771 26.377 4.831 15.825 9.221 21.082 11.054 21.693.32.108 1.15-.537 1.976-1.529a270.708 270.708 0 0110.694-12.07l2.77-2.915 3.349 2.225c1.35.897 2.839 1.406 4.368 1.502l7.987-6.812-1.157 11.808c-.026.265-.039.626.065 1.296l.348 2.238-1.51 1.688-.174.196 4.388 2.025 1.836-2.301z"/><path fill="#336791" d="M115.731 77.44c-13.925 2.873-14.882-1.842-14.882-1.842 14.703-21.816 20.849-49.51 15.545-56.287C101.924.823 76.875 9.566 76.457 9.793l-.135.024c-2.751-.571-5.83-.911-9.291-.967-6.301-.103-11.08 1.652-14.707 4.402 0 0-44.684-18.408-42.606 23.151.442 8.842 12.672 66.899 27.26 49.363 5.332-6.412 10.483-11.834 10.483-11.834 2.559 1.699 5.622 2.567 8.833 2.255l.25-.212c-.078.796-.042 1.575.1 2.497-3.758 4.199-2.654 4.936-10.167 6.482-7.602 1.566-3.136 4.355-.22 5.084 3.534.884 11.712 2.136 17.237-5.598l-.221.882c1.473 1.18 2.507 7.672 2.334 13.557-.174 5.885-.29 9.926.871 13.082 1.16 3.156 2.316 10.256 12.192 8.14 8.252-1.768 12.528-6.351 13.124-13.995.422-5.435 1.377-4.631 1.438-9.49l.767-2.3c.884-7.367.14-9.743 5.225-8.638l1.235.108c3.742.17 8.639-.602 11.514-1.938 6.19-2.871 9.861-7.667 3.758-6.408z"/><path fill="#fff" d="M75.957 122.307c-8.232 0-10.84-6.519-11.907-9.185-1.562-3.907-1.899-19.069-1.551-31.503a1.59 1.59 0 011.64-1.55 1.594 1.594 0 011.55 1.639c-.401 14.341.168 27.337 1.324 30.229 1.804 4.509 4.54 8.453 12.275 6.796 7.343-1.575 10.093-4.359 11.318-11.46.94-5.449 2.799-20.951 3.028-24.01a1.593 1.593 0 011.71-1.472 1.597 1.597 0 011.472 1.71c-.239 3.185-2.089 18.657-3.065 24.315-1.446 8.387-5.185 12.191-13.794 14.037-1.463.313-2.792.453-4 .454zM31.321 90.466a6.71 6.71 0 01-2.116-.35c-5.347-1.784-10.44-10.492-15.138-25.885-3.576-11.717-5.842-23.947-6.041-27.922-.589-11.784 2.445-20.121 9.02-24.778 13.007-9.216 34.888-.44 35.813-.062a1.596 1.596 0 01-1.207 2.955c-.211-.086-21.193-8.492-32.768-.285-5.622 3.986-8.203 11.392-7.672 22.011.167 3.349 2.284 15.285 5.906 27.149 4.194 13.742 8.967 22.413 13.096 23.79.648.216 2.62.873 5.439-2.517A245.272 245.272 0 0145.88 73.046a1.596 1.596 0 012.304 2.208c-.048.05-4.847 5.067-10.077 11.359-2.477 2.979-4.851 3.853-6.786 3.853zm69.429-13.445a1.596 1.596 0 01-1.322-2.487c14.863-22.055 20.08-48.704 15.612-54.414-5.624-7.186-13.565-10.939-23.604-11.156-7.433-.16-13.341 1.738-14.307 2.069l-.243.099c-.971.305-1.716-.227-1.997-.849a1.6 1.6 0 01.631-2.025c.046-.027.192-.089.429-.176l-.021.006.021-.007c1.641-.601 7.639-2.4 15.068-2.315 11.108.118 20.284 4.401 26.534 12.388 2.957 3.779 2.964 12.485.019 23.887-3.002 11.625-8.651 24.118-15.497 34.277-.306.457-.81.703-1.323.703zm.76 10.21c-2.538 0-4.813-.358-6.175-1.174-1.4-.839-1.667-1.979-1.702-2.584-.382-6.71 3.32-7.878 5.208-8.411-.263-.398-.637-.866-1.024-1.349-1.101-1.376-2.609-3.26-3.771-6.078-.182-.44-.752-1.463-1.412-2.648-3.579-6.418-11.026-19.773-6.242-26.612 2.214-3.165 6.623-4.411 13.119-3.716C97.6 28.837 88.5 10.625 66.907 10.271c-6.494-.108-11.82 1.889-15.822 5.93-8.96 9.049-8.636 25.422-8.631 25.586a1.595 1.595 0 11-3.19.084c-.02-.727-.354-17.909 9.554-27.916C53.455 9.272 59.559 6.96 66.96 7.081c13.814.227 22.706 7.25 27.732 13.101 5.479 6.377 8.165 13.411 8.386 15.759.165 1.746-1.088 2.095-1.341 2.147l-.576.013c-6.375-1.021-10.465-.312-12.156 2.104-3.639 5.201 3.406 17.834 6.414 23.229.768 1.376 1.322 2.371 1.576 2.985.988 2.396 2.277 4.006 3.312 5.3.911 1.138 1.7 2.125 1.982 3.283.131.23 1.99 2.98 13.021.703 2.765-.57 4.423-.083 4.93 1.45.997 3.015-4.597 6.532-7.694 7.97-2.775 1.29-7.204 2.106-11.036 2.106zm-4.696-4.021c.35.353 2.101.962 5.727.806 3.224-.138 6.624-.839 8.664-1.786 2.609-1.212 4.351-2.567 5.253-3.492l-.5.092c-7.053 1.456-12.042 1.262-14.828-.577a6.162 6.162 0 01-.54-.401c-.302.119-.581.197-.78.253-1.58.443-3.214.902-2.996 5.105zm-45.562 8.915c-1.752 0-3.596-.239-5.479-.71-1.951-.488-5.24-1.957-5.19-4.37.057-2.707 3.994-3.519 5.476-3.824 5.354-1.103 5.703-1.545 7.376-3.67.488-.619 1.095-1.39 1.923-2.314 1.229-1.376 2.572-2.073 3.992-2.073.989 0 1.8.335 2.336.558 1.708.708 3.133 2.42 3.719 4.467.529 1.847.276 3.625-.71 5.006-3.237 4.533-7.886 6.93-13.443 6.93zm-7.222-4.943c.481.372 1.445.869 2.518 1.137 1.631.408 3.213.615 4.705.615 4.546 0 8.196-1.882 10.847-5.594.553-.774.387-1.757.239-2.274-.31-1.083-1.08-2.068-1.873-2.397-.43-.178-.787-.314-1.115-.314-.176 0-.712 0-1.614 1.009a41.146 41.146 0 00-1.794 2.162c-2.084 2.646-3.039 3.544-9.239 4.821-1.513.31-2.289.626-2.674.835zm12.269-7.36a1.596 1.596 0 01-1.575-1.354 8.218 8.218 0 01-.08-.799c-4.064-.076-7.985-1.82-10.962-4.926-3.764-3.927-5.477-9.368-4.699-14.927.845-6.037.529-11.366.359-14.229-.047-.796-.081-1.371-.079-1.769.003-.505.013-1.844 4.489-4.113 1.592-.807 4.784-2.215 8.271-2.576 5.777-.597 9.585 1.976 10.725 7.246 3.077 14.228.244 20.521-1.825 25.117-.385.856-.749 1.664-1.04 2.447l-.257.69c-1.093 2.931-2.038 5.463-1.748 7.354a1.595 1.595 0 01-1.335 1.819l-.244.02zM42.464 42.26l.062 1.139c.176 2.974.504 8.508-.384 14.86-.641 4.585.759 9.06 3.843 12.276 2.437 2.542 5.644 3.945 8.94 3.945h.068c.369-1.555.982-3.197 1.642-4.966l.255-.686c.329-.884.714-1.74 1.122-2.646 1.991-4.424 4.47-9.931 1.615-23.132-.565-2.615-1.936-4.128-4.189-4.627-4.628-1.022-11.525 2.459-12.974 3.837zm9.63-.677c-.08.564 1.033 2.07 2.485 2.271 1.449.203 2.689-.975 2.768-1.539.079-.564-1.033-1.186-2.485-1.388-1.451-.202-2.691.092-2.768.656zm2.818 2.826l-.407-.028c-.9-.125-1.81-.692-2.433-1.518-.219-.29-.576-.852-.505-1.354.101-.736.999-1.177 2.4-1.177.313 0 .639.023.967.069.766.106 1.477.327 2.002.62.91.508.977 1.075.936 1.368-.112.813-1.405 2.02-2.96 2.02zm-2.289-2.732c.045.348.907 1.496 2.029 1.651l.261.018c1.036 0 1.81-.815 1.901-1.082-.096-.182-.762-.634-2.025-.81a5.823 5.823 0 00-.821-.059c-.812 0-1.243.183-1.345.282zm43.605-1.245c.079.564-1.033 2.07-2.484 2.272-1.45.202-2.691-.975-2.771-1.539-.076-.564 1.036-1.187 2.486-1.388 1.45-.203 2.689.092 2.769.655zm-2.819 2.56c-1.396 0-2.601-1.086-2.7-1.791-.115-.846 1.278-1.489 2.712-1.688.316-.044.629-.066.93-.066 1.238 0 2.058.363 2.14.949.053.379-.238.964-.739 1.492-.331.347-1.026.948-1.973 1.079l-.37.025zm.943-3.013c-.276 0-.564.021-.856.061-1.441.201-2.301.779-2.259 1.089.048.341.968 1.332 2.173 1.332l.297-.021c.787-.109 1.378-.623 1.66-.919.443-.465.619-.903.598-1.052-.028-.198-.56-.49-1.613-.49zm3.965 32.843a1.594 1.594 0 01-1.324-2.483c3.398-5.075 2.776-10.25 2.175-15.255-.257-2.132-.521-4.337-.453-6.453.07-2.177.347-3.973.614-5.71.317-2.058.617-4.002.493-6.31a1.595 1.595 0 113.186-.172c.142 2.638-.197 4.838-.525 6.967-.253 1.643-.515 3.342-.578 5.327-.061 1.874.178 3.864.431 5.97.64 5.322 1.365 11.354-2.691 17.411a1.596 1.596 0 01-1.328.708z"/></g></svg>PostgreSQL</span>
  <span class="cap"><svg class="brand" viewBox="0 0 128 128" aria-hidden="true"><g transform="translate(12.8 12.8) scale(0.8)"><path fill="#ee352c" d="M52.935 0v.002c-.426-.058-7.306 2.42-11.742 4.223-5.988 2.44-10.636 4.766-13.504 6.78-.926.657-2.054 1.75-2.475 2.37l-.007-.021a1.424 1.424 0 0 0-.069.148c-.022.04-.052.086-.066.12a1.812 1.812 0 0 0-.115.66l.064.06c.017.207.065.44.168.695.252.62.988 1.376 1.822 2.15 0 0 8.621 8.409 9.668 9.61 4.766 5.503 6.84 10.927 7.034 18.406.117 4.805-.796 9.03-3.063 13.932-4.03 8.796-12.535 18.504-25.652 29.276l.199-.067c-.09.072-.208.174-.295.242-1.57 1.24-3.896 3.565-5.078 5.038-1.764 2.209-3.157 4.553-3.758 6.355-1.066 3.255-.543 6.548 1.51 9.59 2.636 3.875 7.887 7.83 14.01 10.521 3.12 1.377 8.368 3.14 12.322 4.127 6.567 1.667 19.28 3.469 26.273 3.739 1.414.059 3.312.059 3.39 0 .155-.097 1.241-2.168 2.501-4.744 4.3-8.778 7.399-17.013 9.086-24.047 1.007-4.262 1.801-9.94 2.324-16.663.136-1.88.194-8.177.078-10.308-.175-3.487-.483-6.316-.968-9.086a4.17 4.17 0 0 1-.07-.573c15.578-4.628 32.768-8.821 44.187-10.568l1.764-.271-.272-.428c-1.55-2.403-2.615-3.894-3.894-5.483-3.72-4.61-8.233-8.349-13.756-11.449-7.595-4.244-17.419-7.557-29.858-10.018-2.344-.465-7.495-1.357-11.68-1.996l-.39-.699c-2.287-4.03-4.805-9.027-6.278-12.398-1.142-2.616-2.228-5.639-2.828-7.809C53.187.098 53.15.02 52.935 0Zm-.31.988h.02c.018.02.095.564.173 1.203.33 2.712.931 5.328 1.881 8.157.716 2.13.716 2.015-.117 1.763-1.976-.542-10.83-2.072-17.244-2.964-1.027-.135-1.899-.271-1.899-.291-.077-.078 4.63-2.537 6.703-3.506 2.654-1.22 9.94-4.265 10.483-4.362ZM33.947 9.67l.756.252c4.108 1.395 14.434 3.373 20.13 3.838.64.058 1.182.115 1.2.115.02.02-.52.31-1.219.639-2.75 1.376-5.775 3.061-7.867 4.36-.476.296-.912.546-1.127.648a1193.726 1193.726 0 0 1-1.932-.315l-1.824-1.787a803.536 803.536 0 0 0-7.11-6.84zm-.775.602 2.732 3.41c1.492 1.88 3.003 3.72 3.332 4.127.291.359.503.622.543.7-1.935-.337-4.006-.708-5.6-1.052-1.163-.252-3.39-.775-5.134-1.375-.18-.07-.385-.146-.58-.219v-.205c.02-1.3 1.666-3.238 4.455-5.213zm23.173 4.646c.015-.007.03-.006.04.004.077 0 .172.172.404.695.66 1.453 2.715 5.367 3.219 6.123l.064.104a1193.726 1193.726 0 0 1-10.977-1.79 2.86 2.86 0 0 1 .372-.232c2.035-1.124 4.088-2.557 5.91-4.088.445-.368.851-.715.93-.773a.097.097 0 0 1 .038-.043zm-26.138 3.275c.019-.018.329.1.736.235a50.336 50.336 0 0 0 2.81.851 142.909 142.909 0 0 0 2.557.678c1.162.29 2.132.563 2.15.563.137.136 2.094 6.394 2.753 8.797.252.91.446 1.685.427 1.685-.02.02-.234-.31-.486-.756-2.267-3.99-5.851-8.04-9.998-11.297-.542-.387-.95-.736-.95-.756zm9.513 2.618c0 .038 0 .02.02.02.098 0 .524.057 1.047.173 3.293.736 9.203 1.86 12.98 2.5.64.097 1.143.214 1.143.252 0 .04-.23.175-.522.33-.64.33-3.217 1.86-4.07 2.44-2.15 1.435-4.087 2.983-5.482 4.378a79.99 79.99 0 0 1-1.047 1.028s-.115-.33-.213-.737c-.697-2.694-2.15-6.684-3.469-9.494-.213-.445-.387-.852-.387-.89zm16.8 3.215c.115.04.31.699.697 2.152a31.732 31.732 0 0 1 .93 8.873c-.04.814-.079 1.57-.118 1.668l-.057.191-1.007-.33c-2.073-.658-5.444-1.645-8.33-2.459-1.648-.446-2.985-.852-2.985-.89 0-.117 2.403-2.52 3.43-3.43 1.956-1.725 7.264-5.832 7.44-5.775zm1.335.195c.058-.058 8.024 1.316 11.647 2.014 2.694.523 6.607 1.338 6.84 1.435.115.04-.291.269-1.59.852-5.115 2.305-8.914 4.38-12.692 6.898-.988.66-1.822 1.201-1.84 1.201-.02 0-.039-.562-.039-1.24 0-3.681-.734-7.401-2.091-10.54-.136-.31-.254-.601-.235-.62zm20.596 4.068c.058.057-.193 1.629-.426 2.559-.698 2.887-2.576 7.17-4.88 11.2-.409.716-.778 1.297-.817 1.316-.038.02-.558-.273-1.16-.622-2.247-1.318-4.806-2.555-7.596-3.718-.775-.33-1.454-.601-1.473-.641-.136-.115 6.104-4.242 9.397-6.219 2.617-1.589 6.879-3.952 6.955-3.875zm1.475.233c.174 0 3.7.968 5.54 1.511 4.554 1.356 9.784 3.275 13.194 4.825l1.414.638-.986.233c-8.33 1.918-15.463 4.129-22.342 6.918-.562.233-1.066.425-1.104.425-.039 0 .157-.444.409-.986 2.073-4.399 3.408-8.991 3.738-12.906.019-.368.079-.658.137-.658zm-35.11 8.06c.058-.058 2.751.582 4.205.989 2.21.62 6.899 2.19 6.899 2.304 0 .02-.525.466-1.145 1.008-2.538 2.112-4.98 4.341-7.906 7.17-.871.833-1.606 1.51-1.645 1.51-.04 0-.059-.115-.04-.27.445-3.255.35-7.44-.27-11.683-.06-.543-.117-1.009-.098-1.028zm56.596.059c.038.039-1.24 2.052-2.055 3.195-1.162 1.667-2.867 3.877-6.722 8.72a1289.46 1289.46 0 0 0-5.076 6.413c-.775.969-1.415 1.783-1.436 1.783-.018 0-.27-.35-.541-.775-2.17-3.256-4.767-6.103-7.848-8.66a44.534 44.534 0 0 0-1.431-1.164c-.214-.155-.39-.31-.39-.33 0-.057 3.294-1.472 5.794-2.479 4.38-1.783 10.345-3.913 14.822-5.29 2.344-.735 4.844-1.452 4.883-1.413zm1.492.387c.077-.02.543.214 1.104.543 4.709 2.693 9.32 6.162 12.963 9.726 1.027 1.008 3.564 3.641 3.525 3.66 0 0-.891.08-1.937.157-8.157.62-18.6 2.343-28.635 4.765-.68.155-1.28.291-1.319.291-.038 0 .716-.756 1.666-1.666 5.89-5.677 8.583-9.261 11.76-15.656.446-.948.834-1.762.873-1.82zm-43.148 4.418c.27.058 2.788 1.239 4.687 2.189 1.744.871 4.361 2.266 4.496 2.383.02.019-.91.503-2.054 1.066a135.033 135.033 0 0 0-10.018 5.522c-.93.562-1.704 1.027-1.723 1.027-.078 0-.058-.078.465-1.027 1.744-3.177 3.14-6.975 3.934-10.676.077-.29.155-.484.213-.484zm-2.52.464c.058.058-.6 2.442-1.008 3.74-.795 2.46-2.131 5.54-3.43 7.866-.31.542-.775 1.338-1.027 1.783l-.484.774-1.084-1.045c-1.26-1.22-2.287-1.978-3.604-2.657-.524-.27-.93-.502-.93-.54 0-.156 3.314-3.159 5.852-5.329 1.82-1.57 5.657-4.65 5.715-4.592zm15.404 6.336.95.62c2.17 1.414 4.726 3.295 6.683 4.94 1.104.91 3.235 2.83 3.662 3.294l.233.252-1.57.447c-8.874 2.46-15.733 4.649-23.735 7.594-.892.33-1.647.6-1.705.6-.116 0-.213.096 1.783-1.745 5.115-4.707 9.65-9.898 13.022-14.955zm-4.05 1.008c.04.04-2.614 3.777-4.203 5.889-1.9 2.519-5.272 6.743-7.598 9.494-.968 1.144-1.8 2.092-1.84 2.111-.058.02-.078-.27-.078-.716 0-2.344-.599-4.844-1.645-6.975-.446-.891-.523-1.104-.425-1.201.368-.33 6.004-3.545 9.568-5.463 2.404-1.28 6.163-3.177 6.22-3.139zM44.1 55.26c.057 0 .502.233 1.007.504a21.28 21.28 0 0 1 3.332 2.248c.04.038-.464.446-1.123.93-1.84 1.317-4.63 3.43-6.258 4.728-1.705 1.356-1.763 1.394-1.57 1.104 1.28-1.957 1.92-3.062 2.598-4.477a36.066 36.066 0 0 0 1.627-4.05c.155-.56.347-.987.386-.987zm6.53 5.113c.097-.018.213.157.735.932 1.104 1.647 1.957 3.857 2.17 5.639l.039.386-2.654 1.028c-4.747 1.84-9.126 3.662-12.09 5.02a217.067 217.067 0 0 0-3.237 1.548c-.95.484-1.724.853-1.724.834 0-.02.6-.465 1.336-1.008 5.794-4.204 10.813-8.816 14.572-13.427.407-.484.775-.93.813-.95zm-3.003.737v.002c.078.077-2.132 2.576-3.643 4.107-3.74 3.816-7.441 6.801-12.033 9.707-.582.368-1.104.697-1.162.735-.135.078.038-.116 2.054-2.305a52.694 52.694 0 0 0 3.352-3.97c.736-.95.871-1.086 1.937-1.84 2.85-2.056 9.418-6.513 9.495-6.436zm25.974 2.3c.274 1.057.78 6.126.918 9.481.04.795.019 1.318-.021 1.318-.154 0-3.273-1.84-5.5-3.236-1.93-1.215-5.579-3.634-6.18-4.113a358.495 358.495 0 0 1 10.783-3.45zm-12.867 4.192c.254.11.635.32 1.404.795 3.991 2.5 9.418 5.522 11.743 6.53.716.31.793.193-.854 1.318-3.526 2.402-7.924 4.765-13.31 7.148-.95.426-1.745.756-1.764.756-.04 0 .077-.486.232-1.067 1.297-4.825 2.036-9.705 2.075-13.619.01-.977.014-1.46.039-1.707l.435-.154zm-2.965 1.055c.094.476.021 4.368-.127 5.494a49.361 49.361 0 0 1-1.78 8.428c-.214.717-.41 1.319-.448 1.357-.078.097-2.732-2.5-3.604-3.508-1.51-1.744-2.692-3.486-3.564-5.191-.404-.79-.987-2.205-1.055-2.518a345.346 345.346 0 0 1 8.592-3.355c.617-.232 1.343-.473 1.986-.707zm-12.603 4.9c.047.069.163.327.271.652.62 1.685 2.013 4.165 3.215 5.754 1.318 1.744 3.043 3.605 4.477 4.825.465.387.89.756.949.814.116.117.155.097-3.004 1.299-3.66 1.395-7.652 2.79-12.225 4.262a609.84 609.84 0 0 0-3.275 1.066c-.175.058-.114-.04.389-.834 2.267-3.544 5.714-10.5 7.652-15.422.33-.853.659-1.706.717-1.9.027-.095.066-.15.103-.211l.73-.305zm-4.01 1.7c-.132.39-.973 2.151-1.842 3.853-1.88 3.663-3.933 7.267-6.684 11.646-.466.755-.91 1.453-.97 1.53-.096.136-.135.098-.446-.502-.659-1.3-1.2-2.965-1.492-4.496-.29-1.511-.232-4.146.098-5.774.15-.717.216-.987.36-1.16a225.041 225.041 0 0 1 10.976-5.098zm33.479 1.2v.813c0 4.321-.465 10.25-1.143 14.57-.116.756-.213 1.377-.232 1.397 0 0-.563-.156-1.221-.35a49.985 49.985 0 0 1-8.912-3.816c-1.88-1.027-4.61-2.714-4.533-2.791.019-.02.832-.445 1.78-.95 3.799-1.975 7.441-4.107 10.6-6.22 1.182-.794 2.963-2.071 3.35-2.42zm-48.048 5.737c.074.004.052.163-.062.851a27.507 27.507 0 0 0-.213 2.07c-.155 2.83.31 4.925 1.705 7.792.388.794.698 1.453.678 1.472-.135.117-12.962 3.875-16.992 4.979-1.201.33-2.247.62-2.325.639-.136.04-.155.021-.097-.309.446-2.848 2.617-6.568 5.64-9.707 2.014-2.093 3.622-3.314 6.373-4.883.921-.524 2.066-1.163 3.057-1.71.737-.401 1.484-.799 2.236-1.194zm30.221 5.404h.002c.02-.02.483.232 1.045.56 4.147 2.404 9.921 4.633 14.842 5.776l.445.096-.619.35c-2.576 1.433-11.045 4.96-19.705 8.195-1.26.465-2.498.93-2.73 1.027-.233.097-.448.155-.448.135 0-.02.35-.698.795-1.531 2.422-4.534 4.863-10.055 6.104-13.891.155-.368.25-.697.27-.717zm-3.08 1.006h.002c.02.02-.136.428-.33.893-1.686 4.088-3.895 8.545-6.724 13.543-.716 1.28-1.317 2.306-1.336 2.306-.02 0-.601-.35-1.3-.775-4.106-2.52-7.75-5.62-10.132-8.623l-.35-.426 1.764-.484c6.316-1.724 11.684-3.584 17.012-5.87.756-.31 1.375-.564 1.394-.564zm19.143 6.686c.02.446-.967 4.437-1.781 7.324-.678 2.422-1.26 4.32-2.327 7.672-.464 1.474-.87 2.693-.89 2.693-.02 0-.135-.018-.252-.056-5.754-1.047-10.908-2.501-15.752-4.438-1.356-.543-3.293-1.415-3.41-1.512-.038-.039 1.124-.581 2.597-1.22 8.816-3.856 17.96-8.235 21.1-10.114.368-.233.657-.35.715-.35zM28.677 96.8c.04.04-2.423 3.585-5.87 8.41-1.203 1.686-2.597 3.661-3.12 4.397a77.468 77.468 0 0 0-1.764 2.596l-.814 1.261-.871-.738c-1.027-.853-2.809-2.673-3.604-3.68-1.666-2.073-2.791-4.264-3.236-6.26-.214-.93-.214-1.394-.02-1.45a1459.308 1459.308 0 0 1 10.31-2.424 861.655 861.655 0 0 0 6.935-1.627c1.124-.271 2.035-.485 2.054-.485zm2.479.95.621.697c2.79 3.12 5.637 5.425 9.086 7.44.62.35 1.086.659 1.047.679-.135.096-11.974 4.3-17.457 6.2a462.503 462.503 0 0 1-5.639 1.956c-.019 0-.194-.117-.387-.252l-.35-.252.563-.814c1.82-2.635 4.107-5.521 9.086-11.528zm15.463 11.062c.019-.02.87.29 1.918.68 2.519.949 4.513 1.55 7.187 2.228 3.294.833 8.061 1.646 10.872 1.88.426.037.657.076.58.134-.136.077-2.985 1.028-5.077 1.686-3.333 1.047-13.504 4.05-21.797 6.433a218.736 218.736 0 0 1-2.925.834c-.194.038-.834-.138-.834-.215 0-.038.465-.638 1.027-1.297 2.79-3.333 5.561-7.054 7.867-10.58.64-.969 1.182-1.764 1.182-1.783zm-3.412.098h.002c.019.02-1.357 2.227-3.76 6.025-1.026 1.608-2.17 3.432-2.576 4.07-.388.62-.971 1.59-1.3 2.131l-.56.987-.29-.076c-.699-.195-5.601-1.919-6.9-2.442a48.226 48.226 0 0 1-4.513-2.072c-1.55-.834-3.487-2.074-3.332-2.113.038-.02 2.692-.736 5.889-1.608 8.485-2.306 13.194-3.642 16.275-4.611.562-.175 1.046-.311 1.065-.291zm24.123 5.656h.021c.077.195-3.063 8.913-4.207 11.664-.25.62-.348.776-.484.756-.33-.02-4.881-.657-7.652-1.064-4.824-.736-12.925-2.15-14.958-2.616l-.464-.097 2.886-.659c6.2-1.395 9.184-2.15 12.207-3.08a86.251 86.251 0 0 0 11.413-4.4c.6-.27 1.102-.483 1.238-.502z"/></g></svg>SQL Server</span>
  <span class="cap"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><g transform="translate(0.87 0.87) scale(0.92754)"><rect x="2.5" y="5" width="7" height="5.5" rx="1.3"/><rect x="14.5" y="13.5" width="7" height="5.5" rx="1.3"/><path d="M6 10.5v3.5a2 2 0 0 0 2 2h3"/><path d="M18 13.5V10a2 2 0 0 0-2-2h-3"/><path d="m11 14.5-1.6 1.5 1.6 1.5"/><path d="m13 9.5 1.6-1.5L13 6.5"/></g></svg>Local network link</span>
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
      <h2>Two editions, the same product underneath.</h2>
    </div>
    <p class="intro">
      Start on your own computer: download it and it works, with no server to set up, no
      account to make and nothing to configure before you can ask it something. When your
      company needs one installation everybody works in, that is Enterprise.
    </p>
  </div>

  <div class="eds">
    <div class="ed now">
      <h3>Flexie SAG Personal</h3>
      <p class="who">Everything on one computer, answering only to you. Free.</p>
      <ul>
        <li>Ready to use in about a minute</li>
        <li>Your conversations stay on your disk</li>
        <li>Reaches your own files and folders</li>
        <li>Keeps itself up to date</li>
      </ul>
      <div class="foot">
        <div class="get">
          {{if .Mac}}
          <a class="btn" href="{{.Mac.URL}}"><svg class="pf" width="25" height="25" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M16.37 12.76c.02 2.66 2.33 3.54 2.36 3.56-.02.06-.37 1.27-1.22 2.51-.73 1.08-1.5 2.15-2.7 2.17-1.18.02-1.56-.7-2.9-.7-1.35 0-1.77.68-2.88.72-1.17.04-2.06-1.16-2.8-2.23-1.5-2.19-2.66-6.19-1.11-8.89.77-1.34 2.14-2.19 3.63-2.21 1.13-.02 2.2.77 2.9.77.69 0 1.99-.95 3.36-.81.57.02 2.18.23 3.2 1.75-.08.05-1.91 1.12-1.89 3.36M14.2 4.6c.62-.75 1.03-1.79.92-2.83-.89.04-1.97.6-2.61 1.35-.57.66-1.07 1.72-.94 2.74.99.08 2-.51 2.63-1.26"/></svg>Download for Mac</a>
          {{else}}
          <a class="btn ghost" href="mailto:sales@flexie.io?subject=Flexie%20SAG%20Personal">Tell me when it is ready</a>
          {{end}}
          {{if .Win}}
          <a class="btn" href="{{.Win.URL}}"><svg class="pf" width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M3 5.6l7.2-1v7.1H3V5.6m0 12.8l7.2 1v-7H3v6M11.2 4.4L21 3v8.7h-9.8V4.4m0 15.2L21 21v-8.6h-9.8v7.2"/></svg>Download for Windows</a>
          {{end}}
        </div>

      </div>
    </div>
    <div class="ed">
      <h3>Flexie SAG Enterprise</h3>
      <p class="who">One installation your whole company works in, on your own servers.</p>
      <ul>
        <li>People, teams and who may do what</li>
        <li>Shared models, tools and knowledge</li>
        <li>Every permission checked on every request</li>
        <li>Runs on your own servers</li>
      </ul>
      <div class="foot">
        <a class="btn" href="mailto:sales@flexie.io?subject=Flexie%20SAG%20Enterprise"><svg class="pf" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6.5h18v11H3z"/><path d="m3 7 9 6 9-6"/></svg>Talk to us</a>
        <p class="under"><a href="mailto:sales@flexie.io">sales@flexie.io</a> &middot; taking early customers</p>
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
      Open the console, go to <strong>Inference</strong> and add a machine. It hands you a
      single line to paste into a terminal on that machine, already filled in: the address
      of your own installation, and a token that stands for an hour. There is nothing to
      look up and nothing to type.
    </p>
    <p>
      Paste it and the machine does the rest. It reads the graphics card, fetches the build
      that matches it and starts a service that comes back after a reboot. Nothing is
      compiled on your machine, and if the card is one we do not publish for, it says so and
      writes nothing at all.
    </p>
    <p>
      The machine then appears in the console on its own, and the models on it can be given
      to whichever workspaces should have them.
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
      You do not need any of this to install: the installer picks the right build and
      checks it. It is here because a download you cannot verify is a download you are
      trusting on faith. Beside each file is a <code class="inline">.sha256</code>, a
      short fingerprint of it, so you can confirm the file you received is the file we
      published and not something altered on the way. And
      <a href="{{.Download}}/builds.txt">builds.txt</a> is the plain list of which builds
      exist right now, for anyone mirroring them or checking before they start.
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
  <span>A product by <a href="https://flexie.io/">Flexie CRM</a></span>
  <a href="mailto:sales@flexie.io">sales@flexie.io</a>
  <a href="https://github.com/flexie-crm/smart-agent-gateway">Source</a>
  {{if .Mac}}<a href="{{.Mac.URL}}">Download for Mac</a>
  {{if .Mac.Version}}<span>Personal {{.Mac.Version}}</span>{{end}}{{end}}
</footer>
</div>

</body>
</html>
`))
