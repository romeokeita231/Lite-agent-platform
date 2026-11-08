param(
    [string]$BaseUrl = 'http://localhost:8080',
    [string]$ApiKeyA = '',
    [string]$ApiKeyB = ''
)

$ErrorActionPreference = 'Stop'
$BaseUrl = $BaseUrl.TrimEnd('/')

if (-not $ApiKeyA) {
    $ApiKeyA = if ($env:DEMO_API_KEY_A) { $env:DEMO_API_KEY_A } else { 'demo-a-api-key' }
}
if (-not $ApiKeyB) {
    $ApiKeyB = if ($env:DEMO_API_KEY_B) { $env:DEMO_API_KEY_B } else { 'demo-b-api-key' }
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
    $agentA = Get-AgentId $BaseUrl $ApiKeyA 'demo'
    $agentB = Get-AgentId $BaseUrl $ApiKeyB 'demo'
    $runs = @()

    for ($i = 1; $i -le 6; $i++) {
        $run = New-DemoRun $BaseUrl $ApiKeyA $agentA "tenant-a-run-$i"
        $runs += @{ Tenant = 'A'; Key = $ApiKeyA; Id = $run.id; Started = [DateTime]::UtcNow; Finished = $null; Status = $run.status }
    }

    Start-Sleep -Milliseconds 300
    $runB = New-DemoRun $BaseUrl $ApiKeyB $agentB 'tenant-b-run-1'
    $runs += @{ Tenant = 'B'; Key = $ApiKeyB; Id = $runB.id; Started = [DateTime]::UtcNow; Finished = $null; Status = $runB.status }

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
                $item.Finished = [DateTime]::UtcNow
            }
        }
        Start-Sleep -Milliseconds 200
    }

    Write-Host ''
    Write-Host ('{0,-8} {1,-6} {2,-11} {3,10}' -f 'tenant', 'run', 'status', 'elapsed_ms')
    for ($i = 0; $i -lt $runs.Count; $i++) {
        $item = $runs[$i]
        $elapsed = [math]::Round((($item.Finished - $item.Started).TotalMilliseconds))
        Write-Host ('{0,-8} {1,-6} {2,-11} {3,10}' -f $item.Tenant, "run-$i", $item.Status, $elapsed)
    }

    $broken = @($runs | Where-Object { $_.Status -ne 'completed' })
    if ($broken.Count -gt 0) {
        throw "$($broken.Count) run(s) did not complete"
    }

    $aTimes = @($runs | Where-Object { $_.Tenant -eq 'A' } | ForEach-Object { ($_.Finished - $_.Started).TotalMilliseconds })
    $aMax = [int]($aTimes | Measure-Object -Maximum).Maximum
    $bTime = [int](@($runs | Where-Object { $_.Tenant -eq 'B' } | ForEach-Object { ($_.Finished - $_.Started).TotalMilliseconds })[0])

    Write-Host ''
    Write-Host "tenant A slowest run: $aMax ms"
    Write-Host "tenant B only run:    $bTime ms"
    if ($bTime -ge $aMax) {
        throw "tenant B ($bTime ms) was queued behind tenant A's backlog (slowest A run $aMax ms)"
    }
    Write-Host 'OK: tenant B was not starved by tenant A saturating its concurrency limit.'
    exit 0
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
