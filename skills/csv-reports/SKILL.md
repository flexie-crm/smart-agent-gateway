---
name: csv-reports
description: "Use when somebody has a CSV file and wants it turned into something they can read or send on, such as a styled Excel workbook with totals, or a quick summary of what is in each column. Also use when asked to check or profile a CSV before trusting it: row counts, blank cells, column types, duplicate keys."
license: Apache-2.0
metadata:
  title: CSV Reports
---

# CSV Reports

Turns a CSV file into something a person can use: an Excel workbook they can
send to somebody, or a summary that says what is actually in the file.

Two scripts, and they answer different questions.

## Which script to use

| The person wants | Run |
|---|---|
| a spreadsheet they can open, style and send | `scripts/to_excel.py` |
| to know what is IN the file before trusting it | `scripts/summary.js` |

Both take the CSV as their first argument and write next to it unless told
otherwise. Neither ever edits the CSV it was given.

## Making a workbook

```
scripts/to_excel.py <input.csv> [output.xlsx]
```

It writes a real `.xlsx`: a bold header row that stays put when you scroll,
columns widened to fit what is in them, numbers right-aligned, and a totals row
under every column that is entirely numeric. If the output path is left out it
writes beside the input with the same name and an `.xlsx` extension.

Say what it did afterwards, including the sheet name and the number of rows, so
the person knows what to open.

## Profiling a file

```
scripts/summary.js <input.csv>
```

It prints one block per column: the type it looks like, how many values are
blank, the range for numbers, and the commonest values for text. At the end it
says whether any row is a duplicate of another.

Read the output before answering. It is how you tell "the file has 900 rows" from
"the file has 900 rows and 400 of them have no price".

## Before the first run, on a machine that has not used this skill

**Both scripts need a library that is not part of Python or Node.** The first
run on a new machine will fail saying which one, and that is expected rather
than a problem: install it and run the script again.

- `to_excel.py` needs **openpyxl**
- `summary.js` needs **csv-parse**

`references/setup.md` has the exact commands, including what to do about
"externally-managed-environment", which is what a recent Python on macOS or
Debian answers to a plain `pip install`.

Do not rewrite a script to avoid a missing library. Install the library.

## What to check before you say it worked

- the output file exists and is not zero bytes
- the row count matches what `summary.js` reported for the same file
- if the CSV had a column of numbers, the workbook has a totals row

## Sample

`assets/sample-sales.csv` is a small, deliberately messy file: blank prices, a
duplicate row, a column that looks numeric but is not. Use it to check the
scripts run at all before pointing them at somebody's real data.
