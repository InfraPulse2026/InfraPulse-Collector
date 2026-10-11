param(
    [Parameter(Mandatory=$true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$')][string]$Version,
    [string]$GoPath = 'go',
    [string]$OutputDir = '.cache/release',
    [switch]$AllowDirty
)
# Packaging only: no installation, tag, commit, push or publication.
$ErrorActionPreference='Stop'
Push-Location (Split-Path -Parent $PSScriptRoot)
$saved=@{}
foreach ($name in @('GOOS','GOARCH','GOARM','GOMIPS','CGO_ENABLED')) { $saved[$name]=[Environment]::GetEnvironmentVariable($name) }
try {
    $goExecutable=(Get-Command $GoPath).Source
    $revision=(& git rev-parse HEAD).Trim()
    if ($LASTEXITCODE) { throw 'Git HEAD required' }
    $dirty=[bool](& git status --porcelain)
    if ($dirty -and -not $AllowDirty) { throw 'Working tree is dirty; use -AllowDirty only for a review candidate, never a final release.' }
    $commitLabel=$revision
    if ($dirty) { $commitLabel+='-dirty' }
    $epoch=(& git show -s --format=%ct HEAD).Trim()
    $buildTime=[DateTimeOffset]::FromUnixTimeSeconds([long]$epoch).UtcDateTime.ToString('yyyy-MM-ddTHH:mm:ssZ')
    $output=[IO.Path]::GetFullPath((Join-Path (Get-Location) $OutputDir))
    New-Item -ItemType Directory -Force -Path $output | Out-Null
    $sourceFiles=@(& git ls-files --cached --others --exclude-standard api cmd internal scripts deployments configs go.mod go.sum)
    $sourceManifest=@($sourceFiles | Sort-Object -Unique | ForEach-Object {
        [pscustomobject]@{path=$_;sha256=(Get-FileHash -LiteralPath $_ -Algorithm SHA256).Hash.ToLowerInvariant()}
    })
    $env:CGO_ENABLED='0'; $env:GOOS='linux'; $env:GOARM='7'; $env:GOMIPS='hardfloat'
    $archives=@()
    foreach ($arch in @('amd64','arm64','arm','mips','mipsle')) {
        $env:GOARCH=$arch
        $name='infrapulse-collector-'+$Version+'-linux-'+$arch
        $stage=Join-Path $output $name
        if (Test-Path -LiteralPath $stage) { throw "Output already exists: $stage; select a new output directory." }
        New-Item -ItemType Directory -Path $stage | Out-Null
        $flags='-s -w -X InfraPulseAgent/internal/version.Version='+$Version+' -X InfraPulseAgent/internal/version.Commit='+$commitLabel+' -X InfraPulseAgent/internal/version.BuildTime='+$buildTime
        & $goExecutable build -trimpath -buildvcs=false ('-ldflags='+$flags) -o (Join-Path $stage 'infrapulse-agent') ./cmd/infrapulse-agent
        if ($LASTEXITCODE) { throw "build failed: linux/$arch" }
        foreach ($directory in @('configs','deployments')) { New-Item -ItemType Directory -Path (Join-Path $stage $directory) | Out-Null }
        # Explicit templates only; never recursively copy local config or keys.
        foreach ($file in Get-ChildItem configs -File -Filter 'config.*.example.yaml') {
            Copy-Item -LiteralPath $file.FullName -Destination (Join-Path $stage 'configs')
        }
        Copy-Item -LiteralPath configs/config.example.yaml -Destination (Join-Path $stage 'configs')
        Copy-Item -LiteralPath configs/rsyslog-forwarding.example.conf -Destination (Join-Path $stage 'configs')
        foreach ($file in $sourceFiles | Where-Object { $_ -like 'deployments/*' -and ($_ -like '*.service' -or $_ -like '*.init') }) {
            $destination=Join-Path $stage $file
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $destination) | Out-Null
            Copy-Item -LiteralPath $file -Destination $destination
        }
        New-Item -ItemType Directory -Path (Join-Path $stage 'docs') | Out-Null
        foreach ($doc in Get-ChildItem docs -File -Filter '*.md') {
            Copy-Item -LiteralPath $doc.FullName -Destination (Join-Path $stage 'docs')
        }
        Copy-Item -LiteralPath README.md -Destination $stage
        [pscustomobject]@{version=$Version;commit=$revision;dirty=$dirty;go=(& $goExecutable version);target=('linux/'+$arch);goarm=$env:GOARM;gomips=$env:GOMIPS;cgo=0;build_time=$buildTime;build_time_origin='Git HEAD timestamp';sources=$sourceManifest} | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $stage 'build-manifest.json') -Encoding UTF8
        $hashes=@(Get-ChildItem -LiteralPath $stage -File -Recurse | Sort-Object FullName | ForEach-Object {
            (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()+'  '+$_.FullName.Substring($stage.Length+1).Replace([char]92,[char]47)
        })
        [IO.File]::WriteAllText((Join-Path $stage 'SHA256SUMS'),($hashes -join "`n")+"`n",[Text.UTF8Encoding]::new($false))
        $archive=Join-Path $output ($name+'.tar.gz')
        & tar -czf $archive -C $output $name
        if ($LASTEXITCODE) { throw 'tar failed' }
        $archives += (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()+'  '+(Split-Path -Leaf $archive)
        $name+': '+(Get-Item (Join-Path $stage 'infrapulse-agent')).Length+' bytes'
    }
    [IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'),($archives -join "`n")+"`n",[Text.UTF8Encoding]::new($false))
} finally {
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name,$saved[$name]) }
    Pop-Location
}
