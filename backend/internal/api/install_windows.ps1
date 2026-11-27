param(
    [Parameter(Mandatory = $true)][string]$Server,
    [Parameter(Mandatory = $true)][string]$NodeKey,
    [string]$Fingerprint = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Open PowerShell as Administrator and run the GhostHand install command again.'
}
if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -or [Environment]::OSVersion.Version.Build -lt 19041) {
    throw 'GhostHand requires 64-bit PowerShell on Windows 10 (build 19041) or later, x64.'
}
$origin = [uri]$Server
if (-not $origin.IsAbsoluteUri -or $origin.AbsolutePath -ne '/' -or $origin.UserInfo -or $origin.Query -or $origin.Fragment -or
    ($origin.Scheme -ne 'https' -and ($origin.Scheme -ne 'http' -or -not $origin.IsLoopback))) {
    throw 'Use the HTTPS controller origin to install GhostHand.'
}

$serviceName = 'GhostHandSvc'
$installDir = Join-Path $env:ProgramFiles 'GhostHand'
if ((Get-Service -Name $serviceName -ErrorAction SilentlyContinue) -or (Test-Path -LiteralPath $installDir)) {
    throw 'GhostHand is already installed or its installation directory exists.'
}

$baseUrl = $Server + '/api/agent/install/windows/'
$headers = @{ Authorization = 'Bearer ' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($NodeKey)) }
$manifest = Invoke-RestMethod -UseBasicParsing -MaximumRedirection 0 -TimeoutSec 120 -Uri ($baseUrl + 'manifest') -Headers $headers
$names = 'GhostHand.Helper.exe', 'ghosthand-core.exe', 'ghosthand-launcher.exe'
$stage = Join-Path $env:ProgramFiles ('GhostHand.install-' + [guid]::NewGuid().ToString('N'))
$moved = $false
try {
    New-Item -Path $stage -ItemType Directory | Out-Null
    foreach ($name in $names) {
        $entries = @($manifest | Where-Object { $_.name -ceq $name })
        if ($entries.Count -ne 1 -or $entries[0].sha256 -notmatch '^[0-9a-f]{64}$') {
            throw "Invalid GhostHand manifest entry for $name."
        }
        $download = Join-Path $stage $name
        Write-Host "Downloading $name..."
        Invoke-WebRequest -UseBasicParsing -MaximumRedirection 0 -TimeoutSec 600 -Uri ($baseUrl + 'files/' + $name) -Headers $headers -OutFile $download
        if ((Get-Item -LiteralPath $download).Length -ne $entries[0].size -or
            (Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash -ne $entries[0].sha256) {
            throw "Checksum mismatch for $name. Run the install command again after uploads are complete."
        }
    }
    [IO.Directory]::Move($stage, $installDir)
    $moved = $true

    $savedServer, $savedKey, $savedFingerprint = $env:GH_SERVER, $env:GH_KEY, $env:GH_FINGERPRINT
    try {
        $env:GH_SERVER = $Server
        $env:GH_KEY = $NodeKey
        $env:GH_FINGERPRINT = $Fingerprint
        & (Join-Path $installDir 'ghosthand-launcher.exe') install
        if ($LASTEXITCODE -ne 0) {
            throw "GhostHand service installation failed (exit code $LASTEXITCODE)."
        }
    } finally {
        $env:GH_SERVER, $env:GH_KEY, $env:GH_FINGERPRINT = $savedServer, $savedKey, $savedFingerprint
    }
    (Get-Service -Name $serviceName).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    Write-Host 'GhostHand service is running and will connect to the controller.'
} catch {
    if ($moved -and -not (Get-Service -Name $serviceName -ErrorAction SilentlyContinue)) {
        Remove-Item -LiteralPath $installDir -Recurse -Force -ErrorAction SilentlyContinue
    }
    throw
} finally {
    if (Test-Path -LiteralPath $stage) {
        Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
    }
}
