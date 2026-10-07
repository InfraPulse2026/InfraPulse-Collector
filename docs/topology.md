# Физическая topology observations

Collector собирает LLDP-MIB через Generic SNMP и RouterOS neighbor discovery,
нормализует observations и передаёт `TopologyUpdate` через outbound gRPC.
Mock Controller показывает их в JSON. Global reconciliation графа, история
и Web UI остаются Backend responsibilities.

## Реальный pipeline

```text
SNMP LLDP-MIB / RouterOS HTTPS neighbors
                  |
                  v
bounded collection + timestamps + completeness
                  |
                  v
raw identifiers/subtypes + evidence local port mapping
                  |
                  v
TopologyUpdate + observation metadata -> sanitizer -> memory/durable outbox -> gRPC
                  |
                  v
Mock JSON / future Backend reconciliation
```

Topology использует отдельный slow interval (default 5m), не выполняет полный
neighbor walk каждые 3s. Remote scheduler ограничивает workers; каждый probe
имеет timeout, table/response limits и per-device exclusion с commands.

## Normalized contract

`TopologyUpdate`: stable local device ID, observed/collected timestamp, source,
complete/partial/failed, collector TTL seconds, expiry assumption и observations.
Каждый Neighbor сохраняет:

| Поле | Смысл |
|---|---|
| Local device/interface ID/name | Configured device и доказанный canonical interface; пустой interface допустим |
| Local port number/ID/subtype | Исходные LLDP local port сведения; number не ifIndex |
| Remote chassis bytes/subtype | Исходный advertised identifier; RouterOS MAC имеет unknown subtype |
| Remote port bytes/subtype | Advertised port identifier, если protocol/API его реально предоставляет |
| Remote system name/port description | Необязательные данные соседа |
| Protocol/source | LLDP, CDP, MNDP или unknown и фактический collector |
| ObservedAt/ExpiresAt | Время наблюдения и expiry по явно указанному предположению |
| Evidence/confidence | Основание mapping и high/medium/low/unresolved |

Raw bytes и subtypes не сводятся к одной строке: MAC, network address и locally
assigned identifier имеют разные значения. Remote hostname не stable unique ID;
неизвестный сосед остаётся unresolved advertised node.

## LLDP-MIB collection и local mapping

