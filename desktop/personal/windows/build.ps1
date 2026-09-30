# Build SAG Personal for Windows (KB/36).
#
#   .\desktop\personal\windows\build.ps1              everything, no engine
#   .\desktop\personal\windows\build.ps1 -Engine      with the processor engine, which is a long build
#   .\desktop\personal\windows\build.ps1 -PayloadOnly stop before the installer (this is what the gate drives)
#
# Produces the installer:
#
#   desktop\.local\out\personal\SAG-Personal-<version>-x64-setup.exe
#
# It carries the whole product: the gateway, its MariaDB, both pages. Installing
# it and opening it starts the gateway; closing it stops it.
#
# WHY A SECOND SCRIPT RATHER THAN BRANCHES IN build.sh. That one is macOS-shaped
# in almost every line: lipo merging two architectures, .app bundles, hdiutil,
# codesign, xcrun. There is nothing to merge here, no bundle format, and the
# signing story is a different one entirely. Two scripts each doing one platform
# honestly beats one script with an `if` on every line, and the macOS one is
# proven.
#
# ORDER MATTERS, and it is the same order as the macOS script for the same
# reason: JS, then Go, then Rust. The Rust shell packages whatever the payload
# directory holds at the moment it runs, so anything built after it is simply not
# in the application. Getting this wrong does not fail the build: it ships an
# application that silently predates the fix, which is worse than a failure
# because somebody then tests it and reports the bug again.
[CmdletBinding()]
param(
	# Everything up to but not including the installer. `make desktop-e2e` drives
	# the payload directly, so a change to the gateway or the pages can be gated
	# without waiting for Rust.
	[switch] $PayloadOnly,
	# Skip re-bundling MariaDB when one is already cached. It is the same 99 MB
	# download and the same copy every time.
	[switch] $SkipDatabase
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$here = $PSScriptRoot
$out = Join-Path $repo 'desktop\.local'
$payload = Join-Path $out 'payload'
$mariadb = Join-Path $out 'windows\mariadb'

# Fail on a missing tool by NAME, before anything is built, rather than three
# minutes in with a message from whatever tried to use it.
function Need($command, $why) {
	if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
		throw "$command is not on PATH, and $why"
	}
}
Need 'go' 'the gateway is written in it'
Need 'npm' 'the console and the chat are built with it'
# Only the application shell needs Rust here: this platform ships no engine (see
# "the engine" below), so a payload-only build needs no Rust at all.
if (-not $PayloadOnly) { Need 'cargo' 'the application shell is written in Rust' }

# Run a native command and stop the build when it fails.
#
# PowerShell does not do this by itself: $ErrorActionPreference governs cmdlets,
# and a native program that exits non-zero carries straight on to the next line.
# Every failure below would otherwise produce a payload missing a piece and an
# installer built happily around the hole.
function Run($what, [scriptblock] $block) {
	& $block
	if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" }
}

Remove-Item $payload -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $payload | Out-Null

# ---------------------------------------------------------------- the database
Write-Host "==> the database"
if ($SkipDatabase -and (Test-Path (Join-Path $mariadb 'bin\mariadbd.exe'))) {
	Write-Host "    keeping the bundle already in $mariadb"
} else {
	Run 'bundling MariaDB' { & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $here 'mariadb.ps1') }
}

# ------------------------------------------------------------------- the pages
# The chat is BUILT for /chat/, not merely served there: its asset URLs are
# absolute, so a build made for the root asks for /assets/..., the console's
# catch-all answers, and the chat is blank with no error anywhere.
#
# VITE_SAG_PERSONAL is what makes these DESKTOP builds rather than builds that
# ask at run time what they are. A bundle that has to ask has a moment where it
# does not know, and a request that fails leaves it showing the wrong product.
$env:VITE_SAG_PERSONAL = '1'
# What this platform does NOT have is an engine inside the application, so this
# computer is not one of the machines models can run on. One sentence on the
# Inference screen says which machines it means, and it is the only thing that
# changes: everything else on that screen is about machines somewhere else, which
# this platform has exactly like every other (see below).
$env:VITE_SAG_LOCAL_ENGINE = '0'

