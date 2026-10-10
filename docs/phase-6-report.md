# Phase 6 — Identity & Physical Topology Enrichment: отчёт

Дата: 2026-10-10. Этап обогащает существующие observations Collector/Edge Gateway.
Backend source code, global Asset Registry и Entity Resolution не добавлены.
Phase 7 не начиналась. Проверки относятся к development fixtures и локальному
Linux runtime; production Backend/PostgreSQL и реальные удалённые устройства
не участвовали.

## 1. Git state и базовый commit

Начальные `git status`, `git branch --show-current`, `git branch -vv` и
`git log -5 --oneline` показали чистый `main` с HEAD `14abc4e`:
`feat(agent): реализован аппаратный мониторинг IPMI в Phase 5`.
Phase 5 уже была сохранена в Git; незакоммиченные изменения этой фазы отсутствовали.
Из этой базы создана `feat/phase-6-identity-topology`. HEAD не изменён, Phase 6
остаётся в рабочем дереве. Commit/push/merge/rebase/reset/clean/stash не выполнялись.
Отдельный linked worktree не создавался.
Индекс пуст: `git diff --cached --name-only` не вывел файлов. go.mod/go.sum
не изменены; `.cache`, `.tools`, `bin`, `state` не входят в tracked files.

## 2. Baseline до изменений

SDK — `go1.27.1 windows/amd64`, module `InfraPulseAgent`, Go 1.27.
Production builds CGO0; Windows race CGO1 с portable LLVM-MinGW в ignored `.tools`.
Baseline logs — ignored `.cache/phase6-baseline/`.

| Проверка | Фактический результат |
|---|---|
| `gofmt -l` raw workspace | Непустой список 24 Phase 5 файлов с CRLF; не заявляется PASS |
| `gofmt -l` LF scratch copy | PASS, 242 Go files; других formatting gaps нет |
| `go build ./...` | PASS, 4,886 s |
| `go vet ./...` | PASS, 1,243 s |
| `go test -count=1 ./...` | PASS, 14,207 s |
| `go test -race -count=1 ./...` | PASS, 43,950 s |
| `go mod verify` | PASS |
| `go test -count=1 -p=1 -tags=integration ./...` | PASS, 67,883 s |
| Native/cross build scripts, protobuf scratch SHA256 | PASS |
| Config examples | PASS, 11 YAML examples |
| Compiled Linux suites в WSL | PASS, 18 packages |

IPMI regression включена в обычные, race, integration и Linux suites: secure
RMCP+/LANplus reference BMC, FRU/SDR/sensors, cache, simulated lifecycle и replay.
Baseline Linux stripped binary — 25 948 320 bytes; unstripped — 38 257 990 bytes.
Linux `time-sync-status` вернул exit0/unavailable (`systemctl` отсутствует);
live corporate NTP synchronization baseline не подтверждена.

## 3. Existing architecture

Переиспользованы `IdentityEvidence`, `SourceRef`, `ObservationMetadata`,
`ObservationTime`, hardware models, `InventoryBatchReader`, `HardwareReader`,
SNMP/RouterOS topology readers и registry completeness/expiry semantics.
Pipeline остаётся `Adapters → normalized observations → ObserveEnvelope →
Agent.push → Sanitizer → durable outbox → gRPC → application ACK`.
Core не получает OID/REST endpoints или concrete protobuf types.

## 4. Закрытые gaps

Generic inventory/telemetry metadata и identity evidence были построены в domain,
но wire mapping передавал их только для hardware. Topology и lifecycle Event
теряли source/time metadata; services/networks/firewall/SIP также теряли её.
LLDP management addresses, capabilities/sysDescr и local interface признаки
не доходили до observation. RouterOS optional neighbor facts терялись.
Redfish Manager/Chassis UUID и собственные Ethernet interface subjects не
сохранялись полностью. Новые identity types/diagnostics потребовали отдельного
optional wire extension для strict Phase 5 decoder. Эти gaps закрыты additive.

## 5. Domain models

`IdentityEvidence` расширена subject kind, original value, origin, fact time и
normalization status; существующие Source/SubjectID/RawValue/Subtype сохранены.
Добавлены evidence types `ip` и `interface_name`.
`Neighbor` расширен stable ObservationID, Kind, local ifIndex/MAC/description/alias,
typed management addresses, remote description/platform/model/software,
capabilities и reported age. Новые поля optional; новых Asset IDs нет.
Hardware relationships остаются прежними typed relations, не network links.

