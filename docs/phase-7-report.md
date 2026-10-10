# Phase 7 — Backend-managed Collector Configuration: отчёт

Работа и проверки: 2026-10-10–11; финальная проверка 2026-10-11.
Выполнена только Collector Phase 7. Production Backend B9,
Web UI/API и PostgreSQL в этом репозитории не реализованы. Проверки используют
development Mock Controller, protocol fixtures и локальный Linux runtime;
это не production Backend end-to-end validation. Phase 8 не начиналась.

## 1. Git branch и база

Исходные `git status`, `git branch --show-current`, `git branch -vv` и
`git log -5 --oneline` показали чистый `main` на `61f4783`:
`feat(agent): обогащены данные идентичности и L2-соседства в Phase 6`.
Phase 6 уже включена в main. Phase 5 `14abc4e` и предыдущие фазы присутствуют
в истории. Из main создана `feat/phase-7-managed-config`, base commit `61f4783`.
Изменения Phase 7 находятся в рабочем дереве; commit/push/merge/rebase/reset/
clean/stash не выполнялись. `.cache`, `.tools`, `bin`, `state` остаются ignored.

## 2. Baseline

До изменения tracked sources выполнены Go build/vet/uncached tests/race,
module verification, Windows integration, native/cross scripts, 11 YAML examples,
18 compiled Linux suites в WSL и Phase 6 durable replay/ACK fixture E2E.
SDK `go1.27.1 windows/amd64`, module `InfraPulseAgent`, Go 1.27. Production CGO0;
Windows race CGO1 с portable LLVM-MinGW. Логи: `.cache/phase7-baseline/`.

| Команда / проверка | Baseline |
|---|---|
| `gofmt -l` workspace | 49 CRLF-файлов; raw verification не PASS |
| `gofmt -l` LF scratch | PASS, 262 Go files, иных formatting gaps нет |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS |
| `go test -race -count=1 ./...` | PASS |
| `go mod verify` | PASS |
| `go test -count=1 -p=1 -tags=integration ./...` | PASS, 94,481 s |
| `scripts/build.ps1` native / `-Cross` | PASS, 5,287 / 17,279 s |
| YAML examples / 18 Linux suites | PASS |
| Protobuf raw SHA256 scratch | FAIL только из-за CRLF `agent.pb.go`; LF-normalized comparison PASS |
| `go test -count=1 -run '^TestPhase6AdaptersIdentityTopologyDurableReplayACK$' -v ./internal/agent` | Первый запуск FAIL: ошибочно оставлен GOOS=linux; повтор с Windows env PASS, 2,312 s |

Phase 6 descriptor сохранён для field/enum/RPC compatibility test. IdentityEvidence,
TopologyObservation, SourceRef, Provenance и IPMI models/security присутствовали
до начала работы. Linux amd64 stripped baseline — **26 108 064 bytes** при
`CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w'`.
`time-sync-status` в WSL вернул exit0/unavailable: отсутствует systemctl;
live corporate NTP не подтверждён.

## 3. Existing configuration architecture

Сохранён lifecycle Agent/Collector, persistent UUID и process Session ID.
Статическая strict YAML конфигурация создаёт local collectors, remote adapters,
SIP endpoints, Syslog receiver и административные security resources.
RemoteAdapterFactory регистрирует существующие SNMP/RouterOS/Redfish/IPMI
adapters; SIP обёрнут вокруг существующего OPTIONS collector. Registry хранит
target state, scheduler — bounded jobs/workers. Один sender и receiver обслуживают
существующий gRPC stream. Commands проходят прежние policy, protected-resource,
journal, snapshot и rollback проверки.

Pipeline observations сохранён: adapter → normalized models → SourceRef/time
enrichment → sanitizer → durable outbox → gRPC → application ACK/replay.

## 4. Local/remote ownership

Статические devices и local SIP endpoints не принадлежат Backend. Полный remote
snapshot заменяет только managed entries; совпадение raw target ID с local
definition отклоняется. Нормализованный SourceRef managed target — `remote:<id>`.
Удаление target не удаляет исторические observations или outbox records.
Основной config.yaml никогда не перезаписывается.

PKI, identity, Backend endpoint/trust, enrollment, state/outbox paths, NTP,
Syslog keys/listeners, service permissions, local collectors и command policies
остаются local administrator-owned. Для них нет typed fields в desired snapshot;
неизвестные wire fields v1 отвергаются.

