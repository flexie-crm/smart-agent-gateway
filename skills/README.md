# Skills

Packages in the Agent Skills format, kept here so the ones we ship or test with
are in the repository rather than in somebody's downloads folder.

A skill is a DIRECTORY whose name equals the `name` in its `SKILL.md`
frontmatter, with `SKILL.md` at its root. Zip the directory, not its contents:

    cd skills && zip -r ../csv-reports.zip csv-reports

Then import it on the Skills screen, or:

    curl -X POST -H "Authorization: Bearer $TOKEN" \
         -F "file=@csv-reports.zip" http://localhost:8080/v1/skills

## csv-reports

A real one, and the one to test the agent side with, because it needs something
it does not have. Two scripts in two languages, each needing a library that is
not part of that language:

  scripts/to_excel.py   CSV -> a styled .xlsx with totals. Needs openpyxl.
  scripts/summary.js    CSV -> a profile of every column. Needs csv-parse.

The first run of either FAILS, saying which library is missing and how to
install it, and `references/setup.md` has the commands including what to do
about "externally-managed-environment" (which is what a recent Python on macOS
or Debian answers to a plain `pip install`). That is the scenario: the agent
runs a script, reads the failure, installs the library with the terminal, and
runs it again.

`assets/sample-sales.csv` is deliberately messy: a blank price, a genuinely
duplicated order line, and a `notes` column that is mostly empty with an "n/a"
in it, so a column that looks numeric is not.

Both scripts were run for real before this was committed: the workbook has a
frozen bold header and SUM formulas over exactly the numeric columns, and the
profile finds the blank price, the duplicate, and the two text columns.
