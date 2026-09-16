# lib/sqlserver — reading T-SQL, held in the tree

`lib/` is where third-party source lives when we hold it ourselves instead of
fetching it. One folder per scope: everything needed for one job sits together,
with its licences beside it.

This scope is **reading Transact-SQL**, which is what the SQL Server query policy
rests on (`internal/sqlguard/sqlserver`, KB/34). Nothing here is ours.

```
lib/sqlserver/
  antlr/   the parsing engine       BSD 3-Clause, The ANTLR Project
  tsql/    the T-SQL parser         BSD 3-Clause, Bytebase
           the grammar (.g4)        MIT, five holders (see the file)
```

**Nothing in here is fetched from the network.** `go.mod` names neither, and a
build with no internet produces the same binary.

## The two halves

**`antlr/`** is the engine every ANTLR parser calls into: stepping through
tokens, tracking position, reporting an error. There is nothing about SQL in it.
54 files, ~576 KB, taken from `github.com/antlr4-go/antlr/v4` v4.13.1.

One change was made to it, and only one: `golang.org/x/exp/slices` became the
standard library's `slices`, in `lexer_action_executor.go`. It used two functions
from it (`Equal`, `EqualFunc`), both of which have been in the standard library
since Go 1.21 with the same signatures. That removed the last thing this folder
needed from outside. The upstream test files were not taken: they are ANTLR's own
and would need the same import rewriting on every update, for no benefit here.

**`tsql/`** is the parser for T-SQL specifically, from
`github.com/bytebase/parser` at commit `57b6ef7a2640` (2026-04-17).

Two kinds of file in it, and the difference matters:

- **`TSqlLexer.g4`, `TSqlParser.g4`** — the grammar. ~7,500 lines, readable, and
  the only thing anybody would edit. A rule looks like this:

  ```
  table_sources
      : source+=table_source (',' source+=table_source)*
      ;
  ```

- **everything ending `.go`** — ~9.5 MB written by a tool (ANTLR) from that
  grammar. **Nobody edits these.** A change made here is silently lost the moment
  anybody regenerates, and nothing in the file would show it had been deliberate.

`examples/` is 142 T-SQL files from upstream, used as a regression corpus if the
grammar is ever changed. They are no evidence about coverage: they were written
to pass.

## If the grammar ever has to change

It probably never will. The files here work, and they are tested
(`tsql/vendored_test.go`, plus the whole of `internal/sqlguard/sqlserver`). This
is written down for the day somebody needs it, not as a routine step.

A known example: `TABLESAMPLE` is not in the grammar, so a statement using it is
refused. Refusing is the safe direction, and if it ever matters:

```sh
# needs ANTLR (Java): brew install antlr
cd orchestrator/lib/sqlserver/tsql
antlr -Dlanguage=Go -package tsql -visitor -o . TSqlLexer.g4 TSqlParser.g4

# ANTLR writes the upstream import path back in; point it at our copy again
sed -i '' 's|"github.com/antlr4-go/antlr/v4"|"flexie.io/sag/lib/sqlserver/antlr"|' *.go
gofmt -w .
```

Then run `go test ./lib/... ./internal/sqlguard/sqlserver/` before committing the
regenerated output. That `sed` is the price of holding the runtime ourselves, and
it is why it is written down here rather than remembered.

This is a developer step on purpose: the generated files are committed, so an
ordinary build needs no Java and no ANTLR.

## Licences

Every licence file here ships with the code and must not be removed. All three
are permissive and none places any obligation on the rest of this work; they
require only that their notices travel with it. `COPYRIGHT` at the repository
root names them too.

| what | licence | file |
|---|---|---|
| the parsing engine | BSD 3-Clause, The ANTLR Project | `antlr/LICENSE` |
| the generated parser | BSD 3-Clause, Bytebase | `tsql/LICENSE.bytebase` |
| the grammar | MIT, grammars-v4 | `tsql/LICENSE.grammar` |

## What it costs

Measured, so nobody has to wonder:

- **+18 MB** on the `sag` binary, which reaches the desktop installers.
- **+29 s** on a cold build, once per machine: Go caches the package and nothing
  in here changes.
- **`make vet` skips `lib/`.** Leaving the package out of the list it is given
  does not work, and that was tried first: `go vet` follows imports and prints a
  dependency's diagnostics along with the importer's. So the output is filtered,
  with a control confirming real problems elsewhere still fail the build.
- **`golangci-lint` skips `lib/` by path**, set in `.golangci.yml`. The
  generated parser needs no help (the linter honours the
  `// Code generated ... DO NOT EDIT.` line by itself, measured at zero findings
  with no exclusion), but the ANTLR runtime beside it is hand-written and is not
  skipped that way: it reports 40-odd findings, none of them ours to fix.
