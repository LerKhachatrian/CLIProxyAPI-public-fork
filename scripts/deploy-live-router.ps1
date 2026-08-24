[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$CandidatePath,

    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$LivePath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern("^[A-Fa-f0-9]{64}$")]
    [string]$ExpectedCandidateSha256,

    [Parameter(Mandatory = $true)]
    [ValidatePattern("^[A-Fa-f0-9]{64}$")]
    [string]$ExpectedLiveSha256,

    [Parameter(Mandatory = $true)]
    [string]$RollbackPath,

    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$ConfigPath,

    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Container })]
    [string]$WorkingDirectory,

    [ValidateRange(1, 65535)]
    [int]$Port = 48317,

    [string]$HealthUri,

    [ValidateRange(2, 120)]
    [int]$HealthTimeoutSeconds = 20,

    [ValidateRange(0, 5000)]
    [int]$ExternalRespawnerGraceMilliseconds = 250,

    [string[]]$AdditionalArguments = @(),

    [switch]$PreflightOnly,

    [switch]$Execute
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Resolve-ExistingPath {
    param([string]$Path)

    return [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path)
}

function Resolve-TargetPath {
    param([string]$Path)

    return [System.IO.Path]::GetFullPath($Path)
}

function Test-SamePath {
    param(
        [string]$Left,
        [string]$Right
    )

    return [string]::Equals(
        [System.IO.Path]::GetFullPath($Left),
        [System.IO.Path]::GetFullPath($Right),
        [System.StringComparison]::OrdinalIgnoreCase
    )
}

function Get-Sha256 {
    param([string]$Path)

    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256 -ErrorAction Stop).Hash.ToUpperInvariant()
}

function Assert-Hash {
    param(
        [string]$Path,
        [string]$Expected,
        [string]$Label
    )

    $actual = Get-Sha256 -Path $Path
    if ($actual -ne $Expected.ToUpperInvariant()) {
        throw "$Label SHA-256 mismatch at $Path. Expected $Expected; found $actual"
    }
    return $actual
}

function Get-SingleListener {
    param([int]$ListenerPort)

    $listeners = @(Get-NetTCPConnection -State Listen -LocalPort $ListenerPort -ErrorAction SilentlyContinue)
    if ($listeners.Count -gt 1) {
        throw "Expected at most one listener on port $ListenerPort; found $($listeners.Count)"
    }
    if ($listeners.Count -eq 0) {
        return $null
    }
    return $listeners[0]
}

function Get-ExecutablePath {
    param([int]$ProcessId)

    $processInfo = Get-CimInstance Win32_Process -Filter "ProcessId=$ProcessId" -ErrorAction Stop
    if ($null -eq $processInfo -or [string]::IsNullOrWhiteSpace($processInfo.ExecutablePath)) {
        throw "Could not resolve executable path for PID $ProcessId"
    }
    return [System.IO.Path]::GetFullPath($processInfo.ExecutablePath)
}

function Assert-ListenerOwnedByLivePath {
    param(
        [object]$Listener,
        [string]$ExpectedPath
    )

    if ($null -eq $Listener) {
        throw "No listener exists on port $Port"
    }
    $ownerPath = Get-ExecutablePath -ProcessId ([int]$Listener.OwningProcess)
    if (-not (Test-SamePath -Left $ownerPath -Right $ExpectedPath)) {
        throw "Port $Port is owned by unexpected executable $ownerPath (PID $($Listener.OwningProcess))"
    }
    return $ownerPath
}

function Test-HttpReady {
    param([string]$Uri)

    try {
        $response = Invoke-WebRequest -UseBasicParsing -Method Get -Uri $Uri -TimeoutSec 2
        return ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300)
    }
    catch {
        return $false
    }
}

function Wait-HttpReady {
    param(
        [string]$Uri,
        [int]$TimeoutSeconds
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-HttpReady -Uri $Uri) {
            return $true
        }
        Start-Sleep -Milliseconds 125
    }
    return $false
}

