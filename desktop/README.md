# The desktop applications

Two of them, from one workspace.

| | folder | what it is |
|---|---|---|
| **SAG Personal** | `personal/` | One person, one machine. Carries the gateway, a bundled MariaDB, both front ends and the inference node. Nothing to set up and nothing to connect to. |
| **SAG Enterprise** | `enterprise/` | The chat, against a server somebody else runs. Carries none of the above: it asks for an address, checks it, and opens a window on it. |
| shared | `shared/` | What both genuinely share, which is deliberately little: opening an outside address in the person's own browser, the failure and progress reporting on the waiting page, and the icons. |

The console is **not** in either. It lives on the web, served by the
orchestrator, because that is where an administrator already works.

## Why Enterprise is an application and not a bookmark

So the assistant can reach the computer it is installed on. A web page cannot
read a file on somebody's disk or run a command for them; the Rust half can,
and that is where those tools will live. The window is the part that exists
today; the tools are the reason it is a window rather than a tab.

## Building

    ./desktop/personal/build.sh --universal      → desktop/.local/out/personal/
    ./desktop/personal/dev.sh                    → the fast loop, no build

Both editions build from the one Cargo workspace at `desktop/`, so
`cargo build`, `cargo test` and `cargo clippy` at that level cover them and the
shared crate together. Everything else about the personal edition, including
what its installer contains and why the database is bundled, is in
[KB/36](../KB/36-desktop-application.md).

## Which edition a page is built for

A build-time flag, `VITE_SAG_PERSONAL=1`, set only by the personal edition's
build. Without it the pages are the ordinary ones: sign-in, workspace
switching, sign-out, user administration. It is not a security boundary and
never has been: the passwordless sign-in route is not registered at all off the
desktop, so a page built wrong finds a button that answers 404.