## 5. Typed DesiredConfig contract

Additive `ControllerEnvelope.desired_config = 4` и
`AgentEnvelope.config_status = 21` используют существующий RPC Connect.
DesiredConfigSnapshot содержит collector_id, session_id, uint64 revision,
configuration_digest, optional issued_at и полный список ManagedTargetSpec.
Target содержит protocol/enabled/endpoint/credential_ref, metadata/location,
polling и фиксированные SNMP/SIP/Redfish/IPMI options. RouterOS использует
фиксированный read-only adapter и CA reference. Raw YAML, secrets, shell,
argv, arbitrary endpoints/commands и system settings отсутствуют.

Полный независимый wire contract и byte-level digest specification:
[backend-managed-config-contract.md](backend-managed-config-contract.md).

## 6. Revision/digest semantics

Revision > 0, монотонный uint64 watermark хранится и для REJECTED/FAILED.
Новая ревизия ниже watermark — stale; та же ревизия с другим normalized content
или supplied digest — conflict. Повтор фактически applied revision с тем же
digest возвращает исходный applied_at без adapter/schedule recreation, включая
случай последующей rejected revision. MAX_UINT64 допустим; rollover к 0 запрещён.
Backend history loss требует восстановления истории либо явного local
administrative recovery, автоматического rollback нет.

SHA-256 вычисляется по canonical binary v1 после фиксированных defaults и сортировки
targets по ID. Collector identity включена; revision/session/issued_at/digest
исключены. Maps и plaintext secrets отсутствуют. Независимый Python-encoded
332-byte vector: `5f1cf1a25a39ec033d81033706c417593c89d411521768fc00b4cb04c2b09bec`.
Digest проверяет consistency и не заменяет mTLS authentication.

## 7–9. Authentication, credentials и local policy

Remote management disabled by default; enabled требует production mTLS.
Collector объявляет managed_config_v1 отдельно от application ACK capability.
Update worker допускает config только после подтверждённого TLS handshake,
HelloAck negotiation и совпадения Collector ID/process Session ID. Development
plaintext и неподтверждённые custom sessions не получают management privileges.

CredentialProvider разрешает только уже существующие локальные references,
включённые в allowed_credential_refs. Пустой allowlist запрещает authenticated
targets; отсутствующий или неправильный credential отклоняет весь snapshot.
Resolved values не сохраняются и не включаются в diagnostics/status. SIP OPTIONS
не поддерживает credential delivery/authentication через managed config.

Policy ограничивает protocols, target count, payload, intervals, apply timeout,
credential refs и local CA refs. При заданных allowed_destinations CIDRs все
DNS answers должны быть разрешены; adapter подключается к выбранному numeric IP
до следующей revision/restart. HTTPS origin/Host/SNI сохраняются, redirects
не обходят destination policy. Existing TLS validation, SNMP bounds и IPMI
LANplus/cipher17 (либо explicit3)/user-or-operator policy остаются обязательными. Local scheduler
concurrency/resource limits remote config не изменяет.

## 10–12. Target lifecycle, hot reload и recovery

Validate full snapshot → prepare new adapters/diff → cancel and drain changed
jobs → publish complete registry with replacement jobs paused → persist state →
commit/start jobs → APPLIED. Existing scheduler расширен Change API; второй
scheduler не создан. Unchanged immutable specs сохраняют instance и schedules.
ADD/UPDATE/ENABLE/DISABLE/REMOVE работают в одном runtime и managed namespace.

При prepare failure текущие runnable targets продолжают работу. Replacement
отменяет старый poll и ждёт bounded drain, stale WithDevice вызовы блокируются
retired flag/pointer recheck. При runtime/persistence failure выполняется bounded
rollback, восстановление previous runnable entries и запись previous LKG с
failed revision watermark. APPLIED не отправляется до успешной публикации state.
Repeated state failure блокирует дальнейшие updates и отражается как degraded.
Это компенсирующее восстановление, а не OS transaction при произвольной disk/power
failure. Production adapter constructors не делают equipment connection probes.

## 13–14. Last-known-good и offline/restart

