# Архитектура InfraPulse Collector

Module path — `InfraPulseAgent`, Go — 1.27. InfraPulse Collector / Edge Gateway
использует один существующий `internal/agent.Agent` runtime: локальный
Linux/OpenWrt, remote collection, file/network logs/audit и SIP health. Исторические
CLI/protobuf/identity names сохранены; второго runtime нет.
Backend, Web UI, LLM, server database и историческое хранилище находятся вне
репозитория; embedded durable state принадлежит Collector.
Mock Controller, Mock Enrollment и Simulator — development-компоненты.

Collector обслуживает management/local network и не требует установки на
каждый server. Zabbix/Prometheus integration, Ansible automation и Grafana
visualization относятся к Control Plane. Collector отвечает за edge/network/
hardware protocols; локальные метрики остаются standalone/fallback mode.
Domain boundaries и архитектурный freeze Phase 1 описаны в
[Collector Core](collector-core.md).

## Границы компонентов

| Компонент | Реальная ответственность |
|---|---|
| Collector Core (`internal/agent`) | Identity/session, targets, lifecycle adapters, запуск scheduler, normalized observations, outbound stream, diagnostics и admission команд |
| Local Platform | Linux identity, CPU/memory/interfaces из `/proc` и `/sys`; не управляет удалёнными устройствами |
| Host Time Setup | Trusted local `time_sync`, выбор стандартной NTP-службы ОС, opt-in deployment apply и read-only status; не создаёт clock model или remote-device command |
| Remote Registry | Configured targets, lifecycle adapters, bounded polling, per-target exclusion, last successful observation, connectivity failures и state events |
| Managed Configuration | Administrator-bounded typed snapshots, fixed canonical digest/revisions, full-set validation, prepared Registry/scheduler transition, last-known-good и statuses; не equipment commands |
| Remote Driver | Нормализация данных источника и динамические capabilities; не читает YAML или protobuf и не выполняет глобальный Asset merge |
| Protocol Adapter | SNMP v2c/v3 USM/UDP, RouterOS/Redfish HTTPS или IPMI LANplus UDP, authentication, deadlines, cancellation, response limits и безопасные категории ошибок |
| CredentialProvider | Получение credentials по именованной ссылке из environment; заменяемый interface |
| Observation Pipeline | Общие normalized payloads, SourceRef/provenance, identity evidence, temporal metadata, presence, counters, безопасные deltas/rates и outbox |
| Command Pipeline | Typed validation → admission/journal → target/capability/policy/protection → persistent snapshot/executing → mutation → verification → persistent result/ACK |
| Durable Outbox | Bounded bbolt admission, protected results, stable message IDs, leases/replay и deletion after ACK |
| PKI Manager | Local private key/CSR, verified HTTPS enrollment, identity binding, atomic bundle rotation и safe status |
| gRPC Transport | Единственная wire mapping граница; один sender и один receiver на session |
| OpenWrt Platform | Linux decorator, typed ubus read-only operations и allowlisted normalized inventory schemas |
| Vendor Profiles | Подтверждённый sysObjectID namespace; capabilities и standard MIB mappings остаются у driver |
| Log Pipeline | Bounded file reads, parsers, sanitizer, rotation и persistent admission cursors |
| Syslog Receiver | Opt-in bounded TCP/TLS ingress, framing, verified peer/source attribution и normalized LogBatch через существующий admission |
| Telephony | Fixed SIP OPTIONS UDP/TCP/TLS, verified CA/SAN/SNI, latency/response/error semantics и отдельные state transitions |
| Simulator | Deterministic development scenarios с simulated source через настоящий transport |

Core не знает OID, RouterOS/Redfish endpoints, IPMI commands/SDR layouts и
concrete protobuf types. Cross-platform
описывает ОС запуска Agent; multi-vendor — удалённые устройства. На Windows
доступен collector hosting с `local.enabled: false`; Windows LocalPlatform
не реализован и сознательно перенесён в post-MVP/backlog; готовность P3 от него
не зависит.

## Packages и interfaces

