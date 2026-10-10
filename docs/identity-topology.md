# Identity evidence и topology enrichment — Phase 6

Phase 6 сохраняет идентичность, происхождение и время уже собираемых фактов.
Collector передаёт observations, которые Backend может использовать для
SourceBindings и глобальной топологии. Global Asset Registry, entity merge/split,
cross-collector matching и разрешение неизвестного соседа остаются в Backend.
Новые Target или соединения из LLDP/CDP/MNDP advertisements не создаются.

## Существующий pipeline

```text
SNMP / RouterOS / Redfish / IPMI / local / Syslog / Simulator
  -> существующие domain models + IdentityEvidence / SourceRef
  -> ObserveEnvelope + directed Neighbor enrichment
  -> Agent.push -> sanitizer -> существующий outbox
  -> gRPC -> application ACK / restart replay
```

Переиспользуются `Device`, `InventoryComponent`, `HardwareInventory`,
`ResourceRelationship`, `TopologyUpdate`, `Neighbor`, `IdentityEvidence`,
`SourceRef`, `ObservationMetadata`, `ObservationTime`. Отдельной ingestion path
или topology database нет. Normalized models не знают SNMP OID, REST endpoints
или protobuf types; wire mapping остаётся в `internal/transport/grpc`.

## Субъекты и источники

`IdentityEvidence.SubjectKind` различает `network_device`, `network_interface`,
`computer_system`, `chassis`, `manager`, `component`, `remote_neighbor`.
`SubjectID` задаёт субъект внутри source scope. Пустой `SubjectID` означает
владельца observation, например текущий `Device`, и сохраняет legacy semantics.
ID интерфейса, component ID, resource URI и Target ID не являются Backend Asset ID.

`SourceRef` сохраняет Collector ID, configured Target ID, source type, carrier
protocol, optional instance/resource и provenance. Например, LLDP через SNMP
имеет `type=lldp`, `protocol=snmp`; discovery protocol не заменяет carrier.
Ссылка `/topology/<observation_id>` обозначает локальное observation resource.
URI Redfish и `/ipmi/...` обозначают protocol resource в конкретном Target.
Одинаковые URI, MAC, hostname или ifName на разных Targets остаются разными facts.

Evidence содержит type/value, optional raw bytes/subtype, source, subject,
origin, время и normalization status. Origin указывает реальное свойство или
локальное основание: например `snmp.sysName`, `redfish.SerialNumber`,
`redfish.PermanentMACAddress`, `ipmi.fru.product.serial_number`.
`configured.management_address` и `configured.ipmi.endpoint` прямо обозначают
конфигурацию, а не подтверждённый адрес всех объектов за endpoint.

## Нормализация и сила evidence

| Identifier | Обработка |
|---|---|
| MAC | Canonical representation стандартного parser; EUI address lengths не смешиваются; invalid MAC не становится matching key |
| UUID | Проверенный canonical UUID; отсутствующий, nil/max или invalid UUID не заменяется новым |
| IP | IPv4/IPv6 canonical text; IPv4-mapped IPv6 сохраняется как IPv6; zones не принимаются как unscoped identity |
| Hostname/sysName/interface name | Проверенные UTF-8 и limits; case, trailing dot, Unicode и значащие символы сохраняются |
| Serial | Удаляются только крайние ASCII spaces; ведущие нули, case и punctuation сохраняются |
| LLDP binary ID | Raw bytes + subtype; `Value` содержит base64, а не выдуманный UUID/serial |

`canonical` означает успешную нормализацию. `OriginalValue` сохраняет безопасный
исходный текст, если он отличается. `invalid` сохраняет ограниченное исходное
значение как diagnostic, `rejected` заменяет hostile/oversized/secret input
безопасной diagnostic строкой. Эти два статуса нельзя использовать для matching.
Пустой status — legacy fact с неизвестным состоянием нормализации. Отсутствующее
поле не заполняется fake serial/UUID/IP; отсутствие evidence не означает deletion.

На wire единый domain list разделяется для compatibility: legacy
`Device.identity_evidence=41` / `TelemetrySample.identity_evidence=15` содержит
только usable Phase 5 types с absent/canonical normalization. Новые `ip`/
`interface_name` types и invalid/rejected diagnostics идут в additive
`Device.extended_identity_evidence=42` / `TelemetrySample.extended_identity_evidence=16`.
Old Backend не получает unknown types или diagnostic placeholders в прежних
matching lists. Backend Phase 6 сохраняет обе секции; decoder соединяет legacy,
затем extended в одну domain модель без merge конфликтующих facts. Neighbor20
целиком additive Phase 6 и содержит все его evidence. Порядок фактов внутри
каждой секции стабилен; между секциями identity трактуется как набор evidence.

