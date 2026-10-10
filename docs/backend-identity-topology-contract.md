# Collector → Backend: identity и topology contract

Документ предназначен для Backend B1 (protocol compatibility), B3 (Unified Asset
Model/SourceBindings), B7 (Global Physical Topology) и B8 (hardware/logs/telephony
projections). Описан фактически передаваемый `infrapulse.v1` contract Phase 6.
Backend source code в этой фазе не изменяется. Источник схемы —
[`agent.proto`](../api/proto/infrapulse/v1/agent.proto).

## 1. Boundary и правила приема

Collector собирает protocol facts и локально нормализует source observations.
Backend разрешает Assets, SourceBindings, global topology, merge/split,
supporting relationships и историю. Collector не передаёт Global Asset ID.
`Device.id`, `InventoryComponent.id`, `TargetID`, `ResourceID` и `observation_id`
имеют разные значения; ни один из них нельзя автоматически принять за Asset ID.

Все новые fields additive/optional, RPC signatures и старые номера не меняются.
Старый Backend может игнорировать additions; Backend с Phase 6 должен сохранять
их, не заставляя legacy Agent присылать отсутствующую metadata. Если
`ObservationMetadata` присутствует, внутри обязательны корректные source и time.
Пустой optional message — отсутствие knowledge; присутствующий incomplete
message не является корректной заменой legacy absence.

Device/Telemetry используют два typed identity lists. Старые fields передают
пригодные Phase 5 types, новые optional fields — Phase 6 types и diagnostics;
это wire compatibility partition единой domain `IdentityEvidence`, не две
модели identity. Backend Phase 6 обязан сохранять оба списка до projections.

Receipt/dedup key durable delivery — `(agent_id,message_id)` существующего
AgentEnvelope. Повторный `Neighbor.observation_id` является новой оценкой
состояния того же source observation, а не duplicate transport message.
ACK следует после application acceptance/commit всех поддерживаемых observations.
Replay сохраняет message ID и исходную observation metadata; session/sequence
и sent_at относятся к попытке отправки. Exactly-once contract отсутствует;
см. [delivery](delivery.md).

## 2. SourceRef и время

| Domain field | Protobuf field | Значение Backend |
|---|---|---|
| `SourceRef.Type` | `SourceRef.type = 1` | local/snmp/routeros/lldp/cdp/mndp/sip/syslog/auditd/redfish/ipmi: observation domain |
| `CollectorID` | `collector_id = 2` | Runtime Collector identity; scope вместе с authenticated sender/AgentHello |
| `TargetID` | `target_id = 3` | Configured polling/receiver target, не обнаруженный Asset |
| `Protocol` | `protocol = 4` | Carrier, например snmp/routeros/ipmi/tls; не заменяет Type |
| `InstanceID` | `instance_id = 5` | Optional source instance identifier |
| `Provenance` | `provenance = 6` | real/simulated/manual/external; пустое legacy значение означает отсутствующий explicit marker |
| `ResourceID` | `resource_id = 7` | Optional source-scoped resource URI; не глобальный ID и не URL для подключения |
| `ObservationMetadata.Source` | `ObservationMetadata.source = 1` | SourceRef владельца observation |
| `ObservationMetadata.Time` | `ObservationMetadata.time = 2` | Независимые clock/freshness сведения |
| `ObservationTime.SourceTimestamp` | `ObservationTime.source_timestamp = 1` | Optional timestamp источника; не создавать, когда protocol его не предоставляет |
| `ObservedAt` | `observed_at = 2` | Collector assessment времени наблюдаемого состояния |
| `CollectedAt` | `collected_at = 3` | Завершение/запись collection в Collector |
| `ClockOffset` | `clock_offset = 4` | Optional source clock minus Collector clock; present zero — measured zero |
| `Uncertainty` | `uncertainty = 5` | Optional неотрицательная +/- bound; absence неизвестна |
| `Quality` | `clock_quality = 6` | unknown/synced/acceptable/degraded/unsynced; legacy пустая quality допустима |

Backend устанавливает `received_at` самостоятельно. Нельзя подменять observed
или source timestamp временем приема/отправки. Коррекция wall clock может нарушать
порядок timestamps; Collector не требует `source <= observed <= collected`.
Inventory identity time может быть старше очередного hardware health sample;
Backend сохраняет time самого fact. Phase 2 NTP не означает измеренный source
offset для произвольного SNMP/BMC устройства.