```text
internal/agent/              composition root, diagnostics, command admission
internal/config/             strict YAML, defaults и limits
internal/credentials/        Provider и environment references
internal/remote/             registry, polling и переходы состояния
internal/managedconfig/      full snapshots, local policy, revision/digest, apply/recovery и LKG
internal/driver/             Adapter, optional readers, compile-time Registry и InterfaceManager
internal/driver/snmp/        SYSTEM/ENTITY/IF/IF-X/LLDP normalization
internal/driver/routeros/    REST normalization, dependency resolution и verification
internal/driver/redfish/     bounded gofish HTTPS/session adapter и hardware normalization
internal/driver/ipmi/        bounded LANplus/session gate, FRU/SDR и hardware normalization
internal/protocol/snmp/      read-only GoSNMP adapter
internal/protocol/routeros/  fixed HTTPS REST operations
internal/model/              Target, SourceRef, observations, time и identity evidence
internal/telemetry/          local/remote rates
internal/commands/           validation/policy, persistent journal и snapshots/rollback
internal/queue/              memory/durable priority outbox, reservation и ACK/replay
internal/pki/                enrollment/CSR, certificate lifecycle и private state
internal/transport/grpc/     protobuf mapping и verified TLS/mTLS
internal/platform/openwrt/   decorator над Linux, typed ubus и schema mappings
internal/vendorprofiles/     verified PEN detection, без отдельных SNMP drivers
internal/logs/               file identity/cursors, syslog/audit parsing и redaction
internal/syslogreceiver/     TCP/TLS lifecycle, framing, configured source mapping и bounded admission
internal/telephony/          bounded OPTIONS и endpoint health state
internal/simulator/          development-only generator и transport lifecycle
```

Существующие `Platform`, `Transport/Session`, `scheduler.Job`, queue и
normalized models переиспользуются. `driver.Adapter` содержит lifecycle
`Capabilities`/`Close`; `ProbeReader`, `InventoryReader`, `TelemetryReader`
и `TopologyReader` подключаются отдельно. Существующий `Driver` объединяет
их для SNMP/RouterOS. `driver.Registry[T]` выбирает compile-time constructors
в composition root; отсутствующий reader не создаёт fake observation.
`remote.New` остаётся compatibility bridge к `remote.NewAdapters`: оба
используют один registry/scheduler. Только RouterOS
добавляет `InterfaceManager`: `ResolveInterface` и `SetInterface`.
`credentials.Provider.Get(context.Context, ref)` возвращает credential,
который не входит в normalized/wire models. Узкий fakeable SNMP client
используется mapping fixtures; protocol adapters остаются отдельными packages.
Redfish добавляет optional `InventoryBatchReader` и `HardwareReader`: bounded
multi-resource batch и несколько equipment health/metric samples. Legacy
SNMP/RouterOS readers не меняются. Read plan из inventory ограничивает очередной
hardware poll; HTTP/session/library types остаются внутри Redfish package.
System, Chassis и Manager сохраняют раздельные URI-derived observed IDs и
typed roles/evidence, без physical Asset merge или LLDP topology. Generic
hardware protobuf additions и completeness идут через прежний sanitizer/outbox/
ACK flow. Redfish не зависит от enabled Syslog receiver. [Contract](redfish.md).

Standalone `probe-device` использует тот же registered driver и normalized
source-bound Device/identity evidence, без запуска runtime/transport.
Collector ID в этом read отсутствует, поскольку persistent identity не загружается.

IPMI Phase 5 реализует те же `InventoryBatchReader`/`HardwareReader` в
`internal/driver/ipmi`. Фабрика выбирает только явно заданный `driver: ipmi`;
это fallback compatibility, без network scanning или переключения после
Redfish ошибок. Pure-Go typed client и проверка входящих RMCP+ пакетов изолированы
в adapter; Collector Core не знает IPMI commands/SDR layout/library types.
FRU areas остаются scoped components, BMC Manager и chassis имеют независимые
IDs; имя sensor не является его ключом. Sensor owner/channel/LUN/number и
entity IDs формируют стабильный source-relative identity. Shared IP одинаковых
Redfish/IPMI targets не вызывает merge. Глобальный Entity Resolution — Backend.

Один poll использует одну LANplus session для последовательных reads и закрывает
её в bounded cleanup. SDR cache ограничен records/bytes/TTL; repository changes
и BMC identity invalidation требуют refresh. Existing registry gate и scheduler
ограничивают target concurrency, timeout и cooldown/backoff; fast network
telemetry не меняется. Нормализованные results проходят `Agent.push`/Sanitizer,
тот же durable outbox и gRPC/application ACK. [IPMI mapping и gaps](ipmi.md).

