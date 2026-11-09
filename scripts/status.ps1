param(
    [string]$BaseUrl = 'http://localhost:8080',
    [string]$PolicyUrl = 'http://localhost:9090'
)

$ErrorActionPreference = 'Stop'

function Get-Endpoint([string]$Url) {
    try {
        $response = Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 5
        return "$($response.StatusCode) $($response.Content.Trim())"
    }
    catch {
        return "unreachable ($($_.Exception.Message))"
    }
}

function Get-Sql([string]$Query) {
    $dockerArgs = @('compose', 'exec', '-T', 'db', 'psql', '-U', 'postgres', '-d', 'platform', '-At', '-F', '  ', '-c', $Query)
    $out = & docker @dockerArgs 2>&1
    if ($LASTEXITCODE -ne 0) { throw ($out | Out-String) }
    return @($out | Where-Object { $_ -ne '' })
}

Write-Output '== containers'
& docker compose ps --format '{{.Name}}  {{.Status}}'

Write-Output ''
Write-Output '== endpoints'
Write-Output ("platform /healthz  {0}" -f (Get-Endpoint "$BaseUrl/healthz"))
Write-Output ("platform /readyz   {0}" -f (Get-Endpoint "$BaseUrl/readyz"))
Write-Output ("policy   /healthz  {0}" -f (Get-Endpoint "$PolicyUrl/healthz"))

try {
    Write-Output ''
    Write-Output '== runs by status'
    $byStatus = Get-Sql @'
select status || '  ' || count(*) from runs group by status order by status
'@
    if ($byStatus.Count -eq 0) { Write-Output '(no runs yet)' } else { $byStatus | ForEach-Object { Write-Output $_ } }

    Write-Output ''
    Write-Output '== in flight (queued / running / waiting_human)'
    $inflight = Get-Sql @'
select r.status || '  ' || coalesce(a.name,'?') || '  ' || r.id || '  ' || to_char(r.created_at,'HH24:MI:SS')
from runs r left join agents a on a.id = r.agent_id
where r.status in ('queued','running','waiting_human') order by r.created_at
'@
    if ($inflight.Count -eq 0) { Write-Output '(none)' } else { $inflight | ForEach-Object { Write-Output $_ } }

    Write-Output ''
    Write-Output '== last 5 runs'
    $recent = Get-Sql @'
select to_char(r.created_at,'HH24:MI:SS') || '  ' || coalesce(a.name,'?') || '  ' || r.status || '  ' || coalesce(r.terminal_reason,'-') || '  ' || r.id
from runs r left join agents a on a.id = r.agent_id
order by r.created_at desc limit 5
'@
    if ($recent.Count -eq 0) { Write-Output '(none)' } else { $recent | ForEach-Object { Write-Output $_ } }

    Write-Output ''
    Write-Output '== ledger'
    Get-Sql @'
select 'checkpoints  ' || (select count(*) from checkpoints) || '  spans  ' || (select count(*) from spans)
'@ | ForEach-Object { Write-Output $_ }
}
catch {
    Write-Output ''
    Write-Output "== database"
    Write-Output "cannot query the ledger: $($_.Exception.Message.Trim())"
    exit 1
}