## 6. Нормализация идентификаторов

Helpers используют стандартную библиотеку и уже существующий UUID dependency.
MAC canonical сохраняет 6/8/20-byte distinctions; UUID валидируется, nil/max UUID
не становятся аппаратной identity. IPv4, IPv6 и mapped IPv6 различаются, zones
не превращаются в глобальную IP identity. Serial сохраняет ведущие нули,
case и punctuation; hostname сохраняет case/trailing dot/Unicode. Удаляются
только безопасные крайние ASCII spaces. DNS/network I/O и UUID generation нет.
Malformed bounded значения становятся `invalid`, oversized/control/invalid UTF-8
— bounded `rejected` diagnostic. Diagnostics не являются matching keys.

## 7. SNMP identity evidence

Сохранены sysName/sysObjectID, profile/vendor facts, ENTITY-MIB component serial/model
и interface MAC/name/IP из уже выполняемого inventory read. Sole ENTITY chassis
serial/model передаются на chassis subject; legacy projected Device fields
остаются совместимыми. Enrichment не повторяет IF-MIB/inventory polling.
SNMP v2c/v3 transport и commands не менялись.

## 8. RouterOS identity evidence

Используются system identity, RouterBOARD model/serial, interface MAC/name/IP и
software version существующего adapter. Leading-zero serial сохраняется;
software-id не становится serial. Дополнительного inventory engine, write
operation или изменения rollback не добавлено.

## 9. Redfish identity evidence

ComputerSystem, Chassis, Manager имеют собственные stable IDs и source resource
URIs. UUID/serial/manufacturer/model/firmware относятся к своему resource.
EthernetInterface — bounded component с parent relationship, own URI и отдельными
current/permanent MAC origins, собственными IP facts. Endpoint IP не переносится
на все Systems/Chassis. Multi-resource discovery и existing stable IDs сохранены.

## 10. IPMI identity evidence

ManufacturerID/ProductID/DeviceID остаются BMC facts; DeviceID не становится
Server Serial. FRU product/board/chassis serial имеют разные component/chassis
subjects и resource keys. SDR sensor identity сохранена. Hardware GUID command
и fake UUID не добавлены; LANplus security/session implementation не менялась.

## 11. Local interface identity

Передаются подтверждённые local interface ID/name, ifIndex, MAC, description/alias,
а raw local port number/subtype/ID сохраняются отдельно. `lldpLocPortNum` не
объявлен ifIndex. Ambiguous bridge/LAG mappings остаются unresolved. Interface
IDs scoped Target/Collector; ifIndex не заявляется постоянным hardware identifier.

## 12. LLDP/CDP/MNDP topology

LLDP chassis/port octets и subtypes, sysName/sysDescr/port description,
capabilities и management IPv4/IPv6/unknown-family facts сохраняются.
Optional bounded LLDP management-table walk добавлен только в slow topology job.
RouterOS расширяет существующий GET, сохраняет отдельный Neighbor для каждого
`discovered-by` protocol, platform/board/version/age/capability claims.
RouterOS generic identity не объявлена raw CDP Device-ID. CDP remote port
используется только при фактическом `interface-name`; отсутствующие ports остаются
неизвестными. Отдельный direct Cisco CDP reader не добавлен.

## 13. Directed link semantics

`Kind=observed_l2_neighbor`: local interface → advertised unresolved endpoint.
Два наблюдения с разных сторон/Collectors остаются независимыми. Stable SHA256
key включает source scope, local mapping, protocol и raw remote identifiers/subtypes;
не содержит current timestamps/timeMark/remIndex. Runtime Collector binding
пересчитывает предварительный adapter key детерминированно. ARP, subnet и
hardware containment не создают кабели или verified bidirectional links.

## 14. TTL/expiry

Existing `observed_at`, `expires_at`, batch `ttl_seconds`/`expiry_assumption` и
registry protection сохранены. LLDP-MIB не предоставляет received neighbor TTL;
expiry здесь — configured Collector assumption. RouterOS age не является TTL.
Failed/partial poll не подтверждает removal; missing field не является deletion.
Новых explicit withdrawal semantics или выдуманного source timestamp нет.