В administrator state_dir используется bounded `managed-config.json`: strict
versioned JSON, checksum, identity/revision/digest binding, temporary file,
0600 permissions, fsync/rename и directory fsync на Unix. Symlink/special files,
oversized/corrupt state и insecure Unix permissions отвергаются. Хранятся одна
LKG snapshot и минимальные terminal/watermark metadata, без resolved secrets.

Restore выполняется до Backend connection и повторяет актуальную local policy,
credential и DNS validation. Disabled management не запускает stored targets.
Policy denial не маскируется persisted APPLIED: Hello сообщает actual revision0,
status FAILED; duplicate stored revision получает applied_state_inactive.
Local monitoring сохраняется, security policy никогда не сбрасывается.

## 15–16. Status/diagnostics и Mock Controller

RECEIVED, VALIDATING, APPLIED, REJECTED и FAILED — отдельные typed statuses.
Status содержит identity/revision/digest, actual applied revision/digest/time,
fixed sanitized error code/message. Heartbeat и reconnect Hello сообщают applied
state; diagnostics добавляют desired revision, stored LKG revision, last status,
rejection/error/time/successful sync, target count и storage degradation.

Один sequential config worker и channel capacity1 ограничивают pending transitions.
Burst overflow закрывает stream для retry; receiver не ждёт длительный prepare.
Config result может идти через outbox, но result APPLIED отличается от receipt
MessageACK. Повтор snapshot восстанавливает потерянный semantic status.

Existing Mock получил typed snapshot sequence, revision/digest, repeat, stale/
invalid delivery, terminal status observation и disconnect/reconnect scenario.
CLI принимает bounded protojson `-desired-config` files, `-repeat-config` и
`-disconnect-after-config-status`. Это development tooling, не Backend B9.

## 17–21. Unit/security, concurrency, E2E, regression и cross-build

После code freeze и повторной проверки исправлений выполнены:

| Реальная команда | Итог |
|---|---|
| `gofmt -w` всех Go sources; `gofmt -l` workspace и LF scratch | PASS, 295 files |
| `go build ./...` | PASS, 2,296 s |
| `go vet ./...` | PASS, 0,676 s |
| `go test -count=1 ./...` | PASS, 17,065 s |
| `go test -race -count=1 ./...` | PASS, 45,352 s; CGO1/portable LLVM-MinGW |
| `go mod verify` | PASS, 0,412 s |
| `go test -count=3 -run '^TestManagedConfiguration' -v ./internal/agent` | PASS, 6,383 s, 3 tests ×3 runs |
| `go test -race -count=3 -run '^TestManagedConfiguration' -v ./internal/agent` | PASS, 11,396 s, 3 tests ×3 runs |
| `go test -count=1 ./internal/model ./internal/transport/grpc ./internal/mockcontroller` | PASS |
| `go test -race -count=1 ./internal/model ./internal/transport/grpc ./internal/mockcontroller` | PASS |
| `go vet ./internal/model ./internal/transport/... ./internal/mockcontroller ./cmd/infrapulse-mock-controller` | PASS |
| `go test -count=1 ./internal/managedconfig ./internal/config` | PASS |
| `go test -race -count=1 ./internal/managedconfig ./internal/config` | PASS |

Unit/security tests проверяют defaults/strict schema, unknown administrative
fields, malformed/oversized wire, digest/default/order determinism, independent
vector, stale/duplicate/conflict/MAX_UINT64, static/managed collisions,
credentials/CA refs/DNS answers, interval/resource limits, opt-in/identity/session/
authentication, unsafe cipher, SIP auth denial, all five lifecycle protocols,
prepare/runtime/persistence failures/recovery, corruption/restore/policy changes
и shutdown. Windows symlink test SKIP из-за отсутствующего account privilege1314;
тот же test и Unix mode checks включены в Linux suite.

Concurrency tests проверяют queued/in-flight cancellation, no overlap при
replacement, rollback resume, serialized rapid revisions, concurrent diagnostics,
unchanged jobs/instances и cleanup. SourceRef и retained durable records проходят
отдельный remove/reopen/replay/ACK test. Общие suites сохраняют SNMPv2c/v3,
RouterOS/protected commands/journal/rollback, LLDP/CDP/MNDP evidence, SIP, local
Linux/OpenWrt, Syslog, NTP domain, Redfish/IPMI security и Simulator.