Сохранять весь SourceRef. Для source binding использовать Collector/sender,
Target, protocol/domain и source resource/subject; дополнительно owner ID, если
resource URI отсутствует. Пустой Target/Collector из legacy fixture не достраивать
из advertised hostname/IP. В normal runtime Collector привязывает свою identity
перед admission. Нельзя разрешать новые сетевые соединения из ResourceID или
management-address evidence.

## 3. IdentityEvidence

| Domain field | Protobuf field | Наличие / meaning |
|---|---|---|
| `Type` | `IdentityEvidence.type = 1` | Обязателен у present fact: serial/hardware_uuid/mac/chassis_id/lldp_chassis_id/management_ip/hostname/sys_name/sys_object_id/manufacturer/model/ip/interface_name |
| `Value` | `value = 2` | Обязательный bounded текст; при raw bytes это base64 |
| `RawValue` | `raw_value = 3` | Optional исходные binary identifier octets |
| `Subtype` | optional `subtype = 4` | Optional chassis encoding; unknown 0 отличается от field absence; нельзя сравнивать bytes без типа |
| `Source` | `source = 5` | SourceRef конкретного свойства/resource |
| `Confidence` | `confidence = 6` | high/medium/low/unresolved — качество evidence, не global asset certainty |
| `SubjectID` | `subject_id = 7` | Optional local subject ID; empty = владелец содержащей observation |
| `SubjectKind` | `subject_kind = 8` | Optional additive subject category |
| `OriginalValue` | `original_value = 9` | Optional безопасное исходное значение при изменении normalization |
| `Origin` | `origin = 10` | Optional protocol property/локальное основание конкретного fact |
| `Time` | `time = 11` | Optional fact-specific ObservationTime, сохраняется независимо от latest sample time |
| `Normalization` | `normalization = 12` | canonical/invalid/rejected; empty legacy unknown |

Wire placement:

| Owner | Legacy usable facts | Extended facts/diagnostics |
|---|---|---|
| Device | `identity_evidence = 41` | `extended_identity_evidence = 42` |
| TelemetrySample | `identity_evidence = 15` | `extended_identity_evidence = 16` |
| Neighbor | Нет legacy typed identity list | `identity_evidence = 20` содержит все Phase 6 types/statuses |

Legacy lists содержат только Phase 5-known types из таблицы выше без `ip` и
`interface_name`, с normalization absent/canonical. Новые `ip`/`interface_name`
и любые `invalid`/`rejected` facts размещаются только в extended lists. Поэтому
old strict decoder не получает unknown type, а Backend, игнорирующий новый
normalization marker, не принимает diagnostic placeholder за identifier.
Новый decoder соединяет legacy, затем extended в один domain list, сохраняя
порядок внутри partitions, source/origin и conflicting facts; локального
Asset merge или discard facts при этом нет. Между partitions порядок не имеет
семантики: identity evidence трактуется как набор facts. Обе wire секции надо
сохранять независимо от поддержки конкретного identifier type в projection.

Subject kinds: `network_device`, `network_interface`, `computer_system`,
`chassis`, `manager`, `component`, `remote_neighbor`. Backend обязан сохранять
kind и scoped subject, не переносить component serial на System или BMC UUID
на сервер. Empty `subject_id` вместе с kind относится к owner Device/current
resource. Remote neighbor имеет `subject_id=neighbor:<observation_id>` внутри
source scope; это unresolved advertised subject.

`canonical` означает успешный parser/безопасную normalization. `invalid` и
`rejected` — diagnostics, не candidates для matching; их нельзя считать
serial/UUID/MAC, даже если Value напоминает identifier. Safe `OriginalValue`
сохраняется для проверки; hostile/oversized или redacted secrets не передаются
туда. Empty normalization у legacy fact нельзя считать доказанной canonical form.

MAC canonical representation сохраняет address-length distinctions. UUID
валидируется без генерации отсутствующего значения. IPv4/IPv6 включая mapped
IPv6 различаются. Hostname/sysName и serial сохраняют case, punctuation,
ведущие нули и trailing dot; удаляются только безопасные крайние ASCII spaces.
Несколько identical facts внутри observation могут быть deduplicated; different
origin/subject/protocol или конфликтующее value остаются отдельными evidence.
Это не Asset merging и не отменяет прежние факты при partial poll.

