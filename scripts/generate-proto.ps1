param(
    [switch]$Bootstrap,
    [string]$Go = 'go'
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$originalGoBin = $env:GOBIN
Push-Location $projectRoot
try {
    New-Item -ItemType Directory -Force -Path .tools/bin,.tools/protoc,api/gen | Out-Null
    $env:GOBIN = Join-Path $projectRoot '.tools/bin'

    if ($Bootstrap) {
        $archive = Join-Path $projectRoot '.tools/protoc-33.5-win64.zip'
        if (-not (Test-Path -LiteralPath $archive)) {
            Invoke-WebRequest -Uri 'https://github.com/protocolbuffers/protobuf/releases/download/v33.5/protoc-33.5-win64.zip' -OutFile $archive -TimeoutSec 90
        }
        $expectedHash = '7E3468CD1FBD1AE9361A5304D4AC28FBD593AA1A425B5464BD9D4DA5FCA224B4'
        if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $expectedHash) {
            throw 'Official protoc 33.5 Windows archive checksum does not match the pinned artifact.'
        }
        Expand-Archive -LiteralPath $archive -DestinationPath .tools/protoc -Force
        & $Go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
        if ($LASTEXITCODE -ne 0) { throw 'Install protoc-gen-go failed.' }
        & $Go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0
        if ($LASTEXITCODE -ne 0) { throw 'Install protoc-gen-go-grpc failed.' }
    }

    if ((& .tools/protoc/bin/protoc.exe --version) -ne 'libprotoc 33.5') { throw 'Use protoc 33.5; run this script with -Bootstrap.' }
    if ((& .tools/bin/protoc-gen-go.exe --version) -ne 'protoc-gen-go.exe v1.36.12') { throw 'Use protoc-gen-go v1.36.12; run with -Bootstrap.' }
    if ((& .tools/bin/protoc-gen-go-grpc.exe --version) -ne 'protoc-gen-go-grpc 1.6.0') { throw 'Use protoc-gen-go-grpc v1.6.0; run with -Bootstrap.' }

    & .tools/protoc/bin/protoc.exe `
        --proto_path=api/proto --proto_path=.tools/protoc/include `
        --plugin=protoc-gen-go=.tools/bin/protoc-gen-go.exe `
        --plugin=protoc-gen-go-grpc=.tools/bin/protoc-gen-go-grpc.exe `
        --go_out=api/gen --go_opt=paths=source_relative `
        --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative `
        api/proto/infrapulse/v1/agent.proto
    if ($LASTEXITCODE -ne 0) { throw 'Protobuf generation failed.' }
} finally {
    if ($null -eq $originalGoBin) {
        Remove-Item Env:GOBIN -ErrorAction SilentlyContinue
    } else {
        $env:GOBIN = $originalGoBin
    }
    Pop-Location
}
