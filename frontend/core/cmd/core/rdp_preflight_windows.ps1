$ErrorActionPreference = 'Stop'
$Certificate = $null
$Now = Get-Date
$Fingerprint = ''

try {
    $Listener = Get-CimInstance -Namespace 'root/cimv2/TerminalServices' -ClassName Win32_TSGeneralSetting -Filter "TerminalName='RDP-Tcp'"
    $Thumbprint = ([string]$Listener.SSLCertificateSHA1Hash).Trim()
    if ($Thumbprint -match '^[0-9a-fA-F]{40}$' -and $Thumbprint -notmatch '^0+$') {
        $Certificate = Get-Item -LiteralPath "Cert:\LocalMachine\My\$Thumbprint" -ErrorAction SilentlyContinue
        if (-not $Certificate) {
            $Certificate = Get-Item -LiteralPath "Cert:\LocalMachine\Remote Desktop\$Thumbprint" -ErrorAction SilentlyContinue
        }
    }
} catch { }

try {
    if (-not $Certificate) {
        $Certificate = Get-ChildItem -LiteralPath 'Cert:\LocalMachine\Remote Desktop' |
            Where-Object { $_.NotBefore -le $Now -and $_.NotAfter -gt $Now } |
            Sort-Object -Property NotAfter -Descending | Select-Object -First 1
    }
    if ($Certificate -and $Certificate.NotBefore -le $Now -and $Certificate.NotAfter -gt $Now) {
        $SHA256 = [System.Security.Cryptography.SHA256]::Create()
        try { $Fingerprint = [BitConverter]::ToString($SHA256.ComputeHash($Certificate.RawData)).Replace('-', '').ToLowerInvariant() }
        finally { $SHA256.Dispose() }
    }
} catch { }

@{ fingerprint = $Fingerprint } | ConvertTo-Json -Compress
if (-not $ProbeFirewall) { return }

function Test-LoopbackOnly($Addresses) {
    return ($Addresses.Count -gt 0 -and @($Addresses | Where-Object {
        $_ -notin @('127.0.0.0/8', '127.0.0.1', '127.0.0.1/32', '::1', '::1/128')
    }).Count -eq 0)
}

$InboundOpen = $null
try {
    $ActiveProfiles = @(Get-NetConnectionProfile | Where-Object {
        $_.IPv4Connectivity -ne 'Disconnected' -or $_.IPv6Connectivity -ne 'Disconnected'
    } | ForEach-Object {
        switch ($_.NetworkCategory.ToString()) {
            'DomainAuthenticated' { 'Domain' }
            'Private' { 'Private' }
            'Public' { 'Public' }
        }
    } | Sort-Object -Unique)
    if ($ActiveProfiles.Count -gt 0) {
        $Profiles = @(Get-NetFirewallProfile -PolicyStore ActiveStore -Name $ActiveProfiles)
        if (@($Profiles | Where-Object {
            [string]$_.Enabled -eq 'False' -or [string]$_.DefaultInboundAction -eq 'Allow'
        }).Count -gt 0) {
            $InboundOpen = $true
        } elseif ($Profiles.Count -eq $ActiveProfiles.Count -and @($Profiles | Where-Object {
            [string]$_.Enabled -eq 'True' -and [string]$_.DefaultInboundAction -eq 'Block'
        }).Count -eq $Profiles.Count) {
            $InboundOpen = $false
        }
    }
    if ($ActiveProfiles.Count -gt 0 -and $InboundOpen -ne $true) {
        $Filters = Get-NetFirewallPortFilter -PolicyStore ActiveStore | Where-Object {
            $_.Protocol -in @('TCP', 'UDP', '6', '17', 'Any', '256') -and @($_.LocalPort | Where-Object {
                $_ -eq 'Any' -or $_ -eq "$Port" -or ($_ -match '^(\d+)-(\d+)$' -and $Port -ge [int]$Matches[1] -and $Port -le [int]$Matches[2])
            }).Count -gt 0
        }
        foreach ($Filter in $Filters) {
            $Rule = Get-NetFirewallRule -AssociatedNetFirewallPortFilter $Filter
            if ($Rule.Enabled -ne 'True' -or $Rule.Direction -ne 'Inbound' -or $Rule.Action -ne 'Allow') { continue }
            $RuleProfiles = [string]$Rule.Profile -split '\s*,\s*'
            if ('Any' -notin $RuleProfiles -and @($RuleProfiles | Where-Object { $_ -in $ActiveProfiles }).Count -eq 0) { continue }
            $Service = (Get-NetFirewallServiceFilter -AssociatedNetFirewallRule $Rule).Service
            $Application = Get-NetFirewallApplicationFilter -AssociatedNetFirewallRule $Rule
            if ($Service -notin @('Any', 'TermService')) { continue }
            if ($Application.Program -ne 'Any' -and $Application.Program -notlike '*\svchost.exe') { continue }
            if ($Application.Package -and $Application.Package -ne 'Any') { continue }
            $Address = Get-NetFirewallAddressFilter -AssociatedNetFirewallRule $Rule
            if ((Test-LoopbackOnly $Address.LocalAddress) -or (Test-LoopbackOnly $Address.RemoteAddress)) { continue }
            $InboundOpen = $true
            break
        }
    }
} catch { $InboundOpen = $null }

@{ inbound_open = $InboundOpen } | ConvertTo-Json -Compress