## 4. Audit mappings по категориям

| Domain category / field | Protobuf field | Backend expected meaning |
|---|---|---|
| Inventory batch `Observation` | `InventoryUpdate.observation = 4` | Source/time snapshot; observed_at1 и devices2 legacy; completeness3 не задаёт deletion |
| Device `Observation`, `IdentityEvidence` | `Device.observation = 40`, `identity_evidence = 41`, `extended_identity_evidence = 42` | Owner source identity; обе typed identity sections составляют scoped facts |
| Device legacy identity | `id1`, `hostname3`, `management_address4`, `vendor5`, `model6`, `serial_number8`, `chassis_id16`, `source33`, `sys_object_id25` | Сохраняются для старых consumers; typed evidence уточняет origin/subject/time |
| Hardware device/component | `Device.hardware = 39`, `InventoryComponent.hardware = 12`, `observation = 13` | Отдельная identity/resource категория и component provenance |
| Hardware UUID | `HardwareInventory.hardware_uuid = 2` | UUID этого resource type1; не всегда System UUID |
| Hardware relationship | `HardwareInventory.relations = 21` → `ResourceRelationship.type1,target_resource_id2` | Source-reported relationship к resource URI того же Target |
| Component parent | `InventoryComponent.parent_id = 2`, `id = 1` | Наблюдаемое component containment; не сетевой кабель |
| Telemetry `Observation`, `IdentityEvidence` | `TelemetrySample.observation = 14`, `identity_evidence = 15`, `extended_identity_evidence = 16` | Source/time sample; обе typed identity sections составляют interface/hardware facts |
| Metric source/resource | `Metric.resource_id = 4`, `component_id = 5`, `status = 6`, `observation = 7` | Реальный metric resource/component и время конкретного measurement |
| Hardware health | `TelemetrySample.hardware_health = 13` → `ComponentHealth.id1,resource_id2,status4,observation5` | Health-only resource допустим без числовой metric |
| Event provenance/time | `Event.observation = 8` | Lifecycle source metadata, включая simulated; legacy timestamp4/source7 сохраняются |
| Logs/audit batch/record | `LogBatch.observation = 4`, `LogRecord.observation = 19`, `audit = 17` | Receiver/local source/time; AuditRecord host10 — source claim |
| Syslog transport attribution | `LogRecord.syslog = 18` → `SyslogMetadata.peer_ip7,transport8,tls_identity9,authenticated10,managed11` | Peer evidence отдельно от `LogRecord.host = 9` |
| Topology batch | `TopologyUpdate.observation = 9` | Source/time всей collection; вложенный Neighbor уточняет discovery protocol |
| Topology observation | `Neighbor.observation = 19`, `identity_evidence = 20` | Source/time/subject отдельного directed neighbor fact |
| Local services | `Service.observation = 7` | Source/time service inventory |
| Local networks/firewall | `NetworkState.observation = 9`, `FirewallSnapshot.observation = 9` | Existing local source/time snapshot |
| Telephony | `TelephonyUpdate.observation = 4`, `TelephonyHealth.observation = 12` | SIP source/time, без inference физического topology link |

Hardware Type/resources, interfaces, capabilities, vendor evidence и unknown
availability остаются typed fields прежнего contract. Source/time теперь
передаются там, где internal metadata раньше терялась. Общего JSON blob для
identity/topology вместо этих typed fields нет.

## 5. Hardware/source subject mappings