## 15. Conflicts и partial data

Разные serial/MAC/chassis facts, subjects, origins и protocols не заменяют друг
друга. Exact dedup выполняется только внутри observation/collection cycle.
Derived facts bounded при построении: total4096, full-source/subject64, включая
reserved diagnostic. Совпадающие total/per-subject limits переиспользуют diagnostic.
Explicit over-budget facts не удаляются молча: admission отклоняет batch с
безопасной ошибкой. Отдельной растущей identity/topology database нет.

## 16–18. SourceRef, provenance и время

Передаются CollectorID, configured TargetID, discovery Type, carrier Protocol,
InstanceID, source ResourceID и real/simulated/manual/external provenance.
Resource/Subject IDs не становятся Backend AssetID. Simulated fixtures маркируются
явно; локальный loopback endpoint сам по себе не считается simulated.
SNMP/RouterOS fixtures, Redfish mockup, IPMI reference BMC, Syslog и Simulator
проверяют provenance; simulated lifecycle metadata теперь достигает protobuf.
Syslog authenticated peer/TLS identity сохранена отдельно от claimed HOSTNAME.
Phase 1 observed/collected/source timestamp/clock quality и Phase 2 NTP model
переиспользованы. Fact-specific time не заменяется поздним telemetry/send time.
Backend самостоятельно задаёт received_at; clock exchange не добавлен.

## 19–20. Protobuf mapping и Backend compatibility

Полный Domain → Protobuf → meaning audit:
[Backend contract](backend-identity-topology-contract.md).

| Категория | Поля |
|---|---|
| Identity additions | subject_kind8, original_value9, origin10, time11, normalization12 |
| Inventory | Device.observation40; identity_evidence41; extended_identity_evidence42; InventoryUpdate.observation4 |
| Telemetry | observation14; identity_evidence15; extended_identity_evidence16; existing metric/component metadata |
| Topology | TopologyUpdate.observation9; Neighbor observation19/evidence20/ID21/kind22 и fields23–34 |
| Event/local/SIP | Event.observation8, Service7, NetworkState9, FirewallSnapshot9, TelephonyHealth12, TelephonyUpdate4 |
| Logs/audit | Existing batch4/record19 и Syslog TLS peer metadata без смены meanings |

Device41/Telemetry15 содержат usable Phase 5-known types; Device42/Telemetry16
— `ip`/`interface_name` и `invalid`/`rejected` diagnostics. Old decoder безопасно
игнорирует additions. Новый decoder объединяет legacy затем extended в ту же
domain `IdentityEvidence`; evidence order не означает приоритет. Общие лимиты
проверяются по обоим спискам вместе. RPC, старые field numbers/types/oneofs/enums
и legacy meanings сохранены; pinned generated files воспроизводимы.
Backend должен сохранять оба списка, source/subject/origin/time/raw subtypes,
не использовать diagnostics для matching и выполнять глобальный SourceBinding
самостоятельно. Обязательной новой metadata от legacy Agent нет.

## 21. Durable delivery и безопасность admission

Outbox/message IDs/session/ACK/replay/PKI/commands/journal/rollback не переписаны.
Перед admission выполняются existing sanitizer, source-key secret checks и
transport mapping/size/protobuf serialization validation. Secret-bearing source
IDs/relationship URIs отклоняются, не переписываются в ложную identity.
Redacted facts сохраняются как diagnostics. Один malformed/oversized envelope
не попадает в retained queue; следующий valid envelope принимается.
Default JSON outbox max message — 1 MiB, transport size limit сохраняется.
Topology batch splitting отсутствует; over-limit snapshot отклоняется с error.
ACK/replay относится к accepted retained at-least-once messages, не exactly-once.

## 22–24. Unit, cross-source и E2E

Unit/fixture tests покрывают missing/invalid UUID/serial, leading zeroes,
duplicated MAC, одинаковые hostname/IP разных targets/resources, stable IDs,
ownership и temporal/provenance metadata. Cross-source tests сохраняют независимые
SNMP/RouterOS/Redfish/IPMI observations, overlapping и conflicting serials.
Topology tests покрывают raw subtypes, local interfaces, multiple/unknown
neighbors, missing ports, management AF, capabilities, protocol distinctions,
directionality, partial/failure, stable restart IDs, hostile input и limits.
Wire tests проверяют nil/optional additions, legacy descriptor и dynamic Phase 5
decoder, strict legacy identity semantics, round-trip и simulation markers.

