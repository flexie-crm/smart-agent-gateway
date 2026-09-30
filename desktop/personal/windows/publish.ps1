# Publishes a built Windows release to the download host.
#
#   .\desktop\personal\windows\publish.ps1
#   .\desktop\personal\windows\publish.ps1 -WhatIf   say what it would do, touch nothing
#
# The counterpart of desktop/publish.sh, which publishes macOS. Run build.ps1
# with a signing key first: without one it writes no manifest and there is
# nothing here to publish.
#
# WHY A SECOND SCRIPT RATHER THAN BRANCHES IN publish.sh. The same reason the
# build is two scripts, and it was learned the same way. One file that had to
# serve both platforms collected an `if` per line (the manifest glob, the
# invariant across manifests, the installer's extension, the verification URL,
# and which interpreter reads JSON), and then the first time somebody improved
# the macOS half it conflicted with the Windows half in the same block. Two
# scripts each doing one platform honestly beats one doing neither clearly, and
# the macOS one is the proven path: it is not made riskier to make this one
# possible.
#
# WHAT IS SHARED ANYWAY, and this is the part worth protecting: the ORDER. A
# manifest announces an archive by name, so publishing the manifest first tells
# every installation a new version exists and then fails to give it to them.
# Nothing about that is visible from here, because the build is correct and the
# upload succeeds. So the order below is the same order publish.sh uses, for the
# same reason, and it is the thing to keep in step if either changes:
#
#   what there is -> the archive -> prove it downloads -> the installer ->
#   and only then, the news -> ask as an old installation would
[CmdletBinding(SupportsShouldProcess)]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$out = Join-Path $repo 'desktop\.local\out\personal'
$updates = Join-Path $out 'updates'

function Need($command, $why) {
	if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
		throw "$command is not on PATH, and $why"
	}
}
# Windows 10 and later ship OpenSSH and curl, so this needs nothing installed.
# That is also why this script is PowerShell rather than the bash one run under
# Git Bash: there is no interpreter to find and no path translation to get wrong.
Need 'ssh' 'the release is copied to the download host over it'
Need 'scp' 'the release is copied to the download host with it'
Need 'curl.exe' 'what was uploaded is fetched back over the public address'

function Run($what, [scriptblock] $block) {
	& $block
	if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" }
}

# ------------------------------------------------------------ where it goes
# Kept OUT of the repository: it names a machine you own and an account that may
# write to it. deploy/release.env is git-ignored and release.env.example is
# committed, so a fresh checkout knows what to fill in. Read here rather than
# exported by hand, so a release is one command.
#
# Parsed rather than sourced, because it is a shell file and this is not a
# shell: only NAME=value lines are taken, quotes stripped, everything else
# ignored. A `export X=$Y` in there would be read wrongly by any simpler rule,
# so it is skipped loudly rather than half-understood.
$releaseEnv = Join-Path $repo 'deploy\release.env'
if (-not (Test-Path $releaseEnv)) {
	throw @"
no publish target: $releaseEnv does not exist.

  copy deploy\release.env.example to deploy\release.env and fill it in.
"@
}
$settings = @{}
foreach ($line in Get-Content $releaseEnv) {
	$text = $line.Trim()
	if (-not $text -or $text.StartsWith('#')) { continue }
	if ($text -match '^\s*(?:export\s+)?(?<name>[A-Za-z_][A-Za-z0-9_]*)=(?<value>.*)$') {
		$value = $Matches.value.Trim()
		if ($value -match '\$') {
			Write-Warning "  ignoring $($Matches.name): it refers to another variable, which this does not expand"
			continue
		}
		$settings[$Matches.name] = $value.Trim('"').Trim("'")
	}
}
foreach ($required in 'SAG_REPO_SSH', 'SAG_REPO_URL') {
	if (-not $settings.ContainsKey($required)) { throw "$required is not set in $releaseEnv" }
}
$sshHost = $settings['SAG_REPO_SSH']
$public = $settings['SAG_REPO_URL'].TrimEnd('/')
$remote = if ($settings.ContainsKey('SAG_REPO_DIR')) { $settings['SAG_REPO_DIR'] } else { '/srv/sag-repo/dist/updates' }
# Installers sit beside the updates rather than inside them: one is a thing a
# person downloads, the other a thing an installation fetches for itself.
$desktop = if ($settings.ContainsKey('SAG_REPO_DESKTOP_DIR')) {
	$settings['SAG_REPO_DESKTOP_DIR']
} else {
	($remote -replace '/[^/]+/?$', '') + '/desktop'
}