Аппаратный UUID, serial или устойчивый component identifier могут быть сильнее
hostname/IP/MAC, но не гарантируют глобальную уникальность. Serial бывает ошибочным
или повторным, MAC — shared/virtual, hostname — одинаковым, management IP —
изменяемым. LLDP identity является объявлением удалённого устройства.
`Confidence` описывает качество конкретного evidence или local port mapping,
не является глобальной оценкой принадлежности Asset. Решение принимает Backend.

## Источники identity

| Источник | Фактически сохраняемые facts |
|---|---|
| SNMP v2c/v3 | sysName/hostname, sysObjectID, доступные ENTITY-MIB serial/model/manufacturer с component scope; MAC/name/IP уже прочитанных interfaces |
| RouterOS | system identity, реальный RouterBOARD serial/model, software/firmware fields существующего inventory; MAC/name/IP существующих interfaces |
| Linux/OpenWrt | Фактические local inventory fields и interface identity; отсутствие hardware serial/UUID допустимо |
| Redfish | Отдельные UUID/serial/manufacturer/model ComputerSystem, Chassis, Manager; resource URI и наблюдаемые relationships; EthernetInterface component current/permanent MAC и IP |
| IPMI | BMC numeric DeviceID/ManufacturerID/ProductID в VendorEvidence; FRU Product/Board/Chassis serial/manufacturer/model со своими component/resource subjects; SDR sensor identity |
| Discovery | Advertised chassis bytes/subtype, hostname и доступные management addresses на `remote_neighbor` subject |

RouterOS software ID не используется как hardware serial. SNMP не выполняет
дополнительный ENTITY-MIB inventory walk на каждом topology update. Interface
evidence переиспользует уже прочитанные snapshots; chassis serial не присваивается
произвольному interface.

