# SAG Personal on Windows

Everything Windows-shaped about building the personal edition, kept together so
nothing here can disturb the macOS build, which is the proven one.

```powershell
.\desktop\personal\windows\build.ps1               # the installer
.\desktop\personal\windows\build.ps1 -PayloadOnly  # everything except the installer
.\desktop\personal\windows\e2e.ps1                 # drive what was built
```

| file | what it does |
|---|---|
| `mariadb.ps1` | bundles MariaDB from the official Windows ZIP, ~24 MB |
| `build.ps1` | pages, gateway, database, payload, installer |
| `icon.ps1` | re-renders the mark without the macOS grid, into `../icons/windows/` |
| `e2e.ps1` | the desktop gate, the same specs as macOS |

## What is different here, and what is not

The **product** is not different. It is the same `sag` binary, the same two
front ends, the same schema and the same migrations, laid out in the same
payload the macOS build produces, and `internal/localdb` reads one bundle shape
on both platforms. What is Windows-shaped is packaging and three things the
operating system does differently.

**There is nothing to relocate.** The macOS bundler exists mostly to rewrite
absolute library paths into `@executable_path`, because a packaged `mariadbd`
links its dependencies by absolute path and runs on the build machine and
nowhere else. Windows resolves a DLL beside the executable that loaded it, so
the official ZIP is already relocatable and bundling is a copy. That is why
there is one script here where macOS needs two.

**The server is `bin\server.dll`.** `mariadbd.exe` is an 11 KB launcher. Every
DLL in the ZIP's `bin\` comes along (the C++ runtime, zlib, libcurl, about 2 MB
against the 21 MB server); what makes the bundle small is leaving out the forty
client programs, the 92 MB of debug symbols, the headers and the twenty-eight
message translations. No plugin is copied because none is needed: InnoDB is
compiled into `server.dll` rather than loaded, and the bundler asserts that the
server resolves `plugin-dir` inside the bundle so a future build that changed
its mind fails here rather than on somebody's laptop.

**The database is on loopback, not a socket.** `chooseTransport` already knew
this. The consequence it did not have is that the port has to be REMEMBERED,
because a server left behind by a run that was killed can only be asked to stop
by connecting to it.

**There is no signal.** This is the one that reaches furthest, and it cost three
real defects (all now fixed and tested, see `KB/36`):

- `mariadbd` reads SIGTERM as "shut down" and Windows has none, so the request
  goes over a connection instead: `SHUTDOWN` is a statement, and it travels the
  same connection the readiness probe already proved. The account is granted
  that one global privilege at bootstrap for it.
- The gateway cannot be reached either. `taskkill` without `/F` asks a window to
  close, and a gateway has none: it failed on BOTH processes, so closing the
  application used to leave the whole stack running. The application now asks
  through the state directory both sides already share, and the gateway watches
  for it (`cmd/sag/personal_stop.go`).
- Asking whether a process is alive by sending it signal 0 does not fail on
  Windows, it answers wrongly: a running process reads as gone. That is what
  `supervise.Running` exists for.

**The server keeps its own voice.** Without `--console` it takes stderr over and
writes to an error log named after the machine, inside the data directory, so
the log the supervisor holds was completely empty.

**And what the platform supplies when you do not.** Three more, all found by
double-clicking the installed application rather than starting it from a
terminal, which is why `KB/36` says to do that once before automating anything:

- **A console program with no console gets a new one.** A black terminal window
  opened beside the application and stayed. `supervise.HideConsole` and the
  shell's `detached` pass `CREATE_NO_WINDOW`; both are needed, because once the
  gateway has no console its own children each get one.
- **The icon grid is Apple's.** The sources are drawn on it (a 1024 canvas with
  the tile inset to 824), Windows has none, and the same artwork reads as a
  smaller application than everything beside it. `icon.ps1` re-renders the mark
  from the vector with the grid cropped off.
- **The installer's icon is a separate setting** (`installerIcon`), and unset
  means NSIS's own.

And one that was not Windows' fault: `desktop/shared/icons/node_modules` was a
unix symlink committed to the repository, which a Windows checkout writes out as
a text file containing the path. It is ignored now.

## There is no engine on Windows, and no local models

This edition hosts its models. Decided in KB/36 and not an omission: a graphics
build is compiled for ONE compute capability and there are seven, they cannot
all go in one installer, and asking somebody which card they own is asking them
to get it wrong. The processor build is the only one that runs everywhere, and
it is slower by about three orders of magnitude (45.8 s to first token against
46 ms), which is not a product but a way to make somebody believe the whole
application is broken. macOS has no equivalent question because Metal is one
target, so it keeps its engine and is untouched by this.

It is one constant, `config.EngineBundled`, false on this platform by build tag.
Two things read it and cannot drift apart: the personal boot, which starts no
node, and `/v1/meta`, which is how the console knows. The console is built with
`VITE_SAG_LOCAL_MODELS=0` alongside it, so the Machines menu item, its route and
the "Add local model" button are not in the bundle at all.

Nothing degrades and no screen apologises for itself. Every hosted vendor works
exactly as it does on macOS; what is absent is absent everywhere, at once.

## What it needs on the machine

Go (the version in `orchestrator/go.mod`), Node 22 or newer, and a Rust
toolchain with the MSVC build tools. `build.ps1` checks for each by name before
it builds anything, rather than failing three minutes in with a message from
whatever tried to use it.

## Signing

Nothing is signed, so SmartScreen warns on every download until reputation
accumulates. That is paperwork rather than code: since June 2023 an OV
code-signing key must live in hardware or a cloud HSM, so there is no `.pfx` to
put in a repository secret. KB/36 records the two routes. Nothing in these
scripts changes when there is an identity.
