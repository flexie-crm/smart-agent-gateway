# Build a self-contained MariaDB bundle for the Windows desktop application (KB/36).
#
#   .\mariadb.ps1                      the latest stable release, into the default place
#   .\mariadb.ps1 -Version 12.3.2      a specific one
#   .\mariadb.ps1 -Out C:\somewhere    somewhere else
#
# What comes out has the same SHAPE as the macOS bundle, because internal/localdb
# reads one layout on both platforms:
#
#   bin\mariadbd.exe + the libraries it loads
#   share\bootstrap\01-,02-,03-*.sql   the server's own initialisation SQL, numbered
#   share\english\errmsg.sys
#   share\charsets\
#
# WHY THIS IS ONE SCRIPT WHERE MACOS NEEDS TWO. Over there, fetch-bottle.sh exists
# to assemble an install prefix Homebrew would never lay down for a foreign
# architecture, and bundle-macos.sh exists to rewrite absolute library paths into
# @executable_path so the result runs on a machine that has never had MariaDB.
# Neither problem exists here. MariaDB publishes an official ZIP that is already
# relocatable (Windows resolves a DLL beside the executable that loaded it), and
# there is one architecture. So this downloads, verifies, and copies.
#
# THE ZIP AND NOT THE MSI, deliberately: an installer that wants to install is the
# opposite of what a bundled server is. The MSI registers a service, writes to
# Program Files and asks for administrator rights, and every one of those is a
# thing KB/36 says this database must never do.
[CmdletBinding()]
param(
	# Empty means the newest stable release, asked of MariaDB rather than written
	# down here. Same reasoning as fetch-bottle.sh reading Homebrew's own metadata:
	# a version pinned in a build script is a version nobody remembers to move.
	[string] $Version = '',
	[string] $Out = '',
	# Downloads and extractions are kept, because the ZIP is ~99 MB and a rebuild
	# should not fetch it again.
	[string] $Cache = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
if (-not $Out) { $Out = Join-Path $repo 'desktop\.local\windows\mariadb' }
if (-not $Cache) { $Cache = Join-Path $repo 'desktop\.local\windows\cache' }

$api = 'https://downloads.mariadb.org/rest-api/mariadb'

# Resolve the newest STABLE release.
#
# Sorted rather than taken in the order the API happens to answer in, and
# filtered on release_status: the list carries Preview and RC entries above the
# stable ones, and shipping a release candidate inside a desktop application
# because it sorted first is exactly the accident this avoids.
function Resolve-LatestVersion {
	$index = Invoke-RestMethod "$api/"
	$stable = $index.major_releases |
		Where-Object { $_.release_status -eq 'Stable' } |
		Sort-Object { [version] $_.release_id } -Descending |
		Select-Object -First 1
	if (-not $stable) { throw "no stable major release in $api/" }

	$major = Invoke-RestMethod "$api/$($stable.release_id)/"
	$newest = $major.releases.PSObject.Properties.Name |
		Sort-Object { [version] $_ } -Descending |
		Select-Object -First 1
	if (-not $newest) { throw "no releases under $($stable.release_id)" }
	return $newest
}

if (-not $Version) {
	$Version = Resolve-LatestVersion
	Write-Host "==> the newest stable MariaDB is $Version"
} else {
	Write-Host "==> MariaDB $Version, as asked"
}

# The ZIP, its address and its checksum, all from the same answer. Verifying
# against a hash published beside the download is weak on its own, but it is not
# what it is here for: it catches the truncated transfer, which is the failure
# that actually happens and which otherwise surfaces as a corrupt server hours
# later.
$release = (Invoke-RestMethod "$api/$Version/").release_data.$Version
if (-not $release) { throw "MariaDB publishes no release $Version" }
$name = "mariadb-$Version-winx64.zip"
$file = $release.files | Where-Object { $_.file_name -eq $name }
if (-not $file) {
	throw "MariaDB $Version publishes no $name (this script builds the 64-bit Windows bundle)"
}
$expected = $file.checksum.sha256sum
# The API answers with http:// addresses. Asking for the same thing over TLS
# costs nothing and means the checksum we are about to compare against did not
# arrive over the same plain connection as the file.
$url = $file.file_download_url -replace '^http://', 'https://'

New-Item -ItemType Directory -Force -Path $Cache | Out-Null
$zip = Join-Path $Cache $name

function Get-Sha256($path) { (Get-FileHash $path -Algorithm SHA256).Hash.ToLower() }

if ((Test-Path $zip) -and ((Get-Sha256 $zip) -eq $expected)) {
	Write-Host "    already downloaded"
} else {
	Write-Host "    downloading $url"
	Remove-Item $zip -Force -ErrorAction SilentlyContinue
	# curl.exe, which ships with Windows, rather than Invoke-WebRequest: the
	# cmdlet buffers the whole response through the pipeline and takes tens of
	# minutes over a hundred megabytes. This takes seconds.
	& curl.exe --fail --location --retry 3 --silent --show-error --output $zip $url
	if ($LASTEXITCODE -ne 0) { throw "downloading $url failed (curl exit $LASTEXITCODE)" }

	$got = Get-Sha256 $zip
	if ($got -ne $expected) {
		Remove-Item $zip -Force -ErrorAction SilentlyContinue
		throw "$name arrived with sha256 $got, expected $expected"
	}
}
Write-Host "    verified $([math]::Round((Get-Item $zip).Length / 1MB, 1)) MB"

$extract = Join-Path $Cache "mariadb-$Version"
$prefix = Join-Path $extract "mariadb-$Version-winx64"
if (-not (Test-Path (Join-Path $prefix 'bin\mariadbd.exe'))) {
	Write-Host "    unpacking"
	Remove-Item $extract -Recurse -Force -ErrorAction SilentlyContinue
	Expand-Archive -Path $zip -DestinationPath $extract
}
if (-not (Test-Path (Join-Path $prefix 'bin\mariadbd.exe'))) {
	throw "no bin\mariadbd.exe under $prefix"
}

# ---------------------------------------------------------------- the bundle
Write-Host "==> assembling $Out"
Remove-Item $Out -Recurse -Force -ErrorAction SilentlyContinue
foreach ($dir in 'bin', 'share\bootstrap', 'share\english', 'share\charsets') {
	New-Item -ItemType Directory -Force -Path (Join-Path $Out $dir) | Out-Null
}

# mariadbd.exe is an 11 KB launcher on this platform; the server itself is
# bin\server.dll beside it. Every DLL in bin\ comes along: they are the C++
# runtime, zlib and libcurl, they total about 2 MB against the 21 MB server, and
# working out by import table which of them a given build actually reaches would
# be a fragile way to save nothing. What makes the bundle small is leaving out
# the forty client programs, the 92 MB of debug symbols, the headers, the import
# libraries and the twenty-eight message translations, all of which this does.
Copy-Item (Join-Path $prefix 'bin\mariadbd.exe') (Join-Path $Out 'bin')
Get-ChildItem (Join-Path $prefix 'bin') -Filter '*.dll' -File |
	ForEach-Object { Copy-Item $_.FullName (Join-Path $Out 'bin') }

# The three files the server reads to build its own system tables. Feeding these
# to `mariadbd --bootstrap` is the whole of initialisation: no install script and
# no perl, which is what makes one procedure work on both platforms (KB/36).
#
# Renamed with an order prefix because the order is significant and their own
# names do not carry it: sorted as shipped, mariadb_performance_tables comes
# FIRST and the bootstrap aborts. Numbering them means the obvious way to read
# the directory (a glob, a sorted ReadDir) is also the correct way, rather than
# something a caller has to know. internal/localdb sorts and feeds whatever is
# here, so this numbering is the contract between the two.
$order = @(
	'mariadb_system_tables.sql'
	'mariadb_system_tables_data.sql'
	'mariadb_performance_tables.sql'
)
$i = 1
foreach ($sql in $order) {
	$src = Join-Path $prefix "share\$sql"
	if (-not (Test-Path $src)) { throw "the ZIP has no share\$sql" }
	Copy-Item $src (Join-Path $Out ('share\bootstrap\{0:d2}-{1}' -f $i, $sql))
	$i++
}

Copy-Item (Join-Path $prefix 'share\english\errmsg.sys') (Join-Path $Out 'share\english')
Copy-Item (Join-Path $prefix 'share\charsets\*') (Join-Path $Out 'share\charsets')

# MariaDB Server is GPLv2 and we redistribute it (KB/36, licensing). Its own
# licence and third-party notices travel with it, in the bundle rather than in a
# document somebody has to remember to update.
Copy-Item (Join-Path $prefix 'COPYING') $Out
Copy-Item (Join-Path $prefix 'THIRDPARTY') $Out
Set-Content -Path (Join-Path $Out 'SOURCE.txt') -Encoding utf8 -Value @"
MariaDB Server $Version, redistributed unmodified from the official ZIP:
  $url

MariaDB Server is licensed under the GNU General Public License version 2; see
COPYING beside this file, and THIRDPARTY for the components it carries. The
corresponding source for this exact version is published at:
  https://downloads.mariadb.org/mariadb/$Version/
"@

# ---------------------------------------------------------------- verify it
# Not a file listing: the actual program, started. It is the only check that
# covers what a listing cannot, which is that every library it loads is present
# and loadable. Unlike the macOS bundler there is no cross-architecture case to
# skip for, so this always runs and a bundle that cannot start is a build that
# failed.
$server = Join-Path $Out 'bin\mariadbd.exe'
$reported = & $server --no-defaults --version 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) {
	throw "the bundled server will not run:`n$reported"
}
if ($reported -notmatch [regex]::Escape($Version)) {
	throw "the bundled server reports something other than ${Version}:`n$reported"
}