Основной authenticated Mock E2E фактически выполнил все20 шагов задания:
Hello capability/initial revision0; revision1 SNMP add/apply/persist/collection/
durable ACK; revision2 interval update+Redfish add; invalid3 rejected с работающей2;
offline restart restore2; reconnect Hello revision2/digest; fresh duplicate2
без adapter/job recreation; revision4 remove с сохранённым конкретным SNMP
message ID; reconnect replay этого message, original SourceRef и receipt ACK.
Redfish login/logout balanced, writes0; diagnostics не содержит fixture secrets.

Отдельный real mTLS Mock fixture test проверил RouterOS/SIP/LANplus ADD3,
SIP endpoint/FromUser/interval и IPMI settings/interval UPDATE, RouterOS DISABLE
без новых HTTP requests, REMOVE всех во время unanswered SIP OPTIONS, отсутствие
jobs/stale state/BMC sessions и RouterOS writes0. Другой test двух independently
certified Collector отклонил wrong Collector ID и wrong process Session ID;
правильная revision применена только назначенному Collector.

Исходные failures не скрываются:

| Первый FAIL / команда | Причина и результат повторной проверки |
|---|---|
| Transport targeted mTLS authentication test | gRPC peer metadata читалась до завершения bidi stream; verified standard TLS handshake wrapper заменил её. Повтор targeted normal/race PASS |
| Targeted config/manager tests при разработке | Ошибки test setup: development defaults assertion с production placeholders; fake adapters без hardware/SIP reader; compile typo. Исправлены; final full normal/race PASS |
| Focused Agent race и managed E2E repeat | Over-fast fixture/ACK hold давали durable backlog; retained старый status мог считаться свежим. Selective ACK hold одного message ID, фильтр old status IDs и bounded fixture cadence исправили барьер; count3 normal/race и full normal/race PASS |
| Первый `go test -count=1 ./...`, 19,247 s | Новый remote replay test не ACKнул первый reserved replay и ждал expiry до context deadline. ACK первого replay добавлен; final full normal PASS |
| Первый `go test -race -count=1 ./...`, 46,370 s | Тот же test bug + Windows TempDir ERROR_DIR_NOT_EMPTY в managedconfig/queue/remote; race warnings отсутствовали. Narrow existing Agent cleanup переиспользован; final full race PASS |
| Второй `go test -count=1 ./...`, 19,588 s | Existing PKI renewal test завершил assertions, но NTFS TempDir cleanup получил ERROR_DIR_NOT_EMPTY. Тот же bounded helper переиспользован PKI test; final full normal PASS |
| Windows state symlink probe | Account privilege1314; test явно SKIP на Windows, Linux проверяет real symlink/permissions |
| Первый запуск final orchestration | PowerShell typo `./ .cache/...` не запустил script. Исправлен на `& ./.cache/...`; mandatory checks выполнены |

Cleanup helper вынесен из existing Agent test utility в `internal/testutil`.
Он ограничен Windows ERROR_DIR_NOT_EMPTY после fixture Close и deadline2s,
не tolerates sharing/access failures. Existing real locked-handle negative test
сохранён и прошёл. Linux использует обычный testing.TempDir. Production cleanup,
PKI и outbox code ради test environment не менялись.

Дополнительная финальная matrix:

| Реальная команда / проверка | Итог |
|---|---|
| `go test -count=1 -p=1 -tags=integration ./...` | PASS, 78,221 s, Windows |
| `./scripts/build.ps1 -GoPath 'C:\Users\kiril\sdk\go1.27.1\bin\go.exe'` | PASS, 2,933 s |
| `./scripts/build.ps1 -Cross -GoPath 'C:\Users\kiril\sdk\go1.27.1\bin\go.exe'` | PASS, 11,461 s, CGO0 |
| `./scripts/generate-proto.ps1 -Go 'C:\Users\kiril\sdk\go1.27.1\bin\go.exe'`; pinned scratch generation | PASS, exact raw SHA256 совпадение обоих generated files |
| `./bin/infrapulse-agent.exe validate-config --config <каждый configs/*.yaml>` | PASS, 12 examples, 0,656 s |
| `go test -count=1 -run '^TestPhase6AdaptersIdentityTopologyDurableReplayACK$' -v ./internal/agent` | PASS, 2,196 s; 13 observations retained/restarted/replayed/ACKed |
| `wsl.exe -d docker-desktop --cd /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent -- /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/bin/infrapulse-agent-linux-amd64 time-sync-status --config configs/config.time-sync.example.yaml` | PASS CLI, 0,111 s; available=false/systemctl unavailable, не live NTP sync |
| `go list -deps ./cmd/infrapulse-agent` | PASS; testutil не входит в runtime dependencies; go.mod/go.sum не изменены |
| Frozen source SHA256 verification | PASS, 295 files / 0 mismatches |
| `git diff --check` | PASS |

