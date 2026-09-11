# Build SAG Personal for Windows (KB/36).
#
#   .\desktop\personal\windows\build.ps1              everything, no engine
#   .\desktop\personal\windows\build.ps1 -Engine      with the processor engine, which is a long build
#   .\desktop\personal\windows\build.ps1 -PayloadOnly stop before the installer (this is what the gate drives)
#
# Produces the installer:
#
#   desktop\.local\out\personal\SAG Personal_<version>_x64-setup.exe
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
# And this platform ships no engine (see "the engine" below), so the console is
# built without every surface that would be about one: the Machines menu item,
# its route, and the button that adds a model from a machine. It is set here
# beside the flag it belongs with, and read by PERSONAL_POSTURE.
$env:VITE_SAG_LOCAL_MODELS = '0'

Write-Host "==> the console"
Push-Location (Join-Path $repo 'admin-ui')
try { Run 'building the console' { & npm run build } } finally { Pop-Location }
Copy-Item (Join-Path $repo 'admin-ui\dist') (Join-Path $payload 'console') -Recurse

Write-Host "==> the chat"
Push-Location (Join-Path $repo 'chat-ui')
try {
	Run 'building the chat' { & npm run build }
} finally { Pop-Location }
Copy-Item (Join-Path $repo 'chat-ui\dist\app') (Join-Path $payload 'chat') -Recurse

Remove-Item Env:\VITE_SAG_PERSONAL, Env:\VITE_SAG_LOCAL_MODELS

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
Copy-Item $installer.FullName $apps

# The unpacked application too, beside the installer. It is what the build
# actually produced and what a quick check can run without installing anything;
# the installer is the thing a person downloads. Building only the second is how
# a broken application gets found by installing it rather than by looking at it.
Copy-Item (Join-Path $repo 'desktop\target\release\SAG Personal.exe') $apps

$final = Join-Path $apps $installer.Name
Write-Host ""
Write-Host "  Built:"
Write-Host ("    {0}  ({1:N1} MB)" -f $final, ((Get-Item $final).Length / 1MB))
Write-Host ""
Write-Host "  It is unsigned, so SmartScreen will warn the first time. Run it, then open"
Write-Host "  SAG Personal from the Start menu. A first run creates the database, so give"
Write-Host "  it half a minute. The console is a link away, at the foot of the chat's sidebar."
Write-Host ""
