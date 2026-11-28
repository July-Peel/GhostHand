$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$core = $env:GH_CORE_UPDATE_BIN
$stage = $env:GH_CORE_UPDATE_STAGE
$candidate = Join-Path $stage 'ghosthand-core.exe'
$backup = Join-Path $stage 'previous'
$failed = Join-Path $stage 'failed'
$ready = Join-Path $stage 'ready'
$timeout = [TimeSpan]::FromSeconds(30)
$stopping = $false
$confirmed = $false

function Stop-CoreService {
    $service.Refresh()
    if ($service.Status -eq 'StartPending') {
        $service.WaitForStatus('Running', $timeout)
    }
    if ($service.Status -ne 'Stopped') {
        if ($service.Status -ne 'StopPending') { $service.Stop() }
        $service.WaitForStatus('Stopped', $timeout)
    }
}

function Start-CoreService {
    $service.Refresh()
    if ($service.Status -eq 'StopPending') {
        $service.WaitForStatus('Stopped', $timeout)
    }
    if ($service.Status -eq 'Stopped') { $service.Start() }
    $service.WaitForStatus('Running', $timeout)
}

try {
    $service = Get-Service -Name 'GhostHandSvc'
    $stopping = $true
    Stop-CoreService
    [IO.File]::Replace($candidate, $core, $backup)
    Start-CoreService
    $wait = [Diagnostics.Stopwatch]::StartNew()
    while (-not [IO.File]::Exists($ready) -and $wait.Elapsed.TotalSeconds -lt 120) {
        Start-Sleep -Seconds 1
    }
    $confirmed = [IO.File]::Exists($ready)
    if (-not $confirmed) { throw 'Updated Core did not reconnect in time.' }
} catch {
    if ($stopping) {
        if ([IO.File]::Exists($backup)) {
            Stop-CoreService
            if ([IO.File]::Exists($ready)) {
                Start-CoreService
                $confirmed = $true
                return
            }
            if ([IO.File]::Exists($core)) {
                [IO.File]::Replace($backup, $core, $failed)
            } else {
                [IO.File]::Move($backup, $core)
            }
        }
        Start-CoreService
    }
    throw
} finally {
    if ($confirmed -or -not [IO.File]::Exists($backup)) {
        foreach ($path in $candidate, $backup, $failed, $ready, (Join-Path $stage 'expected')) {
            [IO.File]::Delete($path)
        }
        [IO.Directory]::Delete($stage)
    }
}
