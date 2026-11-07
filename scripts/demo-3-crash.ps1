param(
    [string]$BaseUrl = 'http://localhost:8080',
    [string]$PolicyHealthUrl = 'http://localhost:9090/healthz',
    [string]$ApiKeyA = ''
)

$ErrorActionPreference = 'Stop'
$BaseUrl = $BaseUrl.TrimEnd('/')
$RepoRoot = Split-Path -Parent $PSScriptRoot

if (-not $ApiKeyA) {
    $ApiKeyA = if ($env:DEMO_API_KEY_A) { $env:DEMO_API_KEY_A } else { 'demo-a-api-key' }
}

function Get-AuthHeaders([string]$Key) {
    return @{ Authorization = "Bearer $Key" }
}

function Invoke-Compose {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    Push-Location $RepoRoot
    try {
        & docker compose @Arguments
        if ($LASTEXITCODE -ne 0) {
            throw "docker compose $($Arguments -join ' ') exited with $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
}

function Wait-Http([string]$Url, [int]$TimeoutSeconds = 60) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 3 | Out-Null
            return
        }
        catch {
            Start-Sleep -Milliseconds 500
        }
    }
    throw "timed out waiting for $Url"
}

function Get-AgentId([string]$Base, [string]$Key, [string]$Name) {
    $agents = @(Invoke-RestMethod -Uri "$Base/v1/agents" -Method Get -Headers (Get-AuthHeaders $Key))
    $agent = $agents | Where-Object { $_.name -eq $Name } | Select-Object -First 1
    if (-not $agent) {
        throw "agent '$Name' not found; start the stack so 'platform seed' runs first"
    }
    return $agent.id
}

function New-DemoRun([string]$Base, [string]$Key, [string]$AgentId, [string]$Question) {
    $body = @{ agent_id = $AgentId; input = @{ question = $Question } } | ConvertTo-Json -Depth 5
    return Invoke-RestMethod -Uri "$Base/v1/runs" -Method Post -Headers (Get-AuthHeaders $Key) `
        -ContentType 'application/json' -Body $body
}

function Get-Run([string]$Base, [string]$Key, [string]$RunId) {
    return Invoke-RestMethod -Uri "$Base/v1/runs/$RunId" -Headers (Get-AuthHeaders $Key)
}

function Get-Steps([string]$Base, [string]$Key, [string]$RunId) {
    return @(Invoke-RestMethod -Uri "$Base/v1/runs/$RunId/steps" -Headers (Get-AuthHeaders $Key))
}

function Show-Steps([string]$Base, [string]$Key, [string]$RunId) {
    $steps = Get-Steps $Base $Key $RunId
    Write-Host ('steps: {0}' -f $steps.Count)
    foreach ($step in $steps) {
        Write-Host ('  {0,2} {1}' -f $step.step_index, $step.step_type)
    }
}

try {
    $agent = Get-AgentId $BaseUrl $ApiKeyA 'demo'

    Write-Host '=== phase 1: policy-process down ==='
    Invoke-Compose kill policy-example
    $run = New-DemoRun $BaseUrl $ApiKeyA $agent 'policy-down'
    $maxAttempt = 0
    $deadline = [DateTime]::UtcNow.AddSeconds(120)
    while ([DateTime]::UtcNow -lt $deadline) {
        $state = Get-Run $BaseUrl $ApiKeyA $run.id
        if ($state.attempt -gt $maxAttempt) { $maxAttempt = $state.attempt }
        if ($state.status -in @('completed', 'failed', 'cancelled')) { $run = $state; break }
        Start-Sleep -Milliseconds 200
    }
    if ($run.status -ne 'failed') {
        throw "run ended '$($run.status)', want failed while policy-example is down"
    }
    Write-Host ('attempt={0} next_attempt_at={1} terminal_reason={2} max_attempt_seen={3}' -f `
            $run.attempt, $run.next_attempt_at, $run.terminal_reason, $maxAttempt)
    if ($run.terminal_reason -ne 'policy_unreachable') {
        throw "terminal_reason='$($run.terminal_reason)', want policy_unreachable"
    }
    if ($maxAttempt -lt 1) {
        throw 'no retry was observed before the run failed'
    }

    Write-Host ''
    Write-Host '=== phase 2: platform restart resumes from the ledger ==='
    Invoke-Compose up -d policy-example
    Wait-Http $PolicyHealthUrl

    $run = New-DemoRun $BaseUrl $ApiKeyA $agent 'platform-restart'
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    while ([DateTime]::UtcNow -lt $deadline) {
        $state = Get-Run $BaseUrl $ApiKeyA $run.id
        if ($state.status -eq 'waiting_human') {
            $run = $state
            break
        }
        if ($state.status -in @('completed', 'failed', 'cancelled')) {
            throw "run reached '$($state.status)' before the platform restart"
        }
        Start-Sleep -Milliseconds 200
    }
    if ($run.status -ne 'waiting_human') {
        throw 'run never reached waiting_human before the restart'
    }
    Write-Host 'before restart:'
    Show-Steps $BaseUrl $ApiKeyA $run.id

    Invoke-Compose kill platform
    Invoke-Compose up -d platform
    Wait-Http "$BaseUrl/readyz" 120

    $after = Get-Run $BaseUrl $ApiKeyA $run.id
    if ($after.status -ne 'waiting_human') {
        throw "run status after restart = '$($after.status)', want waiting_human"
    }
    $steps = Get-Steps $BaseUrl $ApiKeyA $run.id
    $body = @{ step_index = $steps.Count; input = @{ approved = $true } } | ConvertTo-Json -Depth 5
    Invoke-RestMethod -Uri "$BaseUrl/v1/runs/$($run.id)/human-input" -Method Post `
        -Headers (Get-AuthHeaders $ApiKeyA) -ContentType 'application/json' -Body $body | Out-Null

    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    while ([DateTime]::UtcNow -lt $deadline) {
        $state = Get-Run $BaseUrl $ApiKeyA $run.id
        if ($state.status -in @('completed', 'failed', 'cancelled')) { $run = $state; break }
        Start-Sleep -Milliseconds 200
    }
    if ($run.status -ne 'completed') {
        throw "run status after resume = '$($run.status)', want completed"
    }

    Write-Host 'after restart:'
    Show-Steps $BaseUrl $ApiKeyA $run.id
    $finalSteps = Get-Steps $BaseUrl $ApiKeyA $run.id
    if ($finalSteps.Count -ne 4 -or $finalSteps[3].step_type -ne 'done') {
        throw 'run did not resume from the ledger to a done step'
    }

    Write-Host ''
    Write-Host 'OK: policy failure was retried and reported; the run survived a platform restart and completed from its checkpoints.'
    exit 0
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
