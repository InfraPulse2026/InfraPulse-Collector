# Протокол Collector ↔ Controller

Действующий контракт находится в [agent.proto](../api/proto/infrapulse/v1/agent.proto).
Collector сохраняет исторические wire names `AgentHello`, `AgentEnvelope`,
`AgentService`, `agent_id` и `session_id`. Phase 1 изменяет внутренние domain
models, не переименовывает RPC/messages и не изменяет номера protobuf fields.
Сгенерированные Go-файлы находятся в `api/gen/infrapulse/v1`: их создали
официальные protoc 33.5, protoc-gen-go v1.36.12 и protoc-gen-go-grpc v1.6.0.
Для обычной сборки protoc не требуется. Команды повторной генерации и закреплённые
версии описаны в [api/proto/README.md](../api/proto/README.md).

## Соединение и handshake

Agent инициирует исходящий HTTP/2 gRPC stream:

```protobuf
package infrapulse.v1;
service AgentService {
  rpc Connect(stream AgentEnvelope) returns (stream ControllerEnvelope);
}
```

Полное имя RPC: `/infrapulse.v1.AgentService/Connect`. Agent не открывает
входящий management port. После открытия stream он отправляет `AgentHello`;
Controller отвечает `HelloAck{accepted: true, protocol_version: 1}`. P3 Agent
запрашивает `AgentHello.application_ack=8`; peer подтверждает
`HelloAck.application_ack=3`. Отсутствующее поле означает legacy peer. До
accepted HelloAck Agent не отправляет сообщения из outbox; после него legacy
поведение зависит от queue mode и явной ACK fallback policy. Отказ, другая
версия, команда вместо acknowledgement или истечение timeout закрывают session.

Каждая session использует один последовательный sender и один receiver. Эти
две операции могут работать одновременно; два конкурентных `Send` или два
`Receive` на одном stream не поддерживаются. Transport не создаёт goroutine на
каждую операцию. Context, переданный в `Connect`, управляет всей жизнью session,
поэтому после handshake он остаётся активным. `Close` отменяет этот context,
закрывает gRPC connection и разблокирует сетевые операции; повторный `Close`
безопасен. Core отдельно ограничивает handshake и отправку watchdog timeout.

При разрыве Agent закрывает старую session, делает exponential backoff с jitter
и открывает новую. Успешный handshake сбрасывает backoff. Persistent `agent_id`
сохраняется после restart; `session_id` относится к запуску процесса и не
меняется при reconnect в этом запуске.

## AgentEnvelope v1

| Поле | Номер | Тип |
|---|---:|---|
| message_id | 1 | string, стабильный UUID сообщения |
| agent_id | 2 | string, persistent UUID Agent |
| session_id | 3 | string, UUID запуска |
| sequence | 4 | uint64, порядковый номер попытки отправки в этом запуске |
| sent_at | 5 | google.protobuf.Timestamp |
| hello | 10 | AgentHello, oneof payload |
| heartbeat | 11 | Heartbeat, oneof payload |
| telemetry | 12 | TelemetryBatch, oneof payload |
| inventory | 13 | InventoryUpdate, oneof payload |
| topology | 14 | TopologyUpdate, oneof payload |
| logs | 15 | LogBatch, oneof payload |
| events | 16 | EventBatch, oneof payload |
| telephony | 17 | TelephonyUpdate, oneof payload |
| command_result | 18 | CommandResult, oneof payload |
| error | 20 | ErrorReport, oneof payload |

В одном envelope ровно один payload. Нормализованная Go-модель дополнительно
проверяется перед encoding: отсутствие и несколько payload являются ошибкой.
Невалидные timestamps, неизвестные enums, nil-элементы и бесконечные/NaN
метрики отклоняются. Базовый transport использует 4 MiB при незаданном лимите;
Agent передаёт `queue.max_message_bytes` (пример/default config: 1 MiB),
Mock Controller использует 1 MiB. Допустимый configurable диапазон
transport — 512 bytes–4 MiB; ограничение действует в обе стороны.