`TestPhase6AdaptersIdentityTopologyDurableReplayACK` использует реальные Go
SNMP/RouterOS/Redfish/IPMI adapters с protocol fixtures: 13 source observations,
mixed LLDP/CDP/MNDP, unresolved ports и same-IP/same-serial hardware subjects.
EmitTarget/Agent.push → durable records без ACK → restart → replay тех же IDs и
payloads → Mock Controller application ACK → удаление accepted records.
Это fixture E2E; production Backend/PostgreSQL persistence и реальные устройства
не проверены. Existing Simulator/Collector→Mock, Syslog TLS/audit и delivery E2E
включены в full suites.

Read-only environment check обнаружил локальный listener5432, но доступный
configured production Backend endpoint/credentials не подтверждены; рядом в
GolandProjects находится только Agent repo, Backend environment bindings отсутствуют.
PostgreSQL connection и записи не выполнялись. Наличие DB listener само по себе
не позволяет выполнить Backend application E2E.

## 25–26. Final regression и cross-build

Final logs и exact commands находятся в ignored `.cache/phase6-final/`.
После security review fixes выполнены:

| Проверка | Результат |
|---|---|
| `gofmt -l` api/cmd/internal | PASS, 262 files, пустой список; CRLF baseline нормализован |
| `go build ./...` | PASS, 2,702 s |
| `go vet ./...` | PASS, 0,898 s |
| `go test -count=1 ./...` | PASS, 15,558 s |
| `go test -race -count=1 ./...` | PASS, 41,488 s |
| `go mod verify` | PASS |
| `go test -count=1 -p=1 -tags=integration ./...` | PASS, 68,747 s |
| `scripts/build.ps1`, `scripts/build.ps1 -Cross` | PASS |
| Pinned protobuf scratch regeneration + SHA256 | PASS |
| `validate-config` по 11 configs | PASS |
| 18 compiled integration-tagged Linux suites | PASS |
| Source hashes до/после проверки | PASS, 262 files / 0 mismatches |
| Separate Phase 6 fixture E2E | PASS, 13 observations replayed/ACKed |
| Markdown links и `git diff --check` | PASS, 10 documents / 219 local links / 0 broken |

Linux packages: platform/linux, logs, queue, commands, protocol/snmp,
driver/snmp, driver/routeros, driver/redfish, driver/ipmi, pki, platform/openwrt,
agent, simulator, telephony, transport/grpc, config, platform, syslogreceiver.
Linux `time-sync-status` exit0/unavailable: systemctl отсутствует, live sync
не заявляется. Tests external Redfish endpoint пропущены без configured endpoint.
Source code совпадает во всех final suites, cross-build и measurements.

Full suites включают SNMP v2c/v3, RouterOS commands/rollback, LLDP/CDP/MNDP,
SIP OPTIONS, Linux/OpenWrt adapters, Syslog TLS/TCP и audit, NTP fixtures,
Redfish multi-resource, IPMI secure RMCP+, credentials/PKI, scheduler, Simulator,
durable ACK/replay и coexistence local/remote logs.
Integration использует `-p=1` для воспроизводимости Windows cleanup.
Linux suites cross-compiled CGO0 и реально выполнены в WSL `docker-desktop`.
Cross-build — Collector linux/amd64, arm64, arm GOARM=7, mips/mipsle hardfloat;
Mock Controller/Simulator/Mock Enrollment linux/amd64 и arm64. Это compilation
matrix, не runtime certification других architectures/OpenWrt firmware.

## 27. Binary, heap, allocations и outbox pressure

Matched Linux amd64 CGO0: `-trimpath -buildvcs=false`, stripped с `-ldflags='-s -w'`.

| Binary | Phase 5 baseline | Phase 6 | Рост |
|---|---:|---:|---:|
| Stripped | 25 948 320 | 26 108 064 | 159 744 bytes / 0,62% |
| Unstripped | 38 257 990 | 38 463 748 | 205 758 bytes / 0,54% |