Phase 6 переиспользует `IdentityEvidence`, `SourceRef`, `ObservationMetadata`,
`ObservationTime` и существующий topology pipeline. Protocol adapters сохраняют
facts на собственных subjects: System, Chassis, BMC, component и interface не
сливаются по общему Target/IP/serial. Общие helpers выполняют безопасную bounded
normalization; source-scoped resource IDs остаются детерминированными. Enrichment
не запускает второй inventory poll, ingestion path, Asset Registry или topology
database. Global resolution и confidence-based merge остаются Backend.

Typed source/time metadata теперь проходит gRPC также для SNMP/RouterOS/local,
Events, topology и SIP/OpenWrt payloads. Device/Telemetry legacy identity lists
содержат только пригодные Phase 5 types; новые types и normalization diagnostics
передаются в additive `extended_identity_evidence`. Decoder объединяет оба списка
в прежнюю domain model. Sanitizer и wire admission проверяют enriched envelope
до durable queue; ACK/replay и message IDs не меняются.
[Phase 6 model](identity-topology.md), [Backend contract](backend-identity-topology-contract.md).

## Configuration и registry

`devices` содержит stable ID, name, driver (`snmp`/`routeros`/`redfish`/`ipmi`), address,
credential reference, optional vendor hint/location, polling overrides и
protocol options. Target ID не выводится из management IP и не меняется при rename.
`local:` namespace зарезервирован для локального устройства. Legacy
`vendor_profile` используется только как hint; vendor profiles этим полем
не включаются.

`config.Device.Target` переводит существующую YAML entry в `model.Target`,
сохраняя intervals/defaults и credential reference. Local host, SIP endpoint
и file log source также имеют Target. Target определяет configured connection,
а `Device.ID` — наблюдаемое устройство; SourceRef хранит эти уровни раздельно.
Для существующих SNMP/RouterOS wire Device IDs сохраняется configured ID;
внутренний Target ID имеет отдельный namespace `remote:<configured-id>`.
Explicit observed Device ID не подменяется Target ID. Глобальный Backend Asset
и multi-source merge находятся вне Collector. [Подробности](collector-core.md).

Config ограничивает devices/credential references 1024 записями. Default remote
concurrency — 4, rows — 4096, response bytes — 1 MiB. Секреты поступают через
`credentials.<ref>.community_env`, `username_env`/`password_env` либо SNMPv3
`username_env`/`auth_password_env`/`privacy_password_env`. Provider
читает значения при request; отсутствие значения означает unavailable.
Нельзя передать credential через Controller или включить его в diagnostics.

Remote registry использует отдельный экземпляр существующего scheduler,
поэтому зависший remote request не занимает local worker pool. На устройство
создаётся один job, который проверяет сроки четырёх групп:

| Группа | Default interval | Работа |
|---|---|---|
| Fast | 3s | Interfaces/state/counters и доступные resource values |
| Medium | 30s | Device probe для SNMP/RouterOS; hardware health/metrics для Redfish/IPMI |
| Inventory | 5m | Inventory/components и медленно меняющиеся адреса |
| Topology | 5m | LLDP/neighbor tables |

Fast/Topology groups используются только при наличии соответствующих readers;
Redfish/IPMI выполняют только Medium и Inventory. Каждый probe имеет собственный
timeout, у job общий предел — четыре probe timeouts. Scheduler запрещает
overlapping runs job; gate устройства также сериализует polling с commands.
Отмена закрывает SNMP/IPMI connections и HTTP I/O.
Tables и response bytes ограничены adapter и driver. Missed runs пропускаются,
а не накапливаются в неограниченной очереди.

Registry хранит last seen и число последовательных connectivity failures;
после трёх ошибок связи устройство становится offline.
Unsupported OID, missing API и malformed data не считаются connectivity failure.
До первого успешного наблюдения/трёх ошибок connectivity state — unknown.
Успешное remote чтение в poll round имеет приоритет над connectivity error;
ошибка outbox не меняет доступность оборудования. На переходах формируются
`device.online/offline`, `interface.up/down`, `topology.link.observed/expired`.
`collector.error` отражает смену состояния ошибки, а не каждый poll.

## Generic SNMP и inventory

