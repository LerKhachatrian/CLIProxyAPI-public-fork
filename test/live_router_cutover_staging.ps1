[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$BaselinePath,

    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$CandidatePath,

    [ValidateRange(1, 65535)]
    [int]$SuccessPort = 48320,

    [ValidateRange(1, 65535)]
    [int]$RollbackPort = 48321
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

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

function Wait-HttpReady {
    param(
        [string]$Uri,
        [int]$TimeoutSeconds = 15
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            Invoke-WebRequest -UseBasicParsing -Method Get -Uri $Uri -TimeoutSec 1 | Out-Null
            return
        }
        catch {
            Start-Sleep -Milliseconds 125
        }
    }
    throw "Timed out waiting for $Uri"
}

function Get-SingleListener {
    param([int]$Port)

    $listeners = @(Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue)
    if ($listeners.Count -gt 1) {
        throw "Expected at most one listener on port $Port; found $($listeners.Count)"
    }
    if ($listeners.Count -eq 0) {
        return $null
    }
    return $listeners[0]
}

function Stop-OwnedPortProcess {
    param(
        [int]$Port,
        [string]$OwnedRoot
    )

    $listener = Get-SingleListener -Port $Port
    if ($null -eq $listener) {
        return
    }
    $processInfo = Get-CimInstance Win32_Process -Filter "ProcessId=$($listener.OwningProcess)"
    $executablePath = if ($null -ne $processInfo) { [System.IO.Path]::GetFullPath($processInfo.ExecutablePath) } else { "" }
    $normalizedRoot = [System.IO.Path]::GetFullPath($OwnedRoot)
    if ([string]::IsNullOrWhiteSpace($executablePath) -or -not $executablePath.StartsWith($normalizedRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to stop unowned PID $($listener.OwningProcess) on port $Port"
    }
    Stop-Process -Id ([int]$listener.OwningProcess) -Force
    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline) {
        if ($null -eq (Get-SingleListener -Port $Port)) {
            return
        }
        Start-Sleep -Milliseconds 100
    }
    throw "Owned PID $($listener.OwningProcess) did not release port $Port"
}

function New-Scenario {
    param(
        [string]$Root,
        [string]$Name,
        [int]$Port,
        [string]$Baseline,
        [string]$Candidate
    )

    $scenarioRoot = Join-Path $Root $Name
    $binRoot = Join-Path $scenarioRoot "bin"
    $authRoot = Join-Path $scenarioRoot "auths"
    $backupRoot = Join-Path $scenarioRoot "backups"
    New-Item -ItemType Directory -Path $binRoot, $authRoot, $backupRoot -Force | Out-Null

    $livePath = Join-Path $binRoot "cliproxyapi.exe"
    $candidatePath = Join-Path $binRoot "candidate.exe"
    [System.IO.File]::Copy($Baseline, $livePath, $false)
    [System.IO.File]::Copy($Candidate, $candidatePath, $false)

    $configPath = Join-Path $scenarioRoot "config.yaml"
    $yamlAuthRoot = $authRoot.Replace("\", "/")
    $config = @"
host: "127.0.0.1"
port: $Port
auth-dir: "$yamlAuthRoot"
api-keys:
  - "cutover-staging"
debug: false
logging-to-file: false
request-retry: 0
"@
    [System.IO.File]::WriteAllText($configPath, $config, [System.Text.UTF8Encoding]::new($false))

    return [pscustomobject]@{
        Root = $scenarioRoot
        Port = $Port
        LivePath = $livePath
        CandidatePath = $candidatePath
        ConfigPath = $configPath
        RollbackPath = Join-Path $backupRoot "cliproxyapi.pre-cutover.exe"
        StdoutPath = Join-Path $scenarioRoot "router.stdout.log"
        StderrPath = Join-Path $scenarioRoot "router.stderr.log"
        HealthUri = "http://127.0.0.1:$Port/healthz"
    }
}

function Start-ScenarioRouter {
    param([object]$Scenario)

    return Start-Process -FilePath $Scenario.LivePath -ArgumentList @("-config", $Scenario.ConfigPath, "-local-model") -WorkingDirectory $Scenario.Root -WindowStyle Hidden -RedirectStandardOutput $Scenario.StdoutPath -RedirectStandardError $Scenario.StderrPath -PassThru
}

if ($SuccessPort -eq $RollbackPort) {
    throw "SuccessPort and RollbackPort must differ"
}
foreach ($port in @($SuccessPort, $RollbackPort)) {
    if (-not (Test-LoopbackPortAvailable -Port $port)) {
        throw "Loopback port $port is already in use"
    }
}

$baselineFullPath = [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $BaselinePath).Path)
$candidateFullPath = [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $CandidatePath).Path)
$baselineHash = (Get-FileHash -LiteralPath $baselineFullPath -Algorithm SHA256).Hash
$candidateHash = (Get-FileHash -LiteralPath $candidateFullPath -Algorithm SHA256).Hash
$deployScript = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot "..\scripts\deploy-live-router.ps1"))
$tempBase = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$tempRoot = [System.IO.Path]::GetFullPath((Join-Path $tempBase ("cliproxy-cutover-e2e-{0}-{1}" -f $PID, [guid]::NewGuid().ToString("N"))))
$tempLeaf = Split-Path -Leaf $tempRoot
if (-not $tempRoot.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -or -not $tempLeaf.StartsWith("cliproxy-cutover-e2e-", [System.StringComparison]::Ordinal)) {
    throw "Refusing unsafe temporary path: $tempRoot"
}