Номер 14 использован для `TopologyUpdate` согласно исходному protocol plan.
P2 использует ранее незанятые 15 и 17 для logs и telephony; 19 остаётся свободным.
Дополнение не переименовывает и не перенумеровывает существующие поля.
Удалённые в будущем поля нужно явно объявлять `reserved`;
занятый номер никогда не используется для другого смысла.

## Payloads

`AgentHello`: agent/session ID, name, build metadata, время запуска, локальный
`Device`, версия протокола. `Device` содержит identity/inventory, uptime,
boot ID, last seen, явно заданное физическое расположение и granular
capabilities. Неизвестные необязательные inventory fields остаются пустыми.
`Capability.supported=false` вместе с `reason` явно означает отсутствие
операции; наличие типа команды в protobuf само по себе не включает capability.

Remote additions `Device.system_description=24`, `sys_object_id=25`,
`system_location=26`, `system_contact=27`, `vendor_hint=28`,
`vendor_evidence=29`, `components=30`, `uptime_available=31`, `uptime_source=32`
сохраняют подтверждённые SYSTEM/ENTITY/API сведения. `Evidence` содержит source,
value и confidence (`high`, `medium`, `low`, `unresolved`). Configured hint
не является подтверждением vendor. Components сохраняют собственные ID/parent
и class/model/serial/revisions. PhysicalLocation не выводится из system location.
Credentials и credential references отсутствуют в Device/wire models.

`Heartbeat`: timestamp, uptime, build metadata, health, remote device count,
число и суммарный размер queued messages, dropped telemetry/other/log counters.
Heartbeat отделён от telemetry. Health отражает ошибки collectors;
это не результат AI или полноценная оценка здоровья инфраструктуры.

`TelemetryBatch.samples` — repeated `TelemetrySample`. Sample содержит
device ID, observed/collected times, source, boot ID, uptime, CPU, memory,
interfaces и небольшие типизированные `Metric{name,value,unit}`. Linux использует
typed CPU/memory structures; RouterOS resource CPU/RAM values передаются metrics,
без выдуманных Linux load/swap полей. Agent отправляет один sample на
envelope. Нормализованный `DecodeEnvelope` Mock Controller принимает ровно один
sample: multi-sample batch отклоняется, а не сокращается до первого. Будущий
Backend должен обрабатывать полный repeated batch непосредственно из protobuf.

CPU usage, memory pressure и interface speed — optional scalars. Измеренный
ноль сохраняет presence и отличается от недоступного значения. Message presence
для interface rates отличает рассчитанные нулевые rates от первого sample или
counter reset, когда rate ещё не определён. Counters — unsigned 64-bit
накопительные значения; rates — double в единицах за секунду. Адреса интерфейсов
передаются строками CIDR. Synthetic source допустим только как `simulated` в
development simulator/tests.

`Interface` дополнен `description=12`, `alias=13`, optional `type=14`,
`metadata=15`, `counter_metadata=16`. Metadata различает отсутствие
index/MTU/admin status. Counter metadata содержит mask доступности каждого
counter, width (`32`, `64`, `0` — unknown/aggregate) и optional discontinuity
ticks. Число с availability=false нельзя считать измеренным нулём.
Nil metadata сохраняет P0 contract: все counters доступны и 64-bit.
`InterfaceRates.available=9` позволяет передать частично определённые rates;
nil mask сохраняет P0 semantics всех rates. Unknown-width aggregate не получает
derived rate. `TelemetrySample.uptime_available=11` и `uptime_source=12`
различают реальный uptime и его отсутствие; у P0 payload эти поля отсутствуют.
SNMP `sysUpTime` относится к management subsystem, не обязательно к ОС.

`InventoryUpdate` содержит observed timestamp и список `Device`; `EventBatch`
содержит события со стабильным ID, type, device ID, timestamp, severity, message
и source. `ErrorReport` содержит component, code, message и timestamp. Секреты
очищаются в pipeline до помещения сообщения в outbox.

Времена событий представлены `google.protobuf.Timestamp` и преобразуются в UTC.
`observed_at` — оценка Collector, к какому моменту относится состояние,
`collected_at` — завершение/фиксация сбора на Collector,
`sent_at` — текущая попытка отправки. Задержка в outbox не изменяет исходные
времена. `BuildInfo.build_time` является строковой build metadata из ldflags,
а не timestamp наблюдения инфраструктуры.

