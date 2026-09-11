# The desktop end-to-end gate, on Windows.
#
#   .\desktop\personal\windows\e2e.ps1
#   make desktop-e2e          (picks this script on Windows and run.sh elsewhere)
#
# What it drives is the BUILT payload in personal mode: the real gateway, the
# real bundled MariaDB, the real console and chat, on a scratch state directory
# and a free port. Nothing here is mocked, because the failures it exists to
# catch were never in the parts a mock replaces.
#
# The SPECS are shared. desktop/personal/e2e/first-run.spec.ts drives a browser
# at a running gateway and has nothing platform-specific in it; only the harness
# around it was bash, which is what this replaces. Two harnesses, one gate, and
# if they ever disagree about what they are testing, that is a bug in one of
# them rather than a Windows dialect of the suite.
[CmdletBinding()]
param(
	[string] $Payload = '',
	[int] $Port = 0,
	# Anything after -- is handed to Playwright, the way run.sh passes "$@".
	[Parameter(ValueFromRemainingArguments = $true)]
	[string[]] $PlaywrightArgs = @()
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
if (-not $Payload) { $Payload = if ($env:SAG_E2E_PAYLOAD) { $env:SAG_E2E_PAYLOAD } else { Join-Path $repo 'desktop\.local\payload' } }
if (-not $Port) { $Port = if ($env:SAG_DESKTOP_E2E_PORT) { [int] $env:SAG_DESKTOP_E2E_PORT } else { 8199 } }

$sag = Join-Path $Payload 'sag.exe'
if (-not (Test-Path $sag)) {
	Write-Error @"
desktop-e2e: no built payload at $Payload
             run .\desktop\personal\windows\build.ps1 -PayloadOnly first
"@
	exit 1
}

$state = Join-Path ([System.IO.Path]::GetTempPath()) ("sag-e2e-" + [System.Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force -Path $state | Out-Null
$log = Join-Path $repo 'desktop\personal\e2e\gateway.log'

$server = $null
function Cleanup {
	if ($script:server -and -not $script:server.HasExited) {
		# Asked the way the application asks, which is the path this gate is
		# here to keep working: the gateway drains and then its database is
		# asked to stop over its own connection. Killing it instead would leave
		# the scratch directory with a server still writing into it, and would
		# quietly stop testing the thing that broke.
		Set-Content -Path (Join-Path $state 'stop') -Value '' -NoNewline -ErrorAction SilentlyContinue
		if (-not $script:server.WaitForExit(30000)) {
			& taskkill /F /T /PID $script:server.Id 2>&1 | Out-Null
		}
	}
	Remove-Item $state -Recurse -Force -ErrorAction SilentlyContinue
}

try {
	# The specs live in desktop/personal/e2e and the toolchain lives in chat-ui,
	# so node has nothing to walk up to. A JUNCTION rather than a symbolic link:
	# both work, and only one of them can be made without administrator rights or
	# Developer Mode turned on, which is not a thing to require to run a test.
	#
	# Checked for being a DIRECTORY and not merely for existing, which is the
	# same trap run.sh documents from the other side: what is usually in the way
	# is a broken link rather than nothing, and on Windows a unix symlink from a
	# checkout is a TEXT FILE holding the path it meant to point at.
	$link = Join-Path $repo 'desktop\personal\e2e\node_modules'
	if (-not (Test-Path $link -PathType Container)) {
		Remove-Item $link -Recurse -Force -ErrorAction SilentlyContinue
		New-Item -ItemType Junction -Path $link -Target (Join-Path $repo 'chat-ui\node_modules') | Out-Null
	}

	Write-Host "desktop-e2e: starting a first run in $state"
	$env:SAG_HTTP_ADDR = "127.0.0.1:$Port"
	$env:SAG_PERSONAL_BUNDLE = Join-Path $Payload 'mariadb'
	$env:SAG_PERSONAL_STATE = $state
	$env:SAG_CONSOLE_DIR = Join-Path $Payload 'console'
	$env:SAG_CHAT_DIR = Join-Path $Payload 'chat'
	$env:SAG_LOG_FORMAT = 'json'
	$server = Start-Process -FilePath $sag -ArgumentList 'personal' -PassThru -NoNewWindow `
		-RedirectStandardOutput $log -RedirectStandardError "$log.err"

	# A first run creates a database and applies every migration, so this is a
	# generous wait rather than an optimistic one.
	$up = $false
	foreach ($i in 1..180) {
		if ($server.HasExited) {
			Write-Host "desktop-e2e: the gateway exited during startup" -ForegroundColor Red
			Get-Content "$log.err" -Tail 20 -ErrorAction SilentlyContinue
			exit 1
		}
		try {
			if ((Invoke-WebRequest -Uri "http://127.0.0.1:$Port/healthz" -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200) {
				$up = $true
				break
			}
		} catch { Start-Sleep -Seconds 1 }
	}
	if (-not $up) {
		Write-Host "desktop-e2e: the gateway never answered" -ForegroundColor Red
		Get-Content "$log.err" -Tail 20 -ErrorAction SilentlyContinue
		exit 1
	}

	Write-Host "desktop-e2e: driving both surfaces"
	Push-Location (Join-Path $repo 'chat-ui')
	try {
		$env:SAG_DESKTOP_E2E_URL = "http://127.0.0.1:$Port"
		& npx playwright test --config (Join-Path $repo 'chat-ui\desktop-e2e.config.ts') @PlaywrightArgs
		$code = $LASTEXITCODE
	} finally { Pop-Location }
	if ($code -ne 0) { exit $code }
} finally {
	Cleanup
}
