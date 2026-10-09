# Результаты проверок InfraPulse Agent

## Завершённая проверка P0

Дата: 2026-10-04. Проверенный SDK: `go1.27.1 windows/amd64`. Проверка runtime выполнена отдельно в WSL Linux amd64, kernel `6.18.33.2-microsoft-standard-WSL2`. Приведённые результаты относятся к реализации P0; последующие изменения README, документации и Git-исключений не изменяли Go-код, protobuf, зависимости или runtime-конфигурацию. Полный набор Go/runtime проверок после этих документационных изменений повторно не запускался.

## Реальный Linux end-to-end

Agent и Mock Controller собраны для `linux/amd64` с `CGO_ENABLED=0` и запущены на loopback `127.0.0.1:19443`. Использовались explicit development mode, local collection, fast interval 3s, heartbeat 2s и inventory 30s. Telemetry не генерировалась Simulator.

| Проверка | Результат |
|---|---|
| AgentHello | Реальный Linux hostname/kernel/architecture/boot identity; driver `local-linux` |
| Fast telemetry | 8 samples, UTC observedAt от 09:50:29.651Z до 09:50:50.650Z; интервалы приблизительно 3s |
| CPU | Первый utilization отсутствует; затем 7 delta samples; 12 logical CPUs |
| RAM | Реальный total 16,647,843,840 bytes; available/used/free/swap получены из procfs |
| Interfaces | 2 реальных interfaces; counters/sysfs metadata/IP addresses |
| Traffic rates | 7 samples с derived rates после первого baseline |
| Heartbeat / inventory | 11 heartbeat, 1 inventory update |
| Typed command | Mock отправил `interface.disable` для `eth-test`; два `CommandResult`: `rejected`, `policy_denied`, `changed=false` |
| Outage / reconnect | Mock остановлен SIGTERM, Agent продолжил работу; 5 retries с jitter/backoff, затем восстановил connection после старта Mock |
| Identity | Agent ID и session ID совпали до и после reconnect в рамках одного запуска |
| Shutdown | Agent и оба экземпляра Mock завершились после SIGTERM с exit code 0 |

Retry delays этой проверки: примерно 197ms, 217ms, 698ms, 812ms, 1943ms при configured initial=200ms и max=2s. Параллельный scheduler продолжал собирать samples при недоступном controller. Проверка не доказывает durable delivery: P0 outbox остаётся volatile/best effort.

Временные артефакты проверки не включены в репозиторий.

## Linux collector tests и cross-build

Обычные tests используют isolated procfs/sysfs fixtures и не требуют Linux, root или сетевого оборудования. Проверены CPU guest accounting/delta/reset/reboot, memory available/fallback/overflow, interface mapping, missing optional metadata, malformed sources, resource limits, cancellation и concurrent collection. `gofmt`, package tests и `go vet ./internal/platform/...` прошли.

Integration binary собран командой `go test -c -tags=integration -o .cache/linux-platform-tests ./internal/platform/linux` для Linux amd64 и выполнен в WSL. `TestRealLinuxCollector` и все fixture tests прошли на реальных `/proc`, `/sys` и interface addresses. Integration test не требует root:

```sh
go test -tags=integration ./internal/platform/linux
```

Финальная сборка через `scripts/build.ps1 -Cross` с выбранным Go SDK успешно собрала полный Agent с CGO disabled для пяти architectures. Отдельно успешно собраны полные Mock Controller binaries для amd64 и arm64. Это builds всех runtime dependencies, а не только platform packages. Бинарные артефакты не включены в репозиторий:

| Binary | Target | Размер, bytes |
|---|---|---:|
| infrapulse-agent-linux-amd64 | linux/amd64 | 19,048,010 |
| infrapulse-agent-linux-arm64 | linux/arm64 | 17,846,356 |
| infrapulse-agent-linux-arm | linux/arm | 18,035,999 |
| infrapulse-agent-linux-mips | linux/mips | 19,684,136 |
| infrapulse-agent-linux-mipsle | linux/mipsle | 19,684,092 |
| infrapulse-mock-controller-linux-amd64 | linux/amd64 | 18,255,279 |
| infrapulse-mock-controller-linux-arm64 | linux/arm64 | 17,120,913 |

Размеры относятся к обычной сборке без stripping; они могут изменяться после правок. `go version -m` подтверждает Go 1.27.1 и CGO_ENABLED=0. Проверенный ARM build использует GOARM=7, MIPS builds — GOMIPS=hardfloat. Для конкретного устройства требуется подобрать CPU/float target; compiled hardfloat binary нельзя автоматически считать подходящим для любого OpenWrt маршрутизатора.

Agent/Mock runtime этой проверки — Linux amd64. Cross-build дополнительных architectures подтверждает компиляцию, но не поддержку конкретной OpenWrt firmware, наличие CA store, доступность native interfaces или приемлемое RAM/flash потребление. Native OpenWrt adapter остаётся P2. После переработки README повторно проверены все его относительные ссылки: указанные файлы существуют.

## Race detector на Windows