## Внутренняя observation model и wire compatibility

`model.ObservationMetadata` содержит SourceRef и ObservationTime; payloads
сохраняют существующие `source` и timestamp fields. Дополнительная metadata
сохраняется во внутренней модели, JSON outbox/diagnostics и typed optional wire
fields. Phase 3 ввела observation для LogRecord/LogBatch, Phase 4 — equipment
metadata. Phase 6 закрывает оставшиеся gaps inventory/telemetry, topology,
Events, local OpenWrt и SIP через существующие messages. Decoder старого peer
не восстанавливает неизвестный Target ID, clock offset или provenance
manual/external и не выдаёт guessed значения за измерения.

| Domain information | Действующий wire contract |
|---|---|
| Collector identity/session | `AgentEnvelope.agent_id/session_id`, `AgentHello` |
| Observed device | Payload `device_id` и `Device.id`; это не общее поле configured Target |
| SIP target | `TelephonyHealth.target_id` |
| File log source | `LogRecord.source_id` |
| Legacy source и simulated marker | Существующие `source` strings; `simulated` сохраняется |
| Observation/collection time | Оба поля у telemetry/topology; один observed timestamp у inventory/telephony, timestamps событий/log records |
| Source-reported log time | Optional `LogRecord.original_timestamp` |
| Raw topology identity | Chassis/port bytes/subtypes, protocol и evidence в `Neighbor` |
| Full SourceRef | Optional observation у InventoryUpdate, Device, InventoryComponent, TelemetrySample, Metric, ComponentHealth, Event, TopologyUpdate/Neighbor, LogRecord/LogBatch, Service, NetworkState, FirewallSnapshot, TelephonyUpdate/Health |
| Normalized IdentityEvidence | Typed Device/Telemetry identity lists и Neighbor.identity_evidence; subject/source/origin/time/normalization сохраняются |
| Clock model | ObservationTime source/observed/collected timestamps, optional offset/uncertainty с presence и clock quality; факт хранит собственное время |

`source_timestamp` означает время, сообщённое source, и не заменяет автоматически
`observed_at`. Unknown offset/uncertainty — absence, не 0. `received_at`
принадлежит Backend и отсутствует в модели Collector. `sent_at` относится к
попытке отправки, не к observation или Backend receipt.

Legacy `Device.identity_evidence=41` и `TelemetrySample.identity_evidence=15`
содержат только usable Phase 5 types с normalization absent/canonical. Additive
`Device.extended_identity_evidence=42` и
`TelemetrySample.extended_identity_evidence=16` содержат новые `ip`/
`interface_name` types и `invalid`/`rejected` diagnostics. Это защищает old strict
decoder от unknown types и от matching diagnostic placeholders, когда peer
игнорирует новый normalization field. Backend Phase 6 сохраняет оба списка;
domain decoder соединяет legacy, затем extended без merge conflicting facts.
`Neighbor.identity_evidence=20` целиком additive Phase 6, partition ему не нужен.

Существующие field numbers сохранены; Duration/Timestamp presence различает
unknown и measured zero. Clock offset estimation требует отдельно согласованного
Collector ↔ Backend exchange и не реализован этой фазой. Точные temporal
semantics и знак offset — в [Collector Core](collector-core.md), полный field
mapping и optional/required rules — в
[Backend identity/topology contract](backend-identity-topology-contract.md).

## TopologyUpdate

`AgentEnvelope.topology=14` содержит один snapshot наблюдаемого устройства:

| Поле | Номер | Смысл |
|---|---:|---|
| device_id | 1 | Observed Device ID; у существующих SNMP/RouterOS совпадает с configured ID |
| observed_at / collected_at | 2 / 3 | UTC observation и collection time |
| source | 4 | Фактический collector source |
| completeness | 5 | Enum COMPLETE/PARTIAL/FAILED |
| ttl_seconds | 6 | Collector expiry assumption, не advertised LLDP TTL |
| expiry_assumption | 7 | Описание предположения срока актуальности |
| observations | 8 | Repeated Neighbor |
| observation | 9 | Optional SourceRef/ObservationTime snapshot |

