# Builds the Windows desktop edition and the Linux server edition.
#
#   .\build.ps1              both editions into dist\
#   .\build.ps1 -Only desktop
#   .\build.ps1 -Only server
#
# The desktop build is linked with -H=windowsgui so double-clicking it never
# opens a console window. The server build is a normal Linux binary with no
# GUI dependency, ready to drop next to a systemd unit.

[CmdletBinding()]
param(
    [ValidateSet("all", "desktop", "server")]
    [string]$Only = "all"
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$dist = Join-Path $PSScriptRoot "dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null

# -trimpath drops local build paths from the binary; -s -w strips the symbol
# table and DWARF data, which roughly halves the file size.
$ldflags = "-s -w"

function Build-Target {
    param(
        [string]$Name,
        [string]$Package,
        [string]$GOOS,
        [string]$GOARCH,
        [string]$Output,
        [string]$ExtraLdflags = ""
    )

    Write-Host "building $Name ($GOOS/$GOARCH) -> $Output"
    $env:CGO_ENABLED = "0"
    $env:GOOS = $GOOS
    $env:GOARCH = $GOARCH

    $flags = $ldflags
    if ($ExtraLdflags) { $flags = "$flags $ExtraLdflags" }

    $target = Join-Path $dist $Output
    & go build -trimpath -ldflags $flags -o $target $Package
    if ($LASTEXITCODE -ne 0) {
        throw "build failed for $Name"
    }
    $size = [math]::Round((Get-Item $target).Length / 1MB, 1)
    Write-Host ("  ok  {0}  ({1} MB)" -f $Output, $size)
}

if ($Only -eq "all" -or $Only -eq "desktop") {
    # windowsgui suppresses the console window; the app logs to
    # %APPDATA%\trading-agent\desktop.log instead.
    Build-Target -Name "desktop" -Package "./cmd/trading-agent-desktop" `
        -GOOS "windows" -GOARCH "amd64" `
        -Output "trading-agent-desktop-windows-amd64.exe" `
        -ExtraLdflags "-H=windowsgui"
}

if ($Only -eq "all" -or $Only -eq "server") {
    Build-Target -Name "server" -Package "./cmd/trading-agent-server" `
        -GOOS "linux" -GOARCH "amd64" `
        -Output "trading-agent-server-linux-amd64"

    Build-Target -Name "server" -Package "./cmd/trading-agent-server" `
        -GOOS "linux" -GOARCH "arm64" `
        -Output "trading-agent-server-linux-arm64"
}

# Reset so later shell commands in this session are not left cross-compiling.
Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "artifacts in $dist"
Get-ChildItem $dist | Select-Object Name, @{ n = "MB"; e = { [math]::Round($_.Length / 1MB, 1) } }
