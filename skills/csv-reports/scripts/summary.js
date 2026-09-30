#!/usr/bin/env node
// Say what is actually in a CSV, so nobody answers a question from a file they
// have not looked at.
//
// One block per column: what it looks like, how much of it is missing, the
// range or the commonest values. Then whether any row is a duplicate.
//
// Needs csv-parse, which is not part of Node. If it is missing this exits with
// a message naming it: install it (references/setup.md) and run this again
// rather than rewriting the script to avoid it.

let parse;
try {
  ({ parse } = require("csv-parse/sync"));
} catch (missing) {
  if (missing.code !== "MODULE_NOT_FOUND") throw missing;
  process.stderr.write(
    "this script needs the csv-parse library and it is not installed.\n" +
      "install it with:  npm install --prefix <the skill_dir in this answer> csv-parse\n" +
      "see references/setup.md for where it has to go and why\n",
  );
  process.exit(3);
}

const fs = require("fs");

function shape(values) {
  const present = values.filter((v) => v !== null && String(v).trim() !== "");
  const blank = values.length - present.length;
  const numbers = present
    .map((v) => Number(String(v).replace(/,/g, "")))
    .filter((n) => Number.isFinite(n));

  // All of what is there is a number: report it as one. A column of mostly
  // numbers with a "n/a" in it is text, and saying so is the point.
  if (present.length > 0 && numbers.length === present.length) {
    const total = numbers.reduce((a, b) => a + b, 0);
    return {
      kind: "number",
      blank,
      min: Math.min(...numbers),
      max: Math.max(...numbers),
      total,
      mean: total / numbers.length,
    };
  }

  const counts = new Map();
  for (const value of present) {
    const key = String(value);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const common = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 3);
  return { kind: "text", blank, distinct: counts.size, common };
}

function main(argv) {
  const source = argv[2];
  if (!source) {
    process.stderr.write("usage: summary.js <input.csv>\n");
    return 2;
  }
  if (!fs.existsSync(source)) {
    process.stderr.write(`there is no file at ${source}\n`);
    return 1;
  }

  const rows = parse(fs.readFileSync(source), { columns: true, skip_empty_lines: true });
  if (rows.length === 0) {
    process.stderr.write(`${source} has no rows\n`);
    return 1;
  }
  const columns = Object.keys(rows[0]);

  console.log(`${source}: ${rows.length} rows, ${columns.length} columns\n`);
  for (const column of columns) {
    const found = shape(rows.map((row) => row[column]));
    const missing = found.blank > 0 ? `  ${found.blank} blank` : "";
    if (found.kind === "number") {
      console.log(
        `${column}  [number]${missing}\n` +
          `  min ${found.min}  max ${found.max}  total ${found.total}  mean ${found.mean.toFixed(2)}`,
      );
    } else {
      const common = found.common.map(([v, n]) => `${v} (${n})`).join(", ");
      console.log(`${column}  [text]${missing}\n  ${found.distinct} distinct: ${common}`);
    }
  }

  // Duplicates last, because it is a statement about the file rather than about
  // a column, and it is the one that most often changes an answer.
  const seen = new Set();
  let duplicates = 0;
  for (const row of rows) {
    const key = JSON.stringify(columns.map((c) => row[c]));
    if (seen.has(key)) duplicates += 1;
    seen.add(key);
  }
  console.log(
    `\n${duplicates === 0 ? "no duplicate rows" : `${duplicates} duplicate row(s): the same values appear more than once`}`,
  );
  return 0;
}

process.exit(main(process.argv));