Complete empty snapshot допустим и отличается от failed snapshot. Partial/failed
не означает удаления прежних links. Backend выполняет reconciliation; Agent
не объединяет неизвестные remote devices по hostname и не передаёт глобальный
graph как доказанную истину.

`Neighbor` сохраняет local device/interface ID/name, raw local port number/bytes
и subtype; raw remote chassis/port bytes и subtypes; remote system name/port
description; discovery protocol (`LLDP`, `CDP`, `MNDP`, `UNKNOWN`), source,
observed/optional expiry timestamps, evidence и confidence. Subtype 0 означает
unknown, LLDP subtypes 1–7 сохраняют исходный смысл. LocalPortNumber не ifIndex.
Отсутствующий LocalInterfaceID остаётся unresolved. RouterOS neighbor MAC имеет
unknown chassis subtype, поскольку API не гарантирует, что это LLDP chassis ID.
ARP/ND/FDB не кодируются как physical discovery protocol.

Phase 6 additive `Neighbor` fields:

| Поля | Номера | Смысл |
|---|---|---|
| observation / identity_evidence | 19 / 20 | Scoped source/time и remote identity facts |
| observation_id / kind | 21 / 22 | Stable source-scoped ID; `observed_l2_neighbor` direction |
| local_interface_index / mac / description / alias | 23 / 24 / 25 / 26 | Optional actual interface evidence; ifIndex не глобальный hardware key |
| remote_management_addresses | 27 | Bounded family/address/raw octets + optional interface numbering |
| remote_system_description / platform / model / software_version | 28 / 29 / 30 / 31 | Advertised optional properties с protocol origin |
| remote_capabilities_supported / enabled | 32 / 33 | Bounded LLDP capability names |
| age_seconds | 34 | Optional reported age, не protocol TTL |

Направление — `Local Interface -> Advertised Remote Endpoint`; отсутствующие
remote Asset/port допустимы. Две стороны дают две independent observations,
не verified bidirectional cable. Observation ID не включает current timestamp
и не меняет durable message ID. Received TTL/explicit withdrawal текущие readers
не предоставляют; expiry остаётся указанной Collector assumption. Полные bounds
и protocol mappings — [identity/topology](identity-topology.md).

Mapping проверяет enums, timestamps, размер raw IDs/evidence/observation lists
и наличие metadata masks; unknown width numbers отклоняются до integer
conversion. Номера и типы существующих P0 fields сохранены.

## ControllerEnvelope и typed commands

`ControllerEnvelope` содержит oneof: `hello_ack=1`, `command=2` или
`message_ack=3`. `HelloAck` содержит `accepted=1`, `protocol_version=2`,
`application_ack=3`; `MessageACK` — `message_id=1`.

`Command` содержит `id=1`, `target_device_id=2`, `requested_at=3`,
`timeout=4` (`google.protobuf.Duration`), `initiator=5`, `idempotency_key=6`
и ровно одну typed action:

- `interface=10`: interface ID и enum `ENABLE`/`DISABLE`;
- `service=11`: service name и enum `START`/`STOP`/`RESTART`.
- `rollback=12`: `RollbackAction.snapshot_id=1`, без произвольного payload.

Initiator enum: `HUMAN`, `AI_SUGGESTED`, `AUTOMATION`. Значение `UNSPECIFIED`,
неизвестные enum numbers, пустая action/resource, отсутствующий requested time,
неположительный timeout и Duration, не помещающийся в Go `time.Duration`,
отклоняются. Transport выполняет structural validation; core дополнительно
проверяет target, срок, capability, policy и protected resources. Все initiators
проходят одинаковые проверки. Shell string, argv и arbitrary CLI отсутствуют.