function Wait-OldListenerGone {
    param(
        [int]$OldProcessId,
        [int]$TimeoutSeconds = 8
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $listener = Get-SingleListener -ListenerPort $Port
        if ($null -eq $listener -or [int]$listener.OwningProcess -ne $OldProcessId) {
            return
        }
        Start-Sleep -Milliseconds 100
    }
    throw "Old PID $OldProcessId still owns port $Port after $TimeoutSeconds seconds"
}

function Start-Router {
    $arguments = @("-config", $script:ConfigFullPath) + @($AdditionalArguments)
    return Start-Process -FilePath $script:LiveFullPath -ArgumentList $arguments -WorkingDirectory $script:WorkingDirectoryFullPath -WindowStyle Hidden -PassThru
}

if ($PreflightOnly -and $Execute) {
    throw "Choose exactly one mode: -PreflightOnly or -Execute"
}
if (-not $PreflightOnly -and -not $Execute) {
    throw "Refusing to continue without an explicit mode. Use -PreflightOnly or -Execute"
}

$script:CandidateFullPath = Resolve-ExistingPath -Path $CandidatePath
$script:LiveFullPath = Resolve-ExistingPath -Path $LivePath
$script:ConfigFullPath = Resolve-ExistingPath -Path $ConfigPath
$script:WorkingDirectoryFullPath = Resolve-ExistingPath -Path $WorkingDirectory
$rollbackFullPath = Resolve-TargetPath -Path $RollbackPath
$expectedCandidateHash = $ExpectedCandidateSha256.ToUpperInvariant()
$expectedLiveHash = $ExpectedLiveSha256.ToUpperInvariant()

