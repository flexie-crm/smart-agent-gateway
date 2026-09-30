// lib/content-normalizer.ts
// ─── Markdown content normalization pipeline ────────────────────────────────
// Each normalizer is an isolated, composable function: (text: string) => string
// The pipeline runs all normalizers in sequence.
// Used by both Response (main content) and ReasoningContent (reasoning text).

type Normalizer = (text: string) => string;

// ─── 1. Code Fence Boundary Normalizer ──────────────────────────────────────
// Models sometimes glue text to ``` boundaries:
//   - Closing fence glued to text: ```Some text  →  ```\nSome text
//   - Text glued to opening fence: text```json   →  text\n```json
// Markdown requires ``` to be on its own line.
// Processes line-by-line, tracks fence open/close state.

const normalizeCodeFenceBoundaries: Normalizer = (text) => {
  const lines = text.split('\n');
  const out: string[] = [];
  let inFence = false;

  for (const line of lines) {
    const trimmed = line.trimStart();

    if (inFence) {
      // Inside a fence — look for closing ```
      if (trimmed.startsWith('```')) {
        const after = trimmed.slice(3);
        if (!after || !after.trimStart()) {
          // Clean close
          out.push(line);
          inFence = false;
        } else {
          // ```glued text → split
          const indent = line.slice(0, line.length - trimmed.length);
          out.push(indent + '```');
          out.push(after.trimStart());
          inFence = false;
        }
      } else {
        out.push(line);
      }
    } else {
      // Outside a fence
      if (trimmed.startsWith('```')) {
        // Opening fence — language id is fine: ```json, ```chart
        out.push(line);
        // Enter fence mode unless this is a single-line fence like ```text```
        if (!trimmed.slice(3).includes('```')) {
          inFence = true;
        }
      } else if (trimmed.includes('```')) {
        // ``` appears mid-line: "some text```json" or "some text```"
        const idx = line.indexOf('```');
        const before = line.slice(0, idx).trimEnd();
        const fencePart = line.slice(idx);
        if (before) {
          out.push(before);
          out.push(fencePart);
          if (!fencePart.slice(3).includes('```')) {
            inFence = true;
          }
        } else {
          out.push(line);
        }
      } else {
        out.push(line);
      }
    }
  }

  return out.join('\n');
};

// ─── 2. Close Unclosed Code Fences ──────────────────────────────────────────
// During streaming, the model may have sent an opening ``` but not the closing
// one yet. Append a closing ``` so ReactMarkdown renders a proper <pre> block.