`CommandResult` содержит command ID, typed status, started/finished times,
changed, error code/message. Statuses: `ACCEPTED`, `RUNNING`, `SUCCESS`, `FAILED`,
`REJECTED`, `TIMEOUT`, `UNSUPPORTED`. Времена начала/окончания имеют message
presence: они могут отсутствовать до соответствующего этапа действия.
`CommandResult.verification=8` содержит canonical InterfaceVerification:
interface ID/name, expected admin state, optional observed state/time,
verified/changed/uncertain и bounded safe evidence. `uncertain=9` у результата
явно отражает неподтверждённый outcome после отправки mutation. Status остаётся
`TIMEOUT`/`FAILED`, а не автоматически `SUCCESS`; fake rollback не выполняется.
Измеренный admin=false сохраняет optional presence. Verification с
verified=true требует совпадающего наблюдения и uncertain=false.
P3 `CommandResult.snapshot_id=10` связывает result с Agent-owned typed snapshot.
Snapshot содержит только interface admin state/identity; его полный content
не передаётся Controller произвольным configuration payload. Rollback проверяет
ownership, latest/superseded state, retention, compatibility, current resource и
обычную policy/protection; успешный rollback также требует verification.

Реальные writes выполняются только RouterOS interface executor после capability,
default-deny policy и canonical protection checks. Local/SNMP и service actions
остаются unsupported. Protocol malformed command может завершить stream.
Command deadline ограничивается requested timestamp/timeout и временем admission.
Все initiators проходят одинаковые checks.

## Доставка и backpressure

Memory mode сохраняет best effort. Durable mode хранит immutable checksummed
records в bounded local bbolt до application ACK. `ControllerEnvelope.message_ack=3`
содержит `MessageACK.message_id=1`: accepted stable message ID. Успешный Send
не удаляет запись. Restart/reconnect повторяет unacked record с тем же ID,
текущими session/sequence/sent_at и исходными observed timestamps.
Backend обязан deduplicate `(agent_id,message_id)` и ACK только после своего
application commit. Mock ACK подтверждает development observer/output receipt,
не production durable storage.

Semantics — at-least-once для admitted retained messages; exactly-once нет.
Retention/eviction допускают потери telemetry/logs, но не pending CommandResult.
In-flight window и ACK timeout ограничены; timeout приводит к reconnect/backoff.
Duplicate/unknown ACK безопасны. Legacy peer в durable default `ack_fallback: reject`
не получает exchange, records остаются bounded. Явный `send` fallback удаляет
после Send и честно означает best effort; fallback виден в diagnostics/warning.
Подробная storage/admission policy — [delivery](delivery.md).

До mutation резервируются outbox result slot/4096 bytes, persistent journal и
future result capacity. Snapshot/executing сохраняются до write. Result envelope
persist'ится до enqueue, включая стабильный message ID. Journal сохраняет
deduplication до retention acknowledged record. Recovery persisted `reserved`
отклоняет interrupted command; persisted `executing` становится uncertain
без повторного PATCH. [Journal и rollback](commands.md).

QueueState P3 fields: `mode=6`, `storage_bytes=7`, `oldest_pending=8`, `unacked=9`,
`pending_results=10`, `storage_error=11`. Heartbeat `certificate=7` передаёт только
expiresAt/remaining lifetime/enrollment/renewal state/last renewal/safe error.
Private keys, tokens и certificates/CSR bytes через Agent stream не передаются.
Все существующие P0/P1/P2 field numbers сохранены; protocol version остаётся 1.

## LogBatch и audit

`AgentEnvelope.logs=15` содержит `observed_at`, source и до 1024 records.
Каждый file record сохраняет deterministic ID, network record — UUID,
стабильный в пределах durable admission/replay. Передаются device ID, source/source ID,
observed time, optional parsed original timestamp, optional severity/facility,
распознанные host/process/PID, sanitized message/raw, truncated flag и исходные
file identity/offset. Нулевые optional values сохраняют presence. BSD timestamp
без года/timezone не превращается в выдуманный UTC timestamp.

Audit record дополнительно передаёт event ID, record type, optional UID/AUID/PID,
executable, syscall number/action, success/result и node host, если parsing уверен.
Records одного event связываются exact source+device+host+event ID; batch не
означает полный audit event, соседние строки не склеиваются. Agent не делает AI
security inference; обычная log line не становится Event.

Cursor advances после успешной local admission всей batch: RAM для memory,
synced transaction для durable. Rejected batch не advance. Cursor persistence
failure блокирует reads и может дать record replay. Controller ACK независимо
удаляет outbox envelope; не управляет filesystem cursor. Retention/eviction могут
терять logs даже после durable admission. `QueueState.dropped_logs=5` считает
отказы/eviction; dropped_other включает их тоже. Durable logs имеют medium,
audit — high priority; memory сохраняет P2 lower log priority. CommandResult защищены.
Подробнее — [logs](logs.md).

