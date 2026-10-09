# Collector Core: архитектурный freeze Phase 0–1

InfraPulse Collector / Edge Gateway обслуживает management/local network
и передаёт нормализованные наблюдения в NetOps Control Plane. Установка
Collector на каждый server не требуется. Existing Zabbix/Prometheus,
Ansible automation, Grafana visualization, TSDB, global Asset merge,
entity resolution, correlation, LLM и отчёты относятся к Backend.
Локальный Linux/OpenWrt monitoring остаётся standalone/fallback mode.

Этот freeze сохраняет рабочий P3: transport, identity/session, durable outbox,
ACK/replay, PKI, protocols, commands/journal/snapshots/rollback и Simulator.
Phase 3 добавляет opt-in Remote Syslog/Audit receiver через existing delivery.
Redfish, IPMI и time exchange не реализованы.

## Baseline и фактический прежний flow

Исходный runtime — `internal/agent.Agent`, composition root CLI
`cmd/infrapulse-agent`. `cmd/infrapulse-mock-controller`,
`cmd/infrapulse-mock-enrollment`, `cmd/infrapulse-simulator` — отдельные
development entrypoints. Config — strict YAML с protocol-specific options,
environment credential references и bounded limits.

До refactor реальный data flow выглядел так:

```text
Local Linux/OpenWrt Platform
  → Agent local scheduler → TelemetrySample/Device → rates → Agent.push
Configured devices
  → Agent.RemoteFactory → SNMP/RouterOS Driver → remote.Registry scheduler
  → TelemetrySample/InventoryUpdate/TopologyUpdate/events → Agent.push
Configured files
  → logs.Pipeline + one-worker scheduler → parsers/sanitizer/cursors
  → LogBatch → Agent.push
SIP endpoints
  → telephony.Collector + bounded scheduler → TelephonyHealth/events → Agent.push
Agent.push → sanitizer → immutable memory/durable queue
  → one gRPC sender → Controller application ACK → durable deletion
Controller typed command → validation/policy/protection → journal/snapshot
  → serialized driver action → readback → persistent result → same outbox
Simulator Generator → normalized envelopes → Runner → same gRPC contract
```

SNMP driver нормализует SYSTEM/ENTITY/IF/IF-X/LLDP через существующий
`internal/protocol/snmp`; RouterOS driver — фиксированные HTTPS REST reads
и typed interface writes через `internal/protocol/routeros`. Linux Platform
читает `/proc`/`/sys`, OpenWrt дополняет его typed ubus inventory. Wire mapping
сосредоточен в `internal/transport/grpc`, а core не знает OID/protobuf.
Protocol implementations не переписаны.

До изменения sources на Windows Go 1.27.1 прошли:

```text
gofmt -l <все Go sources api/cmd/internal>          PASS, список пуст
go build ./...                                   PASS
go vet ./...                                     PASS
go test ./...                                    PASS
go test -count=1 ./...                            PASS без test cache
go test -race ./...                               PASS
go test -race -count=1 ./...                      PASS без test cache
go mod verify                                    PASS
go test -count=1 -tags=integration ./internal/queue ./internal/commands  PASS
scripts/build.ps1                                PASS
scripts/build.ps1 -Cross                         PASS
```

Race использовал portable LLVM-MinGW из ignored `.tools`. GNU make в среде
отсутствует; Makefile изучен, его Go/build/race/cross operations выполнены
прямыми командами и существующим PowerShell script. Pinned protobuf generated
во временный output, SHA-256 обоих generated files совпал с tracked sources;
tracked output не изменялся.

Cross-build baseline: Agent Linux amd64/arm64/arm (`GOARM=7`)/mips/mipsle
(`GOMIPS=hardfloat`); Mock Controller, Simulator, Mock Enrollment — amd64/arm64.
В WSL `docker-desktop` прошли полные Linux suites: platform/linux с integration,
logs, queue с integration, commands с integration, protocol/snmp,
driver/routeros, pki, platform/openwrt, agent с integration, simulator,
telephony и transport/grpc. Проверены настоящие Linux file/proc/sys I/O
и loopback protocol fixtures; OpenWrt hardware, внешние PBX/switches и
production Backend этим не подтверждены. Backend repository в доступной
workspace отсутствует; Collector → Backend → PostgreSQL → REST baseline
не выполнялся. Ошибок baseline tests не было.