Pure-Go `github.com/gosnmp/gosnmp` выполняет BER/SNMP. Adapter предоставляет
Get/GetNext/Walk/BulkWalk SNMP v2c/v3 read-only; driver разбирает стандартные
SYSTEM, ENTITY-MIB, IF-MIB/IF-X-MIB и LLDP-MIB. Library packet logging отключён:
сообщество не появляется в errors или logs. Ошибки и partial walk сохраняются,
поэтому оборванная таблица не выдаётся за complete snapshot.

Первый fast sample получает bounded IF/IF-X baseline. Затем fast polling
использует Get batches до 60 OID по известным indexes для identity/state/counters
и discontinuity; полный interface walk повторяется на slow inventory interval.
Новые interfaces появляются после slow discovery refresh, исчезнувшие known
indexes удаляются при NoSuchInstance. Fast name/MAC refresh защищает rates
при recreation. `remote.max_rows` ограничивает также varbind/table cells,
поэтому большой walk может достичь лимита раньше числа interfaces.

SYSTEM даёт sysDescr/sysObjectID/sysUpTime/sysContact/sysName/sysLocation.
`sysUpTime` — uptime management subsystem, не доказательство uptime ОС.
Отсутствие uptime явно отмечено; remote rates тогда отсутствуют. ENTITY
сохраняет список chassis/modules и реально возвращённые model/serial/revisions.
Device-level model/serial выбираются только при единственной chassis.
PhysicalLocation приходит из config, не из sysLocation/sysDescr.

Vendor evidence сохраняет hint отдельно от подтверждения. Шесть profiles
определяют Cisco/Huawei/MikroTik/Juniper/Eltex/QTECH по подтверждённым IANA
enterprise prefixes. Literal MikroTik в sysDescr — слабое P1 evidence.
Hostname не используется для detection; vendor не создаёт model/serial и
capabilities. Product-specific health OID не угадываются, inventory сохраняет
стандартный ENTITY fallback. Evidence и пределы проверки — [profiles](vendor-profiles.md).

## Presence и rates

Interface одинаков для Linux, SNMP и RouterOS. Новые optional metadata сохраняют
наличие index/MTU/admin state, availability каждого counter, его width и
`ifCounterDiscontinuityTime`. Nil counter metadata сохраняет P0 semantics:
все counters доступны и 64-bit. Remote drivers передают explicit metadata;
нулевое значение с availability=true — измеренный ноль.

HC counters предпочитаются IF-X-MIB. IF-MIB Counter32 используется как fallback.
Сумма fallback packet counters доступна как число с width=0: агрегат не является
одним modular counter, rate по нему не выводится. Width=0 означает unknown
width, не unavailable. Rates имеют свою availability mask.

Rates — delta / actual elapsed time. Первый sample, смена ID/index/name/MAC,
уменьшение uptime, boot identity change, discontinuity marker change и уменьшение
64-bit counter сбрасывают соответствующий baseline. Для 32-bit byte rates нужна
стабильная известная speed и интервал, исключающий multiple wraps. Уменьшение
Counter32 дополнительно требует неизменного доступного discontinuity marker;
неоднозначное reset/overflow даёт unavailable. Для Counter32 packets/errors/
discards rates консервативно не выводятся. Full host reboot не всегда можно
отличить от restart SNMP subsystem; недоступный uptime не подменяется нулём.

## Topology и wire

`model.TopologyUpdate` различает complete/partial/failed, observed/collected
время, collector TTL assumption и Neighbor observations. Raw chassis/port bytes,
subtypes, local port number, protocol/source, evidence/confidence сохраняются.
LLDP local port number не приравнивается ifIndex: используется unique mapping
по ifName/ifAlias/MAC или weaker ifDescr evidence. Неуверенный port остаётся
unresolved. ARP/ND/FDB не собираются как physical links.

`Neighbor.Kind=observed_l2_neighbor` задаёт направление от local device/interface
к advertised remote endpoint. Reciprocal observations остаются независимыми;
unresolved neighbor и отсутствующий remote port допустимы. Phase 6 сохраняет
local ifIndex/name/MAC, optional management addresses/capabilities/age и отдельные
protocol markers. Stable observation ID учитывает Collector/Target/source,
local interface и remote identifiers/subtypes, но не current timestamp.