| Source fact | Subject / origin | Что нельзя выводить |
|---|---|---|
| SNMP sysName/sysObjectID | network_device; `snmp.sysName`, `snmp.sysObjectID` | sysName не глобально уникален |
| ENTITY-MIB serial | component/chassis subject; `snmp.entPhysicalSerialNum` | Несколько chassis не выбираются произвольно |
| SNMP/RouterOS interface MAC/name/IP | network_interface, local interface ID | Одинаковое имя/MAC в другом Target не тот же субъект |
| RouterBOARD serial | network_device; `routeros.system.routerboard.serial-number` | RouterOS software-id не serial |
| Redfish System/Chassis/Manager UUID/serial | computer_system/chassis/manager с отдельными source URIs | Endpoint Target не означает один System или один Asset |
| Redfish Ethernet current/permanent MAC | network_interface component; `redfish.MACAddress` / `redfish.PermanentMACAddress` | Current и permanent address не заменяют друг друга |
| Redfish Ethernet IP | network_interface; `redfish.IPv4Addresses.Address` / `redfish.IPv6Addresses.Address` | Parent aggregate не доказывает management IP всех resources |
| IPMI ManufacturerID | manager; `ipmi.get-device-id.manufacturer_id`, Value `iana:<number>` | Не verified vendor name физического чипа |
| IPMI DeviceID/ProductID | `Device.vendor_evidence29`, numeric `ipmi.get-device-id.*` origins | Не Server SerialNumber или hardware UUID |
| IPMI FRU Board/Product/Chassis serial | component/component/chassis на `/ipmi/fru/<id>/<area>` | FRU Chassis не автоматически `/ipmi/chassis` |
| IPMI SDR sensor | Existing stable component/record identity | Sensor identity не serial всего оборудования |