# And beyond that sentence the console is NOT built differently for this platform,
# though it used to be.
# Shipping no engine was read as having nothing to run models on, so the Inference
# screen, its route and the button that adds a model from a machine were all
# compiled out. That was wrong: a machine is a server with a graphics card in it,
# added by exchanging certificates with it, and on a personal installation that
# is the only way in anyway, because the gateway is on loopback and nothing can
# call it back. Hiding the screen removed the one kind of inference that works
# here. What this platform ships no engine for is the LOCAL node, and that is
# decided where it is started (config.EngineBundled).

# Into their OWN directories, never the ones a server serves. These builds carry
# VITE_SAG_PERSONAL, which makes a different product out of the same source, and
# the web console and the web chat are written to admin-ui/dist and
# chat-ui/dist/app, which is what a running gateway is pointed at
# (SAG_CONSOLE_DIR, SAG_CHAT_DIR). Building this edition over them replaces the
# web console and chat with the desktop ones on a machine that is serving them,
# and nothing announces it: a console link appears in the web chat's sidebar and
# its right-click menu stops working, on a build nobody asked to change.
#
# desktop/personal/build.sh was fixed for that and this script was not, so it
# went on doing it here for every Windows build.
$env:SAG_OUT_DIR = 'dist/personal'

Write-Host "==> the console"
Push-Location (Join-Path $repo 'admin-ui')
try { Run 'building the console' { & npm run build } } finally { Pop-Location }
Copy-Item (Join-Path $repo 'admin-ui\dist\personal') (Join-Path $payload 'console') -Recurse

Write-Host "==> the chat"
Push-Location (Join-Path $repo 'chat-ui')
try {
	Run 'building the chat' { & npm run build }
} finally { Pop-Location }
Copy-Item (Join-Path $repo 'chat-ui\dist\personal') (Join-Path $payload 'chat') -Recurse

Remove-Item Env:\VITE_SAG_PERSONAL, Env:\VITE_SAG_LOCAL_ENGINE, Env:\SAG_OUT_DIR

# ----------------------------------------------------------------- the gateway
Write-Host "==> the gateway"
Push-Location (Join-Path $repo 'orchestrator')
try {
	$env:CGO_ENABLED = '0'
	$env:GOOS = 'windows'
	$env:GOARCH = 'amd64'
	try {
		Run 'building the gateway' { & go build -ldflags="-s -w" -o (Join-Path $payload 'sag.exe') ./cmd/sag }
	} finally {
		Remove-Item Env:\CGO_ENABLED, Env:\GOOS, Env:\GOARCH
	}
} finally { Pop-Location }

# How many migrations a first run will apply. The shell shows real progress
# rather than a bar that creeps towards a number, and only the build knows the
# total.
$migrations = (Get-ChildItem (Join-Path $repo 'orchestrator\internal\migrations\sql') -Filter *.sql -File).Count
# No trailing newline and no BOM: the shell parses this file with a plain
# trim-and-parse, and a byte-order mark makes the first digit unreadable.
[System.IO.File]::WriteAllText((Join-Path $payload 'migrations.count'), "$migrations",
	[System.Text.UTF8Encoding]::new($false))

# --------------------------------------------------------------- the database
Write-Host "==> the payload"
Copy-Item $mariadb (Join-Path $payload 'mariadb') -Recurse

# ---------------------------------------------------------------- the engine
# THERE IS NONE ON WINDOWS, and it is a decision rather than an omission.
#
# A graphics build is compiled for ONE compute capability (KB/35) and there are
# seven of them. They cannot all go in one installer, and asking somebody which
# card they own is asking them to get it wrong. The processor build is the only
# one that runs everywhere, and it is slower by about three orders of magnitude
# (45.8 s to first token against 46 ms), which is not a product: it is a way to
# make somebody believe the whole application is broken.
#
# So this edition hosts its models, and says so. That is not a degraded mode
# with a screen apologising for itself: `config.EngineBundled` is false on this
# platform, so the gateway starts no node and the console has no Machines menu,
# no route and no "add local model" button. Nothing hints at a capability that
# is not here.
#
# macOS is UNAFFECTED and keeps its engine: Metal has no compute-capability
# equivalent, so one build runs on every Mac and there is no question to ask.
# `desktop/personal/build.sh` still builds and bundles it. The `inference/` crate
# and the rack-mounted fleet it serves are untouched by any of this.
Write-Host "==> no inference engine on this platform (KB/36), models are hosted"

