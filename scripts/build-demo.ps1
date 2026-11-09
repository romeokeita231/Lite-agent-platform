param(
    [string]$Version = '0.1.0'
)

$ErrorActionPreference = 'Stop'

$PostgresVersion = '16.4'
$PostgresUrl = 'https://get.enterprisedb.com/postgresql/postgresql-16.4-1-windows-x64-binaries.zip'
$PostgresSize = 338727828
$PostgresSha256 = '3508D8F085BC3980F38211A82E3F31E5FCAE9952105D3DC2F8BE67B64A822BAA'
$FixedTimestamp = [datetime]::new(2025, 1, 1, 0, 0, 0, [DateTimeKind]::Utc)

$RepoRoot = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RepoRoot 'dist'
$CacheDir = Join-Path $DistDir 'cache'
$ArchiveName = 'postgresql-16.4-windows-x64-binaries.zip'
$ArchivePath = Join-Path $CacheDir $ArchiveName
$StagingDir = Join-Path $DistDir "staging\lite-agent-platform-demo-$Version"
$ZipPath = Join-Path $DistDir "lite-agent-platform-demo-$Version.zip"

function Assert-ArchiveChecksum {
    if (-not (Test-Path -LiteralPath $ArchivePath)) {
        throw "Postgres $PostgresVersion archive is missing at $ArchivePath"
    }
    $size = (Get-Item -LiteralPath $ArchivePath).Length
    if ($size -ne $PostgresSize) {
        throw "Postgres $PostgresVersion archive has $size bytes, expected $PostgresSize; delete $ArchivePath and rebuild"
    }
    $hash = (Get-FileHash -LiteralPath $ArchivePath -Algorithm SHA256).Hash
    if ($hash -ne $PostgresSha256) {
        throw "Postgres $PostgresVersion archive checksum is $hash, expected $PostgresSha256; delete $ArchivePath and rebuild"
    }
}

if (-not (Test-Path -LiteralPath $ArchivePath)) {
    New-Item -ItemType Directory -Force -Path $CacheDir | Out-Null
    Write-Output "downloading Postgres $PostgresVersion from $PostgresUrl"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $PostgresUrl -OutFile $ArchivePath
    }
    catch {
        if (Test-Path -LiteralPath $ArchivePath) {
            Remove-Item -LiteralPath $ArchivePath -Force
        }
        throw "download of Postgres $PostgresVersion failed: $($_.Exception.Message)"
    }
}
Assert-ArchiveChecksum

foreach ($command in 'go') {
    if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
        throw "$command was not found on PATH; building the demo zip needs the Go toolchain"
    }
}

if (Test-Path -LiteralPath $StagingDir) {
    Remove-Item -LiteralPath $StagingDir -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $StagingDir | Out-Null

Push-Location $RepoRoot
try {
    & go build -trimpath -o (Join-Path $StagingDir 'platform.exe') ./cmd/platform
    if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/platform failed' }
    & go build -trimpath -o (Join-Path $StagingDir 'policy-example.exe') ./cmd/policy-example
    if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/policy-example failed' }
}
finally {
    Pop-Location
}

function New-PostgresSubsetArchive {
    param([string]$Destination)
    $root = Join-Path $CacheDir 'postgres-subset'
    if (Test-Path -LiteralPath $root) {
        try { [System.IO.Directory]::Delete($root, $true) } catch { Write-Warning "could not clear $root; reusing whatever is there" }
    }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = [System.IO.Compression.ZipFile]::OpenRead($ArchivePath)
    try {
        foreach ($entry in $archive.Entries) {
            $name = $entry.FullName
            if ($name.EndsWith('/')) { continue }
            $keep = $name.StartsWith('pgsql/bin/') -or
                $name.StartsWith('pgsql/lib/') -or
                ($name.StartsWith('pgsql/share/') -and -not $name.StartsWith('pgsql/share/locale/'))
            if (-not $keep) { continue }
            $target = Join-Path $root ($name -replace '/', '\')
            $parent = Split-Path -Parent $target
            if (-not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }
            [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $target, $true)
            [System.IO.File]::SetLastWriteTimeUtc($target, $FixedTimestamp)
        }
    }
    finally {
        $archive.Dispose()
    }
    if (Test-Path -LiteralPath $Destination) { Remove-Item -LiteralPath $Destination -Force }
    $zip = [System.IO.Compression.ZipFile]::Open($Destination, [System.IO.Compression.ZipArchiveMode]::Create)
    try {
        Get-ChildItem -LiteralPath $root -Recurse -File | ForEach-Object {
            $relative = $_.FullName.Substring($root.Length + 1) -replace '\\', '/'
            [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile(
                $zip, $_.FullName, $relative,
                [System.IO.Compression.CompressionLevel]::Optimal) | Out-Null
        }
    }
    finally {
        $zip.Dispose()
    }
    try { [System.IO.Directory]::Delete($root, $true) } catch { Write-Warning "left the extracted Postgres subset in ${root}: $($_.Exception.Message)" }

    $inner = [System.IO.Compression.ZipFile]::OpenRead($Destination)
    try {
        $innerNames = @($inner.Entries | ForEach-Object { $_.FullName })
    }
    finally {
        $inner.Dispose()
    }
    foreach ($required in 'pgsql/bin/initdb.exe', 'pgsql/bin/postgres.exe', 'pgsql/share/postgres.bki', 'pgsql/share/postgresql.conf.sample', 'pgsql/share/timezone/UTC') {
        if ($innerNames -notcontains $required) {
            throw "trimmed postgres.zip is missing $required"
        }
    }
    if ($innerNames -match '^pgsql/(pgAdmin 4|StackBuilder|symbols|include|doc)/') {
        throw 'trimmed postgres.zip still contains a dropped component'
    }
    Write-Output "trimmed postgres.zip: $((Get-Item -LiteralPath $Destination).Length) bytes, $($innerNames.Count) entries"
}

Copy-Item -LiteralPath (Join-Path $RepoRoot 'docs\demo-quickstart.md') -Destination (Join-Path $StagingDir 'QUICKSTART.md')
Copy-Item -LiteralPath (Join-Path $RepoRoot 'THIRD-PARTY-NOTICES.txt') -Destination (Join-Path $StagingDir 'THIRD-PARTY-NOTICES.txt')
New-PostgresSubsetArchive -Destination (Join-Path $StagingDir 'postgres.zip')

Get-ChildItem -LiteralPath $StagingDir -Recurse | ForEach-Object {
    $_.LastWriteTimeUtc = $FixedTimestamp
}

if (Test-Path -LiteralPath $ZipPath) {
    Remove-Item -LiteralPath $ZipPath -Force
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
[System.IO.Compression.ZipFile]::CreateFromDirectory(
    $StagingDir, $ZipPath,
    [System.IO.Compression.CompressionLevel]::NoCompression,
    $false)

$expected = @('platform.exe', 'policy-example.exe', 'postgres.zip', 'QUICKSTART.md', 'THIRD-PARTY-NOTICES.txt')
$zip = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
try {
    $names = @($zip.Entries | ForEach-Object { $_.FullName })
}
finally {
    $zip.Dispose()
}
foreach ($entry in $expected) {
    if ($names -notcontains $entry) {
        throw "packaged zip is missing $entry"
    }
}

$size = (Get-Item -LiteralPath $ZipPath).Length
$hash = (Get-FileHash -LiteralPath $ZipPath -Algorithm SHA256).Hash
Write-Output "created $ZipPath ($size bytes, sha256 $hash)"
Write-Output "contents: $($names -join ', ')"