После финальных documentation edits повторная проверка frozen sources дала
295 files / 0 mismatches. Проверены 186 локальных Markdown links в 11 документах:
broken links0. Индекс пуст, go.mod/go.sum не изменены; все 73 content changes
остаются unstaged. Independent guide/contract review не нашёл blockers.

Linux suites исполнялись в WSL `docker-desktop`, Go/C compiler внутри WSL не
устанавливался. Для каждой строки следующей таблицы фактически выполнены команды
с указанным `$package`/`$binaryName`; compile и run имеют exit0:

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
$binaryPath = Join-Path (Get-Location) ('.cache/phase7-final/'+$binaryName)
go test -c -tags=integration -o $binaryPath ('./internal/'+$package)
wsl.exe -d docker-desktop --cd ($linuxWorkspace+'/internal/'+$package) -- ($linuxPath+'/'+$binaryName) '-test.v=true' '-test.count=1' '-test.timeout=120s'
```

`$linuxWorkspace=/mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent`,
`$linuxPath=$linuxWorkspace/.cache/phase7-final`.

| $package | $binaryName | Compile / run, s | Итог |
|---|---|---:|---|
| platform/linux | linux-platform-linux.test | 0,188 / 2,130 | PASS |
| logs | linux-logs.test | 0,198 / 0,474 | PASS |
| queue | linux-queue.test | 0,188 / 4,501 | PASS |
| commands | linux-commands.test | 0,192 / 0,504 | PASS |
| protocol/snmp | linux-protocol-snmp.test | 0,183 / 1,596 | PASS |
| driver/snmp | linux-driver-snmp.test | 0,192 / 0,087 | PASS |
| driver/routeros | linux-driver-routeros.test | 0,212 / 1,197 | PASS |
| driver/redfish | linux-driver-redfish.test | 0,260 / 0,912 | PASS |
| driver/ipmi | linux-driver-ipmi.test | 0,286 / 0,428 | PASS |
| pki | linux-pki.test | 0,207 / 0,413 | PASS |
| platform/openwrt | linux-platform-openwrt.test | 0,266 / 0,833 | PASS |
| agent | linux-agent.test | 1,797 / 17,488 | PASS, все managed Mock E2E и Phase6 fixtures |
| simulator | linux-simulator.test | 0,309 / 0,735 | PASS |
| telephony | linux-telephony.test | 0,201 / 1,419 | PASS |
| transport/grpc | linux-transport-grpc.test | 1,326 / 0,288 | PASS |
| config | linux-config.test | 0,216 / 0,094 | PASS |
| platform | linux-platform.test | 0,185 / 0,091 | PASS |
| syslogreceiver | linux-syslogreceiver.test | 0,202 / 0,804 | PASS |
| managedconfig | linux-managedconfig.test | 0,217 / 0,488 | PASS, real symlink и Unix permissions assertions |
| scheduler | linux-scheduler.test | 0,166 / 0,158 | PASS |
| remote | linux-remote.test | 0,215 / 0,424 | PASS |
| driver/telephony | linux-driver-telephony.test | 0,651 / 0,206 | PASS |
| protocol/routeros | linux-protocol-routeros.test | 0,211 / 0,180 | PASS |

External/live integration tests без endpoint environment остаются SKIP; fixtures
и реальный Linux proc/sys/file I/O не приравниваются к настоящему remote hardware.

Cross-build matrix полного Collector (`scripts/build.ps1 -Cross`, unstripped,
без matched comparison flags):

| Target | Дополнительные flags | Bytes | Итог |
|---|---|---:|---|
| linux/amd64 | CGO_ENABLED=0 | 38 901 840 | PASS |
| linux/arm64 | CGO_ENABLED=0 | 36 607 843 | PASS |
| linux/arm | CGO_ENABLED=0, GOARM=7 | 36 477 537 | PASS |
| linux/mips | CGO_ENABLED=0, GOMIPS=hardfloat | 39 274 278 | PASS |
| linux/mipsle | CGO_ENABLED=0, GOMIPS=hardfloat | 39 266 908 | PASS |

Mock Controller, Simulator и Mock Enrollment linux/amd64 и arm64 также PASS.
Runtime вне Linux amd64 и реальная OpenWrt firmware не подтверждены cross-build.

## 22. Performance и ресурсы

Windows amd64, Intel i5-12400F, Go1.27.1; fixture measurements не гарантируют
OpenWrt runtime performance.

| Workload / команда | Фактический результат |
|---|---|
| `go test -run '^$' -bench '^BenchmarkManaged' -benchtime=200x -benchmem ./internal/managedconfig ./internal/remote` — validation 1 SNMP | 8 844 ns/op; 4 516 B/op; 46 allocs/op |
| Та же команда — apply с durable state, 1 unchanged target / new revision | 988 112 ns/op; 17 326 B/op; 145 allocs/op |
| Prepare 32 fake targets | 40 731 ns/op; 131 691 B/op; 526 allocs/op |
| Live add/remove 32 targets, pair, без file persistence | 57 284 ns/op; 142 322 B/op; 603 allocs/op |
| `go test -count=1 -run '^TestResourceUsageManagedTransitions$' -v ./internal/managedconfig` | 50 revisions 50,33 ms; total alloc 1 044 632 B; goroutines2→2; one adapter; restore2,135 ms; state1 319 B |
| `go test -count=1 -run '^TestManagedLifecycleResourceProbe$' -v ./internal/remote` | 20×32 add/remove: 24,57 ms; 640 opened/640 closed; goroutines2→7→2; total alloc4 363 336 B; retained queue32 messages/18 560 B |

Lifecycle heap delta: idle +6 096 B, retained queue +94 400 B, after Close
+85 072 B while registry/queue references remain. Individual Windows removal
timers returned 0 at clock resolution; benchmark average above is meaningful.
Ограниченный memory queue намеренно drops telemetry при saturation.

Full process sample после5s: production mTLS, Backend offline, local/remote
targets отсутствуют, durable queue. Phase6 working set20 930 560 B / private
bytes53 710 848 B / 10 threads; Phase7 opt-in working set21 225 472 B / private
bytes53 764 096 B / 10 threads. Private bytes — Windows process accounting,
не Go heap/RSS. Процессы запускались hidden и остановлены после sample.
Matched Linux amd64 binary:
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false
-ldflags='-s -w' -o .cache/phase7-final/collector-linux-amd64-stripped ./cmd/infrapulse-agent`
— **26 366 112 bytes**, baseline26 108 064, **+258 048 (+0,9884%)**.
Без `-ldflags` при тех же flags —38 809 254 bytes, baseline38 463 748,
+345 506. Общий bin каталог содержит historical artifacts; для сравнения
использован только `.cache/phase7-final/matched-binary-sizes.json`.

