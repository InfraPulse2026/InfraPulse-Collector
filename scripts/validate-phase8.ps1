param(
    [ValidateSet('Mandatory','Extras','Linux','All')][string]$Mode = 'All',
    [string]$GoPath = 'go',
    [string]$OutputDir = '.cache/phase8-final',
    [string]$WSLDistro = 'docker-desktop',
    [string]$LinuxWorkspace = '/mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent'
)
$ErrorActionPreference = 'Stop'
Push-Location (Split-Path -Parent $PSScriptRoot)
$saved = @{}
foreach ($name in @('GOOS','GOARCH','CGO_ENABLED','CC')) { $saved[$name] = [Environment]::GetEnvironmentVariable($name) }
$results = [Collections.Generic.List[object]]::new()
try {
    $output = [IO.Path]::GetFullPath((Join-Path (Get-Location) $OutputDir))
    New-Item -ItemType Directory -Force -Path $output | Out-Null
    $goExecutable = (Get-Command $GoPath).Source
    $gofmt = Join-Path (Split-Path -Parent $goExecutable) 'gofmt.exe'
    $env:GOOS = & $goExecutable env GOHOSTOS
    $env:GOARCH = & $goExecutable env GOHOSTARCH
    $env:CGO_ENABLED = '0'
    function Check([string]$name, [string]$command, [scriptblock]$operation) {
        $clock = [Diagnostics.Stopwatch]::StartNew()
        $global:LASTEXITCODE = 0
        $log = Join-Path $output ($name+'.log')
        try { & $operation *> $log; $code = $LASTEXITCODE } catch { $_ | Add-Content -LiteralPath $log; $code = 1 }
        $clock.Stop()
        $status = if ($code -eq 0) { 'PASS' } else { 'FAIL' }
        $results.Add([pscustomobject]@{name=$name;command=$command;status=$status;exit=$code;seconds=$clock.Elapsed.TotalSeconds;log=$log})
        $results | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $output ('results-'+$Mode+'.json'))
        '{0}: {1} ({2:N3}s)' -f $name,$status,$clock.Elapsed.TotalSeconds
        if ($code) { Get-Content -LiteralPath $log -Tail 18 }
    }
    if ($Mode -in @('Mandatory','All')) {
        Check 'version' 'go version; go env' { & $goExecutable version; & $goExecutable env }
        Check 'gofmt' 'gofmt -l <LF-normalized copies, batches of 30>' {
            $files = @(rg --files api cmd internal -g '*.go')
            $copies = @()
            foreach ($file in $files) {
                $copy = Join-Path $output ('gofmt-lf/'+$file)
                New-Item -ItemType Directory -Force -Path (Split-Path -Parent $copy) | Out-Null
                [IO.File]::WriteAllText($copy,[IO.File]::ReadAllText((Join-Path (Get-Location) $file)).Replace("`r`n","`n"),[Text.UTF8Encoding]::new($false))
                $copies += $copy
            }
            for ($i=0; $i -lt $copies.Count; $i+=30) {
                $bad = & $gofmt -l @($copies[$i..([Math]::Min($i+29,$copies.Count-1))])
                if ($LASTEXITCODE -ne 0 -or $bad) { throw "gofmt failed: $bad" }
            }
            'Checked '+$copies.Count+' Go files; CRLF checkout differences normalized without editing sources.'
        }
        Check 'build' 'go build ./...' { & $goExecutable build ./... }
        Check 'vet' 'go vet ./...' { & $goExecutable vet ./... }
        Check 'test' 'go test -count=1 ./...' { & $goExecutable test -count=1 ./... }
        $env:CGO_ENABLED = '1'
        if ($env:GOOS -eq 'windows') { $env:CC = Join-Path (Get-Location) '.tools/llvm-mingw-20260922-ucrt-x86_64/bin/x86_64-w64-mingw32-gcc.exe' }
        Check 'race' 'go test -race -count=1 ./...' { & $goExecutable test -race -count=1 ./... }
        $env:CGO_ENABLED = '0'
        Check 'mod-verify' 'go mod verify' { & $goExecutable mod verify }
    }
    if ($Mode -in @('Extras','All')) {
        Check 'integration' 'go test -count=1 -p=1 -tags=integration ./...' { & $goExecutable test -count=1 -p=1 -tags=integration ./... }
        Check 'native-build' 'scripts/build.ps1' { & ./scripts/build.ps1 -GoPath $goExecutable }
        Check 'cross-build' 'scripts/build.ps1 -Cross' { & ./scripts/build.ps1 -Cross -GoPath $goExecutable }
        Check 'protobuf' 'protoc 33.5 + pinned Go plugins; compare LF-normalized sources' {
            $scratch = Join-Path $output 'proto-generated'
            New-Item -ItemType Directory -Force -Path $scratch | Out-Null
            if ((& .tools/protoc/bin/protoc.exe --version) -ne 'libprotoc 33.5') { throw 'wrong protoc version' }
            if ((& .tools/bin/protoc-gen-go.exe --version) -ne 'protoc-gen-go.exe v1.36.12') { throw 'wrong Go plugin version' }
            if ((& .tools/bin/protoc-gen-go-grpc.exe --version) -ne 'protoc-gen-go-grpc 1.6.0') { throw 'wrong gRPC plugin version' }
            & .tools/protoc/bin/protoc.exe --proto_path=api/proto --proto_path=.tools/protoc/include --plugin=protoc-gen-go=.tools/bin/protoc-gen-go.exe --plugin=protoc-gen-go-grpc=.tools/bin/protoc-gen-go-grpc.exe ('--go_out='+$scratch) --go_opt=paths=source_relative ('--go-grpc_out='+$scratch) --go-grpc_opt=paths=source_relative api/proto/infrapulse/v1/agent.proto
            if ($LASTEXITCODE) { throw 'protoc failed' }
            foreach ($file in @('agent.pb.go','agent_grpc.pb.go')) {
                $actual = Join-Path $scratch ('infrapulse/v1/'+$file)
                $expected = Join-Path (Get-Location) ('api/gen/infrapulse/v1/'+$file)
                'Raw SHA256: '+(Get-FileHash $expected).Hash+' / '+(Get-FileHash $actual).Hash
                if ([IO.File]::ReadAllText($expected).Replace("`r`n","`n") -cne [IO.File]::ReadAllText($actual).Replace("`r`n","`n")) { throw "protobuf differs: $file" }
                $file+': exact LF content equality PASS'
            }
        }
        Check 'examples' 'infrapulse-agent validate-config --config configs/*.yaml' {
            foreach ($file in Get-ChildItem configs -Filter '*.yaml') {
                $file.Name
                & ./bin/infrapulse-agent.exe validate-config --config $file.FullName
                if ($LASTEXITCODE) { throw 'invalid example' }
            }
        }
        $env:GOOS='linux'; $env:GOARCH='amd64'
        Check 'stripped' 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" ./cmd/infrapulse-agent' {
            & $goExecutable build -trimpath -buildvcs=false '-ldflags=-s -w' -o (Join-Path $output 'collector-linux-amd64-stripped') ./cmd/infrapulse-agent
            if ($LASTEXITCODE) { throw 'stripped build failed' }
            (Get-Item (Join-Path $output 'collector-linux-amd64-stripped')).Length
        }
        $env:GOOS=& $goExecutable env GOHOSTOS
        $env:GOARCH=& $goExecutable env GOHOSTARCH
    }
    if ($Mode -in @('Linux','All')) {
        $env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
        foreach ($package in @('platform/linux','logs','queue','commands','protocol/snmp','driver/snmp','driver/routeros','driver/redfish','driver/ipmi','pki','platform/openwrt','agent','simulator','telephony','transport/grpc','config','platform','syslogreceiver','managedconfig','scheduler','remote','driver/telephony','protocol/routeros','model')) {
            $binary = 'linux-'+$package.Replace('/','-')+'.test'
            Check ('compile-'+$binary) ('go test -c -tags=integration ./internal/'+$package) { & $goExecutable test -c -tags=integration -o (Join-Path $output $binary) ('./internal/'+$package) }
            if ($LASTEXITCODE -eq 0) {
                $linuxBinary = $LinuxWorkspace+'/'+$OutputDir.Replace([char]92,[char]47)+'/'+$binary
                Check ('run-'+$binary) ('wsl.exe -d '+$WSLDistro+' --cd '+$LinuxWorkspace+'/internal/'+$package+' -- '+$linuxBinary+' -test.v=true -test.count=1 -test.timeout=120s') { wsl.exe -d $WSLDistro --cd ($LinuxWorkspace+'/internal/'+$package) -- $linuxBinary '-test.v=true' '-test.count=1' '-test.timeout=120s' }
            }
        }
    }
    if (@($results | Where-Object status -EQ 'FAIL').Count) { exit 1 }
} finally {
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name,$saved[$name]) }
    Pop-Location
}