Baseline artifacts сохранены в ignored `.cache/collector-phase1-baseline`.
Результаты последующей regression находятся в [validation](validation.md).

## Решение по runtime и packages

`type Collector = Agent` обозначает тот же runtime. Identity, session lifecycle,
scheduler pools, delivery, command execution и diagnostics переиспользованы.
Mass rename module path/CLI/`Agent*` protobuf messages отсутствует.

Domain types находятся в существующем `internal/model`; YAML projections —
в `config`, `logs`, `telephony`; adapter registry — в `internal/driver`;
source binding — на границе existing orchestration. Второй config/runtime
или динамический plugin loader не создаётся.

`driver.Adapter` содержит только `Capabilities` и `Close`. Optional
`ProbeReader`, `InventoryReader`, `TelemetryReader`, `TopologyReader`
объявляют реально реализованные reads. Existing `Driver` объединяет их
для SNMP/RouterOS; optional mutation interfaces остаются отдельно.
`driver.Registry[T]` хранит compile-time constructors. `RemoteAdapterFactory`
регистрирует существующие SNMP/RouterOS в composition root.
`remote.NewAdapters` и compatibility wrapper `remote.New` используют один
Registry и прежний controlled scheduler. Missing reader не создаёт fake data.

## Target и observed Device

`model.Target` — configured polling/connection или обслуживаемый source:
ID, type, protocol, endpoint, credential reference, enabled, polling,
location и optional labels. `TargetPolling` содержит существующие intervals
и timeout. Нулевое поле в domain projection может обозначать отсутствующую
job group; отрицательные durations недопустимы. Это не меняет existing YAML:
нулевой remote interval по-прежнему заменяется прежним default через
`config.Device.Target(defaults)`. Реально поддерживаемые readers планируются
существующим bounded registry. Protocol-specific options и security validation
остаются у существующей config.

| Existing configuration | Internal Target ID | Observed identity |
|---|---|---|
| `devices[].id: switch-01` | `remote:switch-01` | Existing SNMP/RouterOS `Device.ID=switch-01` |
| Local host | `collector-local:<persistent UUID>` | `Device.ID=local:<persistent UUID>` |
| `telephony.endpoints[].id: pbx-01` | `sip:pbx-01` | Existing health `TargetID=pbx-01`, отдельный SIP endpoint |
| `logs.sources[].id: system` | `logs:system` | Host Device ID и original source ID на records |

`config.Device.Target(defaults)`, `telephony.Endpoint.Target()` и
`logs.Source.Target(interval, timeout)` проецируют existing YAML. Новая
competing YAML schema не введена. Configured entries сохраняют enabled
поведение; Local Target отражает `local.enabled`.

IDs и credential references остаются bounded opaque strings: compatibility
не меняет их trim/case/whitespace semantics, control characters запрещены.
Carrier Protocol ограничен отдельно; network endpoint validation остаётся
protocol-specific. Generic Endpoint допускает existing file paths до 4096 bytes.
Standalone `probe-device` получает source-bound Device и identity evidence
через тот же registered driver; CollectorID остаётся unavailable, поскольку
CLI read не загружает runtime identity и не открывает session.

Target ID не выводится из `Device.ID`, hostname, IP или hardware evidence.
Explicit Device ID от adapter сохраняется; legacy fallback к configured ID
применяется только к отсутствующему observed ID. Target не является окончательным
Backend Asset и не обещает один Asset на connection. Legacy command route
не перенаправляется на другое observed устройство: ambiguous/mismatched
identity запрещает mutation. CredentialRef скрыт из JSON (`json:"-"`),
credentials отсутствуют в SourceRef/observations.

## SourceRef и provenance

`SourceRef` содержит Type, CollectorID, TargetID, Protocol, InstanceID и
Provenance. Type обозначает источник факта; Protocol — carrier. LLDP fact
может иметь `type=lldp, protocol=snmp`, RouterOS neighbor — `type=cdp,
protocol=routeros`. Vendor Cisco не является SNMP source.