Phase 3 использует тот же `AgentEnvelope.logs=15`, добавляя совместимые поля:

| Новое поле | Номер | Смысл |
|---|---:|---|
| `LogRecord.syslog` | 18 | Format/version/MSGID/SD/raw timestamp, peer IP, transport, verified TLS identity, authenticated/managed flags, sanitized non-UTF8 raw bytes |
| `LogRecord.observation` | 19 | Existing SourceRef и ObservationTime на wire |
| `LogBatch.observation` | 4 | Source/time attribution всей batch |

`SourceRef` разделяет type, collector_id, target_id, protocol, instance_id и
provenance. `ObservationTime` содержит optional source timestamp, observed_at,
collected_at, optional offset/uncertainty и clock_quality. Unknown clock data
не кодируется измеренным zero. Номера существующих fields/RPC/version1 сохранены;
old peer может игнорировать additions, но тогда не получит полную attribution.
Новый Backend должен хранить metadata при ingest; изменение Backend здесь
не выполнялось.

Network source использует `syslog:<configured-id>` только как logical Target/
legacy source label, не Global Asset ID. Host/process — sender claims;
authentication отдельно в SyslogMetadata. SourceRef type=syslog либо auditd
при recognized configured audit plugin, protocol=syslog-tls/syslog-tcp.
Raw сохраняется после sanitizer; для non-UTF8 bytes дополнительно используется
`SyslogMetadata.raw_payload`, потому что protobuf/JSON strings требуют UTF-8.

Network admission требует existing durable outbox/application ACK policy.
Collector→Backend ACK не распространяется на rsyslog→Collector: обычный TCP/TLS
syslog не имеет нашего per-record application ACK. Duplicate sender transmission
может стать новой record с новым ID; exactly-once/универсальный sender dedup не
заявляются. Read в socket не означает durable admission.
[Receiver contract](syslog-receiver.md).

## Equipment hardware observations — Phase 4/5

Phase 5 IPMI переиспользует эти же messages/field numbers. SourceRef содержит
`type=ipmi`, `protocol=ipmi` и `/ipmi/...` resource identifiers; Target ID
по-прежнему `remote:<configured-id>`. BMC Manager, chassis и FRU areas не
сливаются с Redfish по адресу или serial. Numeric Device/Manufacturer/Product
IDs сохраняются как ограниченные existing Evidence fields, FRU identity —
scoped IdentityEvidence. SourceTimestamp отсутствует без source evidence.
Новые IPMI-specific messages, RPC и schema fields не добавлены; Syslog/NTP
contract и application ACK/replay не меняются. Backend gaps для сохранения
hardware metadata Phase 4 остаются актуальными. [Exact IPMI mapping](ipmi.md).
Connectivity/error events для configured simulated IPMI/Redfish сохраняют
legacy `Event.Source=simulated`. Полная event Observation/SourceRef metadata
в Phase 5 оставалась normalized/JSON-only; Phase 6 передаёт её в additive
`Event.observation=8`, сохраняя legacy timestamp/source.

Redfish использует existing InventoryUpdate/TelemetrySample/Device/Metric,
без sensor-specific Redfish protobuf messages или нового delivery service.
Добавления protocol-neutral:

| Existing message | Additive fields/numbers |
|---|---|
| Device | `hardware=39`, `observation=40`, `identity_evidence=41` |
| InventoryComponent | `hardware=12`, `observation=13` |
| InventoryUpdate | `completeness=3`, `observation=4` |
| Metric | `resource_id=4`, `component_id=5`, `status=6`, `observation=7` |
| TelemetrySample | `hardware_health=13`, `observation=14`, `identity_evidence=15` |
| SourceRef | `resource_id=7` |

HardwareInventory содержит typed UUID/SKU/part/BIOS/power/chassis/manager,
optional capacity/cores/threads, independent HardwareStatus и
ResourceRelationship. ComponentHealth передаёт health без fabricated metric.
IdentityEvidence и ObservationMetadata сохраняют source/resource scope и
optional timestamps. Health, HealthRollup, State и PowerState различаются.

