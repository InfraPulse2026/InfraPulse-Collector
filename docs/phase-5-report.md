# Phase 5 — IPMI LANplus hardware monitoring: отчёт

Дата baseline: 2026-10-09. Продолжение реализации и review: 2026-10-10.
Read-only IPMI adapter добавлен в существующий Collector/Edge Gateway.
Финальные regression/race, serial integration, Linux/cross-build, wire/config
и bounded resource проверки прошли на development fixtures. Production BMC/vendor firmware,
production Backend/PostgreSQL и OpenWrt runtime не проверены.

## Baseline и рабочее дерево

Основной baseline — чистый `main`, `d456fea`, содержащий завершённую Phase 4.
SDK: `go1.27.1 windows/amd64`; local caches — `.cache/gopath`, `.cache/modules`,
`.cache/build`. Production builds используют `CGO_ENABLED=0`. Windows race
использует `CGO_ENABLED=1` и portable LLVM-MinGW compiler в ignored `.tools`.

| Выполненная baseline проверка | Результат |
|---|---|
| `gofmt -l` по api/cmd/internal | PASS, 226 files, пустой список |
| `go build ./...`, `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS, 13,928 s |
| `go test -race -count=1 ./...` | PASS, 44,158 s |
| `go mod verify` | PASS |
| `go test -count=1 -p=1 -tags=integration ./...` | FAIL, Windows TempDir cleanup existing Syslog test |
| Targeted повтор `TestReceiverDurableRestartReplayAndApplicationACK` | PASS; полный исходный FAIL сохранён |
| Native/cross build scripts, pinned protobuf scratch generation/SHA256 | PASS |
| `validate-config` по 10 examples | PASS |
| 17 compiled Linux amd64 suites в WSL `docker-desktop` | PASS |

Baseline logs и exact commands находятся в ignored `.cache/phase5-baseline/`,
краткий исходный отчёт — `report.md`. В baseline Linux проверены реальные
proc/sys/file reads; remote hardware tests использовали fixtures. External DMTF
server E2E был skipped: endpoint environment не задан. Live corporate NTP
synchronization не проверена, `time-sync-status` сообщил provider unavailable.

Matched baseline Linux amd64 CGO0 builds с `-trimpath -buildvcs=false`:
35 372 897 bytes unstripped и 23 855 264 bytes с `-ldflags='-s -w'`.
Baseline synthetic registry probe: 32 fake SNMP targets / 4 workers, goroutines
1→peak 6→1, active request peak 4, queue peak 32 messages / 29 337 bytes.
Это исходный bounded workload, не IPMI throughput.

Текущая ветка — `feat/phase-5-ipmi`, HEAD — `d456fea`. Используется исходный
workspace; отдельный linked worktree не создан. Реализация и документация остаются
unstaged. Commit, push, merge и переписывание истории не выполнялись.
Локальные заметки `AGENTS.md` дополнены текущим состоянием и ссылками на
Phase 5; этот файл исключён из Git и не входит в состав изменений для commit.

## Реализованная интеграция

`Agent.New → driver.Registry → remote.NewAdapters → bounded scheduler →
normalized models → Agent.push → Sanitizer → existing outbox → gRPC →
application ACK`.

`internal/driver/ipmi` реализует существующие `InventoryBatchReader`,
`HardwareReader` и typed probe. Hardware models Phase 4 переиспользованы;
второго runtime, inventory format, health engine или IPMI delivery service нет.
Capabilities: только `inventory.read`, `health.read`, `telemetry.read`.
SNMP/RouterOS fast telemetry и Redfish IDs сохраняют прежние contracts.

Target выбирается явно через `driver: ipmi`. Redfish failures не переключают
протокол и не запускают network scanning. Один вызов reader использует одну
LANplus session с последовательными requests. Inventory и health имеют
отдельные интервалы; target gate исключает overlap. SDR cache ограничен
records/bytes/TTL и сбрасывается при изменении repository/BMC identity.

Конфигурация находится в существующих `devices`/`credentials`; production
example — [config.ipmi.example.yaml](../configs/config.ipmi.example.yaml).
Credentials получает существующий Provider, literal secrets в YAML отсутствуют.
Address — host/IP; UDP port задаётся отдельно. Nonzero fast/topology intervals,
mutations и несовместимые protocol options отклоняются. Полные defaults/bounds —
[IPMI contract](ipmi.md).
`max_poll_bytes` не меньше FRU/SDR budgets и effective response limit
`min(remote.max_response_bytes, 4096)`, не больше 8 MiB.

## Библиотека и protocol security

Pinned dependency:
`github.com/bougou/go-ipmi v0.9.2-0.20261009051248-b3670df57943`,
Apache-2.0, minimum Go 1.24.0. Native LANplus client — pure Go; библиотека и
protocol-specific types находятся внутри adapter. `ipmitool`, FreeIPMI,
shell-out transport и in-band `/dev/ipmi` не используются runtime Collector.
Module graph содержит также formatting/terminal и optional local-client
dependencies библиотеки; их binary contribution измеряется отдельно.

Default suite 17: RAKP HMAC-SHA256, integrity HMAC-SHA256-128, AES-CBC-128.
Suite 3: HMAC-SHA1/HMAC-SHA1-96/AES-CBC-128, только explicit compatibility.
Cipher 0/null security, silent downgrade и IPMI v1.5 sessions запрещены.
Legacy capability preflight не активирует v1.5 session. USER default,
OPERATOR explicit; ADMINISTRATOR и automatic privilege escalation отсутствуют.
Отдельный/nonzero BMC `Kg` не поддерживается.

Receive gate дополняет отсутствующие upstream checks: exact algorithms,
session IDs, RAKP authentication, integrity HMAC, encrypted/authenticated flags,
sequence/replay, request correlation, AES framing/padding и IPMB checksums.
Library packet construction и crypto primitives переиспользованы. Console ID
получает `crypto/rand`; unrelated datagrams и retries имеют конечные bounds.
Guard также проверяет read/session command allowlist. Raw packets, credentials,
RAKP transcript и keys не входят в normalized output/diagnostics.

Read-only commands: Device ID, Chassis Status, SDR repository/info/reservation,
SDR read, FRU info/read, Sensor Reading. Session privilege/close и read
reservation относятся к protocol lifecycle. Power/reset, thresholds/LAN/users,
FRU/SDR write, SEL clear и firmware update отсутствуют. IPMI/RAKP допускает
offline password guessing; требуются management segmentation и отдельные
сильные monitoring credentials. [Security](security.md).

## Нормализация, identity и время

Get Device ID создаёт отдельный Manager/BMC с numeric manufacturer/product
evidence и проверенной firmware/IPMI version. Numeric ID не превращается в
неподтверждённое vendor name или serial физического сервера. Chassis status
сохраняет PowerState отдельно от fault/health; Off не является Critical.

Несколько FRU devices дают scoped Chassis/Board/Product areas с fields из
проверенных исходных bytes. Checksums, bounds, offsets, addressing и encodings
проверяются; повреждённая area сохраняет валидную sibling area. Chassis area
не получает invented manufacturer. CPU/DIMM/storage model/capacity/serial
не синтезируются из sensor state.

Full/Compact SDR и FRU Locator имеют bounded traversal. Analog conversion
учитывает raw signedness, M/B coefficients, exponents, linearization и units.
Temperature, RPM/percent, volts, amperes и watts передаются через existing
`Metric`; unsupported/unavailable readings не становятся numeric zero.
Threshold/discrete health использует masks и sensor/event types. Таблицы
42-2/42-3 IPMI E6 задают offsets; severity grouping — conservative Collector
policy. Presence/disabled/spare/progress остаются health unknown.

Observed IDs вычисляются из configured target/resource path. Sensor names,
array order и timestamps не участвуют в ID. BMC, Chassis и FRU subjects не
сливаются автоматически между собой или с Redfish. Shared IP не доказывает
Asset identity; BMC management relation не является physical topology.

SourceRef содержит `type=ipmi`, `protocol=ipmi`, target/resource scope и
provenance. Fixtures используют `simulated` в development. Shared Registry
сохраняет configured IPMI/Redfish provenance у online/error/offline/recovery
events, legacy wire `Event.Source` получает `simulated`. Normalized event
Observation сохраняет type/protocol/TargetID; legacy event protobuf не
переносит полную Observation metadata. Observed/collected
times — UTC; SourceTimestamp отсутствует без evidence. Corporate NTP
синхронизирует OS clock, IPMI clock correction не добавлена.

## Wire compatibility и доставка

IPMI-specific protobuf messages/RPC/fields не добавлены. Переиспользованы
hardware additions Phase 4 и existing SourceRef/identity/observation mapping.
Wire DeviceType остаётся UNKNOWN, resource role находится в HardwareInventory.
Generic hardware mapping сохраняет metadata также у metric-only samples.

Existing durable outbox, stable message IDs, replay/restart и negotiated
application ACK используются через прежний admission. Delivery остаётся
at-least-once для retained records; до admission сохранность не обещается,
exactly-once отсутствует. Production Backend должен persist hardware/source
fields и deduplicate; development Mock receipt не подтверждает PostgreSQL.
[Wire contract](protocol.md), [delivery](delivery.md).

## Слои проверки и обнаруженные ошибки

Reference BMC `go-ipmi` обеспечивает session/auth/core command behavior.
Test-only `ipmitest` добавляет независимые FRU/SDR byte fixtures, sensor handler
и response-framing shim для upstream requester-address/LUN defect. Это не
неизменённый server interoperability test и не production BMC firmware.
Production receive validation при этом не ослабляется.

Deterministic fixtures покрывают malformed/partial FRU/SDR, conversion/units,
threshold/discrete flags, duplicate names, cache invalidation и cancellation.
Session guard tests покрывают algorithms/RAKP/HMAC/session/sequence/correlation,
AES/IPMB framing, replay/discard/response bounds и rejected write commands.
Native LANplus проверен с USER; explicit OPERATOR policy покрыта config tests,
успешная native OPERATOR session отдельно не выполнялась.
Packet-loss test использует production guard и reference BMC: lost response
при retries 0 завершается bounded failure; retries 1 восстанавливает чтение
без изменения command/session sequence semantics библиотеки.

Agent E2E проверяет simulated hardware → existing durable outbox при offline
Controller → restart → replay тех же IDs → Mock ACK. Redfish/IPMI equivalent
sensor semantics проверяются через общие models. Матрица mixed-target isolation
проверяет shared loopback address, два scheduler workers, offline IPMI threshold,
продолжение Redfish polling, distinct targets/assets и Mock ACK.
`go test -count=3 -run '^TestAgentMixedRedfishIPMITargetIsolationAndACK$' -v
./internal/agent` — PASS targeted review. Lifecycle provenance test охватывает
default/real/simulated для обоих protocols и online/error/offline/recovery.
Окончательные full-suite результаты после code freeze приведены ниже.

Промежуточный Agent fixture запуск обнаружил отсутствие admitted observations
и equivalent sensor; после test fixture исправления targeted повтор прошёл.
Исходные logs сохранены в `.cache/phase5-final/initial/root-e2e.log`, повтор —
`root-e2e-fixed.log`. Это targeted результат, не итоговый полный suite PASS.
Review сверил discrete tables и исправил generic presence State с Enabled
на Present; event type 09 продолжает обозначать Enabled/Disabled.
Первое mixed E2E ожидание full event Observation после protobuf исправлено
в test: legacy wire переносит `Source=simulated`, без новых Phase 5 fields.

## Финальные quality gates и Linux/cross-build

После Go code freeze выполнены mandatory checks и затем отдельный extras
script; full Windows suites не запускались одновременно. Production builds
использовали CGO0; race — CGO1, Windows amd64 и portable LLVM-MinGW.

| Фактически выполненная финальная проверка | Результат |
|---|---|
| `gofmt -l` по api/cmd/internal, также LF scratch copy | PASS, пустой список |
| `go build ./...`, `go vet ./...` | PASS |
| `go test -count=1 ./...` | PASS, 14,805 s |
| `go test -race -count=1 ./...` | PASS, 44,899 s |
| `go mod verify` | PASS |
| `go test -count=1 -p=1 -tags=integration ./...` | PASS, 73,046 s, все packages |
| `scripts/build.ps1 -GoPath <verified SDK>` | PASS |
| `scripts/build.ps1 -Cross -GoPath <verified SDK>` | PASS |
| `validate-config` по всем 11 configs/*.yaml | PASS, включая IPMI |
| Pinned protoc 33.5 / protoc-gen-go 1.36.12 / protoc-gen-go-grpc 1.6.0 scratch generation | PASS, оба generated files SHA256 unchanged |
| 18 compiled Linux amd64 suites в WSL `docker-desktop` | PASS |
| Go source SHA256 verification после suites | PASS, 242 files, 0 mismatches |

Exact scripts/logs: ignored `.cache/phase5-final/{mandatory,extras}.ps1`,
`status.log`, `extras-status.log` и individual outputs. Исторический baseline
Windows cleanup FAIL сохранён; окончательный serial integration прошёл без
исключения tests. Default package-parallel Windows integration в финальной
Phase 5 отдельно не запускался и здесь не заявляется.

Compiled Linux suites: platform/linux, logs, queue, commands, protocol/snmp,
driver/snmp, driver/routeros, driver/redfish, driver/ipmi, pki, platform/openwrt,
agent, simulator, telephony, transport/grpc, config, platform, syslogreceiver.
Для каждого package выполнены `go test -c -tags=integration` с
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, затем binary в WSL с
`-test.v=true -test.count=1 -test.timeout=120s`.

Linux Agent suite подтвердил IPMI suite 17/USER inventory 2 devices / 4 metrics,
durable offline/restart/replay/ACK и 0 active BMC sessions. Mixed-target test и
existing Redfish/Syslog/SNMP/RouterOS/SIP checks прошли. Real Linux collector
интервалы telemetry — 3,000669536 s и 3,000129639 s. Remote BMC/network hardware
по-прежнему fixtures. External DMTF service E2E skipped: endpoint environment
не задан; service был проверен в Phase 4, но в этом этапе не запускался.
Linux `time-sync-status` прошёл с `available=false`, systemctl unavailable;
live corporate NTP synchronization не подтверждена.

Cross-build: Collector Linux amd64/arm64/arm/mips/mipsle, ARM `GOARM=7`,
MIPS hardfloat; Mock Controller, Simulator и Mock Enrollment Linux amd64/arm64.
Это compile validation; OpenWrt firmware/runtime не подтверждены.
Итоговые записи — [validation](validation.md).

## Измерения ресурсов и размера

Matched Linux amd64 `CGO_ENABLED=0`, `-trimpath -buildvcs=false` builds:

| Build | Phase 4 baseline bytes | Phase 5 bytes | Рост |
|---|---:|---:|---:|
| Unstripped | 35 372 897 | 38 257 990 | +2 885 093 / 8,16% |
| `-ldflags='-s -w'` | 23 855 264 | 25 948 320 | +2 093 056 / 8,77% |

Existing Redfish 50-poll probe: 243,2513 ms, retained heap delta +98 112 bytes,
goroutine delta 0, request peak 1, inventory JSON 19 750 bytes, largest sample
11 338 bytes / 9 metrics, resource plan 23. Baseline соответствующего probe:
244,3235 ms, retained heap +206 192 bytes. Эти одиночные samples подтверждают
выполненный workload, а не статистическое улучшение.

Повтор baseline-equivalent registry workload сохранил active peak 4,
goroutines 1→6→1 и queue peak 32 messages; peak bytes 31 000, retained heap
после shutdown 640 416 bytes, TotalAlloc work 2 783 864 bytes. Finite overload
дал bounded priority drops/evictions. `BenchmarkObserveTelemetry`: baseline
502,4 ns/op → final 445,2 ns/op; оба 1152 B/op, 8 allocs/op.

`TestResourceUsageBoundedNativePolling` — PASS: 50 health polls заняли
78,7598 ms; retained heap delta +156 920 bytes, TotalAlloc 24 355 264 bytes,
goroutine delta 0, socket peak 1. Inventory + 50 health создали 51 sessions,
закрыли 51 sockets, active BMC sessions 0. Writes/reads 749/749; maximum session
49 writes / 7105 bytes, maximum datagram 100 bytes. Inventory JSON 11 703 bytes,
maximum sample 6071 bytes / 4 metrics / 6 health items. Cache: 5 sensors,
1 FRU ID; configured poll budget 64 KiB.

| Фактически выполненный IPMI benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkHardwareCachedFixture` | 7782 | 11 383 | 107 |
| `BenchmarkHardwareNativeReferenceBMC` | 1 387 179 | 443 202 | 4343 |