Vocabulary: local/snmp/routeros/lldp/cdp/mndp/sip/syslog/auditd/redfish/ipmi.
Последние два значения подготавливают domain model и не включают capabilities.
Provenance: real/simulated/manual/external. Existing `source=simulated`
сохраняется в legacy fields и устанавливает simulated provenance. Unknown
manual/external source нельзя передать полным SourceRef через старый protobuf.

`ObservationMetadata` объединяет SourceRef и ObservationTime. Existing
payloads остаются типизированными: Device, InventoryUpdate, TelemetrySample,
TopologyUpdate/Neighbor, LogBatch/LogRecord, TelephonyUpdate/Health и Event.
Общего giant payload struct нет. `ObserveEnvelope` адаптирует их в одном
месте перед delivery. Existing metadata копируется с optional pointer ownership;
binding заполняет отсутствующий CollectorID, не переписывая source clocks
или уже заданный Target ID. Authoritative legacy `source=simulated` сохраняет
simulated provenance даже при inconsistent incoming metadata. JSON immutable
outbox сохраняет новые поля.

## IdentityEvidence

`IdentityEvidence` содержит Type, Value, optional RawValue/Subtype, SourceRef,
Confidence и optional SubjectID. SubjectID относит факт к interface/component,
не является Asset ID или Target ID. Evidence quality high/medium/low/unresolved
не гарантирует глобальную уникальность.

Используются уже доступные serial, chassis ID, hostname/sysName, sysObjectID,
manufacturer/model, literal management IP, component serial/model/manufacturer,
interface MAC и topology chassis/name. Hardware UUID предусмотрен типом;
значение не генерируется при отсутствии реального source. Configured Name
и VendorHint не становятся hardware evidence. Hostname/management IP имеют
low confidence, hostname не выдаётся за уникальную hardware identity.

Raw chassis bytes и subtype сохраняются без потери, Value для binary evidence
кодируется base64. Unknown subtype не превращается в LLDP MAC/UUID. Для
RouterOS neighbor без подтверждённого LLDP chassis subtype используется
generic chassis evidence. Global Asset merge выполняет Backend.

## Time domain

`ObservationTime` различает независимые clocks:

| Поле | Точная semantics |
|---|---|
| SourceTimestamp | Optional timestamp, сообщённый source/device/log. Его отсутствие — unknown; он не становится автоматически observed time |
| ObservedAt | Оценка Collector, к какому моменту относится наблюдаемое состояние |
| CollectedAt | Когда Collector завершил или зафиксировал collection |
| ClockOffset | Optional измеренная разница `source clock - Collector clock`. Положительное значение означает, что source впереди |
| Uncertainty | Optional неотрицательная граница ± вокруг оценки времени; nil означает неизвестную точность |
| Quality | unknown/synced/acceptable/degraded/unsynced; это качество часов, не provenance/vendor |

NewObservation адаптирует legacy times в UTC. Если legacy observed time отсутствует,
используется время фиксации collection. Для существующих синхронных reads времена
могут совпадать. Optional source timestamp копируется отдельно; из разницы
с collected time offset автоматически не вычисляется. Unknown offset и uncertainty
остаются nil; explicit measured zero возможен только с pointer presence.
Default quality — unknown: наличие local timestamp не доказывает synchronization.
Go `time.Duration` сохраняет internal numeric nanoseconds в JSON.

`received_at` ставит Backend при приёме; поля в Collector нет. `sent_at` относится
к попытке отправки. Outbox delay/replay не меняет исходные observation times.
Модель не требует монотонного порядка wall-clock timestamps: корректировка часов
может сдвигать их назад. Для polling/rates сохраняется существующая логика.

Production deployment предполагает корпоративную NTP hierarchy для Backend,
Collector hosts и оборудования, где синхронизация поддерживается. Phase 2
добавляет opt-in `time_sync` в существующий YAML и отдельный administrative
`apply-time-sync` для стандартных Linux NTP clients. Обычный `run` читает status
без записи system config и продолжает monitoring при unavailable/unsynchronized
состоянии. OpenWrt setup остаётся administrator-owned UCI configuration.
Подробности — [time-sync](time-sync.md).

