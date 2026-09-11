# Deploying and building Flexie SAG

Everything needed to develop, build, sign, release and run this product.

Every command here was read out of the file that runs it. Where something is
known to be broken or untested, it says so.

**Contents**

1. [What you need installed](#1-what-you-need-installed)
2. [Development](#2-development)
3. [Building the applications](#3-building-the-applications)
4. [Signing and releasing](#4-signing-and-releasing)
5. [Production](#5-production)
6. [Secrets and keys](#6-secrets-and-keys)
7. [Setting up a new build machine](#7-setting-up-a-new-build-machine)
8. [When it goes wrong](#8-when-it-goes-wrong)

---

## 1. What you need installed

| | Version | Needed for |
|---|---|---|
| Go | 1.26 | the server, always |
| Node | 22 or newer | both front ends, always |
| Rust | stable | the desktop applications and the inference node |
| MariaDB | 11.4 | the server in development and in production |
| PostgreSQL | 17 | the test suite only |
| Docker | any current | production images, and building the inference node |

**macOS additionally:** Xcode command line tools, and for signing an Apple
Developer account.

**Windows additionally:** PowerShell 7, and Visual Studio Build Tools with the
C++ workload (Rust needs the MSVC linker).

Check what you have:

```sh
go version && node --version && cargo --version && docker --version
```

### A trap worth knowing before it costs you an hour

If your Go is **newer** than the one in `go.mod`, the linter's type checker
panics with `file requires newer Go version`. The Makefile already forces the
pinned toolchain when it runs the linter, so `make lint` works — but if you run
`golangci-lint` by hand, set `GOTOOLCHAIN` to match `go.mod` first.

---

## 2. Development

### The four loops

Pick the one that matches what you are changing.

```sh
make dev           # the server, rebuilt and restarted on every Go file you save
make ui-dev        # the chat, on :5173
make admin-dev     # the console, on :5174
make dev-personal  # the WHOLE desktop product on :8123, pages reloading
```

`make dev` runs the server only. It serves **no front end** unless you tell it
where the pages are:

```sh
SAG_CONSOLE_DIR=admin-ui/dist SAG_CHAT_DIR=chat-ui/dist/app make dev
```

Both of those also accept an **address** instead of a directory, which is what
`make dev-personal` uses to put the dev servers behind the gateway. That matters
because the origin, the cookies and the paths are then the shipped
application's, not a browser's idea of them.

### Running the whole personal edition from source

```sh
make dev-personal                      # pages served by vite, reloading
make dev-personal DEV_ARGS=--built     # serve the built pages instead
```

Ports: the app on `8123`, the console dev server on `5174`, the chat on `5173`.
Override with `SAG_DESKTOP_PORT`, `SAG_CONSOLE_DEV_PORT`, `SAG_CHAT_DEV_PORT`.

This is for building the product *inside* the application. To prove the
**application itself**, build and install it — see section 3.

### The database in development

You install MariaDB yourself and create the database once. Then:

```sh
make migrate            # apply every migration to the dev database
make schema-check       # what would change to reach the declared schema
sag schema --dump-sql   # the SQL that would create it from nothing
sag schema --update     # apply it (refuses to destroy data without a flag)
```

The **test** databases are separate and disposable:

```sh
make test-db-up    # MariaDB on 3307 and PostgreSQL on 5433, in memory
make test-db-down  # stop them and throw the data away
```

A migration is not done until it has run against the database you are actually
looking at. The test suite creates and destroys its own scratch databases, so
`make ci` goes green while your dev database sits on the old schema.

### Tests

```sh
make ci            # THE GATE: format, tidy, vet, lint, Go tests, both front ends
make test-race     # the race detector — run it after a concurrency change
make e2e           # a real browser against a real server
make desktop-e2e   # the built desktop application, first run to ready
make link-e2e      # the real desktop client, against a real server and database
make node-ci       # the inference node's own gate
```

`make ci` is what has to be green. The browser gates are deliberately separate
because they are slow.

Running the Go tests against real databases needs two variables:

```sh
export SAG_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/?parseTime=true'
export SAG_TEST_PG_DSN='postgres://user:pass@127.0.0.1:5432/postgres?sslmode=disable'
```

Without them those suites **skip**, and a whole package can pass without running.

---

## 3. Building the applications

### The order is JS, then Go, then Rust — and it matters

The Rust shell packages whatever is in the payload directory *at the moment it
runs*. Anything built after it is simply not in the application, and nothing
fails: you ship something that silently predates your fix. The build scripts do
this in the right order. Do not run the steps by hand.

### macOS

```sh
./desktop/personal/build.sh                 # everything, engine included (~30 min)
./desktop/personal/build.sh --no-engine     # skip the inference engine (much faster)
./desktop/personal/build.sh --universal     # one app for Apple silicon and Intel
./desktop/enterprise/build.sh --universal   # the enterprise edition
```

Output lands in `desktop/.local/out/personal/` — the `.app` and a `.dmg` beside
it.

Build, gate and install in one step:

```sh
make desktop-install BUILD_ARGS=--no-engine
make enterprise-install
```

`desktop-install` installs **only if the gate passes**.

### Windows

Run these in PowerShell, from the repository root.

```powershell
.\desktop\personal\windows\build.ps1                # the full installer
.\desktop\personal\windows\build.ps1 -PayloadOnly   # stop before the installer
.\desktop\personal\windows\build.ps1 -SkipDatabase  # keep the bundled MariaDB you have
.\desktop\personal\windows\e2e.ps1                  # the desktop gate
```

Output: `desktop\.local\out\personal\SAG Personal_<version>_x64-setup.exe`.

`make desktop-build` and `make desktop-e2e` dispatch to these on Windows, so the
same command works on either platform.

Two things about the Windows edition specifically:

- **It carries no inference engine and no local models.** That is a decision,
  not a gap: a graphics build is compiled for one card generation and there are
  seven of them, and the processor-only build took 45 seconds to a first word
  against 46 milliseconds, which reads as a broken product rather than a slower
  one. Every screen about local models is *absent*, not disabled. Hosted models
  work exactly as they do everywhere else.
- **There is no `-Engine` flag.** Older notes mention one. It was removed, and
  because the script runs under strict mode, passing it is an error rather than
  something ignored.

### The inference node

```sh
make node-check        # seconds: the control plane, against a scripted engine
make node-lint
make node-test
make node-build ACCEL=cuda       # or metal, mkl, accelerate
make node-ci                     # its own full gate
```

Release tarballs are built **in Docker**, so what ships depends on the base
image and nothing about your machine. You do **not** need a GPU to build a CUDA
binary — compiling needs the toolkit, running needs the card.

```sh
cd inference
./packaging/release.sh              # cpu and cuda, into ./dist
./packaging/release.sh matrix       # cpu plus every published card generation
./packaging/release.sh cuda /srv/dist
```

`matrix` produces seven builds: `cpu`, and CUDA for compute capability 80, 86,
89, 90, 100, 103 and 120. The installer reads the card and fetches the match, so
the command a customer pastes is identical on every machine.

---

## 4. Signing and releasing

### Why any of this is necessary

Apple silicon **refuses to execute an unsigned binary at all**. That is
invisible on Intel, which is exactly how an unsigned build reaches somebody else
unnoticed. Unsigned is not "works with a warning" — it is "does not run".

### macOS: what you need once

Three separate things, which are easy to confuse:

| | What it proves | If it leaks |
|---|---|---|
| **Developer ID certificate** | this application was built by Flexie | somebody signs malware as Flexie: revoke and disclose |
| **App Store Connect API key** | this machine may talk to Apple's notary as Flexie | somebody notarises as Flexie: revoke, cheap |
| **Update signing key**, one per edition | this *release* came from Flexie | somebody pushes a release to that edition |

The update key is per edition on purpose: a leak of the personal key must not
let anybody push a release to enterprise.

**The G2 intermediate is not optional.** A certificate plus its private key
still reports `0 valid identities found` until Apple's *Developer ID
Certification Authority — G2* is in the keychain. Xcode installs it; a machine
without Xcode has never seen it, and nothing tells you that is the problem.

```sh
curl -O https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
security import DeveloperIDG2CA.cer -k ~/Library/Keychains/login.keychain-db
security find-identity -v -p codesigning    # should now list the identity
```

### macOS: building a signed release

```sh
export APPLE_SIGNING_IDENTITY="Developer ID Application: <NAME> (<TEAM ID>)"
export APPLE_API_KEY=<KEY ID>
export APPLE_API_ISSUER=<ISSUER UUID>
export APPLE_API_KEY_PATH="$HOME/apple-signing/AuthKey_<KEY ID>.p8"
export TAURI_SIGNING_PRIVATE_KEY_PATH="$HOME/apple-signing/sag-updater-personal.key"
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD=""

./desktop/personal/build.sh --universal
```

The build signs everything the bundle *carries*, not just what Tauri made — the
gateway, the whole bundled database and its libraries. Apple's notary reads
every Mach-O in a bundle and refuses the lot if one is unsigned.

It then checks its own work and **fails the build** if any of this is wrong:

- the architectures are what you asked for
- the signature verifies all the way through
- the signature carries a secure timestamp (without it, the application stops
  working the day the certificate expires)
- the `.app` and the `.dmg` are both notarised and stapled

Without `TAURI_SIGNING_PRIVATE_KEY_PATH` the release step is skipped: you get a
build, not a failure.

### Windows: signing

**Nothing is signed on Windows today.** What a user currently sees:

1. The browser warns before the file is even saved — "isn't commonly
   downloaded", and increasingly a block with "Keep anyway" hidden in a menu.
2. SmartScreen shows a blue "Windows protected your PC" box on first run, with
   "Run anyway" behind a "More info" link.
3. If the machine is set to allow only reputable apps, there is **no way past
   it at all**.

**The cheapest workable fix is Azure Artifact Signing** (formerly Trusted
Signing) at **about USD 10 a month**. Reasoning, since price is not the only
thing that matters:

- The key lives in Microsoft's managed HSM. Since June 2023 every publicly
  trusted code signing certificate must be on hardware; this satisfies that
  without you buying, shipping or losing a USB token.
- It signs **headlessly** from CI with a service principal. A physical token
  cannot.
- The certificate is issued to the **company**, so the publisher name a user
  sees is Flexie.

Cheaper options exist and are the wrong instrument:

- **Certum Open Source** (about EUR 49) is issued **only to an individual**, its
  organisation field is the literal string "Open Source Developer", and Certum
  revokes it if the software is distributed commercially. A company's product
  is commercial whether or not its source is public.
- Every other publicly trusted route starts around **USD 220 a year** and adds
  either a token you must physically plug in or a separate cloud-HSM
  subscription to get automation back.

Two things to know before starting: it needs a **paid** Azure subscription
(free and trial are refused), and organisation validation takes 1–20 business
days with **three** document attempts before you are locked out. Pick a
fallback first — Sectigo OV at about USD 220 a year with the bundled token.

**An EV certificate is no longer worth the extra money.** Microsoft's own
documentation now says EV does not bypass SmartScreen. Reputation still has to
build either way.

When you have the certificate, Tauri signs the NSIS output through
`bundle.windows.signCommand`. Sign **both** the `.exe` and the installer.

### Publishing a release

Where releases go is **not in the repository**. It names a machine you own and
an account that may write to it, so it lives in a file git cannot see:

```sh
cp deploy/release.env.example deploy/release.env
$EDITOR deploy/release.env          # the host, the path, the public address
chmod 600 deploy/release.env
```

`deploy/release.env` is ignored; `deploy/release.env.example` is committed so a
fresh checkout knows what to fill in. The publish script reads it on its own, so
a release is one command and nothing has to be exported by hand.

```sh
make release-publish                     # personal
make release-publish EDITION=enterprise
```

Without that file and with nothing exported it stops and tells you what to do.
For a one-off target, set `SAG_REPO_SSH` and `SAG_REPO_URL` for the single
command instead.

Order matters and the script enforces it, because a manifest announces an
archive *by name*:

1. Every manifest is read and must agree on the version and name the same
   archive, and that archive must exist — checked **before anything uploads**.
2. It refuses to publish over a version already being served.
3. The archive goes up and is **fetched back over the public address** to prove
   it is really there.
4. Only then the manifests, all of them in one command.
5. Then it asks, per architecture, the question an old installation asks, and
   requires the new version back.

**Publish every architecture's manifest.** A universal build that publishes only
one leaves the other half of your users frozen with nothing to tell them.

### How an installed copy updates itself

It asks 90 seconds after launch, then every six hours. It downloads and installs
quietly, then shows one chip saying to reopen. It never restarts itself — the
gateway is holding a database.

---

## 5. Production

### First time on a box

```sh
cd deploy
./secrets.sh > .env      # ONCE. Back this up off the machine.
$EDITOR .env             # fill in the two domains and the certificate email
./build.sh               # build the images
./build.sh machine       # and the model machine, if this box runs one (~20 min)
```

`secrets.sh` refuses to overwrite by design — it writes to standard output, so
`> .env` onto an existing file is your decision, not its.

### Bringing it up

```sh
# a box that already runs Traefik in Swarm
docker stack deploy -c sag.yml -c edge.traefik.yml sag

# the download host, if this box serves it
docker stack deploy -c repo.yml sagrepo
```

> **The Caddy route is currently broken.** `edge.caddy.yml` declares a
> dependency on a `redis` service that no longer exists, so
> `docker compose -f sag.yml -f edge.caddy.yml config` fails outright. Use the
> Traefik file, or delete the `depends_on` block first. Verified, not assumed.

### Deploying a change

Never edit files on the server. Ship a commit.

```sh
C=$(git rev-parse --short HEAD)
git archive --format=tar HEAD orchestrator scripts deploy | gzip > /tmp/sag-$C.tgz
scp /tmp/sag-$C.tgz you@server:/tmp/

ssh you@server "set -e
  rm -rf ~/sag/build/orchestrator ~/sag/build/scripts ~/sag/build/deploy
  cd ~/sag/build && tar xzf /tmp/sag-$C.tgz
  docker build -t flexie-sag/orchestrator:$C -t flexie-sag/orchestrator:latest \
    --build-arg SAG_VERSION=$C -f orchestrator/Dockerfile .
  docker service update --force --image flexie-sag/orchestrator:latest sag_orchestrator"
```

**Clear the code directories first.** `tar x` overlays; it never removes. A file
deleted in a commit stays on the box for ever, and the failure is a build error
about a symbol declared twice, in a file the commit no longer contains.

Migrations are their own step, before the roll:

```sh
ssh you@server 'docker run --rm --network sag_default \
  -e SAG_DB_DSN="..." flexie-sag/orchestrator:latest migrate up'
```

Then check. A service log interleaves old runs and reads as if it worked:

```sql
SELECT MAX(version_id) FROM sag.sag_db_version;
```

### Verifying from outside

```sh
curl -sS https://your-host/healthz
curl -sS https://sag-repo.example/updates/personal/darwin/x86_64/0.0.1
```

Every binary reports the build it came from, on **every log line** and from the
command line:

```sh
docker exec <container> sag version    # e.g. 0.1.5+1550136
```

### Rolling back

```sh
docker service update --force --image flexie-sag/orchestrator:<previous> sag_orchestrator
```

An update to a desktop application **cannot** be recalled, only superseded by a
higher version. The publish script refuses to overwrite a live one for exactly
that reason.

---

## 6. Secrets and keys

**Nothing here is in the repository, and `.gitignore` is written to keep it that
way.** It covers the directories these live in as well as the file extensions,
because a directory is easier to get right than an extension somebody invents.

| Secret | Protects | Lives | If lost |
|---|---|---|---|
| Developer ID certificate + key | signing as Flexie on macOS | macOS login keychain; backup in `~/apple-signing/` | Recoverable: revoke and re-issue. Nothing shipped is affected. |
| App Store Connect API key (`.p8`) | notarising as Flexie | `~/apple-signing/` | Recoverable: mint a new one. Apple lets a `.p8` be downloaded **once**, so a lost file means a new key. |
| Update key, personal | pushing releases to the personal edition | `~/apple-signing/sag-updater-personal.key` | **Not recoverable.** The public half is compiled into every installed copy. Losing it means nobody can be updated again. |
| Update key, enterprise | the same, for enterprise | `~/apple-signing/sag-updater-enterprise.key` | The same. |
| `SAG_ENCRYPTION_KEYS` | every stored credential: vendor keys, machine keys, the certificate authority | `deploy/.env`, generated by `secrets.sh` | **Not recoverable.** Every sealed credential is lost with it. |
| Database password | the production database | `deploy/.env` | Recoverable by resetting it. |
| `deploy/release.env` | the machine releases are published to, and the account that may write to it | `deploy/release.env`, git-ignored; copy `release.env.example` | Recoverable: it is a hostname and a path, not a credential. The ssh key it uses is your own. |
| Node join token | admitting a new machine to the fleet | minted per person, valid one hour, destroyed by the machine that uses it | Recoverable: mint another. |

**Back up the two update keys and `deploy/.env` somewhere that is not the build
machine.** Those are the ones with no way back.

### Checking nothing sensitive can be committed

```sh
git check-ignore -v <path>          # why a file is ignored
git ls-files | grep -iE '\.(key|pem|p12|p8|pfx|cer)$'   # must return nothing
```

---

## 7. Setting up a new build machine

### macOS

1. Install Xcode command line tools, Go, Node, Rust, Docker.
2. Import the Developer ID certificate (`.p12`) into the login keychain.
3. Import the G2 intermediate — see section 4. Without it the identity is
   invisible and nothing says why.
4. Copy `AuthKey_<id>.p8` and both `sag-updater-*.key` files into
   `~/apple-signing/`, mode `0600`.
5. Check: `security find-identity -v -p codesigning` lists the identity.
6. Build something with `--no-engine` and confirm it says *notarised and
   stapled*.

### Windows

1. Install PowerShell 7, Go, Node, Rust, and Visual Studio Build Tools with the
   C++ workload.
2. Clone the repository. Nothing else is needed for an unsigned build.
3. `.\desktop\personal\windows\build.ps1`
4. `.\desktop\personal\windows\e2e.ps1` to prove it actually starts.

Signing is separate and not set up yet — section 4.

---

## 8. When it goes wrong

**The build succeeded but the application is the old one.**
The order is JS, then Go, then Rust. The Rust shell packages the payload as it
finds it. Rebuild with the script, not by hand.

**`0 valid identities found` with the certificate installed.**
The G2 intermediate is missing. Section 4.

**Notarisation refused, naming files.**
Something the bundle carries is unsigned or has no timestamp. The build signs
the payload itself; if you assembled anything by hand, that is why.

**Timestamps differ by N seconds.**
Apple's timestamp service had a moment. Run it again — it is not your clock
unless it is minutes out.

**`make dev` serves a stale binary, or nothing is listening.**
A browser with the console open holds a connection open, so the outgoing server
takes its full shutdown grace and the new one finds the port busy. The server
now waits for a draining predecessor instead of dying. If nothing listens at
all, check for an orphan.

**A test suite passes without running.**
The database suites skip without `SAG_TEST_DSN` and `SAG_TEST_PG_DSN`. A whole
package can go green having executed nothing.

**The chat is blank with no error.**
The chat is *built* for `/chat/`, not merely served there. A build made for the
root asks for `/assets/...`, the console's catch-all answers, and you get a
blank page and no clue. Use the build scripts.