$success = $null
$rollback = $null
$successRespawner = $null
$successSentinel = $null
$successReady = $null

try {
    New-Item -ItemType Directory -Path $tempRoot -Force | Out-Null

    $success = New-Scenario -Root $tempRoot -Name "success" -Port $SuccessPort -Baseline $baselineFullPath -Candidate $candidateFullPath
    Start-ScenarioRouter -Scenario $success | Out-Null
    Wait-HttpReady -Uri $success.HealthUri

    $successSentinel = Join-Path $success.Root "respawner.enabled"
    $successReady = Join-Path $success.Root "respawner.ready"
    $respawnerStdout = Join-Path $success.Root "respawner-router.stdout.log"
    $respawnerStderr = Join-Path $success.Root "respawner-router.stderr.log"
    [System.IO.File]::WriteAllText($successSentinel, "enabled", [System.Text.UTF8Encoding]::new($false))
    $successRespawner = Start-Job -ArgumentList $successSentinel, $successReady, $success.LivePath, $success.ConfigPath, $success.Root, $respawnerStdout, $respawnerStderr, $success.HealthUri, $success.Port -ScriptBlock {
        param($Sentinel, $ReadyPath, $LivePath, $ConfigPath, $WorkingRoot, $StdoutPath, $StderrPath, $HealthUri, $Port)

        $deadline = (Get-Date).AddSeconds(90)
        [System.IO.File]::WriteAllText($ReadyPath, "ready", [System.Text.UTF8Encoding]::new($false))
        while ((Test-Path -LiteralPath $Sentinel) -and (Get-Date) -lt $deadline) {
            $healthy = $false
            try {
                Invoke-WebRequest -UseBasicParsing -Method Get -Uri $HealthUri -TimeoutSec 1 | Out-Null
                $healthy = $true
            }
            catch {
            }
            if (-not $healthy) {
                $listener = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue | Select-Object -First 1
                if ($null -eq $listener -and (Test-Path -LiteralPath $LivePath -PathType Leaf)) {
                    try {
                        Start-Process -FilePath $LivePath -ArgumentList @("-config", $ConfigPath, "-local-model") -WorkingDirectory $WorkingRoot -WindowStyle Hidden -RedirectStandardOutput $StdoutPath -RedirectStandardError $StderrPath | Out-Null
                    }
                    catch {
                    }
                    Start-Sleep -Milliseconds 500
                }
            }
            Start-Sleep -Milliseconds 75
        }
    }

    $respawnerReadyDeadline = (Get-Date).AddSeconds(10)
    while (-not (Test-Path -LiteralPath $successReady -PathType Leaf) -and (Get-Date) -lt $respawnerReadyDeadline) {
        Start-Sleep -Milliseconds 100
    }
    if (-not (Test-Path -LiteralPath $successReady -PathType Leaf)) {
        throw "Timed out waiting for the staging respawner readiness handshake"
    }

    $successOutput = & $deployScript -CandidatePath $success.CandidatePath -LivePath $success.LivePath -ExpectedCandidateSha256 $candidateHash -ExpectedLiveSha256 $baselineHash -RollbackPath $success.RollbackPath -ConfigPath $success.ConfigPath -WorkingDirectory $success.Root -Port $success.Port -HealthUri $success.HealthUri -AdditionalArguments "-local-model" -HealthTimeoutSeconds 15 -ExternalRespawnerGraceMilliseconds 5000 -Execute
    $successResult = ($successOutput -join [Environment]::NewLine) | ConvertFrom-Json
    if ($successResult.status -ne "deployed") {
        throw "Success scenario did not report deployed status"
    }
    if ($successResult.launcher_race_winner -ne "external-respawner") {
        throw "Success scenario did not exercise the external respawner path"
    }
    if ((Get-FileHash -LiteralPath $success.LivePath -Algorithm SHA256).Hash -ne $candidateHash) {
        throw "Success scenario live hash does not match candidate"
    }
    if ((Get-FileHash -LiteralPath $success.RollbackPath -Algorithm SHA256).Hash -ne $baselineHash) {
        throw "Success scenario rollback hash does not match baseline"
    }
    Wait-HttpReady -Uri $success.HealthUri

    [System.IO.File]::Delete($successSentinel)
    Wait-Job -Job $successRespawner -Timeout 5 | Out-Null
    if ($successRespawner.State -notin @("Completed", "Failed", "Stopped")) {
        Stop-Job -Job $successRespawner -ErrorAction SilentlyContinue
        Wait-Job -Job $successRespawner -Timeout 5 | Out-Null
    }
    Remove-Job -Job $successRespawner -Force -ErrorAction SilentlyContinue
    $successRespawner = $null
    Stop-OwnedPortProcess -Port $success.Port -OwnedRoot $success.Root

    $invalidCandidate = Join-Path $tempRoot "invalid-candidate.exe"
    $wherePath = Join-Path $env:SystemRoot "System32\where.exe"
    [System.IO.File]::Copy($wherePath, $invalidCandidate, $false)
    $invalidCandidateHash = (Get-FileHash -LiteralPath $invalidCandidate -Algorithm SHA256).Hash

    $rollback = New-Scenario -Root $tempRoot -Name "rollback" -Port $RollbackPort -Baseline $baselineFullPath -Candidate $invalidCandidate
    Start-ScenarioRouter -Scenario $rollback | Out-Null
    Wait-HttpReady -Uri $rollback.HealthUri

    $rollbackFailureObserved = $false
    try {
        & $deployScript -CandidatePath $rollback.CandidatePath -LivePath $rollback.LivePath -ExpectedCandidateSha256 $invalidCandidateHash -ExpectedLiveSha256 $baselineHash -RollbackPath $rollback.RollbackPath -ConfigPath $rollback.ConfigPath -WorkingDirectory $rollback.Root -Port $rollback.Port -HealthUri $rollback.HealthUri -AdditionalArguments "-local-model" -HealthTimeoutSeconds 3 -Execute | Out-Null
    }
    catch {
        if ($_.Exception.Message -notlike "*automatic rollback succeeded*") {
            throw
        }
        $rollbackFailureObserved = $true
    }
    if (-not $rollbackFailureObserved) {
        throw "Rollback scenario unexpectedly succeeded"
    }
    if ((Get-FileHash -LiteralPath $rollback.LivePath -Algorithm SHA256).Hash -ne $baselineHash) {
        throw "Rollback scenario did not restore the baseline hash"
    }
    if ((Get-FileHash -LiteralPath $rollback.RollbackPath -Algorithm SHA256).Hash -ne $baselineHash) {
        throw "Rollback scenario backup hash does not match baseline"
    }
    Wait-HttpReady -Uri $rollback.HealthUri
    Stop-OwnedPortProcess -Port $rollback.Port -OwnedRoot $rollback.Root

    [pscustomobject]@{
        status = "passed"
        baseline_sha256 = $baselineHash
        candidate_sha256 = $candidateHash
        success_port = $SuccessPort
        success_launcher_race_winner = $successResult.launcher_race_winner
        rollback_port = $RollbackPort
        rollback_restored = $true
    } | ConvertTo-Json -Depth 4
}
finally {
    if ($null -ne $successSentinel -and (Test-Path -LiteralPath $successSentinel -PathType Leaf)) {
        [System.IO.File]::Delete($successSentinel)
    }
    if ($null -ne $successRespawner) {
        Wait-Job -Job $successRespawner -Timeout 2 | Out-Null
        if ($successRespawner.State -notin @("Completed", "Failed", "Stopped")) {
            Stop-Job -Job $successRespawner -ErrorAction SilentlyContinue
            Wait-Job -Job $successRespawner -Timeout 5 | Out-Null
        }
        Remove-Job -Job $successRespawner -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $success) {
        Stop-OwnedPortProcess -Port $success.Port -OwnedRoot $success.Root
    }
    if ($null -ne $rollback) {
        Stop-OwnedPortProcess -Port $rollback.Port -OwnedRoot $rollback.Root
    }
    if (Test-Path -LiteralPath $tempRoot -PathType Container) {
        $resolvedCleanupTarget = [System.IO.Path]::GetFullPath($tempRoot)
        $cleanupLeaf = Split-Path -Leaf $resolvedCleanupTarget
        if ($resolvedCleanupTarget.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -and $cleanupLeaf.StartsWith("cliproxy-cutover-e2e-", [System.StringComparison]::Ordinal)) {
            Remove-Item -LiteralPath $resolvedCleanupTarget -Recurse -Force
        }
        else {
            Write-Warning "Skipped unsafe cleanup target: $resolvedCleanupTarget"
        }
    }
}