Redfish relations (`SYSTEM_IN_CHASSIS`, `CHASSIS_CONTAINS_SYSTEM`,
`BMC_MANAGES_SYSTEM`, `BMC_MANAGES_CHASSIS`, `MANAGED_BY_BMC`) и IPMI BMC/chassis
relationship сохраняются независимо от network neighbors. Основание hardware
semantics — [DMTF schema index](https://redfish.dmtf.org/redfish/schema_index)
и [IPMI v2.0 E6](https://www.intel.com/content/dam/www/public/us/en/documents/product-specifications/ipmi-v2-rev1-1-spec-errata-6-markup.pdf).
GUID command не добавлен. Redfish/IPMI facts с равным serial, конфликтующими
serial или одинаковым endpoint IP поступают отдельно; Backend решает SourceBinding.

## 6. TopologyUpdate и Neighbor

`TopologyUpdate`: device_id1, observed_at2, collected_at3, source4,
completeness5, ttl_seconds6, expiry_assumption7, observations8, observation9.
Completeness — complete/partial/failed. `TTLSeconds` — collector assumption,
не received LLDP/CDP/MNDP TTL. Поля 1–8 сохраняют legacy meaning.

| Domain field | `Neighbor` protobuf field | Наличие / meaning |
|---|---|---|
| LocalDeviceID | local_device_id1 | Обязательный local owner в batch scope |
| LocalInterfaceID/Name | local_interface_id2/name3 | Optional подтверждённый local mapping; empty допустим |
| LocalPortNumber/ID/Subtype | local_port_number4/id5/subtype6 | Raw local protocol evidence; port number не обязательно ifIndex |
| RemoteChassisID/Subtype | remote_chassis_id7/subtype8 | Optional raw advertised endpoint identity + encoding; unknown0 сохраняется |
| RemotePortID/Subtype | remote_port_id9/subtype10 | Optional raw endpoint port; отсутствие не заменяется fake port |
| RemoteSystemName | remote_system_name11 | Optional advertised name, не resolved Asset |
| RemotePortDescription | remote_port_description12 | Optional advertised description |
| Protocol | protocol13 | LLDP/CDP/MNDP/UNKNOWN enum; source protocol distinctions сохраняются |
| Source | source14 | Legacy collector source marker |
| ObservedAt/ExpiresAt | observed_at15/expires_at16 | Observed required; expiry optional, Collector assumption |
| Evidence/Confidence | evidence17/confidence18 | Mapping/protocol evidence; не глобальная asset certainty |
| Observation/IdentityEvidence | observation19/identity_evidence20 | Optional additive source/time и remote subject facts |
| ObservationID | observation_id21 | Stable source-scoped SHA256 observation key; новый poll не новый Asset |
| Kind | kind22 | `observed_l2_neighbor`; empty legacy treated as unspecified semantics |
| LocalInterfaceIndex | optional local_interface_index23 | Present positive ifIndex; может меняться после reboot |
| LocalInterfaceMAC | local_interface_mac24 | Optional canonical local address; не уникален глобально |
| LocalInterfaceDescription/Alias | local_interface_description25/alias26 | Optional дополнительные local mapping facts |
| RemoteManagementAddresses | remote_management_addresses27 | Bounded advertised list; не создавать Target/credentials/connection |
| RemoteSystemDescription | remote_system_description28 | Optional LLDP system description |
| RemotePlatform/Model/SoftwareVersion | remote_platform29/model30/software_version31 | Optional RouterOS neighbor claims |
| RemoteCapabilitiesSupported/Enabled | remote_capabilities_supported32/enabled33 | Optional bounded capability names; LLDP semantics |
| AgeSeconds | optional age_seconds34 | Reported age since last discovery packet, не TTL; present0 отличается от absence |

`ManagementAddress`: address_family1 — IANA number (IPv4=1, IPv6=2), address2 —
canonical IP для known family, raw_address3 — исходные octets,
optional interface_subtype4/interface_id5 — LLDP numbering evidence.
Unknown AF сохраняет только raw, без выдуманного IP. Raw/family/subtype обязательны
для интерпретации; семейство mapped IPv6 остается IPv6. Capability unknown bit
сохраняется как `bit_N`, unknown RouterOS bounded name также допустим.

SNMP сохраняет LLDP chassis/port subtype, sysName/sysDescr/port description,
capabilities и optional management table, а local mapping допускается только
при единственном candidate. RouterOS сохраняет отдельный Neighbor для каждого
`discovered-by`; neighbor MAC имеет unknown chassis subtype. CDP port приходит
из документированного `interface-name`, LLDP/MNDP port неизвестен.
Generic RouterOS identity не объявляется raw CDP Device-ID; собственного Cisco
CDP collector нет. LLDP-only description/capabilities не приписываются CDP/MNDP.
Primary mappings: [IEEE LLDP-MIB](https://www.ieee802.org/1/files/public/MIBs/LLDP-MIB-200505060000Z.mib),
[RouterOS neighbor reference](https://manual.mikrotik.com/docs/cli-reference/ip/neighbor/).

## 7. Direction, lifetime и conflict policy

Observation направлена `local device/interface -> advertised remote endpoint`.
Она не доказывает direct cable, ни reciprocal observation, ни resolved Asset.
Две стороны или два Collector дают независимые records. Backend может построить
SourceBindings/TopologyEdge и позднее подтвердить bidirectionality, сохраняя
supporting observations. Направление согласуется с концепцией
[RFC 8345](https://www.rfc-editor.org/rfc/rfc8345#section-4.4.5), но Collector
не предоставляет NETCONF/YANG server и не вычисляет глобальный graph.

Stable ID учитывает Collector/Target/source type/carrier/instance/legacy source,
local device/interface/raw local port и remote raw identity/subtypes/protocol.
Timestamp, age, LLDP timeMark/remIndex не участвуют. Unknown chassis использует
имеющиеся name/addresses как fallback; полностью неразличимые anonymous endpoints
нельзя уверенно разделить. Runtime Collector binding пересчитывает key;
производный URI `/topology/<hash>` обновляется. Это не generation random Asset ID.

Сохранить TTL assumption/expiry вместе с observation. RouterOS age означает
время после последнего packet и может сдвигать observed_at назад; advertised
TTL текущие readers не получают. Explicit withdrawal unsupported: нет field,
который мог бы достоверно его объявить. TTL=0 в configured assumption не следует
интерпретировать как protocol shutdown packet.

Failed/partial poll не удаляет прошлые links/facts. Complete empty показывает
отсутствие neighbors в успешном snapshot, но не подтверждает кабельное removal.
Existing registry expiry event появляется только после complete absence и
достижения прежнего collector expiry. Event — источник freshness evidence,
не global topology deletion command. Registry history volatile/bounded; eviction
и restart не создают negative observations. Backend сохраняет conflicting facts
с origin/time и не заменяет хороший serial пустым полем partial poll.

## 8. Provenance и Syslog

Explicit `simulated` передается в SourceRef и legacy source marker; lifecycle
events также сохраняют marker. Real/simulated не выводится из адреса endpoint:
SNMP/RouterOS/Redfish/IPMI fixture должен явно установить test provenance.
Manual/external являются допустимыми existing provenance значениями, но readers
не создают их автоматически. Новый Backend сохраняет provenance каждого fact,
а legacy string остается для старых consumers.

Syslog HOSTNAME и audit host — заявленные payload fields. TLS authenticated peer
identity/peer IP/transport относятся к receiver attribution и не являются
доказательством достоверности всех имен/serial в сообщении. Их нужно сохранять
раздельно. Fixture replay или Simulator не должен создавать real production
Asset projection без явно выбранной Backend policy.

## 9. Bounds и delivery compatibility

Identity: максимум 4096 facts суммарно legacy+extended на owner и 64 на сочетание
полного SourceRef, SubjectID и SubjectKind (не 64 для каждой wire section),
Value/OriginalValue4096, Origin256, raw identity bytes1024, scoped SubjectID256.
Derived facts ограничиваются до cloning/encoding; последний slot total/subject
budget занят `normalization=rejected` diagnostic с origin
`collector.identity_evidence_limit` или `collector.identity_subject_limit`.
Interface input сортируется по stable ID для детерминированного bounded subset.
Backend сохраняет diagnostic вместе с SourceRef и subject, не используя его
для resolution. Явный malformed/over-budget source list не удаляется молча:
admission отклоняет такой envelope с безопасной ошибкой.
Topology: configured `remote.max_rows` (до16384), 16 addresses/neighbor,
16 capabilities/list, raw discovery IDs255 в adapters/1024 legacy wire,
management raw1..31, collected names255, descriptions4096, age до365days.
Limits новых collectors не заменяют historical validation retained legacy records:
`remote_system_name` до4096 и прежние правила local interface name/IDs сохраняются
при отсутствии additions. Новые optional text fields ограничены отдельно.
Hardware relationships остаются bounded1024, address lists64 и configured
Redfish/IPMI discovery limits. Malformed text/UUID/MAC/IP и controls дают
диагностику; не превращать diagnostic в matching key.

Существующие response/timeout/scheduler limits применяются к extra optional
LLDP management table. RouterOS расширяет `.proplist` того же fixed GET.
Read-only discovered addresses не вызывают network requests.
Sanitizer перед outbox заменяет redacted evidence на `rejected`, сохраняет
source attribution, но не обещает очистку любого неизвестного secret pattern.
Known secret в source/subject/resource key вызывает безопасный reject envelope
до admission: переписывание source identity создало бы ложное сопоставление.

Default queue message budget — 1MiB; configurable предел до4MiB. Automatic
topology splitting отсутствует. Oversized/invalid batch отклоняется до
admission с безопасным diagnostic и не блокирует уже retained queue records.
Обновление protobuf не меняет outbox message IDs, ACK/replay, transport lifecycle,
PKI, command journal или rollback. Backend должен принимать новые optional fields
в тех же envelopes и сохранять metadata при собственных projections/replay.

## 10. Known gaps и требования к реализации Backend

- Legacy agents могут не передавать metadata, normalization, subjects и stable
  topology observation IDs; не достраивать их из hostname/IP как verified facts.
- Нет global matching/merge, Collector Asset ID, reciprocal-link confirmation,
  physical location inference или local topology database.
- Received protocol TTL, explicit withdrawal и raw CDP Device-ID через текущий
  RouterOS API не подтверждены; native Cisco CDP reader не добавлен.
- LLDP optional management data может быть unavailable; unknown AF не дает IP.
  Management OID column не превращается в новую hardware relation.
- Generic RouterOS multi-protocol row не предоставляет точное происхождение
  каждого raw TLV; evidence сохраняет API carrier и actual discovery marker.
- IPMI GUID отсутствует без optional separate command; DeviceID/ProductID не
  substitutes serial. Redfish URI/UUID не гарантируют единственную physical asset.
- Identity/interface IDs стабильны при том же protocol input/scope; ifIndex,
  hostname, endpoint address и firmware resource names могут изменяться.
- Durable delivery at-least-once и bounded retention; Mock ACK — development
  receipt, не доказательство production PostgreSQL commit.

Backend B1 сохраняет typed additions, обе identity sections и legacy compatibility;
B3 использует
scoped subjects для SourceBindings без переноса идентификаторов между BMC/System/
Chassis; B7 строит edges на supporting directed observations с freshness/conflict
policy; B8 сохраняет hardware/log/telephony source/time distinctions. Реальные
baseline/regression/E2E результаты и live limitations находятся в
[validation](validation.md), не выводятся из наличия schema или fixture.