Linux manager probe из compiled suite:50 revisions142,231ms, total alloc
1 003 152 B, heap506 168→478 920 B, goroutines2→2, restore135,101µs,
state1 323 B. Это fake adapters и storage timing, отдельно от actual adapter
Agent E2E restore1,208ms и RouterOS/SIP/IPMI removal43,293ms. Windows race fixture
restore3,048–4,732ms/remove70,85–83,64ms. Timing samples не production SLA.

## 23. Changed files

Изменения затрагивают typed domain/protobuf и mapping, opt-in policy,
managedconfig manager/validators/store, scheduler/remote lifecycle, DNS pinning
существующих adapters, SIP wrapper, Agent stream/status/diagnostics, sanitizer,
outbox admission, Mock CLI/controller, tests, examples и документацию.
Всего 73 файлов с содержательным diff или новым содержимым; ignored
tool/cache/build/state artifacts в список не входят. Общий gofmt также
нормализовал CRLF в существующих Go files без содержательного Git diff.

```text
api/gen/infrapulse/v1/agent.pb.go
api/proto/infrapulse/v1/agent.proto
cmd/infrapulse-mock-controller/main.go
configs/config.managed.example.yaml
docs/architecture.md
docs/backend-managed-config-contract.md
docs/deployment.md
docs/managed-configuration.md
docs/openwrt.md
docs/phase-7-report.md
docs/protocol.md
docs/security.md
docs/validation.md
docs/vendor-support.md
internal/agent/agent.go
internal/agent/managed_ca_test.go
internal/agent/managed_e2e_test.go
internal/agent/observations.go
internal/agent/remote.go
internal/agent/tempdir_windows_test.go
internal/config/config.go
internal/config/managed_test.go
internal/config/managed.go
internal/config/target.go
internal/driver/driver.go
internal/driver/ipmi/adapter.go
internal/driver/ipmi/pinning_test.go
internal/driver/redfish/client.go
internal/driver/redfish/pinning_test.go
internal/driver/telephony/adapter_test.go
internal/driver/telephony/adapter.go
internal/managedconfig/digest_vector_test.go
internal/managedconfig/failure_test.go
internal/managedconfig/manager_test.go
internal/managedconfig/manager.go
internal/managedconfig/normalize.go
internal/managedconfig/store.go
internal/managedconfig/testdata/digest-v1.json
internal/managedconfig/validate.go
internal/mockcontroller/controller.go
internal/mockcontroller/managed_config_test.go
internal/mockcontroller/managed_config.go
internal/model/managed_config_test.go
internal/model/managed_config.go
internal/model/model.go
internal/pki/manager_test.go
internal/protocol/routeros/client.go
internal/protocol/routeros/pinning_test.go
internal/queue/durable_test.go
internal/queue/managed_test.go
internal/queue/queue.go
internal/remote/managed_resources_test.go
internal/remote/managed_test.go
internal/remote/managed.go
internal/remote/registry.go
internal/sanitize/managed_test.go
internal/sanitize/sanitize.go
internal/scheduler/change_test.go
internal/scheduler/change.go
internal/scheduler/scheduler.go
internal/telephony/endpoint.go
internal/telephony/pinning_test.go
internal/telephony/probe.go
internal/testutil/tempdir_other.go
internal/testutil/tempdir_windows.go
internal/transport/grpc/client.go
internal/transport/grpc/managed_config_test.go
internal/transport/grpc/managed_config.go
internal/transport/grpc/mapping.go
internal/transport/grpc/phase7_compatibility_test.go
internal/transport/grpc/testdata/phase6-agent.desc
internal/transport/transport.go
README.md
```