RouterOS neighbor discovery сохраняет LLDP/CDP/MNDP/unknown. Neighbor MAC не
объявляется LLDP chassis subtype=MAC, а CDP interface-name не переносится
автоматически на LLDP/MNDP. Наблюдение на общем bridged segment не подтверждает
один прямой кабель. Global graph reconciliation остаётся Backend.

LLDP-MIB не предоставляет received TTL. `remote.topology_ttl` (default 15m)
явно передаётся как collector assumption. Partial/failed collection не удаляет
предыдущие links; локальное expiry event допускается после complete snapshot,
если отсутствующее observation уже достигло expiry.

Совместимый payload `AgentEnvelope.topology=14` добавлен без перенумерации P0.
Wire presence/evidence/verification отражают normalized model. Mock Controller
выводит decoded observations в JSON. Details — [protocol](protocol.md) и
[topology](topology.md).

## RouterOS и actions

HTTPS REST client использует фиксированные GET identity/resource/routerboard,
interface/address/neighbor и protection metadata menus; PATCH может менять
только `disabled` существующего canonical interface `.id`. Controller не
передаёт URL/path/method/JSON/CLI. TLS проверяет CA/SAN; redirects, proxy и
HTTPS→HTTP fallback отключены. При custom device CA доверяется этому PEM pool,
иначе используются системные roots.

Capabilities появляются после фактических успешных операций. Writes требуют
`commands.enabled` и per-device `enable_mutations`, а также положительных
`read/write/api/rest-api` permissions authenticated user/group. Недоступная
metadata, hidden/RADIUS-only user или отсутствующая dependency table оставляют
mutation unsupported. Permission discovery повторяется перед write.

Canonical aliases включают `.id`, name/default-name/MAC и связанные
bridge/bond/VLAN interfaces. Dependency graph проверяется консервативно:
protection любого связанного management alias запрещает disruptive operation.
Автоматическое определение management path отсутствует.

До action Agent резервирует 4096 bytes и message slot для CommandResult;
недостаточная queue capacity/малый message limit не допускают action. Отдельный
последовательный worker с backlog=1 не блокирует receiver ACK. Per-device gate
исключает конфликт poll. Persistent bounded journal проверяет command ID/key
и fingerprint target/action/initiator; результат и execution state переживают restart.

SetInterface проверяет текущий canonical record, выполняет один PATCH и
read-after-write GET после любой отправленной попытки, включая HTTP error или
невалидное тело ответа. Для verification оставляется часть deadline. Повторяется
только безопасное чтение; если исход
не подтверждён, result содержит uncertain. Автоматического retry PATCH или
rollback нет; отдельный typed rollback требует проверенного snapshot.

Memory telemetry outbox остаётся best effort. Durable outbox сохраняет records
до ACK. Persistent execution journal работает независимо от queue mode и не
даёт повторно исполнить retained duplicate после restart. Interrupted executing
command становится uncertain; result replay не повторяет write. Это не
exactly-once внешнего устройства. [Commands](commands.md), [security](security.md).

## Источники и следующие этапы