# Where it looks for plugins, asked rather than assumed.
#
# KB/36's rule is that no path is left to a compiled-in default, because a
# packaged build's defaults point wherever the packager's server lived, and one
# of those once had our server trying to lock the system MariaDB's live data
# directory. internal/localdb passes every path it knows about. plugin_dir is the
# one it does not, so the assumption that this build resolves it relative to
# basedir is checked here, where a MariaDB that changed its mind would be caught
# by a build rather than by somebody's laptop loading a stranger's DLL.
$reportedPluginDir = & $server --no-defaults "--basedir=$Out" --verbose --help 2>&1 |
	Select-String -Pattern '^plugin-dir\s+(\S.*)$' |
	Select-Object -First 1
if (-not $reportedPluginDir) {
	throw "this build does not report plugin-dir in --verbose --help; the check below cannot be trusted"
}
$pluginDir = $reportedPluginDir.Matches[0].Groups[1].Value.Trim()
if ($pluginDir -and -not $pluginDir.StartsWith($Out, [System.StringComparison]::OrdinalIgnoreCase)) {
	throw @"
this build resolves plugin-dir to $pluginDir, which is outside the bundle.
internal/localdb must pass --plugin-dir explicitly before this bundle is safe to ship.
"@
}

$size = (Get-ChildItem $Out -Recurse -File | Measure-Object Length -Sum).Sum / 1MB
Write-Host ""
Write-Host ("  MariaDB {0} bundled into {1}" -f $Version, $Out)
Write-Host ("  {0:N1} MB, and it starts." -f $size)
Write-Host ""
Write-Host "  It bootstraps and answers only when internal/localdb says so. Prove that:"
Write-Host "    `$env:SAG_DB_BUNDLE = '$Out'"
Write-Host "    cd orchestrator; go test ./internal/localdb/ -count=1 -v"
Write-Host ""