`BenchmarkObserveTelemetry`: baseline 433,8 ns/op / 1152 B/op / 8 allocs;
Phase 6 6832 ns/op / 8494 B/op / 38 allocs. Теперь benchmark создаёт scoped
interface facts; рост 7342 B/op и 30 allocs отражает дополнительную работу,
не заявляется performance parity.

`BenchmarkTopologyEnrichment`: 1 neighbor 2080 ns / 1633 B / 13 allocs;
128 — 279 166 ns / 209 050 B / 1664 allocs;
1024 — 2 013 749 ns / 1 672 777 B / 13 315 allocs.

| Wire workload | Encode ns/op | B/op | allocs/op | JSON bytes | Protobuf bytes |
|---|---:|---:|---:|---:|---:|
| 1 legacy | 2568 | 1952 | 21 | 954 | 309 |
| 1 enriched | 16 733 | 8384 | 80 | 4133 | 1852 |
| 64 legacy | 112 048 | 75 408 | 657 | 37 163 | 11 255 |
| 64 enriched | 981 764 | 432 373 | 4055 | 218 624 | 102 511 |
| 1024 legacy | 1 660 487 | 1 193 755 | 10 262 | 591 063 | 180 196 |
| 1024 enriched | 9 304 876 | 6 855 024 | 64 540 | 3 490 274 | 1 639 388 |

1024 enriched fixture neighbors превышают default 1 MiB JSON admission budget.
Это size measurement, не успешная admission этого snapshot. Default limit
сохранён; автоматического splitting нет.

`TestResourceUsageBoundedIdentityTopologyAdmission` — PASS: 100 cycles ×64 neighbors,
910,333 ms, TotalAlloc339 278 296 bytes, retained heap с outbox +1 537 408 bytes,
после Close +1 489 104 bytes, goroutine delta0; peak5 messages /951 005 bytes
при max32 messages/1 MiB, JSON190 085 bytes, priority eviction95 records.
Heap after Close включает still-referenced Agent/queue; освобождение всех объектов
после удаления Agent references и isolated Collector RSS не измерялось.

Тот же synthetic registry probe 32 fake SNMP targets /4workers: active peak4,
goroutines1→7→1 (baseline1→6→1), queue peak32 messages /41 400 bytes
(baseline31 048), heap354 520→648 368 bytes (baseline354 696→642 024),
TotalAlloc work4 842 000 bytes (baseline2 888 288). Finite bounded workload
сохранил limits; single peak goroutine sample не равен leak.

Redfish50poll:273,7534 ms, heap+136 632 bytes (baseline233,7423 ms/+86 744),
goroutine delta0, request peak1, plan23; inventory JSON23 856 bytes
(baseline19 739), sample max11 768 bytes (baseline11 338), metrics9.
IPMI50poll:80,1469 ms, heap+183 040, TotalAlloc25 071 112, goroutine delta0,
socket peak1, opened51/closed51/activeBMC0, maxsession7105 bytes;
inventory JSON14 338, maxsample6511 bytes, metrics4/health6.
IPMI cached benchmark10 314 ns/12 137 B/113 allocs; native reference BMC
1 454 488 ns/443 598 B/4326 allocs. IPMI Windows resource numbers здесь final;
отдельного matched Windows IPMI microbenchmark baseline Phase 6 не запускалось.

Measurements — single-host samples; fixture/reference server и Collector
работают в том же process. Это не изолированные Collector RSS, production
throughput или статистически подтверждённое ускорение/замедление.
Default outbox budgets не увеличены. Новых module dependencies и mandatory CGO нет.

## 28. Изменённые файлы

Line endings исходных CRLF Go files Phase 5 нормализованы gofmt без изменения их логики. Ниже приведены файлы с содержательным Git diff и новые файлы этапа.