$payloadSize = (Get-ChildItem $payload -Recurse -File | Measure-Object Length -Sum).Sum / 1MB
Write-Host ("    payload is {0:N1} MB" -f $payloadSize)

if ($PayloadOnly) {
	Write-Host ""
	Write-Host "  Built the payload only, at $payload"
	Write-Host "  Drive it with: make desktop-e2e"
	Write-Host ""
	return
}

# ------------------------------------------------------------------- the app
Write-Host "==> the icon"
Run 'building the icon' { & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $here 'icon.ps1') }

Write-Host "==> the application"
# The NSIS target and its settings live in tauri.windows.conf.json, which Tauri
# merges in on this platform and nowhere else. That is the reason the macOS
# bundle, which is the proven one, needs no edit to make this work.
#
# AND NO --config, which is what makes that merge count. The macOS script passes
# `--config tauri.conf.json`, harmlessly there because it names the file Tauri
# would have read anyway. Here it is not harmless: --config is applied as an
# override on TOP of everything, so naming the base file put its
# `targets: ["app"]` back over the `["nsis"]` the platform file had just set.
# The build then succeeded, said "Built application at ...", and produced no
# installer at all, which is a failure that looks like a success right up until
# you go looking for the file.
#
# UNSIGNED. SmartScreen will warn on every download until it is signed, and
# signing is paperwork rather than code: since June 2023 an OV code-signing key
# must live in hardware or a cloud HSM, so there is no .pfx to put in a secret
# (KB/36, signing). Nothing here changes when there is one.
Push-Location (Join-Path $repo 'desktop\personal\shell')
try {
	Run 'building the application' { & npx --yes '@tauri-apps/cli@2' build }
} finally { Pop-Location }

$apps = Join-Path $out 'out\personal'
Remove-Item $apps -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $apps | Out-Null

$bundles = Join-Path $repo 'desktop\target\release\bundle\nsis'
$installer = Get-ChildItem $bundles -Filter '*-setup.exe' -File -ErrorAction SilentlyContinue |
	Sort-Object LastWriteTime -Descending |
	Select-Object -First 1
if (-not $installer) { throw "the bundle step produced no installer in $bundles" }

# Renamed on the way out, and the name matters more than it looks.
#
# Tauri names it from the product, so it arrives as "SAG Personal_0.1.5_x64-setup.exe",
# with a space. That space becomes %20 in every address it is ever served under,
# and publish.sh carries the file's name into a remote shell command where it
# would split into two arguments and move nothing. The macOS archive has been
# SAG-Personal-<version>... since the beginning for the same reason; this is the
# same name in this platform's shape.
#
# THE VERSION COMES FROM THE INSTALLER THE BUNDLER JUST WROTE, not from the
# configuration file, and then the two have to agree.
#
# Reading the file was wrong and it took a real build to show it. The bundler
# reads the version when it starts; this runs minutes later, and a `git pull`
# in between moved the file from 0.1.5 to 0.1.6 while cargo was still
# compiling. The result was an installer carrying a 0.1.5 application under the
# name 0.1.6, and it did not fail anywhere: the manifest would have announced a
# version that was never built, every installation would have downloaded it,
# and the updater compares against the version compiled into the RUNNING
# process, so each one would have installed 0.1.5, found itself still older
# than 0.1.6, and downloaded it again on the next tick, for ever.
#
# So the name is taken from the artefact, which cannot be wrong about itself,
# and the disagreement is a build failure rather than a file with a misleading
# name. Loudly: what makes this dangerous is that every step after it succeeds.
if ($installer.Name -notmatch '_(?<v>[0-9]+\.[0-9]+\.[0-9]+[^_]*)_x64-setup\.exe$') {
	throw "cannot read a version out of the bundler's name for it: $($installer.Name)"
}
$version = $Matches.v
$declared = (Get-Content (Join-Path $repo 'desktop\personal\shell\tauri.conf.json') -Raw |
	ConvertFrom-Json).version
