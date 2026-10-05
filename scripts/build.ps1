param([string]$GoPath = 'go', [switch]$Cross)
$ErrorActionPreference = 'Stop'
Push-Location (Split-Path -Parent $PSScriptRoot)
$previousCGO=$env:CGO_ENABLED; $previousOS=$env:GOOS; $previousArch=$env:GOARCH
try {
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    $env:CGO_ENABLED='0'
    if ($Cross) {
        $env:GOOS='linux'
        foreach ($targetArch in @('amd64','arm64','arm','mips','mipsle')) {
            $env:GOARCH=$targetArch
            & $GoPath build -o "bin/infrapulse-agent-linux-$targetArch" './cmd/infrapulse-agent'
            if ($LASTEXITCODE -ne 0) { throw "build failed: linux/$targetArch" }
        }
    } else {
        $nativeOS = & $GoPath env GOHOSTOS
        $suffix = if ($nativeOS -eq 'windows') { '.exe' } else { '' }
        $env:GOOS=$nativeOS; $env:GOARCH=(& $GoPath env GOHOSTARCH)
        foreach ($binary in @('infrapulse-agent','infrapulse-mock-controller','infrapulse-simulator')) {
            & $GoPath build -o "bin/$binary$suffix" "./cmd/$binary"
            if ($LASTEXITCODE -ne 0) { throw "build failed: $binary" }
        }
    }
} finally { $env:CGO_ENABLED=$previousCGO; $env:GOOS=$previousOS; $env:GOARCH=$previousArch; Pop-Location }
