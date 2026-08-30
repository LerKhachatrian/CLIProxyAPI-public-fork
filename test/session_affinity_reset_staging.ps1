[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$CandidatePath,

    [ValidateRange(1, 65535)]
    [int]$ProxyPort = 48318
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-LoopbackPortAvailable {
    param([int]$Port)

    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, $Port)
    try {
        $listener.Start()
        return $true
    }
    catch {
        return $false
    }
    finally {
        $listener.Stop()
    }
}

if (-not (Test-LoopbackPortAvailable -Port $ProxyPort)) {
    throw "Loopback port $ProxyPort is already in use"
}

$candidateFullPath = [System.IO.Path]::GetFullPath($CandidatePath)
$tempBase = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$tempRoot = Join-Path $tempBase ("cliproxy-affinity-reset-e2e-{0}-{1}" -f $PID, [guid]::NewGuid().ToString('N'))
$tempRoot = [System.IO.Path]::GetFullPath($tempRoot)
$tempLeaf = Split-Path -Leaf $tempRoot
if (-not $tempRoot.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -or
    -not $tempLeaf.StartsWith('cliproxy-affinity-reset-e2e-', [System.StringComparison]::Ordinal)) {
    throw "Refusing unsafe temporary path: $tempRoot"
}

$authDir = Join-Path $tempRoot 'auths'
$configPath = Join-Path $tempRoot 'config.yaml'
$stdoutPath = Join-Path $tempRoot 'proxy.stdout.log'
$stderrPath = Join-Path $tempRoot 'proxy.stderr.log'
$managementKey = "staging-management-$([guid]::NewGuid().ToString('N'))"
$proxyProcess = $null
$failure = $null
$result = $null

try {
    New-Item -ItemType Directory -Path $authDir -Force | Out-Null
    $yamlAuthDir = $authDir.Replace('\', '/')
    $config = @"
host: "127.0.0.1"
port: $ProxyPort
auth-dir: "$yamlAuthDir"
remote-management:
  allow-remote: false
  secret-key: "$managementKey"
commercial-mode: true
logging-to-file: false
routing:
  strategy: "round-robin"
  session-affinity: true
  session-affinity-ttl: "1h"
"@
    [System.IO.File]::WriteAllText($configPath, $config, [System.Text.UTF8Encoding]::new($false))

    $proxyProcess = Start-Process -FilePath $candidateFullPath `
        -ArgumentList @('-config', $configPath, '-local-model') `
        -PassThru `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutPath `
        -RedirectStandardError $stderrPath

    $ready = $false
    for ($attempt = 1; $attempt -le 80; $attempt++) {
        if ($proxyProcess.HasExited) {
            throw "Staging candidate exited before health became ready"
        }
        try {
            Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:$ProxyPort/healthz" -TimeoutSec 1 | Out-Null
            $ready = $true
            break
        }
        catch {
            Start-Sleep -Milliseconds 125
        }
    }
    if (-not $ready) {
        throw "Timed out waiting for staging health on port $ProxyPort"
    }

    $statusUri = "http://127.0.0.1:$ProxyPort/v0/management/routing/session-affinity"
    try {
        Invoke-RestMethod -Method Get -Uri $statusUri -TimeoutSec 5 | Out-Null
        throw 'Unauthenticated management request unexpectedly succeeded'
    }
    catch {
        $statusCode = if ($null -ne $_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
        if ($statusCode -ne 401) {
            throw "Unauthenticated management request returned $statusCode instead of 401"
        }
    }

    $headers = @{ Authorization = "Bearer $managementKey" }
    $status = Invoke-RestMethod -Method Get -Uri $statusUri -Headers $headers -TimeoutSec 5
    if ($status.enabled -ne $true -or [int]$status.session_keys -ne 0) {
        throw 'Session-affinity status did not report enabled with zero staging keys'
    }

    $resetUri = "$statusUri/reset"
    $firstReset = Invoke-RestMethod -Method Post -Uri $resetUri -Headers $headers -TimeoutSec 5
    $secondReset = Invoke-RestMethod -Method Post -Uri $resetUri -Headers $headers -TimeoutSec 5
    if ($firstReset.status -ne 'ok' -or [int]$firstReset.cleared_session_keys -ne 0) {
        throw 'First empty-cache reset did not return the idempotent success contract'
    }
    if ($secondReset.status -ne 'ok' -or [int]$secondReset.cleared_session_keys -ne 0) {
        throw 'Repeated empty-cache reset did not return the idempotent success contract'
    }

    $result = [pscustomobject]@{
        status = 'passed'
        candidate_sha256 = (Get-FileHash -LiteralPath $candidateFullPath -Algorithm SHA256).Hash
        proxy_port = $ProxyPort
        unauthenticated_status = 401
        affinity_enabled = [bool]$status.enabled
        initial_session_keys = [int]$status.session_keys
        first_cleared_session_keys = [int]$firstReset.cleared_session_keys
        repeated_cleared_session_keys = [int]$secondReset.cleared_session_keys
    }
}
catch {
    $failure = $_
}
finally {
    if ($null -ne $proxyProcess -and -not $proxyProcess.HasExited) {
        Stop-Process -Id $proxyProcess.Id -Force
        $proxyProcess.WaitForExit()
    }
    if (Test-Path -LiteralPath $tempRoot) {
        $resolvedCleanupTarget = [System.IO.Path]::GetFullPath($tempRoot)
        $cleanupLeaf = Split-Path -Leaf $resolvedCleanupTarget
        if ($resolvedCleanupTarget.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -and
            $cleanupLeaf.StartsWith('cliproxy-affinity-reset-e2e-', [System.StringComparison]::Ordinal)) {
            Remove-Item -LiteralPath $resolvedCleanupTarget -Recurse -Force
        }
        else {
            Write-Warning "Skipped unsafe cleanup target: $resolvedCleanupTarget"
        }
    }
}

if ($null -ne $failure) {
    Write-Error -ErrorRecord $failure
    exit 1
}
if (-not (Test-LoopbackPortAvailable -Port $ProxyPort)) {
    throw "Staging process still owns port $ProxyPort after teardown"
}

$result | ConvertTo-Json -Depth 3