if ($version -ne $declared) {
	throw @"
the bundler built $version and the configuration now says $declared.

The tree moved while the build was running, so the application in the installer
is not the one that would be announced. Nothing downstream can detect this.
Build again on a settled tree.
"@
}
$name = "SAG-Personal-$version-x64-setup.exe"
Copy-Item $installer.FullName (Join-Path $apps $name)

# The unpacked application too, beside the installer. It is what the build
# actually produced and what a quick check can run without installing anything;
# the installer is the thing a person downloads. Building only the second is how
# a broken application gets found by installing it rather than by looking at it.
Copy-Item (Join-Path $repo 'desktop\target\release\SAG Personal.exe') $apps

$final = Join-Path $apps $name

# ------------------------------------------------------------------ the update
# What an installed copy replaces itself with, and the manifest that points at
# it. Written HERE, by the same run that made the installer, because a build and
# the announcement of it are one act: published separately you get a version
# announced that was never uploaded, which is every installation downloading a
# 404.
#
# THE ARTEFACT IS THE INSTALLER ITSELF, and that is not a shortcut. macOS needs
# two files because its two jobs need two formats: a .dmg is a thing a person
# mounts and the updater cannot use it, so a .tar.gz of the .app exists beside
# it. Here there is one format that does both. tauri-plugin-updater sniffs what
# it downloaded (`extract` in updater.rs) and accepts a bare .exe through
# `extract_exe`, handing it to the NSIS path; the .zip it also accepts is behind
# a cargo feature and would only wrap the same bytes. So one file is built,
# signed once, and published twice under two names.
#
# Signed with the UPDATE key, which is per edition, so a leak of one cannot push
# a release to the other. Without a key the installer is still built and simply
# not announced: a build without it is a build, not a failure. That is why every
# line below is inside the check rather than guarded one at a time.
if ($env:TAURI_SIGNING_PRIVATE_KEY -or $env:TAURI_SIGNING_PRIVATE_KEY_PATH) {
	Write-Host "==> the update"
	# If the key carries a password, TAURI_SIGNING_PRIVATE_KEY_PASSWORD must be
	# set too: the signer asks for one on a terminal otherwise, and a build that
	# stops to ask is a build that hangs on a machine nobody is watching.
	Push-Location $apps
	try {
		Run 'signing the installer' { & npx --yes '@tauri-apps/cli@2' signer sign $name }
	} finally { Pop-Location }
	$sig = "$final.sig"
	if (-not (Test-Path $sig)) { throw "the installer was not signed: $sig is missing" }

	# One manifest, named the way the server reads it back. The two halves of
	# that name are not ours to choose: tauri-plugin-updater substitutes its own
	# `target()` and `updater_arch()` into the endpoint, which on this platform
	# are "windows" and "x86_64" (updater.rs). Name it anything else and the
	# request 204s for ever, which is indistinguishable from being up to date.
	$updates = Join-Path $apps 'updates'
	New-Item -ItemType Directory -Force -Path $updates | Out-Null
	$manifest = [ordered] @{
		version   = $version
		pub_date  = [DateTime]::UtcNow.ToString('o')
		signature = (Get-Content $sig -Raw).Trim()
		# A path, not an address: the host fills itself in, so a manifest written
		# here does not have to know what the download host is called.
		url       = "/updates/personal/$name"
	}
	# No BOM. Go reads this with encoding/json, which has no idea what a byte
	# order mark is and refuses the whole file for the three bytes in front of
	# the first brace. Out-File and > both write one on this version of
	# PowerShell, which is why neither is used here.
	[System.IO.File]::WriteAllText(
		(Join-Path $updates 'personal-windows-x86_64.json'),
		($manifest | ConvertTo-Json),
		[System.Text.UTF8Encoding]::new($false))
	Write-Host "    personal-windows-x86_64.json announces $version"
} else {
	Write-Host "==> no update manifest (no signing key), so this build cannot be published"
}

Write-Host ""
Write-Host "  Built:"
Write-Host ("    {0}  ({1:N1} MB)" -f $final, ((Get-Item $final).Length / 1MB))
Write-Host ""
Write-Host "  It is unsigned, so SmartScreen will warn the first time. Run it, then open"
Write-Host "  SAG Personal from the Start menu. A first run creates the database, so give"
Write-Host "  it half a minute. The console is a link away, at the foot of the chat's sidebar."
Write-Host ""
