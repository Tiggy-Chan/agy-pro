# One-line installer for agy-pro on Windows PowerShell (amd64 & arm64)
# Usage: irm https://raw.githubusercontent.com/Tiggy-Chan/agy-pro/main/install.ps1 | iex
$ErrorActionPreference = "Stop"

$Repo = "Tiggy-Chan/agy-pro"
$InstallDir = if ($env:AGY_PRO_INSTALL_DIR) { $env:AGY_PRO_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "agy\bin" }
$Version = $env:AGY_PRO_VERSION

$Arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()) {
    "x64"   { "amd64" }
    "arm64" { "arm64" }
    default { throw "Unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
}

if (-not $Version) {
    $req = [System.Net.HttpWebRequest]::Create("https://github.com/$Repo/releases/latest")
    $req.AllowAutoRedirect = $true
    $resp = $req.GetResponse()
    try {
        $finalUrl = $resp.ResponseUri.AbsoluteUri
    } finally {
        $resp.Close()
    }
    $Version = ($finalUrl -split "/")[-1]
    if (-not $Version -or $Version -eq "releases" -or $Version -eq "latest") {
        throw "Failed to resolve latest release tag for $Repo"
    }
}

$Asset = "agy-pro-windows-$Arch.exe"
$BaseUrl = "https://github.com/$Repo/releases/download/$Version"
$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("agy-pro-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TmpDir | Out-Null

try {
    $BinTmp = Join-Path $TmpDir $Asset
    $SumTmp = Join-Path $TmpDir "checksums.txt"

    Write-Host "Downloading agy-pro $Version (windows/$Arch)..."
    Invoke-WebRequest -Uri "$BaseUrl/$Asset" -OutFile $BinTmp -UseBasicParsing
    Invoke-WebRequest -Uri "$BaseUrl/checksums.txt" -OutFile $SumTmp -UseBasicParsing

    $Expected = $null
    foreach ($line in Get-Content $SumTmp) {
        if ($line -match "^([0-9a-fA-F]{64})\s+$([regex]::Escape($Asset))$") {
            $Expected = $Matches[1].ToLowerInvariant()
            break
        }
    }
    if (-not $Expected) {
        throw "Checksum entry for $Asset not found in checksums.txt"
    }
    $Actual = (Get-FileHash -Path $BinTmp -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) {
        throw "SHA-256 verification failed for $Asset (expected $Expected, got $Actual)"
    }

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }
    $Target = Join-Path $InstallDir "agy-pro.exe"
    Copy-Item -Path $BinTmp -Destination $Target -Force
    Unblock-File -Path $Target -ErrorAction SilentlyContinue

    # Ensure InstallDir is in User PATH (matches official agy\bin so CMD & PowerShell both find agy-pro)
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $Parts = if ($UserPath) { $UserPath -split ";" } else { @() }
    if (-not ($Parts | Where-Object { $_.TrimEnd("\") -ieq $InstallDir.TrimEnd("\") })) {
        $NewPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
        if ($env:Path -notlike "*$InstallDir*") {
            $env:Path = "$env:Path;$InstallDir"
        }
        Write-Host "Added $InstallDir to your User PATH."
    }

    Write-Host "Installed agy-pro $Version -> $Target"
} finally {
    Remove-Item -Path $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