Driver читает local port table и remote table стандартной
[IEEE LLDP-MIB](https://www.ieee802.org/1/files/public/MIBs/LLDP-MIB-200505060000Z.mib).
Remote rows индексируются полным `timeMark.localPortNum.remIndex`, поэтому
несколько neighbors на одном port не объединяются случайно. `lldpStatsRemTablesLastChangeTime`
проверяется до/после collection; изменившаяся таблица даёт partial.

`lldpLocPortNum` не приравнивается `ifIndex`. Возможные mappings:

| LLDP evidence | Interface evidence | Confidence |
|---|---|---|
| interfaceName port subtype/ID | ifName | high |
| interfaceAlias port subtype/ID | ifAlias | high |
| macAddress port subtype/ID | ifPhysAddress | high |
| local port description | ifDescr | medium |

Mapping принимается только при единственном candidate. Коллизия, отсутствие
подходящего subtype/ID или metadata оставляют LocalInterfaceID пустым и
confidence=unresolved. Raw evidence сохраняется, interface не придумывается.
Complete empty snapshot требует доказательства доступной LLDP-MIB: local ports
или доступного last-change scalar. Отсутствие MIB не превращается в complete
empty graph. Missing/malformed identifiers и прерванный walk сохраняют partial
или failed semantics.

## RouterOS neighbors

Fixed GET `/rest/ip/neighbor` сохраняет `discovered-by` для LLDP/CDP/MNDP;
при отсутствии/неизвестном protocol применяется unknown. Несколько protocols
дают отдельные observations. `age` корректирует observed time, если доступен.
Unique interface name связывается с canonical RouterOS `.id`; неоднозначный
master/slave string остаётся unresolved.

Neighbor `mac-address` не гарантированно LLDP chassis ID: raw MAC сохраняется
с unknown subtype и отдельным слабым evidence. `interface-name` документирован
как CDP field и используется как remote port только для CDP. Отсутствующий
LLDP/MNDP port ID остаётся отсутствующим. LLDP/CDP mapped observations получают
medium confidence, MNDP — low: discovery на общем segment/bridge не доказывает
один point-to-point кабель. Details —
[RouterOS Neighbor discovery](https://help.mikrotik.com/docs/spaces/ROS/pages/24805517/Neighbor+discovery).

ARP/ND/FDB не являются источниками P1 physical observations и не кодируются
как discovery protocols. Они не доказывают прямой физический кабель.
PhysicalLocation задаётся config; LLDP/sysLocation не создают rack/room metadata.

## Completeness и lifetime

Complete означает успешный bounded snapshot; partial — есть observations,
но часть collection/mapping не подтверждена; failed — успешный snapshot
не получен. Все состояния передаются явно. Partial/failed не удаляют прежние
links. Complete empty означает отсутствие наблюдаемых neighbors на этом poll,
но сам по себе не отменяет collector TTL.

LLDP-MIB не предоставляет received neighbor TTL. `remote.topology_ttl`
(default 15m) — collector assumption, записанная в payload; это не advertised
LLDP TTL. RouterOS observation age и collector TTL также не подтверждают
remote protocol lifetime. Backend учитывает source/completeness/expiry
и выполняет global reconciliation.

Registry выдаёт `topology.link.observed` при появлении нового observation;
`topology.link.expired` — лишь когда complete snapshot не содержит ранее
наблюдавшийся link и его ExpiresAt уже наступил. Partial/failed не инициируют
expiry. Local dedup history ограничена `remote.max_rows`; при memory eviction
expiry event не выдаётся. История volatile, после restart/eviction observed
событие может повториться. Timestamp сохраняется после ожидания в очереди.
Memory outbox остаётся best effort; durable mode сохраняет retained envelopes
до negotiated application ACK, replay сохраняет message ID. Retention/eviction
могут потерять topology update/event; persistent outbox не делает registry
dedup history постоянной. Подробности — [delivery](delivery.md).

## Collector observation boundary

Discovery сохраняет существующий SNMP/RouterOS mapping. Remote target job
передаёт normalized `TopologyUpdate` в `ObserveEnvelope`, затем в существующий
outbox и gRPC mapping. Adapters не отправляют gRPC напрямую.

SourceRef содержит configured target и carrier (`snmp`/`routeros`). SNMP LLDP
snapshot имеет source type `lldp`; mixed RouterOS snapshot — `routeros`,
а вложенный Neighbor уточняет actual discovery source `lldp`/`cdp`/`mndp`,
когда он известен. Vendor не подменяет source. Provenance `simulated` сохраняется
отдельно от discovery protocol и в legacy source marker.

Normalized IdentityEvidence сохраняет raw remote chassis bytes и subtype,
remote system name как low-confidence hostname. Неизвестный subtype остаётся
unknown; RouterOS neighbor MAC не становится hardware UUID или подтверждённым
LLDP chassis ID. Local Target ID и observed local Device ID раздельны;
remote chassis evidence не выполняет Asset merge. ClockOffset/Uncertainty
по умолчанию отсутствуют; observed/collected times сохраняют прежние значения.
Полная новая metadata пока internal-only; существующие topology wire fields
остаются совместимыми. [Collector Core](collector-core.md), [wire gaps](protocol.md).

## Проверка и ограничения

Unit/fixture tests покрывают localPortNum≠ifIndex, unique ifName/MAC mappings,
unresolved port, raw subtypes, несколько neighbors, неизвестный remote,
partial/failed и complete empty. Registry tests проверяют expiry только после
complete snapshot и bounds history. Wire tests проверяют binary IDs, enums,
presence и реальную gRPC передачу в Mock Controller. Обычные tests не требуют
SNMP/MikroTik hardware. Фактические команды и результаты записаны в
[validation](validation.md).

Реальный equipment graph MikroTik→Eltex→server/AP отдельно не подтверждён.
SNMP Generic MIB fixtures не означают hardware support; Simulator topology
scenarios имеют explicit simulated source и не доказывают реальные links.
Неизвестные remote devices не объединяются по имени,
взаимное подтверждение observations и graph reconciliation выполняет Backend.