if ([string]::IsNullOrWhiteSpace($HealthUri)) {
    $HealthUri = "http://127.0.0.1:$Port/healthz"
}
if (-not $HealthUri.StartsWith("http://127.0.0.1:$Port/", [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "HealthUri must use loopback port $Port"
}
if (Test-SamePath -Left $script:CandidateFullPath -Right $script:LiveFullPath) {
    throw "CandidatePath and LivePath must differ"
}
if (Test-SamePath -Left $rollbackFullPath -Right $script:LiveFullPath) {
    throw "RollbackPath and LivePath must differ"
}
if (Test-SamePath -Left $rollbackFullPath -Right $script:CandidateFullPath) {
    throw "RollbackPath and CandidatePath must differ"
}

$candidateHash = Assert-Hash -Path $script:CandidateFullPath -Expected $expectedCandidateHash -Label "Candidate"
$liveHash = Assert-Hash -Path $script:LiveFullPath -Expected $expectedLiveHash -Label "Live binary"
$initialListener = Get-SingleListener -ListenerPort $Port
Assert-ListenerOwnedByLivePath -Listener $initialListener -ExpectedPath $script:LiveFullPath | Out-Null
$oldProcessId = [int]$initialListener.OwningProcess
if (-not (Test-HttpReady -Uri $HealthUri)) {
    throw "Live router is not healthy at $HealthUri"
}

$rollbackExists = Test-Path -LiteralPath $rollbackFullPath -PathType Leaf
if ($rollbackExists) {
    Assert-Hash -Path $rollbackFullPath -Expected $expectedLiveHash -Label "Rollback binary" | Out-Null
}
elseif ($PreflightOnly) {
    throw "Rollback binary is missing during preflight: $rollbackFullPath"
}

if ($PreflightOnly) {
    [pscustomobject]@{
        status = "ready"
        port = $Port
        live_pid = $oldProcessId
        live_path = $script:LiveFullPath
        live_sha256 = $liveHash
        candidate_path = $script:CandidateFullPath
        candidate_sha256 = $candidateHash
        rollback_path = $rollbackFullPath
        rollback_sha256 = $expectedLiveHash
        health_uri = $HealthUri
    } | ConvertTo-Json -Depth 4
    return
}

$rollbackDirectory = [System.IO.Path]::GetDirectoryName($rollbackFullPath)
if ([string]::IsNullOrWhiteSpace($rollbackDirectory)) {
    throw "RollbackPath must include a parent directory"
}
if (-not (Test-Path -LiteralPath $rollbackDirectory -PathType Container)) {
    New-Item -ItemType Directory -Path $rollbackDirectory -Force | Out-Null
}
if (-not $rollbackExists) {
    [System.IO.File]::Copy($script:LiveFullPath, $rollbackFullPath, $false)
    Assert-Hash -Path $rollbackFullPath -Expected $expectedLiveHash -Label "New rollback binary" | Out-Null
}

$liveDirectory = [System.IO.Path]::GetDirectoryName($script:LiveFullPath)
$liveFileName = [System.IO.Path]::GetFileNameWithoutExtension($script:LiveFullPath)
$liveExtension = [System.IO.Path]::GetExtension($script:LiveFullPath)
$cutoverId = [guid]::NewGuid().ToString("N")
$nextPath = Join-Path $liveDirectory ("{0}.next-{1}{2}" -f $liveFileName, $cutoverId, $liveExtension)
$displacedPath = Join-Path $liveDirectory ("{0}.pre-cutover-{1}{2}" -f $liveFileName, $cutoverId, $liveExtension)
$failedCandidatePath = Join-Path $liveDirectory ("{0}.failed-{1}{2}" -f $liveFileName, $cutoverId, $liveExtension)
$canonicalSwapped = $false
$oldStopped = $false
$startAttempt = $null
$rollbackSucceeded = $false

try {
    [System.IO.File]::Copy($script:CandidateFullPath, $nextPath, $false)
    Assert-Hash -Path $nextPath -Expected $expectedCandidateHash -Label "Prepared next binary" | Out-Null

    [System.IO.File]::Move($script:LiveFullPath, $displacedPath)
    try {
        [System.IO.File]::Move($nextPath, $script:LiveFullPath)
    }
    catch {
        [System.IO.File]::Move($displacedPath, $script:LiveFullPath)
        throw
    }
    $canonicalSwapped = $true
    Assert-Hash -Path $script:LiveFullPath -Expected $expectedCandidateHash -Label "Swapped live binary" | Out-Null

    Stop-Process -Id $oldProcessId -Force -ErrorAction Stop
    $oldStopped = $true
    $oldProcess = Get-Process -Id $oldProcessId -ErrorAction SilentlyContinue
    if ($null -ne $oldProcess) {
        $oldProcess.WaitForExit(5000) | Out-Null
    }
    Wait-OldListenerGone -OldProcessId $oldProcessId

    if ($ExternalRespawnerGraceMilliseconds -gt 0) {
        $respawnerDeadline = (Get-Date).AddMilliseconds($ExternalRespawnerGraceMilliseconds)
        while ((Get-Date) -lt $respawnerDeadline) {
            if ($null -ne (Get-SingleListener -ListenerPort $Port)) {
                break
            }
            Start-Sleep -Milliseconds 25
        }
    }

    $listenerAfterStop = Get-SingleListener -ListenerPort $Port
    if ($null -eq $listenerAfterStop) {
        $startAttempt = Start-Router
    }

    if (-not (Wait-HttpReady -Uri $HealthUri -TimeoutSeconds $HealthTimeoutSeconds)) {
        throw "Candidate router did not become healthy at $HealthUri"
    }

    $newListener = Get-SingleListener -ListenerPort $Port
    Assert-ListenerOwnedByLivePath -Listener $newListener -ExpectedPath $script:LiveFullPath | Out-Null
    $newProcessId = [int]$newListener.OwningProcess
    if ($newProcessId -eq $oldProcessId) {
        throw "Router PID did not change during cutover"
    }
    Assert-Hash -Path $script:LiveFullPath -Expected $expectedCandidateHash -Label "Final live binary" | Out-Null
    Assert-Hash -Path $rollbackFullPath -Expected $expectedLiveHash -Label "Final rollback binary" | Out-Null

    if (Test-Path -LiteralPath $displacedPath -PathType Leaf) {
        Assert-Hash -Path $displacedPath -Expected $expectedLiveHash -Label "Displaced old binary" | Out-Null
        [System.IO.File]::Delete($displacedPath)
    }

    [pscustomobject]@{
        status = "deployed"
        port = $Port
        old_pid = $oldProcessId
        new_pid = $newProcessId
        live_path = $script:LiveFullPath
        live_sha256 = $expectedCandidateHash
        rollback_path = $rollbackFullPath
        rollback_sha256 = $expectedLiveHash
        health_uri = $HealthUri
        launcher_race_winner = if ($null -ne $startAttempt -and $newProcessId -eq $startAttempt.Id) { "deployment-script" } else { "external-respawner" }
    } | ConvertTo-Json -Depth 4
}
catch {
    $cutoverError = $_
    try {
        if ($canonicalSwapped) {
            $currentListener = Get-SingleListener -ListenerPort $Port
            $oldStillServing = $null -ne $currentListener -and [int]$currentListener.OwningProcess -eq $oldProcessId -and (Test-HttpReady -Uri $HealthUri)

            if (-not $oldStillServing -and $null -ne $currentListener) {
                Assert-ListenerOwnedByLivePath -Listener $currentListener -ExpectedPath $script:LiveFullPath | Out-Null
                Stop-Process -Id ([int]$currentListener.OwningProcess) -Force -ErrorAction Stop
                Wait-OldListenerGone -OldProcessId ([int]$currentListener.OwningProcess)
            }

            if (Test-Path -LiteralPath $script:LiveFullPath -PathType Leaf) {
                [System.IO.File]::Move($script:LiveFullPath, $failedCandidatePath)
            }
            if (Test-Path -LiteralPath $displacedPath -PathType Leaf) {
                [System.IO.File]::Move($displacedPath, $script:LiveFullPath)
            }
            else {
                [System.IO.File]::Copy($rollbackFullPath, $script:LiveFullPath, $false)
            }
            Assert-Hash -Path $script:LiveFullPath -Expected $expectedLiveHash -Label "Restored live binary" | Out-Null

            if (-not $oldStillServing) {
                $rollbackListener = Get-SingleListener -ListenerPort $Port
                if ($null -eq $rollbackListener) {
                    Start-Router | Out-Null
                }
                if (-not (Wait-HttpReady -Uri $HealthUri -TimeoutSeconds $HealthTimeoutSeconds)) {
                    throw "Rollback router did not become healthy at $HealthUri"
                }
                $rollbackListener = Get-SingleListener -ListenerPort $Port
                Assert-ListenerOwnedByLivePath -Listener $rollbackListener -ExpectedPath $script:LiveFullPath | Out-Null
            }

            if (Test-Path -LiteralPath $failedCandidatePath -PathType Leaf) {
                Assert-Hash -Path $failedCandidatePath -Expected $expectedCandidateHash -Label "Failed candidate binary" | Out-Null
                [System.IO.File]::Delete($failedCandidatePath)
            }
        }
        $rollbackSucceeded = $true
    }
    catch {
        $rollbackError = $_
        throw "Cutover failed: $($cutoverError.Exception.Message). Automatic rollback also failed: $($rollbackError.Exception.Message)"
    }
    if ($rollbackSucceeded) {
        throw "Cutover failed and automatic rollback succeeded: $($cutoverError.Exception.Message)"
    }
    throw
}
finally {
    if (Test-Path -LiteralPath $nextPath -PathType Leaf) {
        Assert-Hash -Path $nextPath -Expected $expectedCandidateHash -Label "Unused next binary" | Out-Null
        [System.IO.File]::Delete($nextPath)
    }
}
