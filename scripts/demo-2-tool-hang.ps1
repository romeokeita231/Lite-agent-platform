param(
    [string]$BaseUrl = 'http://localhost:8080',
    [string]$ApiKeyA = '',
    [int]$DemoRuns = 3
)

$ErrorActionPreference = 'Stop'
$BaseUrl = $BaseUrl.TrimEnd('/')

if (-not $ApiKeyA) {
    $ApiKeyA = if ($env:DEMO_API_KEY_A) { $env:DEMO_API_KEY_A } else { 'demo-a-api-key' }
}

function Get-AuthHeaders([string]$Key) {
    return @{ Authorization = "Bearer $Key" }
}

function Get-AgentId([string]$Base, [string]$Key, [string]$Name) {
    $agents = @(Invoke-RestMethod -Uri "$Base/v1/agents" -Method Get -Headers (Get-AuthHeaders $Key) | ForEach-Object { $_ })
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

function Step-Run([string]$Base, [string]$Key, [string]$RunId) {
    $run = Invoke-RestMethod -Uri "$Base/v1/runs/$RunId" -Headers (Get-AuthHeaders $Key)
    if ($run.status -eq 'waiting_human') {
        $steps = @(Invoke-RestMethod -Uri "$Base/v1/runs/$RunId/steps" -Headers (Get-AuthHeaders $Key) | ForEach-Object { $_ })
        $body = @{ step_index = $steps.Count; input = @{ approved = $true } } | ConvertTo-Json -Depth 5
        Invoke-RestMethod -Uri "$Base/v1/runs/$RunId/human-input" -Method Post `
            -Headers (Get-AuthHeaders $Key) -ContentType 'application/json' -Body $body | Out-Null
    }
    return $run
}

try {
    $demoAgent = Get-AgentId $BaseUrl $ApiKeyA 'demo'
    $slowAgent = Get-AgentId $BaseUrl $ApiKeyA 'tool-slow'
    $runs = @()

    $slow = New-DemoRun $BaseUrl $ApiKeyA $slowAgent 'hung-tool'
    $runs += @{ Name = 'tool-slow'; Key = $ApiKeyA; Id = $slow.id; Finished = $null; Status = $slow.status; Reason = $null }

    for ($i = 1; $i -le $DemoRuns; $i++) {
        $run = New-DemoRun $BaseUrl $ApiKeyA $demoAgent "healthy-$i"
        $runs += @{ Name = "demo-$i"; Key = $ApiKeyA; Id = $run.id; Finished = $null; Status = $run.status; Reason = $null }
    }

    $deadline = [DateTime]::UtcNow.AddSeconds(180)
    while (@($runs | Where-Object { -not $_.Finished }).Count -gt 0) {
        if ([DateTime]::UtcNow -gt $deadline) {
            throw 'timed out waiting for runs to reach a terminal state'
        }
        foreach ($item in $runs) {
            if ($item.Finished) { continue }
            $state = Step-Run $BaseUrl $item.Key $item.Id
            if ($state.status -in @('completed', 'failed', 'cancelled')) {
                $item.Status = $state.status
                $item.Reason = $state.terminal_reason
                $item.Finished = [DateTime]::UtcNow
            }
        }
        Start-Sleep -Milliseconds 200
    }

    Write-Host ''
    Write-Host ('{0,-11} {1,-11} {2,-6}' -f 'run', 'status', 'reason')
    foreach ($item in $runs) {
        Write-Host ('{0,-11} {1,-11} {2,-6}' -f $item.Name, $item.Status, $item.Reason)
    }

    $slow = $runs[0]
    $healthy = @($runs | Where-Object { $_.Name -ne 'tool-slow' })

    if ($slow.Status -ne 'failed') {
        throw "tool-slow run ended '$($slow.Status)', want failed after STEP_TIMEOUT retries"
    }
    if (-not $slow.Reason) {
        throw 'tool-slow run has no terminal_reason'
    }
    $broken = @($healthy | Where-Object { $_.Status -ne 'completed' })
    if ($broken.Count -gt 0) {
        throw "$($broken.Count) healthy run(s) did not complete"
    }

    Write-Host ''
    Write-Host "OK: the hung tool run failed with '$($slow.Reason)' while the other runs completed."
    exit 0
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