Standard mappings: [RFC 3418](https://www.rfc-editor.org/rfc/rfc3418),
[RFC 2863](https://www.rfc-editor.org/rfc/rfc2863),
[RFC 6933](https://www.rfc-editor.org/rfc/rfc6933),
[IEEE LLDP-MIB](https://www.ieee802.org/1/files/public/MIBs/LLDP-MIB-200505060000Z.mib),
[MikroTik SNMP](https://help.mikrotik.com/docs/spaces/ROS/pages/8978519/SNMP).
RouterOS operations: [REST API](https://manual.mikrotik.com/docs/developer-guides/rest-api/),
[interfaces](https://manual.mikrotik.com/docs/cli-reference/interface/),
[neighbors](https://help.mikrotik.com/docs/spaces/ROS/pages/24805517/Neighbor+discovery).

Реализованный P3 содержит SNMPv3, bounded durable delivery/ACK/replay,
persistent journal/results, enrollment/rotation и typed interface rollback.
Collector Core Phase 1 сохраняет этот фундамент и добавляет Target/Source,
identity evidence и временную модель. Phase 2 использует standard OS corporate
NTP через отдельный deployment apply. Local Platform collection interface,
observation clocks и Backend не меняются. Phase 3 добавляет opt-in Syslog/Audit
network source и additive logs metadata на gRPC. Phase 4 добавляет read-only
Redfish inventory/health/telemetry в тот же runtime/outbox. Phase 5 добавляет
explicit IPMI LANplus targets через те же hardware contracts; Collector ↔
Backend clock offset estimation отсутствует. Phase 6 завершает scoped identity
и directed topology evidence с additive wire metadata и Backend handoff,
сохраняя тот же runtime/outbox. [Architecture freeze](collector-core.md),
[identity/topology](identity-topology.md).
Phase 7 добавляет opt-in Backend-managed monitoring через existing gRPC/mTLS:
typed snapshots отделены от commands и local YAML, `internal/managedconfig`
использует existing adapter factory/Registry/scheduler. Новый scheduler для
managed Targets не создаётся. Validate/prepare выполняются до остановки старых
изменяемых jobs; replacement jobs paused до persistent LKG publication, затем
commit. Failure восстанавливает предыдущий runnable набор bounded recovery.
Unchanged entries и static local jobs сохраняются; outbox/journal не очищаются.
Highest-seen revision/result и LKG identity-bound file сохраняются отдельно от
actual active state: denied restore не выдаёт stored APPLIED за runtime success.
[Ownership/lifecycle](managed-configuration.md),
[Backend B9 contract](backend-managed-config-contract.md).
Windows LocalPlatform относится к post-MVP/backlog. NETCONF/gNMI deferred:
без подтверждённого typed use case новый protocol не добавляется.
Фактически выполненные проверки находятся в [validation](validation.md).

## OpenWrt, logs и telephony

`platform.NewMode(auto/linux/openwrt)` сохраняет один Agent lifecycle. Native
OpenWrt переиспользует Linux Identity/Collect и CPU/rate baselines; slow inventory
дополняется board/release, network.interface dump, service list и UCI firewall.
`/bin/ubus` вызывается через закрытый Operation enum и фиксированные arguments;
stdout/stderr и каждый вызов ограничены. Optional reads делят общий deadline,
missing ubus/rpcd оставляет working Linux data и unavailable evidence.
Services/firewall доступны только read-only; snapshot UCI не подтверждает active
ruleset. Подробности — [OpenWrt](openwrt.md).

Logs используют отдельный scheduler с одним worker, SIP — отдельный bounded
pool (default 2). Log burst не занимает local/remote telemetry workers.
`logs.Pipeline.Poll` читает одну bounded batch, применяет literal configured
secret redaction и known patterns до parsing/serialization. Audit records
сохраняют source+EventID; exact correlation делается по этому ключу в batch
или Backend, long-lived heuristic correlation cache отсутствует.

Cursor state — отдельные protected файлы per source в `state_dir/log-cursors`.
Device/inode identity, generation, admitted offset, anchor hash и PEM/discard
state сохраняются crash-safe temp+sync+rename. Cursor advancement следует за
local queue admission; в durable mode Push ждёт synced transaction. Reject не
advance, cursor persistence error блокирует reads. Filesystem cursor, local
admission и Controller ACK независимы. Retention/eviction могут терять logs;
crash между admission/cursor persistence допускает record replay. [Logs](logs.md).

SIP endpoint имеет stable ID и собственный state; это не remote device state.
Fixed OPTIONS проверяет transaction identifiers, response framing и code,
различает DNS/connect/timeout/malformed/authentication/server errors. Parent
cancel не создаёт ложный outage; scheduler deadline оставляет небольшой запас
после собственного probe timeout. Подробнее — [telephony](telephony.md).

Device inventory дополнен services/firewall/networks/evidence, envelope —
LogBatch и TelephonyUpdate. Services events возникают только при подтверждённом
running↔stopped; каждый log record не превращается в Event. Diagnostics содержит
bounded source states и SIP health, без raw log text/configuration/credentials.

## Development Simulator

Simulator — отдельный executable, production Agent его не импортирует.
Generator создаёт bounded deterministic device set и временные trends. Runner
использует тот же Transport/Session, gRPC handshake, reconnect и normalized
envelopes; все вложенные synthetic observations имеют `source=simulated`.
Это demonstration data, не hardware validation. [Simulator](simulator.md).

## Durable delivery и safety state P3

Queue abstraction сохраняет два режима. `memory` — совместимый best effort;
`durable` — embedded [bbolt v1.5.0](https://github.com/etcd-io/bbolt/releases/tag/v1.5.0),
pure Go без CGO/server. Выбор основан на maintained etcd implementation, synced
ACID transactions, bounded file growth через `MaxSize` и переносимости Linux
amd64/arm64/arm/mips/mipsle. Mmap использует OS page cache; это не обещание
фиксированного RSS. Database endian-dependent, автоматической compaction нет.

Immutable record сохраняет stable UUID, envelope, priority, creation time и
checksum. Payload/messages, physical file, message size, retention и in-flight
leases имеют независимые пределы. Concurrent admissions/ACK используют bounded
Batch; Push ждёт synced commit, `NoSync` не используется. Flash wear остаётся
deployment risk: OpenWrt может использовать memory или external writable volume.

```text
collect → immutable envelope → local admission → Send → application acceptance
                                                   → ACK → local deletion
command → policy/protection → durable reservation → snapshot/execution boundary
        → one mutation → readback → durable journal result → outbox → ACK
```

Pop durable record выдаёт lease, не удаляет payload. Disconnect сбрасывает
leases, timeout вызывает reconnect/backoff. Negotiated ACK отсутствует у старого
peer: default durable `reject` сохраняет bounded очередь и явно сообщает ошибку;
operator opt-in `send` даёт best effort deletion после Send. Controller должен
deduplicate по `(agent_id,message_id)`. Semantics — at-least-once retained
messages, с возможной потерей low/medium priority по retention/eviction.
Pending CommandResult никогда не вытесняется telemetry.

Journal/snapshots — отдельный bounded checksummed atomic JSON state, а не
telemetry history. Это небольшие редкие state transitions; file/directory Sync
используются на каждой durable execution границе. Один process владеет lock.
Reserved capacity включает будущий result; storage failure до execution
блокирует mutation. Полная физическая квота для внешнего filesystem не обещается.
Reserved recovery отклоняет interrupted command; executing recovery создаёт
uncertain result, не повторяя внешнюю mutation. Сохранённый result replay
использует прежний message ID. Journal dedup horizon ограничен retention
acknowledged entries. Подробности — [delivery](delivery.md), [commands](commands.md).

Snapshots только для поддерживаемого RouterOS interface admin state. Typed
rollback проверяет ownership, canonical aliases/dependencies, current protection,
capability/policy, version/schema, TTL, latest generation и expected current
state. Новый snapshot перед rollback и read-after-write verification используют
обычный mutation pipeline. Remote REST не даёт atomic CAS, поэтому внешняя
параллельная запись остаётся risk; blind overwrite и arbitrary payload отсутствуют.

## Enrollment и certificate lifecycle P3

PKI Manager получает persistent Agent UUID до transport creation. Local ECDSA
P-256 key/CSR и one-time token дают certificate через verified HTTPS service.
URI SAN `spiffe://infrapulse/agent/<UUID>` связывает credential с заявленным ID;
Mock Controller проверяет binding после verified mTLS. Production Backend обязан
сохранять ту же проверку и отдельно решать tenancy authorization.

Matching key/certificate/CA хранятся одной atomic bundle с Unix permissions
0700/0600. Key не покидает Agent. Renewal использует current mTLS certificate,
новый local key/CSR и bounded one-minute cadence; token больше не требуется.
Validation/storage failure сохраняет старый рабочий credential. Успешная
rotation отменяет текущую session и reconnect получает новый TLS snapshot.
Heartbeat/diagnostics показывают только expiry/remaining lifetime и safe status.

Filesystem key protection не является HSM/TPM/encryption. Mock token registry
in-memory и development-only; production CA, revocation, CA rollover, expired
certificate recovery и tenant control не реализованы. Manual certificate mode
сохраняется, но сертификаты без URI binding требуют reissue.
[Enrollment contract](enrollment.md), [security](security.md).

## Linux/OpenWrt и deployment review

Generic Linux interfaces — procfs/sysfs, interface addresses и os-release; отдельные
collectors для Astra Linux, ALT Linux и RED OS не нужны. Это оценка совместимости
интерфейсов, не runtime validation дистрибутивов. Доступная runtime среда — WSL
Linux amd64. Russian distributions и настоящая OpenWrt firmware runtime unverified.
Restricted/missing optional sources должны сохранять unavailable/unknown evidence;
container namespace и mounts определяют видимые CPU/network/log data.

Native deployment использует host procfs/sysfs, local log permissions и persistent
state directory. Container Collector может опрашивать SNMP/RouterOS/Redfish/SIP без
обещания полноценного host monitoring; для collector-only выбирается
`local.enabled: false`. Durable state требует persistent writable volume.
Docker не обязателен; отдельная container runtime validation и production
Dockerfile в P3 не добавлены. Service user, log permissions, read-only rootfs с
отдельным writable state, flash/RAM budget и management protection остаются
responsibility deployment. Monitoring degraded storage не разрешает mutation.

Vendor profiles поддерживают Eltex/QTECH наравне с остальными через standard
MIB fallback. P3 не добавляет product health OID: ни одному mapping не получен
полный комплект official semantics, достоверного ответа и fixture. Это не
утверждение отсутствия vendor MIB; отсутствие доказанного mapping сохраняется
явным. SIP расширен verified TLS OPTIONS с CA/SNI/name checks; Digest deferred.

## Решение по NETCONF и gNMI

**NETCONF — deferred.** Конкретный vendor/resource, который нельзя надёжно
покрыть существующими reads/typed RouterOS mutation, не определён. Go candidate
[nemith/netconf](https://github.com/nemith/netconf) предоставляет отдельную library,
но pre-1.0 API/toolchain совместимость не проверялась как dependency. Потребуются
typed operations, SSH host-key/TLS trust, bounded XML и официальные schemas с
fixture strategy. Наличие library не является основанием добавить protocol.

**gNMI — deferred.** [OpenConfig Go reference implementation](https://github.com/openconfig/gnmi)
предоставляет client/collector, но подтверждённые device paths, supported model
versions и потребность в subscription telemetry здесь отсутствуют. SNMP уже даёт
нужные demo signals. Fixture strategy возможна через local gRPC server, однако
она не подтверждает paths реального vendor. При появлении use case — отдельный
read-only adapter, predefined paths и verified TLS; arbitrary writes запрещены.

Production Backend/UI/AI/history, universal backup/configuration manager, media
и Windows LocalPlatform остаются post-MVP или вне scope. P3 production readiness
не означает hardware certification или готовый production PKI/Backend.

## Phase 3: Remote Syslog / Audit Receiver

`internal/syslogreceiver` — transport source в существующем Collector runtime.
Strict YAML содержит listener options отдельно от bounded configured mappings.
Disabled receiver не создаёт listener/worker. Enabled startup проверяет TLS,
создаёт listeners, затем запускает ограниченные accept/connection loops;
shutdown останавливает accept, отменяет processing и закрывает active sockets.
Receiver не использует polling scheduler и не занимает SNMP/SIP/log-file workers.

TLS использует стандартный `crypto/tls`, TLS ≥1.2, отдельный server certificate
и mTLS CA. После transport validation TCP/TLS проходят один path:

```text
bounded frame → existing sanitizer/syslog/audit parser → source/time metadata
             → one LogRecord in LogBatch → existing Agent.push
             → existing durable queue → one gRPC sender → application ACK
```

Protocol source не знает protobuf/session и не отправляет messages напрямую.
Новой logging queue/DB/delivery subsystem нет. Для admission failure connection
worker удерживает одну bounded record, прекращает читать и выполняет bounded
retry; timeout закрывает connection. Accepted envelope получает обычный stable
message ID/ACK/replay. TLS/TCP sender не получает application ACK от Collector.

Managed source имеет `SourceRef.TargetID=syslog:<configured-id>`; HOSTNAME
сохраняется sender claim отдельно от verified TLS SAN/fingerprint и peer IP.
Explicit unmanaged policy не создаёт permanent Target/Asset; IP/hostname claims
не используются для global Asset merge. Optional SyslogMetadata и observation
на LogRecord/LogBatch передают эти сведения через additive wire fields.
Local file reader/cursors/rotation и existing parser остаются общими механизмами.
Audit normalization требует managed source, configured plugin APP-NAME и
canonical kernel timestamp/serial; records сохраняются отдельно.

Default message8192/connection64/per-source8 bounds и 10s read/60s idle/2s admission
timeout предназначены для bounded deployment, а не production throughput claim.
Enabled receiver требует durable mode и ACK reject policy; storage fallback
не разрешает ему признать volatile admission durable. Source clock quality
остаётся unknown; Phase 2 NTP/системные timestamps не изменены.
Практические rules/examples и RFC scope — [syslog-receiver](syslog-receiver.md).
