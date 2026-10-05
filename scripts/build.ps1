param([string]$GoPath = 'go', [switch]$Cross)
$ErrorActionPreference = 'Stop'
Push-Location (Split-Path -Parent $PSScriptRoot)
$previousCGO=$env:CGO_ENABLED; $previousOS=$env:GOOS; $previousArch=$env:GOARCH; $previousARM=$env:GOARM; $previousMIPS=$env:GOMIPS
try {
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    $env:CGO_ENABLED='0'
    if ($Cross) {
        $env:GOOS='linux'
        $env:GOARM='7'; $env:GOMIPS='hardfloat'
        foreach ($targetArch in @('amd64','arm64','arm','mips','mipsle')) {
            $env:GOARCH=$targetArch
            & $GoPath build -o "bin/infrapulse-agent-linux-$targetArch" './cmd/infrapulse-agent'
            if ($LASTEXITCODE -ne 0) { throw "build failed: linux/$targetArch" }
        }
        foreach ($targetArch in @('amd64','arm64')) {
            $env:GOARCH=$targetArch
            foreach ($binary in @('infrapulse-mock-controller','infrapulse-simulator','infrapulse-mock-enrollment')) {
                & $GoPath build -o "bin/$binary-linux-$targetArch" "./cmd/$binary"
                if ($LASTEXITCODE -ne 0) { throw "build failed: $binary linux/$targetArch" }
            }
        }
    } else {
        $nativeOS = & $GoPath env GOHOSTOS
        $suffix = if ($nativeOS -eq 'windows') { '.exe' } else { '' }
        $env:GOOS=$nativeOS; $env:GOARCH=(& $GoPath env GOHOSTARCH)
        foreach ($binary in @('infrapulse-agent','infrapulse-mock-controller','infrapulse-simulator','infrapulse-mock-enrollment')) {
            & $GoPath build -o "bin/$binary$suffix" "./cmd/$binary"
            if ($LASTEXITCODE -ne 0) { throw "build failed: $binary" }
        }
    }
} finally { $env:CGO_ENABLED=$previousCGO; $env:GOOS=$previousOS; $env:GOARCH=$previousArch; $env:GOARM=$previousARM; $env:GOMIPS=$previousMIPS; Pop-Location }