- [api/gen/infrapulse/v1/agent.pb.go](../api/gen/infrapulse/v1/agent.pb.go)
- [api/proto/infrapulse/v1/agent.proto](../api/proto/infrapulse/v1/agent.proto)
- [docs/architecture.md](../docs/architecture.md)
- [docs/backend-identity-topology-contract.md](../docs/backend-identity-topology-contract.md)
- [docs/identity-topology.md](../docs/identity-topology.md)
- [docs/phase-6-report.md](../docs/phase-6-report.md)
- [docs/protocol.md](../docs/protocol.md)
- [docs/security.md](../docs/security.md)
- [docs/topology.md](../docs/topology.md)
- [docs/validation.md](../docs/validation.md)
- [docs/vendor-support.md](../docs/vendor-support.md)
- [internal/agent/agent.go](../internal/agent/agent.go)
- [internal/agent/identity_admission_test.go](../internal/agent/identity_admission_test.go)
- [internal/agent/identity_resource_usage_test.go](../internal/agent/identity_resource_usage_test.go)
- [internal/agent/phase6_e2e_test.go](../internal/agent/phase6_e2e_test.go)
- [internal/agent/routeros_e2e_test.go](../internal/agent/routeros_e2e_test.go)
- [internal/driver/ipmi/adapter.go](../internal/driver/ipmi/adapter.go)
- [internal/driver/ipmi/identity_phase6_test.go](../internal/driver/ipmi/identity_phase6_test.go)
- [internal/driver/redfish/adapter.go](../internal/driver/redfish/adapter.go)
- [internal/driver/redfish/client.go](../internal/driver/redfish/client.go)
- [internal/driver/redfish/identity_phase6_test.go](../internal/driver/redfish/identity_phase6_test.go)
- [internal/driver/redfish/redfish_test.go](../internal/driver/redfish/redfish_test.go)
- [internal/driver/routeros/identity_phase6_test.go](../internal/driver/routeros/identity_phase6_test.go)
- [internal/driver/routeros/routeros.go](../internal/driver/routeros/routeros.go)
- [internal/driver/routeros/topology_enrichment_test.go](../internal/driver/routeros/topology_enrichment_test.go)
- [internal/driver/routeros/topology.go](../internal/driver/routeros/topology.go)
- [internal/driver/snmp/identity_phase6_test.go](../internal/driver/snmp/identity_phase6_test.go)
- [internal/driver/snmp/lldp.go](../internal/driver/snmp/lldp.go)
- [internal/driver/snmp/snmp.go](../internal/driver/snmp/snmp.go)
- [internal/driver/snmp/topology_enrichment_test.go](../internal/driver/snmp/topology_enrichment_test.go)
- [internal/model/identifier_test.go](../internal/model/identifier_test.go)
- [internal/model/identifier.go](../internal/model/identifier.go)
- [internal/model/identity_evidence_test.go](../internal/model/identity_evidence_test.go)
- [internal/model/identity_evidence.go](../internal/model/identity_evidence.go)
- [internal/model/identity_limits_phase6_test.go](../internal/model/identity_limits_phase6_test.go)
- [internal/model/observation.go](../internal/model/observation.go)
- [internal/model/observe.go](../internal/model/observe.go)
- [internal/model/remote.go](../internal/model/remote.go)
- [internal/model/topology_evidence_test.go](../internal/model/topology_evidence_test.go)
- [internal/model/topology_evidence.go](../internal/model/topology_evidence.go)
- [internal/protocol/routeros/client_test.go](../internal/protocol/routeros/client_test.go)
- [internal/protocol/routeros/client.go](../internal/protocol/routeros/client.go)
- [internal/sanitize/hardware_test.go](../internal/sanitize/hardware_test.go)
- [internal/sanitize/hardware.go](../internal/sanitize/hardware.go)
- [internal/sanitize/identity_test.go](../internal/sanitize/identity_test.go)
- [internal/sanitize/identity.go](../internal/sanitize/identity.go)
- [internal/transport/grpc/client.go](../internal/transport/grpc/client.go)
- [internal/transport/grpc/collector_compatibility_test.go](../internal/transport/grpc/collector_compatibility_test.go)
- [internal/transport/grpc/hardware_mapping_test.go](../internal/transport/grpc/hardware_mapping_test.go)
- [internal/transport/grpc/hardware_mapping.go](../internal/transport/grpc/hardware_mapping.go)
- [internal/transport/grpc/identity_legacy_partition_test.go](../internal/transport/grpc/identity_legacy_partition_test.go)
- [internal/transport/grpc/identity_topology_mapping_test.go](../internal/transport/grpc/identity_topology_mapping_test.go)
- [internal/transport/grpc/legacy_topology_compatibility_test.go](../internal/transport/grpc/legacy_topology_compatibility_test.go)
- [internal/transport/grpc/mapping.go](../internal/transport/grpc/mapping.go)
- [internal/transport/grpc/observation_metadata.go](../internal/transport/grpc/observation_metadata.go)
- [internal/transport/grpc/p2_mapping.go](../internal/transport/grpc/p2_mapping.go)
- [internal/transport/grpc/phase6_compatibility_test.go](../internal/transport/grpc/phase6_compatibility_test.go)
- [internal/transport/grpc/remote_mapping.go](../internal/transport/grpc/remote_mapping.go)
- [internal/transport/grpc/testdata/phase5-agent.desc](../internal/transport/grpc/testdata/phase5-agent.desc)
- [internal/transport/grpc/testdata/phase5-contract.md](../internal/transport/grpc/testdata/phase5-contract.md)
- [internal/transport/transport.go](../internal/transport/transport.go)
- [README.md](../README.md)

