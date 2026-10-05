# Архитектура InfraPulse Agent

Module path — `InfraPulseAgent`, Go — 1.27. Agent совмещает локальный Linux
collector, Native OpenWrt, remote collection, file logs/audit и SIP health.
Backend, Web UI, LLM, database и историческое хранилище находятся вне репозитория.
Mock Controller и Simulator — development-компоненты.

## Границы компонентов

| Компонент | Реальная ответственность |
|---|---|
| Agent Core | Identity/session, lifecycle, создание drivers, запуск scheduler, outbound stream, diagnostics и admission команд |
| Local Platform | Linux identity, CPU/memory/interfaces из `/proc` и `/sys`; не управляет удалёнными устройствами |
| Remote Registry | Configured stable IDs, lifecycle drivers, bounded polling, per-device exclusion, last successful observation, connectivity failures и state events |
| Remote Driver | Нормализация данных одного устройства и динамические capabilities; не читает YAML или protobuf |
| Protocol Adapter | SNMP v2c/UDP или RouterOS HTTPS REST, authentication, deadlines, cancellation, response limits и безопасные категории ошибок |
| CredentialProvider | Получение credentials по именованной ссылке из environment; заменяемый interface |
| Telemetry Pipeline | Общие Interface/Device models, presence, counters, безопасные deltas/rates и outbox |
| Command Pipeline | Typed validation → target/deadline/capability/policy → canonical protection → mutation → verification |
| gRPC Transport | Единственная wire mapping граница; один sender и один receiver на session |
| OpenWrt Platform | Linux decorator, typed ubus read-only operations и allowlisted normalized inventory schemas |
| Vendor Profiles | Подтверждённый sysObjectID namespace; capabilities и standard MIB mappings остаются у driver |
| Log Pipeline | Bounded file reads, parsers, sanitizer, rotation и persistent admission cursors |
| Telephony | Fixed SIP OPTIONS UDP/TCP, latency/response/error semantics и отдельные state transitions |
| Simulator | Deterministic development scenarios с simulated source через настоящий transport |

Core не знает OID, RouterOS endpoints и concrete protobuf types. Cross-platform
описывает ОС запуска Agent; multi-vendor — удалённые устройства. На Windows
доступен collector hosting с `local.enabled: false`; Windows LocalPlatform
не реализован.

## Packages и interfaces

```text
internal/agent/              composition root, diagnostics, command admission
internal/config/             strict YAML, defaults и limits
internal/credentials/        Provider и environment references
internal/remote/             registry, polling и переходы состояния
internal/driver/             Driver, Options и optional InterfaceManager
internal/driver/snmp/        SYSTEM/ENTITY/IF/IF-X/LLDP normalization
internal/driver/routeros/    REST normalization, dependency resolution и verification
internal/protocol/snmp/      read-only GoSNMP adapter
internal/protocol/routeros/  fixed HTTPS REST operations
internal/model/              normalized values и remote evidence
internal/telemetry/          local/remote rates
internal/commands/           validation, policy, execution и bounded deduplication
internal/queue/              volatile priority outbox и result reservations
internal/transport/grpc/     protobuf mapping и verified TLS/mTLS
internal/platform/openwrt/   decorator над Linux, typed ubus и schema mappings
internal/vendorprofiles/     verified PEN detection, без отдельных SNMP drivers
internal/logs/               file identity/cursors, syslog/audit parsing и redaction
internal/telephony/          bounded OPTIONS и endpoint health state
internal/simulator/          development-only generator и transport lifecycle
```

Существующие P0 `Platform`, `Transport/Session`, `scheduler.Job`, queue и
normalized models переиспользуются. Remote `Driver` предоставляет `Probe`,
`Inventory`, `Collect`, `Topology`, `Capabilities`, `Close`. Только RouterOS
добавляет `InterfaceManager`: `ResolveInterface` и `SetInterface`.
`credentials.Provider.Get(context.Context, ref)` возвращает credential,
который не входит в normalized/wire models. Узкий fakeable SNMP client
используется mapping fixtures; protocol adapters остаются отдельными packages.

