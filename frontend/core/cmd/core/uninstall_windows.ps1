$ErrorActionPreference = 'Stop'

function Assert-OrdinaryPath([string]$path) {
    while ($path) {
        if (([IO.File]::GetAttributes($path) -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw 'GhostHand installation paths cannot contain reparse points.'
        }
        $path = [IO.Path]::GetDirectoryName($path)
    }
}

function Remove-ProductTree([string]$path) {
    $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
    if ($null -eq $item) { return }
    $isLink = ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0
    if ($item.PSIsContainer) {
        if (-not $isLink) {
            foreach ($entry in [IO.Directory]::EnumerateFileSystemEntries($path)) {
                Remove-ProductTree $entry
            }
        }
        [IO.Directory]::Delete($path)
    } else {
        if (-not $isLink -and $item.IsReadOnly) { $item.IsReadOnly = $false }
        [IO.File]::Delete($path)
    }
}

function Invoke-CleanupWithRetry([scriptblock]$cleanup) {
    for ($attempt = 1; $attempt -le 30; $attempt++) {
        try {
            & $cleanup
            return
        } catch {
            if ($attempt -eq 30) { throw }
            Start-Sleep -Seconds 1
        }
    }
}

$launcher = $env:GHOSTHAND_UNINSTALL_LAUNCHER
$bootstrap = [IO.Path]::GetDirectoryName($launcher)
$dataRoot = $env:GHOSTHAND_UNINSTALL_DATA
Assert-OrdinaryPath $launcher
if ($dataRoot) { Assert-OrdinaryPath $dataRoot }

# Inno Setup owns its uninstall registration and any installer-managed files.
$registration = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{DA1E8410-9319-438F-A531-60C2081347B0}_is1'
$product = Get-ItemProperty -LiteralPath $registration -ErrorAction SilentlyContinue
$uninstaller = $null
if ($product.InstallLocation -and $product.InstallLocation.TrimEnd([char]92) -eq $bootstrap) {
    $uninstaller = $product.UninstallString.Trim([char]34)
    if ([IO.Path]::GetDirectoryName($uninstaller) -ne $bootstrap -or
        [IO.Path]::GetFileName($uninstaller) -notmatch '^unins[0-9]+\.exe$') {
        throw 'Unexpected GhostHand Inno Setup uninstaller path.'
    }
    Assert-OrdinaryPath $uninstaller
}

[Console]::WriteLine('ready')
if ([Console]::ReadLine() -ne 'uninstall') { exit 1 }

Invoke-CleanupWithRetry {
    & $launcher 'uninstall'
    if ($LASTEXITCODE -ne 0) { throw 'GhostHandSvc could not be stopped and removed.' }
}

$versions = if ($dataRoot) { [IO.Path]::Combine($dataRoot, 'versions') } else { $null }
foreach ($process in (Get-Process -Name 'ghosthand-launcher', 'ghosthand-core', 'GhostHand.Helper' -ErrorAction SilentlyContinue)) {
    if (-not $process.Path) { continue }
    $directory = [IO.Path]::GetDirectoryName($process.Path)
    if ($directory -eq $bootstrap -or
        ($versions -and [IO.Path]::GetDirectoryName($directory) -eq $versions -and [IO.Path]::GetFileName($directory) -match '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')) {
        Stop-Process -InputObject $process -Force -ErrorAction SilentlyContinue
    }
}

try {
    if ($uninstaller) {
        $result = Start-Process -FilePath $uninstaller -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART' -Wait -PassThru
        if ($result.ExitCode -ne 0) { throw 'Inno Setup did not complete the GhostHand uninstall.' }
    }
} finally {
    try {
        if ($dataRoot) {
            Invoke-CleanupWithRetry {
                Assert-OrdinaryPath ([IO.Path]::GetDirectoryName($dataRoot))
                Remove-ProductTree $dataRoot
            }
        }
    } finally {
        Invoke-CleanupWithRetry {
            if (-not [IO.Directory]::Exists($bootstrap)) { return }
            Assert-OrdinaryPath $bootstrap
            foreach ($name in 'ghosthand-launcher.exe', 'ghosthand-core.exe', 'GhostHand.Helper.exe') {
                [IO.File]::Delete([IO.Path]::Combine($bootstrap, $name))
            }
            if (-not [IO.Directory]::GetFileSystemEntries($bootstrap).Length) {
                [IO.Directory]::Delete($bootstrap)
            }
        }
    }
}
