# Draw the application's icon for Windows, and put it in the container Windows
# reads.
#
#   .\icon.ps1
#
# The MARK is not drawn here. desktop/shared/icons/sag.svg and sag-small.svg are
# the sources, and every decision about the drawing stays in them and in
# build-icons.sh: which artwork is used at which size (the small one below 64
# real pixels, because the detailed mark turns to mush there), the stroke
# weights, the colours. This renders those same sources, at the same sizes, with
# one thing changed.
#
# THE ONE THING: the macOS grid comes off.
#
# The sources say so themselves: "Drawn on Apple's macOS grid: a 1024 canvas
# with the tile inset to 824". Every Mac icon carries that inset, so the system's
# shadow and spacing land where they should and every icon in the Dock agrees
# with every other. Windows has no such grid. Its icons fill their canvas, so the
# same artwork sits in the taskbar with a tenth of its width in empty space on
# each side and reads as a smaller application than everything beside it. That
# was reported, and it is the reason this file exists rather than a two-line
# repackaging of the PNGs.
#
# So the viewBox is moved in to the tile, 100 100 824 824, and the mark is
# RE-RENDERED at each size from the vector. Not cropped and rescaled from the
# PNGs: at 32 pixels that would either resample a 64 (and quietly use the
# detailed artwork where the small one is intended) or upscale a 32 by a quarter.
# Rendering costs nothing here and both of those cost quality at exactly the
# sizes where a mark either survives or does not.
[CmdletBinding()]
param(
	[string] $Out = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$sources = Join-Path $repo 'desktop\shared\icons'
# Its own directory beside the macOS ones rather than among them, because these
# are a different drawing of the same mark: the same sources at the same sizes
# without the macOS grid. Keeping them apart means neither set can be mistaken
# for the other, and `icons/*.png` goes on meaning what build-icons.sh drew.
$icons = Join-Path $repo 'desktop\personal\icons\windows'
if (-not $Out) { $Out = Join-Path $icons 'icon.ico' }
New-Item -ItemType Directory -Force -Path $icons | Out-Null

if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
	throw "node is not on PATH, and the mark is rendered with it"
}

# Chromium does the drawing, because it is the renderer already in the repo and
# it agrees with what a browser would show (see render.mjs). It comes from the
# chat's toolchain rather than a second install, exactly as build-icons.sh and
# the desktop gate borrow it. A JUNCTION rather than a symbolic link: both work
# and only one of them can be made without administrator rights.
#
# Checked for being a DIRECTORY and not merely for existing. What was there was
# a unix symlink that had been committed to the repository, and a Windows
# checkout writes one of those out as an ordinary text file containing the path
# it meant to point at. It exists, so a presence test is satisfied, and then
# node finds a file where a package should be and nothing can be drawn.
$link = Join-Path $sources 'node_modules'
if (-not (Test-Path $link -PathType Container)) {
	Remove-Item $link -Recurse -Force -ErrorAction SilentlyContinue
	New-Item -ItemType Junction -Path $link -Target (Join-Path $repo 'chat-ui\node_modules') | Out-Null
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("sag-icon-" + [System.Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
	# The tile, without the grid around it. Asserted rather than assumed: a source
	# that stopped carrying this viewBox would otherwise be rendered whole and
	# ship the too-small icon again, silently, which is how it got here.
	function Unframe($name) {
		$src = Join-Path $sources $name
		$svg = Get-Content $src -Raw
		$framed = $svg -replace 'viewBox="0 0 1024 1024"', 'viewBox="100 100 824 824"'
		if ($framed -eq $svg) {
			throw "$name no longer carries the macOS grid this crops away; the mark would ship inset"
		}
		$out = Join-Path $work $name
		Set-Content -Path $out -Value $framed -Encoding utf8 -NoNewline
		return $out
	}
	$detailed = Unframe 'sag.svg'
	$small = Unframe 'sag-small.svg'

	# Which artwork at which size, taken from build-icons.sh rather than decided
	# again here: the small mark where the icon is physically tiny, the detailed
	# one from 64 real pixels up, where there is room for it.
	#
	# 16 (title bar, tray), 32 (desktop, taskbar), 48 (Explorer's medium view),
	# 256 (the large views). 48 is drawn rather than left to Windows to scale,
	# which is the whole point of rendering from the vector: it costs one more
	# invocation and it is a size the system genuinely uses.
	$plan = @(
		@{ Size = 16; Svg = $small }
		@{ Size = 32; Svg = $small }
		@{ Size = 48; Svg = $detailed }
		@{ Size = 64; Svg = $detailed }
		@{ Size = 128; Svg = $detailed }
		@{ Size = 256; Svg = $detailed }
	)

	# The PNGs are KEPT, beside the .ico rather than inside a temporary
	# directory. They are what went in, so a mark that looks wrong can be looked
	# at rather than reasoned about, and the installer's own images can be built
	# from them later without rendering again.
	$render = Join-Path $sources 'render.mjs'
	$images = foreach ($step in $plan) {
		$png = Join-Path $icons "$($step.Size).png"
		& node $render $step.Svg $png $step.Size
		if ($LASTEXITCODE -ne 0) {
			throw @"
rendering the mark at $($step.Size) failed.
If the browser is missing: cd chat-ui; npx playwright install chromium
"@
		}
		[pscustomobject]@{ Size = $step.Size; Bytes = [System.IO.File]::ReadAllBytes($png) }
	}

	# An .ico is a header, a directory, and the images. Since Vista the images may
	# be PNGs held whole, which is what this writes: the bytes that go in are the
	# exact bytes that came out of the renderer, so nothing is resampled on the
	# way and no image library is involved.
	$stream = [System.IO.MemoryStream]::new()
	$w = [System.IO.BinaryWriter]::new($stream)
	try {
		# ICONDIR: reserved, type 1 (icon), how many images.
		$w.Write([uint16] 0)
		$w.Write([uint16] 1)
		$w.Write([uint16] $images.Count)

		# Every image sits after the whole directory, so the offsets are known
		# before a byte of image data is written.
		$offset = 6 + (16 * $images.Count)
		foreach ($image in $images) {
			# 256 is written as 0: the field is one byte and 256 does not fit in
			# it. That is the format's own convention, not a trick.
			$dimension = if ($image.Size -ge 256) { 0 } else { $image.Size }
			$w.Write([byte] $dimension)          # width
			$w.Write([byte] $dimension)          # height
			$w.Write([byte] 0)                   # palette entries: none, it is truecolour
			$w.Write([byte] 0)                   # reserved
			$w.Write([uint16] 1)                 # colour planes
			$w.Write([uint16] 32)                # bits per pixel
			$w.Write([uint32] $image.Bytes.Length)
			$w.Write([uint32] $offset)
			$offset += $image.Bytes.Length
		}
		foreach ($image in $images) { $w.Write($image.Bytes) }
		$w.Flush()
		[System.IO.File]::WriteAllBytes($Out, $stream.ToArray())
	} finally {
		$w.Dispose()
		$stream.Dispose()
	}
} finally {
	Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ("  {0}: {1} sizes ({2}), {3:N0} bytes, filling the frame" -f `
	(Split-Path $Out -Leaf), 6, '16, 32, 48, 64, 128, 256', (Get-Item $Out).Length)