`go tool nm -size` по matched unstripped binary: name-scoped sums для adapter
79 924 bytes, go-ipmi 750 760 bytes, новых formatting/terminal helpers
35 668 bytes. Baseline соответствующих symbols не содержал. Dependency tree
сохранился в `dependency-tree.log`, symbol summary и paired sizes —
`symbol-size-summary.json` / `binary-size-comparison.json`; performance commands
и результаты — `.cache/phase5-final/performance.ps1` / `performance-status.log`.
Symbol sums не атрибутируют весь binary cost; single-host fixture measurements
не являются production throughput или OpenWrt RAM/flash sizing.
Native UDP benchmark/resource probe выполняется в одном test process с
reference BMC и helper: heap/TotalAlloc/allocs включают работу обеих сторон,
а latency включает loopback/server execution. Эти значения не являются
изолированным production Collector RSS или latency реального BMC.

## Ограничения

- Настоящие BMC/vendor firmware, production Backend/PostgreSQL и live IPMI
  management deployment не проверены.
- OpenWrt runtime/RAM/flash и runtime других architectures не проверены;
  cross-build подтверждает только компиляцию.
- IPMI v1.5, nonzero Kg, IPMB bridging, OEM inventory/conversion, SOL,
  SEL ingestion и hardware mutations отсутствуют.
- FRU/SDR не обеспечивают detailed CPU/RAM/storage parity с Redfish.
- RAKP offline guessing и session expiry при недоступном logout остаются
  свойствами протокола/BMC; read-only role/session budget требуют hardware review.
- Auto fallback Redfish→IPMI и global multi-source Asset Resolution отсутствуют.

## Primary sources

[IPMI v2.0 Revision 1.1 E6](https://www.intel.com/content/dam/www/public/us/en/documents/product-specifications/ipmi-v2-rev1-1-spec-errata-6-markup.pdf),
[FRU v1.0 Revision 1.3](https://www.intel.com/content/dam/www/public/us/en/documents/specification-updates/ipmi-platform-mgt-fru-info-storage-def-v1-0-rev-1-3-spec-update.pdf),
[go-ipmi pinned commit](https://github.com/bougou/go-ipmi/commit/b3670df57943),
[client](https://bougou.github.io/go-ipmi/docs/client/),
[reference server](https://bougou.github.io/go-ipmi/docs/server/).