NTP daemon/server и manual timestamp correction отсутствуют. Часы дисциплинирует
ОС; `observed_at`/`collected_at` используют прежнее системное время, исходный
source timestamp сохраняется отдельно. TimeSyncRequest/Response,
four-timestamp exchange, RTT/offset estimation и Backend skew monitoring
не реализуются в Phase 2. Unknown clock quality/offset/uncertainty в observation
model не заменяются результатом проверки NTP-службы.

## Existing adapters после adaptation

```text
Local Linux/OpenWrt Target → existing Platform + local scheduler
  → typed model → ObserveEnvelope(local SourceRef) → existing outbox → gRPC
Remote Target → registered SNMP/RouterOS Adapter → optional readers
  → existing bounded Registry/scheduler → typed model + identity evidence
  → ObserveEnvelope(target SourceRef) → existing outbox → gRPC
LLDP/CDP/MNDP → existing mapping → Neighbor raw IDs/subtypes
  → discovery SourceRef + identity evidence → same topology delivery
File Target → existing Pipeline/cursors/sanitizer → LogBatch/records
  → syslog/auditd/file SourceRef + optional original source time → same delivery
SIP Target → existing telephony Collector + bounded pool
  → Health/events + SIP SourceRef → same delivery
Simulator → existing deterministic generator
  → same normalized observation model, simulated provenance → existing Runner
```

Scheduler intervals/default fast 3s, cancellation/timeouts, bounded workers,
per-target exclusion, failure isolation и log/SIP pool isolation сохраняются.
Projection Source/Target не создаёт ticker/goroutine на abstraction.
Remote counter baselines принадлежат target entry: одинаковый observed Device ID
на разных connections не смешивает counters. Смена observed primary Device
сбрасывает прежние rates/interface/online history; topology history включает
observed local entity в key. Bounded retention остаётся прежним.
Typed commands продолжают использовать journal, policy/capability/protection,
canonical resource resolution, persistent snapshots, readback и same outbox.
Arbitrary shell/Ansible execution отсутствует.

## Wire compatibility и оставшиеся границы

Hello/Heartbeat/Telemetry/Inventory/Topology/Logs/Telephony/CommandResult,
protobuf field numbers, Agent/session identity, stable message IDs и ACK negotiation
сохранены. Legacy source/timestamps и existing inventory/topology evidence передаются
как раньше. Phase 3 добавляет optional SourceRef/ObservationTime на logs/records
и SyslogMetadata; normalized IdentityEvidence и metadata остальных payloads
остаются internal. Additive protobuf scope описан в [protocol](protocol.md).
Wire metadata нельзя молча упаковать в vendor,
hostname или generic string.

Remote Syslog/Audit Receiver Phase 3 использует bounded input lifecycle/admission,
configured source/Target attribution и existing LogBatch/outbox. Existing file
reader остаётся file pipeline. [Настройка receiver](syslog-receiver.md).
Collector ↔ Backend offset estimation отложен; exchange и Backend implementation
отсутствуют. Additive logs metadata не включает metadata остальных observations.
Backend received_at остаётся server-owned. Phase 4 автоматически не запускается.

Redfish/IPMI можно подключить через зарегистрированный Adapter и небольшие readers,
но protocol implementations, credentials/security options, реальные schemas/mappings
и fixtures отсутствуют. Current InventoryReader возвращает один Device; для
Redfish ComputerSystem/Chassis/Manager нужен additive bounded batch reader,
не переименование Target в Asset. Remote Syslog/Audit receiver реализован отдельным
bounded input component в том же runtime; file reader не стал network receiver.
Backend-managed target reconciliation/configuration
также не реализован. Эти gaps не включают backend-owned monitoring integrations.

Baseline resource probe: 32 fake targets, 4 workers, fast 25ms, fake I/O 2ms,
2s run, memory queue 32/256KiB без consumer. Jobs 32; collect peak 4/end 0;
goroutines 1→6→1; queue peak 32 messages/12541 bytes. Forced-GC Alloc до/registered/
после 314496/401208/522592 bytes; workload TotalAlloc 2148808 bytes. Это small
synthetic workload, не runtime budget OpenWrt hardware. Durable test: 480 reconnect
resets/320 admissions+ACK, max file 65536 bytes, goroutines 2→2. Сравнение после
refactor и остальные фактические checks — в [validation](validation.md).