const closeUnmatchedCodeFences: Normalizer = (text) => {
  const count = (text.match(/```/g) || []).length;
  if (count % 2 === 1) {
    return text + '\n```';
  }
  return text;
};

// ─── 3. Close Unclosed Bold (**) ────────────────────────────────────────────

const closeUnmatchedBold: Normalizer = (text) => {
  const pairs = (text.match(/\*\*/g) || []).length;
  if (pairs % 2 === 1) {
    return text + '**';
  }
  return text;
};

// ─── Where markdown is not markdown ─────────────────────────────────────────

/** Whether a character can be part of a word, for the emphasis rules below. */
const isWordCharacter = (c: string | undefined): boolean =>
  c !== undefined && /[\p{L}\p{N}]/u.test(c);

/**
 * Which characters of the text are inside a code span or a fence.
 *
 * Underscores in there are not emphasis and never were: `wait_for` in
 * backticks is a name, and counting it as an italic delimiter is what put a
 * stray underscore at the end of a finished message.
 *
 * Fences are walked first so a backtick inside one cannot toggle an inline
 * span, which would invert every judgement after it.
 */
const codeMask = (text: string): boolean[] => {
  const masked = new Array<boolean>(text.length).fill(false);
  let i = 0;
  let inFence = false;
  let inSpan = false;
  while (i < text.length) {
    if (text.startsWith('```', i)) {
      inFence = !inFence;
      for (let j = i; j < i + 3 && j < text.length; j++) masked[j] = true;
      i += 3;
      continue;
    }
    if (inFence) {
      masked[i] = true;
      i++;
      continue;
    }
    if (text[i] === '`') {
      inSpan = !inSpan;
      masked[i] = true;
      i++;
      continue;
    }
    masked[i] = inSpan;
    i++;
  }
  return masked;
};

// ─── 4. Close Unclosed Italic (__) ──────────────────────────────────────────

const closeUnmatchedDoubleUnderscore: Normalizer = (text) => {
  // Code spans excluded for the reason the single-underscore rule excludes
  // them: `__init__` in backticks is a name, not bold that somebody forgot to
  // close.
  const inCode = codeMask(text);
  let pairs = 0;
  for (let i = 0; i < text.length - 1; i++) {
    if (text[i] === '_' && text[i + 1] === '_' && !inCode[i]) {
      pairs++;
      i++;
    }
  }
  if (pairs % 2 === 1) {
    return text + '__';
  }
  return text;
};

// ─── 5. Close Unclosed Single Asterisk Italic (*) ───────────────────────────

const closeUnmatchedSingleAsterisk: Normalizer = (text) => {
  // Count single * that aren't part of **
  let count = 0;
  for (let i = 0; i < text.length; i++) {
    if (text[i] === '*' && text[i - 1] !== '*' && text[i + 1] !== '*') {
      count++;
    }
  }
  if (count % 2 === 1) {
    return text + '*';
  }
  return text;
};

// ─── 6. Close Unclosed Single Underscore Italic (_) ─────────────────────────

const closeUnmatchedSingleUnderscore: Normalizer = (text) => {
  const inCode = codeMask(text);
  let count = 0;
  for (let i = 0; i < text.length; i++) {
    if (text[i] !== '_') continue;
    if (text[i - 1] === '_' || text[i + 1] === '_') continue;

    // Not emphasis, and this is where the stray underscore came from. Two
    // kinds of underscore cannot open or close italics, and this counted both:
    //
    //   - one inside a code span. `wait_for` is a name.
    //   - one INSIDE A WORD. CommonMark does not let an underscore flanked by
    //     word characters open or close emphasis, which is exactly so that
    //     snake_case renders as itself. An asterisk has no such rule, which is
    //     why the asterisk normalizer beside this one is right as it stands.
    //
    // Either one made the count odd, and an odd count appended a closing
    // underscore to a message that was already finished. Anything with one
    // snake_case word in it ended in a visible "_".
    if (inCode[i]) continue;
    if (isWordCharacter(text[i - 1]) && isWordCharacter(text[i + 1])) continue;

    count++;
  }
  if (count % 2 === 1) {
    return text + '_';
  }
  return text;
};

// ─── 7. Close Unclosed Strikethrough (~~) ───────────────────────────────────

const closeUnmatchedStrikethrough: Normalizer = (text) => {
  const pairs = (text.match(/~~/g) || []).length;
  if (pairs % 2 === 1) {
    return text + '~~';
  }
  return text;
};

// ─── 8. Close Unclosed Inline Code (`) ──────────────────────────────────────
// Only when we're NOT inside an unclosed code fence (``` count is even).

const closeUnmatchedInlineCode: Normalizer = (text) => {
  const tripleCount = (text.match(/```/g) || []).length;
  // If inside an unclosed code fence, don't touch inline code
  if (tripleCount % 2 === 1) return text;

  let singleCount = 0;
  for (let i = 0; i < text.length; i++) {
    if (text[i] === '`') {
      const isTripleStart = text.substring(i, i + 3) === '```';
      const isTripleMid = i > 0 && text.substring(i - 1, i + 2) === '```';
      const isTripleEnd = i > 1 && text.substring(i - 2, i + 1) === '```';
      if (!(isTripleStart || isTripleMid || isTripleEnd)) {
        singleCount++;
      }
    }
  }
  if (singleCount % 2 === 1) {
    return text + '`';
  }
  return text;
};

// ─── 9. Remove Incomplete Links/Images ──────────────────────────────────────
// Trailing [text or ![text with no closing ] — remove to prevent broken render.

const removeIncompleteLinks: Normalizer = (text) => {
  const match = text.match(/(!?\[)([^\]]*?)$/);
  if (match) {
    const idx = text.lastIndexOf(match[1]);
    return text.substring(0, idx);
  }
  return text;
};

// ─── Pipeline ───────────────────────────────────────────────────────────────
// Order matters:
// 1. Structural (fence boundaries) first
// 2. Then fence closing (depends on boundary normalization)
// 3. Then inline formatting closers
// 4. Links last (removes content, so must be after formatting)

const pipeline: Normalizer[] = [
  normalizeCodeFenceBoundaries,
  closeUnmatchedCodeFences,
  closeUnmatchedBold,
  closeUnmatchedDoubleUnderscore,
  closeUnmatchedSingleAsterisk,
  closeUnmatchedSingleUnderscore,
  closeUnmatchedStrikethrough,
  closeUnmatchedInlineCode,
  removeIncompleteLinks,
];

/**
 * Normalize streaming markdown content for safe rendering.
 * Runs all normalizers in sequence. Safe to call on every render frame.
 */
export function normalizeMarkdown(text: string): string {
  if (!text || typeof text !== 'string') return '';
  return pipeline.reduce((t, fn) => fn(t), text);
}

// Export individual normalizers for direct use or testing
export {
  normalizeCodeFenceBoundaries,
  closeUnmatchedCodeFences,
  closeUnmatchedBold,
  closeUnmatchedDoubleUnderscore,
  closeUnmatchedSingleAsterisk,
  closeUnmatchedSingleUnderscore,
  closeUnmatchedStrikethrough,
  closeUnmatchedInlineCode,
  removeIncompleteLinks,
};
