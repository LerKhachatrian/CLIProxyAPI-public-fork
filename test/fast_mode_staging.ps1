[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string]$CandidatePath,

    [ValidateRange(1, 65535)]
    [int]$ProxyPort = 48318,

    [ValidateRange(1, 65535)]
    [int]$CapturePort = 48319
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
        [hashtable]$Headers = @{},
        [int]$Attempts = 80
    )

    for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
        try {
            Invoke-RestMethod -Method Get -Uri $Uri -Headers $Headers -TimeoutSec 1 | Out-Null
            return
        }
        catch {
            Start-Sleep -Milliseconds 125
        }
    }
    throw "Timed out waiting for $Uri"
}

function Get-RequiredModel {
    param(
        [object[]]$Models,
        [string]$ID
    )

    $matches = @($Models | Where-Object { $_.slug -eq $ID })
    if ($matches.Count -ne 1) {
        throw "Expected one model entry for $ID; found $($matches.Count)"
    }
    return $matches[0]
}

function Assert-FastMetadata {
    param(
        [object]$Model,
        [string]$ID
    )

    $serviceTiers = @($Model.service_tiers)
    if ($serviceTiers.Count -ne 1 -or $serviceTiers[0].id -ne "priority" -or $serviceTiers[0].name -ne "Fast") {
        throw "$ID does not advertise the priority/Fast service tier"
    }
    $speedTiers = @($Model.additional_speed_tiers)
    if ($speedTiers.Count -ne 1 -or $speedTiers[0] -ne "fast") {
        throw "$ID does not advertise additional_speed_tiers=[fast]"
    }
}

if ($ProxyPort -eq $CapturePort) {
    throw "ProxyPort and CapturePort must differ"
}
foreach ($port in @($ProxyPort, $CapturePort)) {
    if (-not (Test-LoopbackPortAvailable -Port $port)) {
        throw "Loopback port $port is already in use"
    }
}

$candidateFullPath = [System.IO.Path]::GetFullPath($CandidatePath)
$tempBase = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$tempRoot = Join-Path $tempBase ("cliproxy-fast-e2e-{0}-{1}" -f $PID, [guid]::NewGuid().ToString("N"))
$tempRoot = [System.IO.Path]::GetFullPath($tempRoot)
$tempLeaf = Split-Path -Leaf $tempRoot
if (-not $tempRoot.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -or -not $tempLeaf.StartsWith("cliproxy-fast-e2e-", [System.StringComparison]::Ordinal)) {
    throw "Refusing unsafe temporary path: $tempRoot"
}

$authDir = Join-Path $tempRoot "auths"
$configPath = Join-Path $tempRoot "config.yaml"
$capturePath = Join-Path $tempRoot "captured-requests.jsonl"
$stdoutPath = Join-Path $tempRoot "proxy.stdout.log"
$stderrPath = Join-Path $tempRoot "proxy.stderr.log"
$clientKey = "staging-client-$([guid]::NewGuid().ToString('N'))"
$upstreamKey = "staging-upstream-$([guid]::NewGuid().ToString('N'))"
$proxyProcess = $null
$captureJob = $null
$failure = $null
$result = $null