Redfish endpoint может описывать несколько Systems/Chassis/Managers.
`Manager.UUID` относится к Manager, а `ComputerSystem.UUID` — к System.
EthernetInterface имеет собственный stable component ID, parent и source URI;
`MACAddress` и `PermanentMACAddress` сохраняются как два facts с разными origins.
Адреса EthernetInterface остаются interface facts; parent address aggregate
сохраняется для legacy потребителей. Resource links передаются только по
фактическим Redfish relationships. Поля основаны на
[DMTF schema index](https://redfish.dmtf.org/redfish/schema_index).

IPMI DeviceID/ProductID описывают controller, не серийный номер сервера.
ManufacturerID передаётся как numeric IANA evidence и не доказывает имя
производителя физического чипа. FRU Chassis serial остаётся на
`/ipmi/fru/<id>/chassis`; это не доказательство идентичности `/ipmi/chassis`.
Optional Get System GUID не добавлялся. Основание —
[IPMI v2.0 E6, разделы 20.1/20.8](https://www.intel.com/content/dam/www/public/us/en/documents/product-specifications/ipmi-v2-rev1-1-spec-errata-6-markup.pdf).

Совпавшие или конфликтующие Redfish/IPMI serial сохраняются независимо.
Одинаковый management IP не объединяет Systems, Chassis и BMC. Hardware
`BMC_MANAGES_SYSTEM`, `SYSTEM_IN_CHASSIS`, `BMC_MANAGES_CHASSIS` и component
parent relationships не выводятся на network map как кабели.

## Directed topology observations

`Neighbor.Kind=observed_l2_neighbor` описывает направление
`Local Device/Interface -> Advertised Remote Endpoint`. Две стороны соединения
передают две observations; Collector не превращает их в подтверждённый
bidirectional physical link. LLDP/CDP/MNDP могут проходить через bridge,
transparent или virtual resources, LAG и общий L2 segment. ARP, IP subnet и
общий management address не являются доказательством кабеля.
Концептуальный ориентир Node/Termination Point/direction —
[RFC 8345, раздел 4.4.5](https://www.rfc-editor.org/rfc/rfc8345#section-4.4.5);
NETCONF/YANG server не реализуется.

Observation ID — SHA-256 упорядоченного source/endpoint key: Collector, Target,
source type/carrier/instance, original collector, local device/interface/raw port,
remote chassis/port с subtypes и discovery protocol. Когда chassis отсутствует,
используются доступные advertised name/management addresses как fallback evidence.
Текущие timestamp, age, LLDP timeMark/remIndex и порядок Go map в key не входят.
ID повторяется при том же source evidence после poll/restart; изменившаяся
identity/mapping может дать другой ID. Runtime Collector binding пересчитывает
ID и производный resource URI. Это observation key, а не Asset ID.
Если у нескольких неизвестных neighbors вообще нет отличающего endpoint evidence,
Collector не может доказать различие физических субъектов.

## LLDP/CDP/MNDP fields

| Поле | SNMP LLDP-MIB | RouterOS `/ip/neighbor` |
|---|---|---|
| Local interface | Unique ifName/ifAlias/MAC/ifDescr mapping; ID/name/ifIndex/MAC/description/alias | Unique interface name -> RouterOS `.id`/name/MAC; master/slave ambiguity unresolved |
| Local raw port | Number, ID, subtype; number не равен ifIndex | API `interface`; subtype только при подтверждённом name mapping |
| Remote chassis/port | Реальные bytes/subtypes | Neighbor MAC с unknown chassis subtype; CDP `interface-name` как port; LLDP/MNDP port отсутствует |
| Name/description | sysName, port description, sysDescr | identity, platform/board/version; LLDP system-description |
| Management addresses | Optional bounded lldpRemManAddrTable с IANA family/raw octets/interface numbering | address/address4/address6 с canonical family; duplicate address within row удаляется |
| Capabilities | Supported/enabled LLDP BITS как bounded names; unknown bits сохраняются как `bit_N` | system-caps/system-caps-enabled только на LLDP observation |
| Protocol | LLDP | Отдельная observation для каждого discovered-by LLDP/CDP/MNDP; unknown допустим |

Подтипы chassis и port имеют разные enum meanings. MAC subtype и локальная
строка с теми же байтами не сравниваются без учета subtype. LLDP rows соединяются
по полному `timeMark.localPortNum.remIndex`; несколько neighbors на одном port
сохраняются. Optional management-address walk выполняется только в существующем
slow topology job, ограничен текущими SNMP response/table budgets и 16 адресами
на neighbor. Unsupported table означает unknown; transport/parser failure
делает snapshot partial и сохраняет остальные observations. OIDs и numbering
проверены по [IEEE LLDP-MIB](https://www.ieee802.org/1/files/public/MIBs/LLDP-MIB-200505060000Z.mib).

RouterOS использует тот же fixed read-only GET с расширенной `.proplist`;
отдельного engine или запроса к advertised addresses нет. LLDP-only capabilities
и description не приписываются CDP/MNDP. Generic поля агрегированной RouterOS
строки сохраняют origin API; API не гарантирует отдельное raw TLV origin для
каждого protocol. CDP здесь доступен через RouterOS, собственного Cisco CDP reader
нет. Generic `identity` не объявляется достоверным raw CDP Device-ID.
Подтверждённые поля — [RouterOS neighbor reference](https://manual.mikrotik.com/docs/cli-reference/ip/neighbor/).

## Время, expiry и completeness

`ObservationTime` сохраняет `source_timestamp`, если источник действительно его
сообщил, `observed_at`, `collected_at`, optional offset/uncertainty и clock quality.
`received_at` устанавливает Backend. Corporate NTP и Phase 1 time model
переиспользуются; нового clock exchange нет. Исторический inventory fact может
быть старше текущего health sample: его evidence time не заменяется временем
отправки или latest health poll.

`Neighbor.ObservedAt` у SNMP — время наблюдения Collector; RouterOS age корректирует
его на время с последнего discovery packet. `AgeSeconds` сохраняет reported age
с точностью поля seconds, а legacy observed time сохраняет доступную точность.
Age не является source wall-clock timestamp или advertised TTL.
`TopologyUpdate.TTLSeconds` и `Neighbor.ExpiresAt` используют configured collector
TTL/явный `ExpiryAssumption`. LLDP-MIB receiver TTL и RouterOS received protocol TTL
эти readers не предоставляют; explicit withdrawal не поддерживается.

Complete означает успешный bounded snapshot, partial — неполную collection,
failed — отсутствие успешного snapshot. Partial/failed не означают neighbor
removal и не удаляют прошлые links. Existing bounded volatile registry выдаёт
expiry event только когда complete snapshot не содержит прежней observation
и collector expiry уже наступил. Это expiry evidence, не подтверждение
отключённого кабеля. Memory eviction/restart не создают withdrawal.
Отсутствие serial/management address в partial poll не отменяет прежний fact.

## Provenance, security и доставка

Real/simulated provenance сохраняется отдельно от protocol. `source=simulated`
и `SourceRef.Provenance=simulated` проходят нормализацию, protobuf и lifecycle
events. Fixture/mock/reference BMC требует явно заданного simulated marker;
локальный тестовый endpoint сам по себе не превращает driver output в simulated.
Collector не классифицирует все данные как real или simulated по IP/hostname.

Syslog TLS `peer_ip`, `tls_identity`, `authenticated`, `managed` сохраняются
независимо от заявленного HOSTNAME. Authenticated peer не подтверждает все
identity claims в payload. Logs/audit, services/networks/firewall, telephony
и Events доставляют существующую source/time metadata через общий wire mapping.

Limits применяются до delivery: evidence — 64 facts на сочетание полного
SourceRef/SubjectID/SubjectKind и 4096 суммарно legacy+extended на owner;
topology neighbors — `remote.max_rows` (до 16384), management addresses —
16/neighbor, capabilities — 16/list, raw discovery IDs — 255 bytes в adapters
(legacy wire максимум 1024), management raw — 31 bytes, names — 255, descriptions —
4096. Hardware address/relationship/resource budgets сохраняются из Phase 4/5.
Collection-time limits для legacy names не ужесточают чтение старых durable
envelopes: historical wire `RemoteSystemName` до 4096 и прежние правила
`LocalInterfaceName`/local IDs сохраняются. Новые optional поля имеют свои bounds.
Derived identity builder ограничивает facts уже при построении: последний slot
total/subject budget резервируется для `rejected` diagnostic с origin
`collector.identity_evidence_limit` или `collector.identity_subject_limit`.
Interface inputs сортируются по stable ID, поэтому bounded subset детерминирован.
Такое диагностическое ограничение derived facts не удаляет явные source facts:
malformed или over-budget explicit list отклоняется при admission с ошибкой,
source attribution сохраняется. Diagnostic не является matching key.
Некорректные optional topology fields дают безопасный diagnostic и partial
snapshot, пригодные neighbors сохраняются. Unknown address families сохраняют
raw bytes без выдуманного IP. Identifiers не становятся shell/paths/SQL/URL.

Sanitizer не должен превращать скрытый secret в canonical matching key;
redacted identity становится `rejected`. Known secret в source/subject/resource
key вызывает безопасный reject envelope до outbox, поскольку переписывание
такого key исказило бы identity. Это не гарантия очистки неизвестных secrets.
Admission сохраняет существующие
message/byte/storage limits. Default `queue.max_message_bytes` — 1 MiB; topology
автоматически не разбивается на batches. Слишком большой envelope отклоняется
с diagnostic до admission, retained durable messages продолжают delivery.
Message IDs, ACK, replay, transport lifecycle, PKI и commands/rollback не меняются.
At-least-once относится к admitted retained durable messages, не к любым
непринятым observations; детали — [delivery](delivery.md).

## Проверки и ограничения

Unit/fixture coverage включает нормализацию/invalid/missing facts, отдельные
subjects, совпадающие/конфликтующие serial, duplicate MAC/name/IP разных sources,
stable/rebound topology IDs, raw subtypes, one-sided observations, missing ports,
LLDP management/capabilities, mixed RouterOS protocols, malformed/oversized input,
partial/failed semantics, simulation/time metadata и wire round-trip/replay.
Фактические результаты baseline, integration, E2E, race и cross-builds фиксируются
в [validation](validation.md); этот документ не заявляет live equipment PASS.

Подробный handoff с field numbers —
[Backend identity/topology contract](backend-identity-topology-contract.md).
Backend/PostgreSQL acceptance, реальные vendor firmware, физические кабели,
глобальное разрешение неоднозначных subjects и OpenWrt RAM/flash runtime
не подтверждаются unit/fixture tests. Данные остаются observations с provenance,
а Backend реализует confidence-based correlation и историческое хранение.