# --------------------------------------------------------------- what there is
$manifestPath = Join-Path $updates 'personal-windows-x86_64.json'
if (-not (Test-Path $manifestPath)) {
	throw @"
nothing to publish: $manifestPath does not exist.

  build.ps1 writes it only when a signing key is set. Set
  TAURI_SIGNING_PRIVATE_KEY_PATH (and TAURI_SIGNING_PRIVATE_KEY_PASSWORD if the
  key has one) and build again.
"@
}
$manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json
foreach ($field in 'version', 'signature', 'url') {
	if (-not $manifest.$field) { throw "the manifest has no $field, so it announces nothing" }
}
$version = $manifest.version
# The manifest stores a PATH so the host can fill itself in; the file it names
# is what has to exist here.
$name = Split-Path $manifest.url -Leaf
$archive = Join-Path $out $name
if (-not (Test-Path $archive)) { throw "the manifest names $name, which was not built" }
if (-not (Test-Path "$archive.sig")) { throw "$name.sig is missing, so nothing could verify it" }

Write-Host "==> personal $version"
Write-Host ("    {0} ({1:N1} MB)" -f $name, ((Get-Item $archive).Length / 1MB))
Write-Host "    personal-windows-x86_64.json"

# Refuse to publish over a version already out there. Replacing an archive under
# a live manifest is how an installation ends up with a signature that does not
# match what it downloaded.
curl.exe -fsI "$public$($manifest.url)" *> $null
if ($LASTEXITCODE -eq 0) {
	throw @"
$version is ALREADY published. Bump the version instead: an update cannot be
recalled, only superseded.
"@
}
# And forget that failure. It is the ANSWER here (nothing is published yet), but
# PowerShell keeps the last native exit code, and a -WhatIf run ends right after
# this: the script would report success and exit 22, which is the inverse of
# every failure this file exists to prevent.
$global:LASTEXITCODE = 0

if ($WhatIfPreference) {
	Write-Host ""
	Write-Host "  Would publish $name and announce $version. Nothing was uploaded."
	return
}

# ----------------------------------------------------- what is downloaded
Write-Host "==> the archive"
Run 'copying the archive' { & scp -q $archive "$archive.sig" "${sshHost}:/tmp/" }
Run 'moving the archive into place' {
	& ssh $sshHost "sudo mkdir -p '$remote/personal' && sudo mv '/tmp/$name' '/tmp/$name.sig' '$remote/personal/' && sudo chmod -R a+rX '$remote'"
}

# Fetched back over the public address, as an installation would. That the file
# landed on the server is a different claim: the path it is served under, the
# proxy in front of it and the handler that routes /updates/ have all been wrong
# before, and each of those leaves a correct file on disk.
Write-Host "==> proving it can be downloaded"
$served = (& curl.exe -fsI "$public$($manifest.url)") -join "`n"
$length = [regex]::Match($served, '(?im)^content-length:\s*(\d+)')
if (-not $length.Success) { throw "    $public$($manifest.url) is not being served" }
$local = (Get-Item $archive).Length
if ([int64] $length.Groups[1].Value -ne $local) {
	throw "    it is served, but at $($length.Groups[1].Value) bytes rather than $local"
}
Write-Host "    $local bytes, at $public$($manifest.url)"

# -------------------------------------------------------------- the installer
# On this platform the installer IS the archive: one format does both jobs, so
# the same bytes go up a second time under the fixed name the download page
# links to. The versioned name stays beside it for anybody who wants a specific
# build.
Write-Host "==> the installer"
$stable = 'SAG-Personal.exe'
Run 'copying the installer' { & scp -q $archive "${sshHost}:/tmp/$stable" }
Run 'moving the installer into place' {
	& ssh $sshHost "sudo mkdir -p '$desktop' && sudo cp '/tmp/$stable' '$desktop/$name' && sudo mv '/tmp/$stable' '$desktop/$stable' && sudo chmod -R a+rX '$desktop'"
}
$servedInstaller = (& curl.exe -fsI "$public/desktop/$stable") -join "`n"
if (-not [regex]::IsMatch($servedInstaller, '(?im)^content-length:\s*\d+')) {
	throw "    $public/desktop/$stable is not being served"
}
Write-Host "    $stable, and $name beside it"

# --------------------------------------------------- and only then, the news
Write-Host "==> announcing it"
Run 'copying the manifest' { & scp -q $manifestPath "${sshHost}:/tmp/" }
Run 'moving the manifest into place' {
	& ssh $sshHost "sudo mv '/tmp/personal-windows-x86_64.json' '$remote/' && sudo chmod -R a+rX '$remote'"
}

# And asked the way an older installation asks, which is the only question that
# matters and the only one nothing above has actually put.
Write-Host "==> asking as an old installation would"
$answer = (& curl.exe -fsS "$public/updates/personal/windows/x86_64/0.0.1") -join ''
$offered = ''
if ($answer) { try { $offered = ($answer | ConvertFrom-Json).version } catch { $offered = '' } }
if ($offered -ne $version) {
	throw "    windows x86_64 was offered '$(if ($offered) { $offered } else { 'nothing' })', not $version"
}
Write-Host "    windows x86_64 is offered $version"

Write-Host ""
Write-Host "  SAG Personal $version is published for Windows."
Write-Host "  Every installation older than it will have it within six hours."
Write-Host ""