try {
    New-Item -ItemType Directory -Path $authDir -Force | Out-Null
    $yamlAuthDir = $authDir.Replace("\", "/")
    $config = @"
host: "127.0.0.1"
port: $ProxyPort
auth-dir: "$yamlAuthDir"
api-keys:
  - "$clientKey"
debug: false
commercial-mode: true
logging-to-file: false
request-retry: 0
codex-api-key:
  - api-key: "$upstreamKey"
    base-url: "http://127.0.0.1:$CapturePort"
    request-retry: 0
    models:
      - name: "gpt-6-astra"
        alias: "gpt-6-astra"
      - name: "gpt-5.6-sol"
        alias: "gpt-5.6-sol"
      - name: "gpt-5.6-sol-ultrafast"
        alias: "gpt-5.6-sol-ultrafast"
      - name: "gpt-5.6-terra"
        alias: "gpt-5.6-terra"
      - name: "gpt-5.6-luna"
        alias: "gpt-5.6-luna"
      - name: "custom-fast-disabled"
        alias: "custom-fast-disabled"
"@
    [System.IO.File]::WriteAllText($configPath, $config, [System.Text.UTF8Encoding]::new($false))

    $captureJob = Start-Job -ArgumentList $CapturePort, $capturePath -ScriptBlock {
        param([int]$Port, [string]$CapturePath)

        $listener = [System.Net.HttpListener]::new()
        $listener.Prefixes.Add("http://127.0.0.1:$Port/")
        $listener.Start()
        try {
            while ($listener.IsListening) {
                $context = $listener.GetContext()
                try {
                    if ($context.Request.Url.AbsolutePath -eq "/ready") {
                        $readyBytes = [System.Text.Encoding]::UTF8.GetBytes('{"ok":true}')
                        $context.Response.StatusCode = 200
                        $context.Response.ContentType = "application/json"
                        $context.Response.ContentLength64 = $readyBytes.Length
                        $context.Response.OutputStream.Write($readyBytes, 0, $readyBytes.Length)
                        continue
                    }
                    if ($context.Request.Url.AbsolutePath -eq "/shutdown") {
                        $shutdownBytes = [System.Text.Encoding]::UTF8.GetBytes('{"ok":true}')
                        $context.Response.StatusCode = 200
                        $context.Response.ContentType = "application/json"
                        $context.Response.ContentLength64 = $shutdownBytes.Length
                        $context.Response.OutputStream.Write($shutdownBytes, 0, $shutdownBytes.Length)
                        break
                    }

                    $reader = [System.IO.StreamReader]::new($context.Request.InputStream, $context.Request.ContentEncoding)
                    try {
                        $requestBody = $reader.ReadToEnd()
                    }
                    finally {
                        $reader.Dispose()
                    }
                    [System.IO.File]::AppendAllText($CapturePath, $requestBody + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))

                    $responseBody = 'data: {"type":"response.completed","response":{"id":"resp_staging","object":"response","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}' + "`n`n"
                    $responseBytes = [System.Text.Encoding]::UTF8.GetBytes($responseBody)
                    $context.Response.StatusCode = 200
                    $context.Response.ContentType = "text/event-stream"
                    $context.Response.ContentLength64 = $responseBytes.Length
                    $context.Response.OutputStream.Write($responseBytes, 0, $responseBytes.Length)
                }
                finally {
                    $context.Response.OutputStream.Close()
                }
            }
        }
        finally {
            $listener.Close()
        }
    }

    Wait-HttpReady -Uri "http://127.0.0.1:$CapturePort/ready"

    $proxyProcess = Start-Process -FilePath $candidateFullPath -ArgumentList @("-config", $configPath, "-local-model") -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath
    Wait-HttpReady -Uri "http://127.0.0.1:$ProxyPort/healthz"

    $headers = @{
        Authorization = "Bearer $clientKey"
        "User-Agent" = "codex_cli_rs/staging-e2e"
    }
    $catalog = Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:$ProxyPort/v1/models?client_version=0.144.0" -Headers $headers -TimeoutSec 10
    $models = @($catalog.models)
    $fastModelIDs = @("gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-sol-ultrafast", "gpt-5.6-terra", "gpt-5.6-luna")
    foreach ($modelID in $fastModelIDs) {
        Assert-FastMetadata -Model (Get-RequiredModel -Models $models -ID $modelID) -ID $modelID
    }

    $astra = Get-RequiredModel -Models $models -ID "gpt-6-astra"
    $astraEfforts = @($astra.supported_reasoning_levels | ForEach-Object { $_.effort })
    if ($astra.visibility -ne "list" -or ($astraEfforts -join ",") -ne "low,medium,high,xhigh,max,ultra") {
        throw "gpt-6-astra visibility or reasoning metadata is incomplete"
    }

    $unsupported = Get-RequiredModel -Models $models -ID "custom-fast-disabled"
    if (@($unsupported.service_tiers).Count -ne 0 -or @($unsupported.additional_speed_tiers).Count -ne 0) {
        throw "Unsupported custom model unexpectedly advertises Fast"
    }

    $requests = @(
        @{ Label = "fast"; Tier = "fast" },
        @{ Label = "priority"; Tier = "priority" },
        @{ Label = "standard"; Tier = "default" }
    )
    foreach ($request in $requests) {
        $requestBody = @{
            model = "gpt-5.6-sol"
            service_tier = $request.Tier
            input = "staging-$($request.Label)"
        } | ConvertTo-Json -Compress
        Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$ProxyPort/v1/responses" -Headers $headers -ContentType "application/json" -Body $requestBody -TimeoutSec 15 | Out-Null
    }

    for ($attempt = 1; $attempt -le 40; $attempt++) {
        if ((Test-Path -LiteralPath $capturePath) -and @(Get-Content -LiteralPath $capturePath).Count -ge 3) {
            break
        }
        Start-Sleep -Milliseconds 125
    }
    $capturedLines = @(Get-Content -LiteralPath $capturePath)
    if ($capturedLines.Count -ne 3) {
        throw "Expected three captured upstream requests; found $($capturedLines.Count)"
    }
    $captured = @($capturedLines | ForEach-Object { $_ | ConvertFrom-Json })
    if ($captured[0].service_tier -ne "priority") {
        throw "Fast request was not normalized to priority"
    }
    if ($captured[1].service_tier -ne "priority") {
        throw "Priority request was not preserved"
    }
    if ($captured[2].PSObject.Properties.Name -contains "service_tier") {
        throw "Standard request still contains service_tier"
    }

    $result = [pscustomobject]@{
        status = "passed"
        candidate_sha256 = (Get-FileHash -LiteralPath $candidateFullPath -Algorithm SHA256).Hash
        proxy_port = $ProxyPort
        capture_port = $CapturePort
        fast_models = $fastModelIDs
        unsupported_model = "fast-disabled"
        fast_forwarded_tier = $captured[0].service_tier
        priority_forwarded_tier = $captured[1].service_tier
        standard_forwarded_tier = "omitted"
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
    if ($null -ne $captureJob) {
        try {
            Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:$CapturePort/shutdown" -TimeoutSec 2 | Out-Null
        }
        catch {
        }
        Wait-Job -Job $captureJob -Timeout 5 | Out-Null
        if ($captureJob.State -notin @("Completed", "Failed", "Stopped")) {
            Stop-Job -Job $captureJob -ErrorAction SilentlyContinue
            Wait-Job -Job $captureJob -Timeout 5 | Out-Null
        }
        Remove-Job -Job $captureJob -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $tempRoot) {
        $resolvedCleanupTarget = [System.IO.Path]::GetFullPath($tempRoot)
        $cleanupLeaf = Split-Path -Leaf $resolvedCleanupTarget
        if ($resolvedCleanupTarget.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -and $cleanupLeaf.StartsWith("cliproxy-fast-e2e-", [System.StringComparison]::Ordinal)) {
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

$result | ConvertTo-Json -Depth 4