Первоначальный SDK запускал tests с CGO disabled. Для race probe получен portable [официальный LLVM MinGW release 20260922](https://github.com/mstorsjo/llvm-mingw/releases/tag/20260922), asset `llvm-mingw-20260922-ucrt-x86_64.zip`, LLVM 23.1.2. Архив расположен только в ignored `.tools`; системная установка и постоянное изменение PATH не выполнялись. Published GitHub SHA256 проверен:

```text
e3ad77d117a4bea19a7a3b333341824d79a5a371004a10e25b8504e7b3047666
```

Compiler возвращает полный путь `libsynchronization.a`, соответствуя [требованиям Go race detector на Windows](https://go.dev/doc/articles/race_detector). Первоначальный platform probe и финальный **`go test -race ./...` прошли** с exit code 0, включая Agent end-to-end и transport mTLS tests. Проверка выполнена для Windows amd64 с `CGO_ENABLED=1` и `CC`, указывающим на подготовленный C compiler.

Race detector проверяет только выполненные paths. Linux runtime integration и Windows race tests дополняют друг друга; один запуск не доказывает отсутствие любых будущих races или корректность ещё не реализованных drivers.

## Финальные quality gates

После завершения всех P0 изменений успешно выполнены:

- `gofmt -l` по всем Go sources: список пуст;
- `go build ./...`, `go vet ./...`, `go test ./...`;
- `go test -race ./...` с portable compiler;
- `go mod verify`: all modules verified;
- CLI `version`, `validate-config` для обоих example YAML и безопасный `diagnostics`;
- повторная protobuf generation: SHA256 обоих generated sources не изменился.

Обычная suite проверяет config/defaults/strict YAML/resource limits, identity persistence/corruption/concurrent creation, scheduler timeout/non-overlap/error isolation, bounded queue/priority/results retention, counter reset/rates, typed validation/capability/policy/protection и sanitizer. Transport tests проверяют protobuf presence/roundtrips/malformed inputs, TCP bidi, cancellation и mTLS positive/negative cases (CA/SAN/client certificates).

Agent tests используют локальный TCP Mock и test-only fixtures с source=simulated: hello/inventory/telemetry/rates/heartbeat/typed rejection, Controller restart с reconnect, offline queue bounds, slow-reader backpressure и graceful shutdown. Они не требуют real network equipment, root или внешнего Controller.

## Что не проверялось этим demo

Remote SNMP/RouterOS, шесть vendor profiles, LLDP/topology, native OpenWrt, logs/auditd, services/firewall actions, SIP/PBX statistics и Simulator не реализованы в P0. Поэтому эта проверка подтверждает foundation: реальные Linux measurements, outbound stream, typed rejection, intervals, reconnect и shutdown. Она не является подтверждением полного hackathon E2E с remote equipment.

## Проверка P1 — 2026-10-04

SDK: `go1.27.1 windows/amd64`. P1 продолжает существующий P0; Git history и
коммиты не изменялись. Обычные tests используют fixtures и локальные UDP/HTTPS/
gRPC servers. Реальное сетевое оборудование не использовалось.

После завершения реализации и исправлений финального review прошли:

- `gofmt -w` и `gofmt -l` по всем Go sources: список неформатированных файлов пуст;
- `go build ./...`, `go vet ./...`, `go test ./...`;
- `go test -race ./...` на Windows amd64 с `CGO_ENABLED=1` и подготовленным
  LLVM MinGW compiler из ignored `.tools`;
- `go mod verify`: `all modules verified`; `go mod tidy` выполнен;
- CLI `version` и `validate-config` для всех трёх example YAML;
- generation через `scripts/generate-proto.ps1`: SHA256 обоих generated sources
  остались неизменными при повторном запуске;
- проверка относительных ссылок README/docs и `git diff --check`.

GoSNMP закреплён как direct dependency `github.com/gosnmp/gosnmp v1.45.0`.
Runtime dependencies остаются pure Go. Generated hashes:

```text
agent.pb.go:      1A6277883A51221770D273D941E0949B036DBC1095325C215E209017E364D8CB
agent_grpc.pb.go: AC213C387B28E787A7793438C46341276EF6F6E6CB1B05CFCF18A8F667348FA2
```

### Unit / fixture / protocol tests

| Блок | Реально проверенные сценарии |
|---|---|
| Config / credentials | Strict references/limits, environment rotation/cancellation, credential JSON/fmt/error redaction |
| Registry | Bounded workers, exclusion/cancellation, unknown/online/offline после трёх connectivity failures, unsupported OID, backpressure, успешный medium при ошибке fast, сохранение slow inventory |
| SNMP adapter | Actual UDP Get/GetNext/Walk/BulkWalk, packet errors, NoSuchObject/Instance, retries, timeout/cancellation/Close, OID consistency и response/request limits |
| SNMP mapping | SYSTEM/ENTITY/chassis ambiguity, IF/IF-X HC/fallback/presence, missing/malformed values, vendor evidence, fast batched Get и slow discovery/metadata refresh |
| Rates | First sample, actual elapsed, zero/absent counters, 64-bit precision, reset/reboot/identity/discontinuity, conservative Counter32 wrap и unknown-width aggregates |
| LLDP / topology | Local port 7 ≠ ifIndex 42, name/MAC mapping, ambiguous/unresolved ports, raw subtypes, multiple/unknown neighbors, churn, partial/failed/complete-empty, timeout/expiry, bounded LRU, ARP rejected |
| RouterOS HTTPS | Authentication/rotation, CA/SAN verification, fixed endpoints и canonical `*` IDs, redirects/errors/JSON/limits, timeout/cancellation |
| RouterOS mapping | Identity/resource/RouterBOARD, interfaces/counters/IP cache, medium/slow separation, LLDP/CDP/MNDP/unknown neighbor evidence и bounded expansion |
| Commands | Target/age/deadline/capability/policy, canonical ID/name/MAC formats и bridge/bond/VLAN protection, permission revoke, identity changes, bounded dedup/result reserve |
| Mutations | Enable/disable/no-op/readback, timeout verified/uncertain, malformed/oversized/HTTP500 после PATCH, definitive rejection и cancellation после отправки |
| Wire / Mock | P0 field numbers, topology=14, presence/width/raw bytes/evidence/components/verification, malformed payloads и реальный bidirectional gRPC topology JSON |

### Agent ↔ Mock E2E с remote fixtures

`TestAgentSNMPUDPTopologyRatesOfflineRecovery` использует реальный GoSNMP UDP
exchange с локальным server. Проверены ENTITY model/serial, HC counters/rates,
LLDP mapping и доставка TopologyUpdate, outage/offline/recovery, public
`ProbeDevice`, отсутствие community в wire/diagnostics.

`TestAgentRouterOSRemoteStreamingMutationsAndProtection` использует локальный
HTTPS server с test CA и реальный Agent/Mock gRPC stream. Проверены interfaces/
rates/neighbors, typed disable/enable с verification, replay idempotency key
после Controller reconnect без второго PATCH, protected canonical management
`.id` и отсутствие password в diagnostics.
`TestRemoteTimeoutDoesNotBlockLocalCollectionAndRecovers` подтвердил local
collection при remote timeout и online recovery.
`TestFullOutboxRejectsCommandBeforeWrite` подтвердил отсутствие PATCH без
резерва для CommandResult.

Эти E2E прошли на Windows в обычной suite и с race detector. Полный test binary
`internal/agent` также собран с `CGO_ENABLED=0` для Linux amd64 и выполнен в WSL:

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go test -c -o bin/agent-p1-linux-tests ./internal/agent
wsl --distribution docker-desktop --exec /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/bin/agent-p1-linux-tests
```

Linux suite завершилась `PASS`, exit code 0. Она включает P0 Agent tests и P1
remote E2E, использует fixture local platform и реальные protocol exchanges
с test devices. Это Linux amd64 runtime проверенных remote paths; повторный
live сбор `/proc`/`/sys` P1 этим запуском не заявляется. Docker engine и внешнее
оборудование для tests не требовались.

### Cross-builds P1

После финальных runtime изменений с `CGO_ENABLED=0` успешно собраны:

| Binary | Targets |
|---|---|
| Agent | linux/amd64, linux/arm64, linux/arm (`GOARM=7`), linux/mips и linux/mipsle (`GOMIPS=hardfloat`) |
| Mock Controller | linux/amd64, linux/arm64 |

Команды: `go build -o bin/<target> ./cmd/<component>` с отдельными GOOS/GOARCH.
Binary/cache/compiler/test CA artifacts не включены в Git. Runtime остальных
architectures и OpenWrt firmware не проверялся; cross-build не подтверждает
hardware compatibility или приемлемое RAM/flash потребление.

### Существенные ограничения P1

- Hardware/vendor tests не запускались; matrix отражает fixtures/test servers.
- SNMP только v2c read-only, community не шифруется. Шесть полноценных vendor
  profiles и SNMPv3 остаются вне P1.
- sysUpTime относится к management subsystem. Новые interfaces обнаруживаются
  на slow inventory; max_rows ограничивает raw varbinds и normalized rows,
  поэтому число допустимых interfaces меньше размера полного walk в varbinds.
- Неоднозначные Counter32 и unknown-width aggregate rates отсутствуют;
  unavailable не считается измеренным нулём.
- Neighbor expiry — явная collector assumption; unresolved evidence остаётся
  unresolved, global reconciliation принадлежит будущему Backend.
- RouterOS writes требуют доступных local user/group permissions. Dependency
  protection консервативна, management interfaces задаются оператором явно.
- Outbox, result reservations, topology history и dedup volatile. Commands
  требуют max_message_bytes ≥ 4096; без резерва запись не выполняется.
  Durable ACK/journal/exactly-once/rollback отсутствуют.
- P2/P3, Backend, Web UI, LLM и Simulator не добавлялись.

## Проверка P2 — 2026-10-05

SDK: `go1.27.1 windows/amd64`, module `InfraPulseAgent`, Go 1.27.
Финальные проверки выполнены после review и исправлений logs identity,
sanitizer, SIP parser и Simulator stream/scenario lifecycle. P0/P1 tests
сохранены. Коммиты, индекс и Git history не изменялись.

### Финальные quality gates

| Проверка | Фактический результат |
|---|---|
| `gofmt -w` и `gofmt -l` всех Go sources | PASS; список неформатированных файлов пуст |
| `go build ./...` | PASS, Windows amd64, CGO disabled |
| `go vet ./...` | PASS |
| `go test ./...` | PASS; fixtures и локальные protocol servers |
| `go test -race ./...` | PASS, Windows amd64, `CGO_ENABLED=1`, portable LLVM MinGW compiler |
| `go mod verify` | `all modules verified` |
| `scripts/build.ps1` | Собраны Agent, Mock Controller и отдельный Simulator |
| CLI `validate-config` | Все пять example YAML прошли |
| Protobuf regeneration | Повторный `scripts/generate-proto.ps1` сохранил SHA256 generated sources |
| Production dependencies | `go list -deps ./cmd/infrapulse-agent` не содержит `internal/simulator` |
| Документация и Git diff | Относительные Markdown links всех 12 README/docs файлов и `git diff --check` прошли |

`golang.org/x/sys v0.47.0` используется напрямую для file identity; версия
существовавшей зависимости не изменена. Runtime builds не требуют CGO.
Обычная suite не использует root, Internet, внешний Controller/PBX или
сетевое оборудование. Live Linux tests отделены tag `integration`.

Generated hashes:

```text
agent.pb.go:      A9A9B840B3AB2849DB0B6DBC7F6C328ED54F06D8282E30C60AAFFFCE601FD9E3
agent_grpc.pb.go: AC213C387B28E787A7793438C46341276EF6F6E6CB1B05CFCF18A8F667348FA2
```

P0/P1 field numbers сохранены. Проверены новые `logs=15`, `telephony=17`,
inventory extensions и optional zero/false presence после настоящей protobuf
serialization. Generated sources получены pinned script, а не ручным изменением.

### Unit / fixture / regression coverage

| Блок | Реально выполненные проверки |
|---|---|
| OpenWrt | Release evidence/detection, board/model, Linux reuse/fallback, netifd networks, procd instances и boot symlink, UCI zones/rules/redirects, missing ubus, malformed/partial/bounded responses, timeout/cancellation и wire roundtrip |
| Шесть profiles | Cisco/Huawei/MikroTik/Juniper/Eltex/QTECH PEN registry fixtures: positive/negative, OID arc boundaries, missing/malformed/unknown product, standard Generic fallback и отсутствие ложной capability |
| File pipeline | Append, incomplete/empty file, restart, rename/new file, copytruncate/regrowth, truncate, delete/recreate, identity change/inode reuse, rapid rotation/gap/limits, oversized line, JSON expansion, malformed UTF-8, permissions через injected error, burst и cancellation |
| Cursor semantics | Corrupted state fail-closed, membership/anchor validation, rejected admission, first rejected file после rotation/restart, persistence failure blocking/replay и отсутствие durable RAM delivery |
| Syslog/audit | RFC5424/BSD/rsyslog parsing, uncertain timestamps, malformed/duplicate audit fields, optional UID/AUID/PID/success, exact bounded EventID correlation и sanitized raw records |
| Sanitizer | Configured community/password values, Authorization Bearer/Basic, known secret fields, multiline PEM через restart/rotation, короткие/overlapping secrets, marker-prefix credentials, idempotence и post-sanitization batch bounds |
| SIP UDP/TCP | 200/202, 401/407, 404, 503, timeout, malformed response, cancellation, latency, failure threshold/recovery/transitions, UDP retry, transaction matching, TCP framing/provisional/body/truncation, CRLF/folded headers и strict Content-Length |
| Simulator | Все 12 scenarios и `all`, deterministic seed/trends, wire markers, limits, topology events по snapshot differences, offline LastSeen, JSON/controller CLI, имя 128 bytes, reconnect/cancellation и bounded peer EOF drain |
| Agent / outbox | Logs/audit/SIP gRPC streaming, cursor restart, source/service/telephony transitions, diagnostics/redaction, heartbeat recovery, log storm telemetry isolation и dropped_logs |
| P1 regression | SNMP/ENTITY/IF/LLDP/rates, RouterOS HTTPS enable/disable/protection/readback, remote timeout isolation, result reserve и Agent remote E2E |

OpenWrt fixtures построены по официальным implementation schemas;
vendor fixtures — по подтверждённому IANA registry. Test board/address/PID/product
values не являются captures оборудования. Ни один из шести profiles не получил
vendor-specific health OID или автоматическую capability.

### Linux runtime и Agent ↔ Mock E2E

Среда: WSL `docker-desktop`, Linux amd64,
kernel `6.18.33.2-microsoft-standard-WSL2`. Docker engine не запускался.
Go/C compiler внутри WSL не требуется: test binaries собраны с `CGO_ENABLED=0`
в Windows и выполнены в Linux. После последнего code freeze прошли полные suites:

- `internal/platform/linux` с tag `integration`: реальные `/proc`, `/sys`,
  CPU/RAM/interfaces/addresses и все Linux fixtures;
- `internal/logs`: реальный Linux file I/O, cursor persistence/restart,
  rotation/truncate/delete/recreate и statx birth time regression;
- `internal/telephony`: оба транспорта с локальными SIP servers;
- `internal/platform/openwrt`: schema fixtures, включая реальную Linux symlink;
- `internal/agent` с tag `integration`: 15 tests, включая P0/P1 remote E2E;
- `internal/simulator`: scenarios/trends и настоящий TCP gRPC Mock stream.

Сборка и запуск выполнялись в соответствующем package directory, например:

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go test -c -tags=integration -o bin/agent-p2-linux-tests ./internal/agent
wsl --distribution docker-desktop --cd /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/internal/agent --exec /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/bin/agent-p2-linux-tests '-test.v=true' '-test.count=1'
```

`TestLiveLinuxLogsTelemetrySIPAndRotation` использовал реальный Linux collector,
temp log file, rename/new file, локальный UDP SIP fixture и настоящий Agent/Mock
gRPC stream. `TestAgentCursorRestartOverGRPC` подтвердил продолжение read position
после нового запуска Agent; `TestAgentLogsAuditTelephonyStreamingAndRestart`
проверил audit fixture, redaction, SIP outage/recovery и rotation delivery.
`TestLogStormPreservesTelemetry` подтвердил изоляцию telemetry от log burst.
Cancellation/graceful shutdown paths прошли. Это Linux runtime file pipeline,
но не проверка production rsyslog/auditd daemon или внешнего PBX.

### Simulator CLI ↔ Mock отдельными процессами

После финальной сборки Windows amd64 запущены два отдельных executables на
loopback: Mock Controller и `infrapulse-simulator --scenario all --duration 500ms
--interval 50ms --seed 42 --devices 2 --output controller`. Config name имел
128 bytes. Simulator завершился с exit code 0; число envelopes совпало с
отдельным JSON CLI run того же scenario: **76 из 76**.

| Payload | Получено |
|---|---:|
| Hello | 1 |
| Inventory | 10 |
| Telemetry | 17 |
| Topology | 20 |
| Logs | 10 batches, включая 3 audit records |
| Telephony | 10 |
| Events | 8 batches |

Проверены explicit simulated markers. Final stream использует CloseSend и
bounded ожидание peer EOF; regression с замедленным Mock подтверждает получение
последних low-priority logs/telemetry. Это graceful transport completion,
не application ACK и не durable delivery. Mock process остановлен после проверки;
его graceful signal shutdown этим Windows CLI запуском не заявляется.

### Cross-builds и binary size P2

После последних runtime изменений с `CGO_ENABLED=0` собран полный Agent:

| Target | Размер обычного binary, bytes |
|---|---:|
| linux/amd64 | 21,544,795 |
| linux/arm64 | 20,113,003 |
| linux/arm, GOARM=7 | 20,331,204 |
| linux/mips, GOMIPS=hardfloat | 22,256,727 |
| linux/mipsle, GOMIPS=hardfloat | 22,256,327 |

Дополнительно собраны Linux amd64 binaries с `-ldflags '-s -w'`:
Agent — 14,782,624 bytes, Mock — 12,660,896 bytes,
Simulator — 13,111,456 bytes. Размеры относятся к этому build и не доказывают
приемлемое потребление RAM/flash конкретного OpenWrt устройства.
Simulator не включён в Agent. Binaries, test outputs, compiler/cache, identity,
runtime cursors и временные configs остаются ignored artifacts.

### Фактические ограничения и непроверенные среды

- OpenWrt runtime/hardware **unverified**: доступная WSL среда не содержит
  `/etc/openwrt_release` и `/bin/ubus`. Startup, native ubus/fw4, gRPC, shutdown
  и memory consumption на настоящей firmware не проверялись.
- Cisco/Huawei/MikroTik/Juniper/Eltex/QTECH hardware **не проверено**.
  Profiles — detection-only; Generic compatibility требует standard MIB
  availability. RouterOS fixture не является MikroTik hardware validation.
- SIP только unauthenticated OPTIONS UDP/TCP, без TLS/REGISTER/media.
  Реальные PBX и production rsyslog/auditd deployments не использовались.
- Firewall inventory — configured UCI, не active nftables evidence; rpcd UCI
  object может быть unavailable. Service/firewall mutations отсутствуют.
- Cursor после RAM admission не обеспечивает delivery. Eviction/Send failure/
  crash могут терять logs; persistence failure допускает replay. Без statx
  birth time невозможно отличить equal-content inode reuse; snapshot reads
  не гарантируют обнаружение идентичного overwrite или уже удалённой rotation.
- Sanitizer ограничен известными patterns/values, audit correlation — batch;
  universal anonymization и SIEM/AI inference не реализованы.
- Windows LocalPlatform, SNMPv3, durable outbox/application ACK/enrollment/
  execution journal/rollback и прочие P3 функции не добавлены.

## P3 — надёжность и безопасность, 2026-10-05

Этот раздел описывает текущий P3; предыдущие P0/P1/P2 записи оставлены как
исторические результаты соответствующего этапа. Windows LocalPlatform сознательно
перенесён в post-MVP/backlog и не является критерием готовности P3. Commit/push
не выполнялись, runtime artifacts не входят в Git.

### Toolchain и общая regression

Windows amd64, Go `go1.27.1`; production cross-builds используют `CGO_ENABLED=0`.
Race detector выполнен с portable LLVM-MinGW из ignored `.tools`, без runtime
зависимости production Agent от C compiler.

Прошли:

```text
gofmt всех Go sources api/cmd/internal
go build ./...
go vet ./...
go test ./...
go test -race ./...
go mod verify
go test -tags=integration ./internal/queue ./internal/commands
scripts/build.ps1
scripts/build.ps1 -Cross
```

Обычная suite использует только local files/servers, не требует hardware,
Internet, root, PBX, production CA или secrets. Crash tests выделены tag
`integration`; это завершение/kill test subprocess, не power-loss испытания.
Generated protobuf повторно получены pinned `generate-proto.ps1`; SHA-256 до
и после повторной генерации совпал. Старые field numbers сохранены. Все семь
example configs прошли CLI `validate-config`. Relative Markdown links и
`git diff --check` проверены.

### Unit / fixture / safety coverage

| Блок | Проверенный результат |
|---|---|
| SNMP v3 | Local UDP USM engine discovery/reconnect, authPriv/authNoPriv, modern/explicit legacy algorithms, wrong user/auth/privacy secret, unsupported algorithm, downgrade/malformed response, timeout/cancellation и redaction; SNMPv2 mappings/rates/LLDP regression |
| Durable outbox | Synced admission, stable IDs/restart/replay, duplicate/conflicting IDs, leased Send, delete after ACK, duplicate/unknown ACK, cancellation/timeout, ordering/priorities, protected result reservation, deterministic eviction/retention и hard physical file cap |
| Corruption/storage | Checksum/metadata/schema/index validation, partial/truncated DB, preserved corrupted state, reduced config limits, invalid path/closed storage; journal ENOSPC/EROFS/permission fault fixtures сохраняют state и запрещают mutation |
| Agent ↔ Mock | Actual loopback gRPC ACK/replay, Agent restart, Controller restart, negotiation, explicit legacy reject/send fallback и continued delivery при очереди меньше pending journal; corrupted journal не прерывает ACK telemetry |
| Journal/results | Persistent admission/future result capacity, dedup after restart, historical result при disabled/expired request и persistent alias binding, result pending ACK/recovered delivery, reserved→rejected и executing→uncertain; автоматического повторения mutation нет |
| Snapshot/rollback | Local HTTPS RouterOS fixture: snapshot before PATCH, restart persistence, verified rollback, wrong owner/resource, stale/expired/version-conflicting/superseded snapshot, protected aliases, failed/uncertain write, safe readback и duplicate command |
| Enrollment/PKI | Local ECDSA key/CSR, enrollment/restart, invalid/expired/reused/wrong-Agent token, wrong CA/key/identity/EKU, expiry, malformed/oversized response, timeout/cancellation, atomic storage fault, renewal success/failure и сохранение старого credential |
| PKI E2E | Настоящий Agent.Run ↔ verified mTLS Mock: issued certificate, rejected claimed identity mismatch, rotation/reconnect/restart с новым serial |
| SIP | UDP/TCP regression; verified TLS OPTIONS, CA/SNI/server name/expiry failure, framing/timeout/cancellation и health recovery |
| Linux/OpenWrt | Missing/invalid optional fields, disappearing interface metadata availability, large interface count, CPU/rate reset regression; missing ubus/RPC/partial/malformed responses и service/firewall changes recovery fixtures |
| P0/P1/P2 | Scheduler, transport cancellation, generic SNMP/RouterOS, policy/protection, LLDP/topology, logs/audit/cursors, six profiles и Simulator scenarios/trends/Mock streaming |

### Process crash и resource bounds

Queue subprocess exits проверены после durable enqueue, Send lease и persisted
ACK. Linux повтор: **100 циклов / 300 phase cases PASS**, stable pending ID
сохраняется, acknowledged record отсутствует после reopen. Journal subprocess
tests проверяют reserved, pre-write, external mutation marker, verification,
result persistence, ACK и partial staging write; внешний marker — fixture,
не RouterOS hardware. Linux journal stress: **20 циклов / 140 phase cases PASS**.

При первом Linux повторе выявлен редкий startup `bolt.Open` lock timeout 100ms
после child exit. Причина воспроизведена в isolated ignored diagnostic copy;
production bounded wait увеличен до 1s и проверен указанным stress run.
Mutation retries этим не вводятся.

Windows development suite выявила отдельный fixture cleanup flake: NTFS после
atomic replacement временно перечислял уже исчезнувший `SAFETY-STATE.JSON.tmp`.
Test helpers делают bounded retry только для исчезнувших/delete-pending entries
в собственном TempDir; реальные leftover files/handles остаются ошибкой. После
исправления прошли commands integration **20 повторов** и Agent/RouterOS race
**3 повтора**. Production/Linux код этим изменением не затронут.

Отдельный Linux queue resource test выполнил **480 reconnect resets,
320 admissions/ACK**: messages/bytes/in-flight не превышают config, после ACK
очередь пуста, max physical file **65 536 bytes**, goroutines **2 → 2**.
Это small fixture workload, не benchmark OpenWrt flash/RAM/CPU или proof фиксированного
RSS mmap. Journal/snapshot retention, aliases, response/batch sizes и worker/backlog
limits проверены tests; certificate retry cadence bounded, telemetry `NoSync`
не используется. Hardware flash wear/power-loss не проверены.

### Linux runtime

Среда — WSL `docker-desktop`, kernel
`6.18.33.2-microsoft-standard-WSL2`, Linux amd64, `/etc/os-release` Docker Desktop,
uid 0; test state в `/tmp` на ext-family filesystem. Это не constrained OpenWrt.
Docker engine не запускался, SDK/C compiler внутри WSL не устанавливались.

CGO0 Linux test binaries собраны Go 1.27.1 в Windows и выполнены в соответствующих
package directories через:

```text
wsl.exe -d docker-desktop --cd <package-directory> -- <test-binary> -test.v -test.timeout=120s
```

Прошли полные Linux suites: `platform/linux` (integration), `logs`, `queue`
(integration), `commands` (integration), `protocol/snmp`, `driver/routeros`,
`pki`, `platform/openwrt`, `agent` (integration), `simulator`, `telephony`,
`transport/grpc`. Helper journal crash process ожидаемо skipped в parent,
реальные child stages выполнены. Real `/proc`, `/sys`, addresses, Linux filesystem,
log rotation/cursor restart и Agent/Mock stream проверены. Сетевые устройства,
SIP/PBX и enrollment CA responses — local fixtures. Запуск как root не доказывает
доступность источников у непривилегированного production service user.

### Cross-build matrix

| Executable | CGO0 Linux targets |
|---|---|
| Agent | amd64, arm64, arm (`GOARM=7`), mips/mipsle (`GOMIPS=hardfloat`) |
| Mock Controller | amd64, arm64 |
| Simulator | amd64, arm64 |
| Mock Enrollment | amd64, arm64 |

Windows amd64 development executables также собраны; это не Windows LocalPlatform
runtime support. Binaries/test outputs остаются ignored. Cross-build не доказывает
приемлемый RAM/flash budget конкретного OpenWrt устройства.

### Пределы подтверждения

- **Fixture verified:** SNMPv3 USM, RouterOS snapshots/rollback, verified SIP TLS,
  enrollment/rotation, journal/storage faults, OpenWrt schemas, six vendor profiles.
- **Runtime verified:** Linux amd64 в указанной WSL среде; file/proc/sys I/O,
  queue/journal process crashes, actual loopback gRPC/UDP/HTTPS/TLS fixtures.
- **Hardware verified:** сетевого оборудования нет; реальных OpenWrt firmware,
  MikroTik/switch/PBX, flash endurance и power-loss tests не выполнялось.
- Astra Linux, ALT Linux, RED OS runtime **unverified**. Compatibility оценена
  по generic Linux interfaces; Ubuntu/Debian/WSL не выданы за эти дистрибутивы.
- Vendor-specific health OID не добавлены. Eltex/QTECH и остальные profiles
  сохраняют standard Generic fallback без выдуманных mappings.
- SIP Digest/REGISTER/media, NETCONF/gNMI deferred; production Backend/CA,
  revocation/CA rollover, HSM/TPM/encryption и container runtime validation отсутствуют.
- Delivery — at-least-once retained messages, с explicit retention/eviction loss;
  memory/legacy send fallback — best effort. Journal retention ограничивает dedup
  horizon, remote PATCH не входит в atomic transaction Agent storage.
- Mock enrollment one-time token state in-memory; expired cert требует operator
  recovery. Filesystem sync guarantees и доступность persistent Agent identity
  остаются deployment prerequisites.

## Collector Core Phase 0–1 — baseline и regression, 2026-10-07

Phase 0 baseline выполнен до изменения source files. Phase 1 эволюционно
адаптирует существующий P3 runtime к Collector / Edge Gateway; scope и
dependency/data-flow freeze описаны в [Collector Core](collector-core.md).
Исторические P0–P3 records выше относятся к своим этапам.

Baseline и полный regression после refactor — **PASS**. Включены unit/fixture,
process crash, Linux runtime и cross-build проверки; hardware/Backend gaps
указаны отдельно ниже.
Git commit/push/rebase/merge не выполнялись.

### Среда baseline

Windows amd64, `go version go1.27.1 windows/amd64`, SDK
`C:\Users\kiril\sdk\go1.27.1\bin\go.exe`. Локальные ignored caches:
`.cache/gopath`, `.cache/modules`, `.cache/build`. Production builds CGO0;
race detector использовал portable LLVM-MinGW из `.tools`, `CGO_ENABLED=1`.
GNU make отсутствовал; Makefile прочитан, его команды проверены непосредственно
и через существующий PowerShell build script.

| Проверка | До изменения sources | После Phase 1 |
|---|---|---|
| `gofmt -l` всех Go sources `api/cmd/internal` | PASS, список пуст | PASS |
| `go build ./...` | PASS | PASS |
| `go vet ./...` | PASS | PASS |
| `go test ./...` | PASS | PASS |
| `go test -count=1 ./...` | PASS, без test cache | PASS |
| `go test -race ./...` | PASS | PASS |
| `go test -race -count=1 ./...` | PASS, без test cache | PASS |
| `go mod verify` | PASS, all modules verified | PASS |
| `go test -count=1 -tags=integration ./internal/queue ./internal/commands` | PASS, process crash/retention/resource tests | PASS |
| `scripts/build.ps1` | PASS | PASS |
| `scripts/build.ps1 -Cross` | PASS | PASS |
| Pinned protobuf generation во временный output и SHA-256 compare | PASS, оба generated files byte-identical | PASS |
| `git diff --check` | Исходный `git status --short` пуст | PASS |

Pinned generation: protoc 33.5, protoc-gen-go v1.36.12,
protoc-gen-go-grpc v1.6.0. Использован тот же protoc invocation с временным
`--go_out/--go-grpc_out`, tracked sources не переписывались.
Baseline logs: `.cache/collector-phase1-baseline/status.log` и отдельные logs.
Подготовлен ignored повторяемый script
`.cache/collector-phase1-regression/run.ps1`; его синтаксис проверен.
Полный script выполнен дважды после refactor; последний прогон после source
freeze завершился с exit=0 для всех checks. Итоговые logs находятся в
`.cache/collector-phase1-regression/status.log` и отдельных files. Значения
resource comparison и Linux intervals ниже взяты из этого последнего прогона.

### Cross-build и Linux baseline

| Executable | Baseline CGO0 Linux targets | После Phase 1 |
|---|---|---|
| Agent/Collector | amd64, arm64, arm (`GOARM=7`), mips/mipsle (`GOMIPS=hardfloat`) — PASS | PASS |
| Mock Controller | amd64, arm64 — PASS | PASS |
| Simulator | amd64, arm64 — PASS | PASS |
| Mock Enrollment | amd64, arm64 — PASS | PASS |

Windows development builds проверены как host toolchain, а не Windows
LocalPlatform support. Linux baseline выполнен в WSL `docker-desktop`:
test binaries с CGO0 собраны на Windows и запущены в соответствующем package
directory. Command template:

```text
go test -c -tags=integration -o .cache/collector-phase1-baseline/linux-<package>.test ./internal/<package>
wsl.exe -d docker-desktop --cd <package-directory> -- <test-binary> -test.v=true -test.count=1 -test.timeout=120s
```

Все 12 baseline suites прошли: `platform/linux` с integration, `logs`,
`queue` с integration, `commands` с integration, `protocol/snmp`,
`driver/routeros`, `pki`, `platform/openwrt`, `agent` с integration,
`simulator`, `telephony`, `transport/grpc`. После Phase 1 их повтор
**PASS**. В baseline и regression проверены реальные Linux `/proc`/`/sys`, file I/O,
rotation/cursor restart, process crash boundaries, loopback gRPC/UDP/HTTPS/TLS.
OpenWrt schemas, сетевые devices, SIP/PKI responses — fixtures.

### Реализованная foundation и новые regression tests

Следующая таблица описывает добавленный code/test scope. Новые tests и полная
существующая safety suite прошли в общем regression.

| Подсистема | Реально добавленный scope и tests |
|---|---|
| Runtime/config | Один `Agent` runtime с `Collector` alias; Target projections Local/remote/SIP/file без новой YAML schema; diagnostics targets; tests сохраняют configured IDs/default 3s и скрывают credential reference из JSON |
| Target/Source | Bounded Target validation, separate Target/Device identity, SourceRef type/carrier/provenance; source types для будущих protocols не создают capability |
| Adapter registry | Compile-time `driver.Registry[T]`, minimal lifecycle Adapter и optional readers; tests constructors/duplicates/unsupported reader и legacy full Driver bridge |
| Temporal model | Optional source timestamp/clock offset/uncertainty, independent observed/collected times, unknown clock quality; tests nil≠measured zero, independent clock order, source time не создаёт offset и negative uncertainty rejected |
| Identity evidence | Available facts only, component/interface scope, raw topology chassis bytes/subtypes; tests missing evidence, weak hostname/IP, simulated provenance и clone ownership |
| Remote identity isolation | Explicit observed Device ID сохранён; optional reader не создаёт fake topology; target entry owns counter baselines; tests same Device ID across targets, identity changes/history reset, topology entity scope и ambiguous command rejection |
| Local/logs/SIP adaptation | Source-bound observations идут в прежний delivery; tests Local Target/legacy identity, SIP clocks/source, file source original timestamp optional, metadata included in log batch byte budget, RFC3339Nano completion growth не превышает admission limit |
| Simulator/wire | Existing synthetic envelopes сохраняют `source=simulated`; tests source domain/provenance; protobuf bytes/legacy payload compatibility и internal-only metadata boundary |
| Real Linux default interval | Добавлен `TestCollectorRealLinuxDefaultThreeSecondTelemetry`: настоящий Linux collector → gRPC Mock → durable ACK; Hello/Heartbeat/Inventory и три telemetry samples с интервалами 2.5–3.5s. Linux integration PASS |

Protocol implementations, durable queue/ACK, PKI/enrollment, journal,
snapshot/rollback и typed command policy/protection сохранены. Полная regression
повторяет их существующие suites; отдельное наличие новых unit tests не
заменяет это подтверждение.

Дополнительно выполнены `go test ./internal/agent -run
TestAgentSNMPUDPTopologyRatesOfflineRecovery -count=1` — PASS, включая
standalone `probe-device` source/Target binding; `go test ./internal/model
-run '^$' -bench '^BenchmarkObserveTelemetry$' -benchmem -count=1` — PASS,
407.8 ns/op, 1056 B/op, 8 allocs/op для telemetry с двумя MAC interfaces.
Это Windows microbenchmark metadata adaptation, не throughput equipment.

### Resource comparison

Одинаковый ignored harness `.cache/collector-resource-probe/main.go` использует
32 fake SNMP targets, 4 workers, fast cadence 25ms, fake I/O 2ms, 2s run,
один simulated interface/sample и memory queue 32 messages/256KiB без consumer.
Medium/inventory/topology выполняются только на startup. Fixed workload
подтверждает bounds; wall-time run count может отличаться между запусками.

| Метрика | До refactor | После Phase 1 |
|---|---:|---|
| Scheduler jobs | 32 | 32 |
| Collect concurrency peak/end | 4 / 0 | 4 / 0 |
| Goroutines до/peak/после | 1 / 6 / 1 | 1 / 7 / 1 |
| Queue peak messages/bytes | 32 / 12541 | 32 / 31048 |
| Queue final messages/bytes | 32 / 10044 | 32 / 17592 |
| Dropped telemetry/other | 400 / 244 | 399 / 244 |
| Forced-GC Alloc до/registered/после | 314496 / 401208 / 522592 B | 314896 / 424488 / 583808 B |
| Workload TotalAlloc | 2148808 B | 2778688 B |
| Runs/emitted | 640 / 676 | 647 / 675 |

Baseline durable resource integration: 480 reconnect resets, 320 admissions/ACK,
max physical file 65536 bytes, goroutines 2→2. Это small fixture workload,
не OpenWrt RAM/flash budget, RSS guarantee, hardware power-loss или flash wear
validation. После добавления observation metadata payload bytes/allocations
выросли в этой workload; jobs/workers/queue остались bounded, goroutine count
после cancellation вернулся к baseline. Transient peak составил 7 вместо 6;
goroutine на каждую Target abstraction не создаётся. Это наблюдение одного
короткого fixture run, не гарантия RSS или performance на equipment.

### E2E и границы готовности

Соседний InfraPulseBackend repository в доступной workspace отсутствует.
Collector → Backend → PostgreSQL → REST **не выполнялся**. Loopback Mock tests
не являются подтверждением Backend storage/dedup/REST acceptance. Real Linux
default-3s test после refactor прошёл: интервалы между настоящими Linux samples
3.000051248s и 3.001933418s; Hello/Heartbeat/Inventory и durable ACK подтверждены.

Full SourceRef, scoped IdentityEvidence и optional clock model остаются
internal/JSON. Existing protobuf передаёт прежние timestamps/source strings,
inventory facts, raw topology evidence и optional original log timestamp.
Additive wire gaps записаны в [protocol](protocol.md).

Clock offset estimation готов к следующей фазе на уровне domain model,
но exchange/RTT measurement/Backend monitoring и wire negotiation отсутствуют.
Collector не является NTP daemon и не меняет system/device clocks.
Redfish/IPMI vocabulary/registration foundation есть, protocol clients/security
options/schemas/fixtures ещё нужны; multi-resource inventory потребует additive
bounded batch reader. Remote Syslog/Audit receiver не реализован: existing
pipeline читает files. Backend-managed target reconciliation также отсутствует.
Zabbix/Prometheus/Ansible/Grafana/TSDB/Asset merge остаются Backend scope.

## Collector Phase 2 — Corporate NTP, 2026-10-07

Phase 2 использует стандартную службу ОС: explicit deployment apply и один
bounded read-only startup status. Собственный NTP protocol, clock discipline,
offset estimation, virtual clock, Backend wire/API и remote-device time
management не добавлены. Temporal model Phase 1 продолжает использовать
системное время и сохранять отдельный source timestamp.
Contract и administrative setup описаны в [time-sync](time-sync.md).

### Baseline до изменения кода

Исходный `git status --short` пуст. SDK — `go version go1.27.1 windows/amd64`;
выполнен `go env GOOS GOARCH CGO_ENABLED GOPATH GOMODCACHE GOCACHE CC`.
Использованы ignored local caches `.cache/gopath`, `.cache/modules`,
`.cache/build`; для Windows race — portable LLVM-MinGW и `CGO_ENABLED=1`.

| Команда | Фактический результат baseline |
|---|---|
| `gofmt -l` всех Go sources в `api`, `cmd`, `internal` | PASS, список пуст |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS |
| `go test -race -count=1 ./...` | FAIL: существующий LLDP timeout test вернул malformed response вместо DeadlineExceeded |
| Повтор `go test -race -count=1 ./...` | FAIL: Windows TempDir cleanup в двух existing durable queue tests; LLDP прошёл |
| `go mod verify` | PASS, all modules verified |

Это test failures, а не сообщения race detector о data race. LLDP test ожидал
timer callback через `Sleep(1ms)`; заменён только этот test wait на
`<-ctx.Done()`. Production SNMP code не менялся. Queue code/tests не менялись:
последующий полный race и Windows/Linux integration прошли. Windows cleanup
flake baseline сохранён в records, не выдан за исходный PASS.
Logs — `.cache/collector-phase2-baseline/{status,gofmt,build,vet,test,race,race-retry,mod-verify}.log`.

### Новые config/provider/runtime tests

| Scope | Подтверждённые сценарии |
|---|---|
| Config | Default/disabled, IPv4, IPv6, несколько servers, DNS normalization, duplicate removal, empty enabled list, invalid address/URL/port/option/control characters, unknown fields/types и bounded server count |
| Linux selection | Active chrony preferred, active timesyncd при inactive chrony, available/inactive/absent services, conflicting active clients, unit aliases |
| Chrony | Два distro paths, actual process `-f`, confdir/sourcedir/include fragments, idempotency, replacement only owned sources, correct administrator config no-op, refusal other sources/local/refclock/custom layouts/`-x` |
| Timesyncd | Managed drop-in, explicit corporate `NTP` и `FallbackNTP`, no-op, unchanged file/runtime mismatch без repeated restart, old persistent file update, refusal noncorporate per-link/DHCP sources |
| Ownership/failures | Marker required, symlink/nonregular refusal, bounded command output/cancellation, restart failure restores old/absent file, canceled apply still restores through bounded cleanup |
| OpenWrt | Fixed `system.ntp` UCI read, current/desired servers, enabled/use_dhcp, procd sysntpd state, synchronization unknown, permission/missing/malformed/bounded responses; повторный Apply всегда read-only unsupported, commit/restart отсутствуют |
| Runtime/CLI | Structured apply error, disabled no-op, diagnostic clone ownership; telemetry и heartbeat продолжаются при permission denied/service unavailable/not synchronized |

Tests используют temporary fixture filesystem и закрытые command runner
operations. Реальные NTP servers, clock changes и system service writes не
требуются. На Windows symlink test может skip при отсутствии privileges;
в Linux WSL `TestNTPManagedSymlinkRefused` фактически выполнен — PASS.

Targeted проверки:

- `go test -count=1 ./internal/config ./internal/platform/... ./internal/agent ./cmd/infrapulse-agent` — PASS.
- `go test -count=1 ./internal/platform/openwrt` и `go vet ./internal/platform/openwrt` — PASS.
- `go test -race -count=1 ./internal/platform/openwrt` — PASS.
- `go test ./internal/platform/linux` и `go test -race ./internal/platform/linux` — PASS.
- `go test -race -run '^TestLLDPChurnTimeoutAndMalformed$' -count=20 ./internal/driver/snmp` — PASS.
- Независимый review повторил `go test -count=1 ./internal/platform/... ./internal/config ./cmd/infrapulse-agent ./internal/agent` — PASS.

Промежуточный targeted запуск `go test -count=1 ./internal/config
./internal/platform/timesync ./internal/platform/openwrt` во время параллельного
написания provider завершился FAIL: Linux method `chronyLayout` ещё отсутствовал.
После завершения реализации общие targeted и full builds/tests — PASS.

### Полный regression после code freeze

Ignored script `.cache/collector-phase2-regression/run.ps1` выполнен полностью,
все checks exit=0. Logs — `.cache/collector-phase2-regression/status.log` и
отдельные logs; артефакты не входят в Git.

| Команда/проверка | Результат |
|---|---|
| `gofmt -w` всех Go sources, затем `gofmt -l` | PASS, список пуст |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS, без test cache |
| `go test -race -count=1 ./...` | PASS, без test cache |
| `go mod verify` | PASS |
| `go test -count=1 -tags=integration ./internal/queue ./internal/commands` | PASS |
| `scripts/build.ps1 -GoPath <SDK>` | PASS, все четыре executables |
| `scripts/build.ps1 -Cross -GoPath <SDK>` | PASS, прежняя target matrix |
| `bin/infrapulse-agent.exe validate-config --config <каждый configs/*.yaml>` | PASS, все 8 examples |
| Linux WSL test suites ниже | PASS, все 14 |
| Linux `time-sync-status` на реальной WSL среде | PASS: read-only detection выдал unavailable diagnostic |
| Relative Markdown file links и `git diff --check` | PASS |

Cross-build CGO0: Collector Linux amd64/arm64/arm (`GOARM=7`)/mips/mipsle
(`GOMIPS=hardfloat`); Mock Controller, Simulator, Mock Enrollment — amd64/arm64.
Windows development builds не означают Windows LocalPlatform support.

Linux binaries собирались на Windows, затем запускались в WSL `docker-desktop`:

```text
go test -c -tags=integration -o .cache/collector-phase2-regression/linux-<package>.test ./internal/<package>
wsl.exe -d docker-desktop --cd <package-directory> -- <test-binary> -test.v=true -test.count=1 -test.timeout=120s
```

Все suites: `platform/linux`, `logs`, `queue`, `commands`, `protocol/snmp`,
`driver/routeros`, `pki`, `platform/openwrt`, `agent`, `simulator`, `telephony`,
`transport/grpc`, `config`, `platform`. Parent command crash helper ожидаемо
skipped; actual child crash stages выполнены. Проверены реальные Linux
`/proc`/`/sys`, file I/O, log cursors, process crash/replay и loopback
gRPC/UDP/HTTPS/TLS fixtures. Real default-3s Linux Collector → Mock Controller
с durable ACK прошёл: интервалы 3.000627522s и 3.000208578s.

### Runtime NTP validation и пределы

Команда выполнялась только read-only:

```text
wsl.exe -d docker-desktop --cd <workspace> -- <workspace>/bin/infrapulse-agent-linux-amd64 time-sync-status --config configs/config.time-sync.example.yaml
```

Ответ: enabled=true, available=false, active=false, synchronized=null,
configured=false, changed=false; diagnostic сообщает, что `systemctl`
недоступен. PID1 в этой WSL среде — init, не systemd; chrony/timesyncd/ubus
не установлены. Поэтому actual successful chrony/timesyncd apply и NTP
synchronization achieved не проверены. Их configuration/status semantics
проверены fixtures и primary documentation, включая compiled-fallback nuance
systemd v257. Никакие службы, NTP configuration или часы development host
не менялись. OpenWrt UCI/procd — fixtures; firmware/hardware runtime не проверен.
Автоматический Linux apply требует существующий systemd provider и supported
safe layout; другие init/custom installations настраивает администратор.

Полный regression подтверждает сохранение existing SNMPv2c/v3, RouterOS,
LLDP/CDP/MNDP, SIP, file logs/audit, gRPC, durable outbox/ACK/replay, PKI,
command journal/snapshots/rollback и Simulator в пределах unit/fixture и
Linux runtime suites. Реальное оборудование и production Backend не участвовали.
Protobuf/generated sources, temporal model, dependencies и systemd unit
не изменены. Remote Syslog/Audit Receiver остаётся следующим самостоятельным
этапом; Phase 3 не начиналась. Git commit/push/merge/rebase не выполнялись.

## Collector Phase 3 — Remote Syslog / Audit Receiver (2026-10-08)

Следующие записи относятся к завершённой Phase 3, а ограничения выше являются
историческими результатами предыдущих этапов. Workspace — Windows/PowerShell,
Go1.27.1 из `C:\Users\kiril\sdk\go1.27.1\bin\go.exe`; build/cache artifacts
находятся в ignored `.cache/phase3-*`, portable compiler — ignored `.tools`.
Backend repository и production Backend/PostgreSQL не менялись и не участвовали.

### Baseline до изменений

| Реальная команда | Результат |
|---|---|
| `go build ./...` (`CGO_ENABLED=0`) | PASS |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS |
| `go test -race -count=1 ./...` (Windows amd64, CGO1, portable LLVM-MinGW) | PASS |
| `go mod verify` | PASS |
| `go test -count=1 -tags=integration ./internal/queue ./internal/commands` | PASS |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags=integration -o .cache/phase3-baseline/linux-agent.test ./internal/agent` | PASS |
| Аналогичная `go test -c -tags=integration` для `./internal/platform/linux` | PASS |
| WSL `linux-agent.test -test.v=true -test.count=1 -test.timeout=120s -test.run=TestLiveLinuxLogsTelemetrySIPAndRotation\|TestCollectorRealLinuxDefaultThreeSecondTelemetry` через `wsl.exe --exec` | PASS |
| WSL `linux-platform.test -test.v=true -test.count=1 -test.timeout=120s -test.run=TestRealLinuxCollector` через `wsl.exe --exec` | PASS |

Первый WSL test-harness вызов через shell неверно передал test flags/regex
(exit127/2); исправленная direct `--exec` invocation прошла. Это ошибка
запуска проверок, не отказ collector baseline. Commands/status/stdout
сохранены в `.cache/phase3-baseline`.

### Изученная архитектура и standards

До реализации изучены existing runtime/config/lifecycle, Target/SourceRef/time
model, file logs/parser/cursors, Agent.push/sanitizer, durable outbox/admission/
priorities, gRPC mapping/application ACK/replay, TLS/PKI и Simulator/Mock.
Local files проходят `logs.Pipeline → Parse/sanitize/cursor metadata → LogBatch
→ Emit=Agent.push → queue → gRPC`; cursor advances после whole-batch admission.
Receiver использует тот же нормализованный delivery path и не создаёт queue/DB.

Official sources: [RFC5424](https://www.rfc-editor.org/rfc/rfc5424),
[RFC5425](https://www.rfc-editor.org/rfc/rfc5425),
[RFC6587](https://www.rfc-editor.org/rfc/rfc6587),
[RFC3164](https://www.rfc-editor.org/rfc/rfc3164),
[rsyslog omfwd](https://docs.rsyslog.com/doc/configuration/modules/omfwd.html),
[templates](https://docs.rsyslog.com/doc/configuration/templates.html),
[audit-userspace plugin](https://github.com/linux-audit/audit-userspace/blob/master/audisp/plugins/syslog/audisp-syslog.c).
Mappings, setup и поддерживаемый RFC scope — [syslog-receiver](syslog-receiver.md).

### Настоящий isolated rsyslog sender и Mock E2E

WSL `docker-desktop` не содержал rsyslogd/auditd. Для проверки скачаны official
Alpine v3.23 x86_64 `.apk` и dependency closure, распакованные только в ignored
workspace `.cache/syslog-rsyslog/root`. Package installation, системные
service/config changes не выполнялись. Получен настоящий rsyslog8.2512.0 с
GnuTLS module; package SHA256 manifest сохранён в том же artifact directory.
CA/server/client certificates сгенерированы для isolated fixtures; private
keys/credentials не добавлены в Git.

| Реальная команда | Результат и scope |
|---|---|
| `rsyslogd -v` через unpacked runtime и LD_LIBRARY_PATH | PASS, 8.2512.0, Alpine musl x86_64 |
| `wsl -d docker-desktop -- sh /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/syslog-rsyslog/validate.sh` | PASS: `rsyslogd -M <runtime>/usr/lib/rsyslog -N1 -f <isolated>/syntax.conf`; repository sender example с заменой cert/spool paths и imfile input |
| `wsl -d docker-desktop -- sh /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/phase3-e2e/run.sh` | PASS: two validated Collector configs, actual TLS/TCP rsyslog forwarding, restart/replay и bounded SIGTERM shutdown |
| `& .cache/phase3-e2e/verify.ps1` | PASS: normalized Mock JSON, source/auth/time/raw/SD/audit/counts/admission/ACK verification |

Каждый transport phase передал 104 records: 103 через реальный rsyslog omfwd
(system1, burst100, structured-data1, audit-derived SYSCALL1) и отдельную
direct-socket RFC3164 fixture. Rsyslog source input — isolated local imtcp,
а не production `/dev/log`/audit dispatcher; output — configured RFC5424,
octet-counted TLS/mTLS либо explicit plain TCP. Все test sources имеют
`source=simulated` и simulated provenance даже при реальном transport software.
Проверено сохранение claimed HOSTNAME отдельно от logical source/mTLS SAN,
plain authenticated=false, managed Target=`syslog:linux-01`, carrier
syslog-tls/syslog-tcp, structured-data escapes/raw, timezone→UTC и отсутствие
year/timezone inference legacy. Audit fixture сохранила EventID1791450001.123:42,
UID/AUID/PID/syscall/success и независимый enclosing syslog timestamp.

TLS phase сначала работал при offline Mock: Receiver admitted104, durable queue
132 total messages (logs+служебные messages), 198397 payload bytes, file565248
bytes, dropped_logs0. Затем Collector остановлен и запущен с тем же state:
AgentID сохранился, SessionID сменился, Mock получил все104 log records и
отправил application ACK; snapshot queue1 (текущий heartbeat), dropped_logs0.
TCP phase дополнительно отправил malformed huge declared prefix: client
изолирован с safe `syslog_oversized_frame`, последующие104 records delivered/ACKed.
Все собственные sender/Collector/Mock processes остановлены SIGTERM в bounded
время без global process termination.

Первый rehearsal использовал слишком короткое ожидание и остановил Collector
до чтения всей transport backlog. Окончательный сценарий ждёт104 synced
admissions/Mock receipts и использует новый state. Этот rehearsal дополнительно
показывает, почему network write нельзя выдавать за durable log admission.
Он не является доказательством lossless sender→Collector delivery.

Artifacts: `.cache/phase3-e2e/mock-{tls,tcp}.jsonl`, `diagnostics-{offline,acked,tcp}.json`,
`verification.txt`, Collector/sender logs, configs/scripts и retained states.
Live auditd/audisp-syslog/kernel audit generation отсутствовали; canonical
audit-derived fixture не подменяет live dispatcher validation. Production
Backend/PostgreSQL ingest/REST search не проверялись. Это development Mock E2E.

### Unit/fixture, fuzz и полный regression

| Реальная команда | Результат |
|---|---|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -count=1 ./...` (default parallel packages, два запуска) | FAIL: Windows TempDir cleanup `ERROR_DIR_NOT_EMPTY` в existing queue/PKI tests; test bodies не сообщили assertion failure |
| `$env:GOFLAGS='-p=1'; go test -count=1 ./...` | PASS, полный suite без test cache; test logic ради обхода cleanup не изменена |
| `go test -race -count=1 ./...` (Windows amd64, CGO1, portable LLVM-MinGW) | PASS |
| `go test -race -count=1 ./internal/logs ./internal/syslogreceiver` | PASS, final scoped receiver/parser security/lifecycle tests |
| `go mod verify` | PASS |
| `go test -count=1 -tags=integration ./internal/queue ./internal/commands` | PASS |
| `go test ./internal/logs -run '^$' -fuzz '^FuzzSyslogParse$' -fuzztime=20s -parallel=2` | PASS, 247698 executions |
| `go test ./internal/logs -run '^$' -fuzz '^FuzzStructuredDataBoundary$' -fuzztime=20s -parallel=2` | PASS, 354457 executions |
| `go test ./internal/syslogreceiver -run '^$' -fuzz '^FuzzReadFrame$' -fuzztime=20s -parallel=2` | PASS, 275148 executions |

Зависимости/production CGO requirements не добавлены. Parser tests покрывают
RFC5424 fields/NILVALUE/SD escapes, invalid PRI/version/timestamp, timezone,
RFC3164 unknown year/timezone и malformed input, opaque message/raw, audit
SYSCALL/EXECVE/PATH/USER_AUTH/USER_LOGIN, malformed header/duplicate fields и
EventID без fractional seconds. Framing tests покрывают fragmented prefix/body,
multiple/many frames, embedded LF, zero/invalid/huge length/overflow, mismatch,
LF mode и disconnect mid-frame. Fuzz inputs bounded; panic/OOM/leak не наблюдались.

TLS fixtures генерируют свои certificates и проверяют verified sender/server,
missing/unknown/expired client certificate, invalid server trust, SAN mapping,
unknown/ambiguous identity, server-only TLS approved IPs и rejection LF on TLS.
Connection/per-logical-source/per-peer limits, handshake/read/idle deadlines,
startup rollback/bind failure/disabled/duplicate Run, cancellation/shutdown,
queue full retry/stable message ID/storage failure и malformed-client isolation
проверены bounded loopback fixtures. Security fixtures включают non-UTF8 raw
octets, sanitizer/private-key suppression и separate CA/cert-key validation.

Linux runtime regression использовал `CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go test -c -tags=integration -o <artifact> ./internal/<package>`, затем
`wsl.exe -d docker-desktop --cd <workspace>/internal/<package> --exec <artifact>
-test.v=true -test.count=1 -test.timeout=120s`. Все16 compiled suites и их
запуски PASS: platform/linux, logs, queue, commands, protocol/snmp, driver/snmp,
driver/routeros, pki, platform/openwrt, agent, simulator, telephony,
transport/grpc, config, platform, syslogreceiver. Exact commands/status/output
сохранены в `.cache/phase3-regression/{linux-commands,linux-status}.log`.
Linux proc/sys/file I/O реально выполнялся; RouterOS/SNMP/SIP/OpenWrt остаются
local protocol/schema fixtures, не испытанием внешнего hardware/PBX.

Existing default3s Linux telemetry integration: baseline intervals
3.000015139/2.999953559s, final3.000161366/3.000193787s. Phase 2 NTP config/status
tests и existing commands/journal/rollback/PKI/durable ACK/replay/Simulator прошли
в рамках этих suites; NTP synchronization живой системной службы и hardware
результаты этим не заявляются.

### Cross-build и fixture resource measurements

`scripts/build.ps1 -Cross` (проверенный Go SDK в PATH) — PASS,
`CGO_ENABLED=0`: Collector Linux amd64/arm64/arm/mips/mipsle, GOARM7 и
GOMIPS=hardfloat; Mock Controller/Simulator/Mock Enrollment Linux amd64/arm64.
Cross-build сохраняет portable Go matrix и не доказывает OpenWrt runtime.

`TestSustainedInputFixtureResourceSample` внутри Windows/Linux receiver suites
обработал1000 loopback fixture messages: Windows39.1899ms, Linux10.794742ms.
Linux sample TotalAlloc3818952B, heap2119192→2809024B, goroutines baseline4/peak
sample5, connection peak1. Emit в этом small burst sample был memory fixture;
он не измеряет synced durable storage, production throughput, network latency
или долговременный RAM budget. Сравнивать эти числа с Phase 2 как production
performance ratio нельзя: workload/host/cache differ.

Дополнительный sustained probe выполнен командой
`go run ./.cache/phase3-regression/loadprobe/main.go` с environment
`CGO_ENABLED=0`, `GOOS=windows`, `GOARCH=amd64`, тем же SDK и локальными
caches — PASS. Это isolated 5s loopback fixture:
четыре synthetic TCP sender с target 500 messages/s каждый, Receiver → existing
durable outbox 16 messages/128KiB → fixture consumer Pop/ACK с delay 2ms.
Дополнительной delivery queue, gRPC или настоящего Backend в probe нет.

Sender записал 9986 frames (1996.91/s); Receiver выполнил 6634 synced admissions,
без admission errors. Peak queue — 16/16 messages и 25440/131072 bytes (19.4%).
Existing priority eviction вытеснила 6616 logs: 2 consumer ACK подтвердили ещё
retained records, 1206 ACK относились к уже вытесненным records. 3352 sender
frames не достигли admission до отмены и закрытия sockets. Ни TCP write,
ни успешный admission не означают бессрочную retention при перегрузке.
Это измерение подтверждает resource bounds и documented loss semantics,
а не lossless delivery или пригодность этих limits для production workload.

Heap baseline 400008B → sampled peak 3417944B → после shutdown/GC 639328B;
goroutines 12 → sampled peak 14 → 1; peak connections 4, после shutdown 0;
shutdown 3ms. Cumulative TotalAlloc за probe 2211212272B: bounded live heap
не означает низкую стоимость allocations/GC. Высокая allocation churn,
synced admissions, eviction и flash wear требуют отдельного sizing/load test
на реальном storage и constrained Linux/OpenWrt hardware. Production throughput
и сравнимый Phase 2 RAM/CPU benchmark этим probe не подтверждены.
Source provenance simulated; consumer/provenance errors 0. Полные command/env,
JSON и 100ms samples — `.cache/phase3-regression/loadprobe/load-probe.log`.

`go run ./cmd/infrapulse-agent validate-config --config configs/config.syslog.example.yaml`
— PASS для текущего source CLI и disabled receiver example. Проверка старым
`bin/infrapulse-agent.exe` ранее завершилась invalid YAML из-за устаревшей
сборки; это не результат проверки текущего example. `gofmt -l` по всем
Go sources api/cmd/internal не вывел файлов.

Markdown local links изменённых documents и `git diff --check` — PASS.
Git сообщает только configured LF→CRLF warnings, без whitespace errors.

### Delivery/deployment ограничения Phase 3

Collector→Backend использует existing retained at-least-once durable outbox,
stable message IDs и application ACK/replay. TCP/TLS sender→Collector не имеет
per-record application ACK: disconnect/crash может дать loss/duplicates, а
finite sender/outbox capacity и retention/priority eviction допускают потери.
Нет claims exactly-once, lossless sender pipeline или production throughput.
UDP/RELP, Backend LogStore/search, отдельный audit protocol и новая logging DB
не реализованы. Certificate rotation listener выполняется через restart.

OpenWrt/Russian Linux/hardware/PBX production runtime не подтверждены.
Receiver disabled по умолчанию; OpenWrt durable receiver требует подходящего
persistent storage и measurement RAM/flash/concurrency на конкретном device.
Phase 4 Redfish автоматически не начиналась; будущему adapter нужны official
schemas/traversal/credentials/TLS/fixtures и bounded multi-resource inventory.