## 24–26. Limitations, Backend B9 и readiness for Phase 8

Production Backend B9 ещё не реализован/не проверен end-to-end. Backend должен
реализовать negotiation, identity/session binding, exact v1 normalization/digest,
full snapshots, durable monotonic revision history и semantic status reconciliation
по [contract](backend-managed-config-contract.md). Новые messages не отправляются
через arbitrary command payloads старым Collector.

Remote config не доставляет secrets, не меняет local policy или system settings.
Нет incremental patch, multi-collector failover/ownership engine, DNS live
re-resolution, SIP authentication и universal command executor. Нет live real
SNMP/RouterOS/PBX/BMC vendor validation и OpenWrt RAM/flash/runtime evidence;
cross-build не подтверждает эти свойства. Local state filesystem должен быть
доступным и защищённым администратором. При повреждении/утрате revision history
нужна explicit administrative recovery, описанная в guide/contract.

Collector Phase7 acceptance завершена: typed management, local security boundary,
все пять Target lifecycles, revision/digest/idempotency, safe recovery/LKG/restart,
status/diagnostics и Mock handoff проверены; build/vet/tests/race/integration,
23Linux suites, protobuf/examples/cross-build PASS. Независимый review нашёл
inactive restore duplicate status и special CA file prepare gap: исправлены,
regression tests PASS. CA preflight проверяет regular final target и размер;
administrator trust paths сохраняют поддержку symlinks для certificate rotation
и статических adapters при disabled remote management. Linux regression
`TestLocalTrustSymlinksPreserveStaticAndManagedAdapters` фактически PASS без SKIP;
state symlinks по-прежнему запрещены. После этого исправления все mandatory
и extra suites повторены на неизменных 295 Go sources. Основной runtime
и Phase0–6 не заменены.

Можно переходить к отдельной Phase8 — hardening/deployment/final validation,
сохраняя перечисленные production Backend/hardware/OpenWrt ограничения.
Phase8 не выполнялась; Git commit/push не выполняются.