## 29. Known limitations

Global Asset Registry/Entity Resolution/physical cable confirmation остаются
за Backend. LLDP expiry assumption не measured TTL; explicit withdrawal и
direct Cisco CDP Device-ID/TTL отсутствуют. Unknown endpoints допустимы.
Нет automatic Redfish↔IPMI merge или fallback. Topology snapshots автоматически
не split; enrichment увеличивает payload, oversized batch отвергается до queue.
Sanitizer распознаёт known secrets/patterns, не гарантирует очистку всех данных.
Реальные BMC/network equipment/vendor firmware, production Backend/PostgreSQL,
live corporate NTP, physical cables и OpenWrt runtime/RAM/flash не проверены.
IPMI Phase 5 и Redfish Phase 4 protocol limitations остаются в своих документах.

## 30. Readiness for Phase 7

Phase 6 contract готовит source observations для Backend B1/B3/B7/B8.
Все перечисленные final проверки прошли; handoff contract, documentation и resource measurements готовы. Git publication требует отдельного запроса пользователя.
Backend должен реализовать сохранение новых optional fields и application ACK
после acceptance/commit по собственному контракту. Phase 7 backend-managed
configuration не начиналась и не добавлена в scope этого этапа.

## Подготовленный текст commit message

```text
feat(agent): обогащены identity evidence и наблюдения L2-топологии

Идентичность и происхождение:
- Переиспользованы IdentityEvidence, SourceRef и temporal models.
- Добавлены subject kind, origin, original value и normalization diagnostics.
- Сохранены раздельные SNMP/RouterOS interfaces, Redfish System/Chassis/Manager
  и IPMI BMC/FRU subjects без локального сопоставления Assets.
- Добавлены безопасные MAC/UUID/IP/hostname/serial helpers и bounded facts.

Topology и протокол:
- Добавлены стабильные source-scoped directed observation IDs, local interface
  metadata, LLDP management addresses/capabilities и RouterOS neighbor facts.
- Сохранены raw chassis/port subtypes, неизвестные endpoints и expiry assumptions.
- Закрыты protobuf gaps source/time/provenance; новые evidence types и diagnostics
  вынесены в optional fields для совместимости со strict Phase 5 decoder.
- Добавлены admission size/serialization и known-secret source URI checks;
  existing durable delivery, message IDs и ACK/replay сохранены.

Документация и проверки:
- Подготовлены identity/topology описание, Backend handoff и отчёт Phase 6.
- Обновлены README, architecture, protocol, support matrix, security и validation.
- Выполнены gofmt, go build ./..., go vet ./..., go test -count=1 ./...,
  go test -race -count=1 ./... и go mod verify.
- Пройдены integration suites, 18 Linux suites в WSL, cross-build matrix,
  validation 11 configs и воспроизводимая pinned protobuf generation.
- Пройден fixture E2E четырёх adapters с 13 observations, restart/replay и Mock ACK.
- Измерены binary size, heap, allocations, goroutines, topology cost и outbox pressure.

Ограничения:
- Реальные устройства, Backend/PostgreSQL, live NTP и OpenWrt runtime не проверены.
- Expiry остаётся Collector assumption; direct CDP Device-ID/TTL и withdrawal нет.
- Автоматического topology batch splitting и global Asset Resolution нет.
```

Текст не означает выполненный commit или разрешение на Git publication.