Один Target может передавать несколько Devices с устойчивыми IDs из source
resource path:
System/Chassis/Manager не объединяются внутри Collector. Их wire DeviceType
использует существующий UNKNOWN; role задаёт HardwareInventory.resource_type.
Новые DeviceType enums не нужны старому strict decoder. Phase 6 использует
существующую metadata также для SNMP/RouterOS/Local payloads; при отсутствии
optional additions legacy bytes сохраняются. Новые identity types/diagnostics
размещены в Device42/Telemetry16 согласно compatibility rules выше.
Existing field numbers/RPC/version1,
stable message IDs, retained at-least-once durable replay и ACK не меняются.

Old peer может принять прежние fields и игнорировать additions. Это не
подтверждает сохранение full hardware/source/identity/relations, multi-resource
semantics или dedup в production Backend. Backend требует отдельной additive
ingest/storage поддержки; PostgreSQL/REST/Asset resolution в этой фазе не
изменялись и не проверялись. Partial inventory не означает deletion.
[Redfish mapping и validation](redfish.md), [IPMI mapping и границы](ipmi.md).

## TelephonyUpdate

`AgentEnvelope.telephony=17` содержит observed time, source и bounded targets
(wire limit256, config128). Health: stable target ID, address, UDP/TCP/TLS transport,
reachable, optional response code/latency milliseconds, observed time,
consecutive failures, state и safe error category. Matching valid response
подтверждает reachable; 2xx означает успешный unauthenticated OPTIONS, 401/407
— authentication required, 404/5xx — rejected/server error. Timeout/DNS/connect/
malformed различаются. Latency threshold создаёт degraded, failure threshold —
down. Это отдельная telephony state, не device/interface offline.
Transitions `telephony.up/down/degraded` передаются EventBatch. Cancellation
сохраняет предыдущую state. Phase 6 сохраняет SourceRef/ObservationTime через
optional `TelephonyUpdate.observation=4`, `TelephonyHealth.observation=12` и
`Event.observation=8`; это не физические topology links.
Подробности — [telephony](telephony.md).

## Read-only OpenWrt inventory и Simulator

Device дополнен совместимыми полями `source=33`, `board_name=34`,
`platform_evidence=35`, `services=36`, `firewall=37`, `networks=38`.
Services сохраняют instances, running/unknown и optional enabled. Firewall
сохраняет zones/networks/forwardings/rules/redirects и configuration source,
completeness; это UCI snapshot, не доказательство активных runtime rules.
Unknown enabled/up/pending/PID остаются absent, а не false/zero.
Phase 6 добавляет source/time `Service.observation=7`,
`NetworkState.observation=9`, `FirewallSnapshot.observation=9` без изменения
read-only OpenWrt business semantics.

Simulator использует этот же wire contract, protocol version1 и HelloAck.
Все synthetic nested samples, devices, neighbors, log records, health и events
имеют explicit `source=simulated`. Simulated inventory driver также simulated;
production Agent не запускает Generator. Marker должен сохраняться Backend
при дальнейшей обработке. Simulator — development component.

## TLS и enrollment

В `production` Agent проверяет CA и hostname/SAN Controller и предъявляет
client certificate. Server требует сертификат, подписанный доверенным client
CA. Используются TLS ≥ 1.2 и обычная verification; `InsecureSkipVerify` не
включается. Credentials заданы manual файлами либо enrollment bundle.
Explicit `development`
допускает plaintext только для loopback address, включая localhost.

Client URI SAN `spiffe://infrapulse/agent/<UUID>` должен совпадать с claimed
`agent_id`; production Mock проверяет это до observe/HelloAck. Manual certificates
без URI binding требуют reissue. Agent-side enrollment использует отдельные
фиксированные HTTPS `/enroll` и `/renew`, не новый protobuf service. Запрос —
Agent ID/CSR и только для enroll one-time token; ответ — certificate/CA chain,
никогда private key. Renewal mTLS использует прежний certificate identity.
[PKI contract/ограничения](enrollment.md). Tenancy/revocation/production CA service
остаются обязанностью будущего Backend.
Mock Controller даже с mTLS остаётся development-компонентом и не является
production системой регистрации, авторизации или хранения данных.
