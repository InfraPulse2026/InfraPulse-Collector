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