## Configuration и registry

`devices` содержит stable ID, name, driver (`snmp`/`routeros`), address,
credential reference, optional vendor hint/location, polling overrides и
protocol options. ID не выводится из management IP и не меняется при rename.
`local:` namespace зарезервирован для локального устройства. Legacy
`vendor_profile` используется только как hint; vendor profiles этим полем
не включаются.

Config ограничивает devices/credential references 1024 записями. Default remote
concurrency — 4, rows — 4096, response bytes — 1 MiB. Секреты поступают через
`credentials.<ref>.community_env` или `username_env`/`password_env`. Provider
читает значения при request; отсутствие значения означает unavailable.
Нельзя передать credential через Controller или включить его в diagnostics.

Remote registry использует отдельный экземпляр существующего scheduler,
поэтому зависший remote request не занимает local worker pool. На устройство
создаётся один job, который проверяет сроки четырёх групп:

| Группа | Default interval | Работа |
|---|---|---|
| Fast | 3s | Interfaces/state/counters и доступные resource values |
| Medium | 30s | Device probe |
| Inventory | 5m | Inventory/components и медленно меняющиеся адреса |
| Topology | 5m | LLDP/neighbor tables |

Каждый probe имеет собственный timeout, у job общий предел — четыре probe
timeouts. Scheduler запрещает overlapping runs job; gate устройства также
сериализует polling с commands. Отмена закрывает SNMP connections и HTTP I/O.
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
Get/GetNext/Walk/BulkWalk только SNMP v2c read-only; driver разбирает стандартные
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
недостаточная queue capacity/малый message limit не допускают action. Receiver
исполняет commands последовательно; per-device gate исключает конфликт poll.
Bounded volatile dedup cache содержит до 256 commands/results и проверяет
fingerprint target/action/initiator. In-flight identity не вытесняется.

SetInterface проверяет текущий canonical record, выполняет один PATCH и
read-after-write GET после любой отправленной попытки, включая HTTP error или
невалидное тело ответа. Для verification оставляется часть deadline. Повторяется
только безопасное чтение; если исход
не подтверждён, result содержит uncertain. Fake rollback отсутствует.

Outbox/result reservations/dedup находятся в RAM: restart теряет состояние,
ошибка Send после Pop может потерять result. Это не durable execution journal
и не exactly-once. Полная security model — [security](security.md).

## Источники и следующие этапы

Standard mappings: [RFC 3418](https://www.rfc-editor.org/rfc/rfc3418),
[RFC 2863](https://www.rfc-editor.org/rfc/rfc2863),
[RFC 6933](https://www.rfc-editor.org/rfc/rfc6933),
[IEEE LLDP-MIB](https://www.ieee802.org/1/files/public/MIBs/LLDP-MIB-200505060000Z.mib),
[MikroTik SNMP](https://help.mikrotik.com/docs/spaces/ROS/pages/8978519/SNMP).
RouterOS operations: [REST API](https://manual.mikrotik.com/docs/developer-guides/rest-api/),
[interfaces](https://manual.mikrotik.com/docs/cli-reference/interface/),
[neighbors](https://help.mikrotik.com/docs/spaces/ROS/pages/24805517/Neighbor+discovery).

P3 остаётся отдельным этапом: Windows
LocalPlatform, SNMPv3/другие протоколы по необходимости, durable delivery,
enrollment и snapshots/rollback. Их наличие в roadmap не означает capability.
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
RAM queue admission; reject не advance, persistence error блокирует новые reads.
Rotation gaps и bounds видны как events/diagnostics. Crash после admission,
очередь eviction и Pop/Send loss всё ещё теряют logs; запись cursor не durable
transport ACK. О деталях — [logs](logs.md).

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
