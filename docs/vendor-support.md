# Поддержка платформ и производителей

Матрица отражает реализованные collectors и способ проверки. Generic MIB
support не подтверждает работу каждой модели/firmware vendor. Наличие
config hint, action в protobuf или driver package не объявляет capability:
remote capabilities появляются после наблюдения реальных операций.

## Реализованные пути

| Платформа / driver | Inventory / interfaces | Telemetry | Topology | Commands | Проверка |
|---|---|---|---|---|---|
| Local Linux | OS/kernel/architecture/boot identity; доступные DMI; interfaces/IP | CPU/load, RAM/swap/optional PSI; counters/rates | Unsupported | Unsupported | Procfs/sysfs fixtures и P0 WSL Linux amd64 runtime |
| Generic SNMP v2c | SYSTEM; optional ENTITY components; IF/IF-X index/name/description/alias/MAC/state/speed | HC counters при наличии, Counter32 fallback; availability/rates | LLDP-MIB, local mapping и unresolved evidence | Read-only, writes unsupported | Mapping fixtures и локальный UDP SNMP server |
| MikroTik RouterOS HTTPS REST | Identity/resource; optional RouterBOARD model/serial/firmware; interfaces и IPv4/IPv6 addresses | Interface counters/rates; реально возвращённые CPU/RAM metrics | Neighbor LLDP/CDP/MNDP/unknown observations | Typed interface enable/disable при permissions/policy/protection/verification | JSON fixtures и локальный HTTPS server; hardware не проверен |
| Local Windows | LocalPlatform unsupported | Unsupported | Unsupported | Unsupported | Collector hosting `local.enabled: false`; это не Windows monitoring |
| Local OpenWrt | Native adapter отсутствует | Native adapters отсутствуют | Native topology отсутствует | Unsupported | Linux cross-build не доказывает runtime конкретной firmware |

Current RouterOS REST рассчитан на RouterOS 7 с `www-ssl` и verified certificate.
Availability endpoints/properties зависит от версии и устройства; например,
`discovered-by` появился в RouterOS 7.7. Отсутствующие поля не угадываются.
Linux/SNMP/RouterOS interfaces используют один normalized model; RouterOS
CPU/RAM values передаются как typed Metric entries, без выдуманных Linux
load/swap/PSI semantics.

## Vendor evidence и profiles

| Vendor | Реальная P1 detection | Vendor-specific profile / hardware support |
|---|---|---|
| MikroTik | Verified SNMP PEN `.1.3.6.1.4.1.14988`; слабый literal MikroTik sysDescr; RouterOS REST response evidence | RouterOS driver реализован с fixtures; семейства hardware отдельно не подтверждены |
| Cisco | Только configured hint/raw sysObjectID evidence; автоматический profile отсутствует | P2 |
| Huawei | Только configured hint/raw sysObjectID evidence; автоматический profile отсутствует | P2 |
| Juniper | Только configured hint/raw sysObjectID evidence; автоматический profile отсутствует | P2 |
| Eltex | Только configured hint/raw sysObjectID evidence; автоматический profile отсутствует | P2 |
| QTECH | Только configured hint/raw sysObjectID evidence; автоматический profile отсутствует | P2 |

Стандартные SYSTEM/IF/IF-X/ENTITY/LLDP MIB можно собирать одним Generic SNMP
driver при их реальной доступности. Это не отдельные шесть проверенных profiles.
Hint не подтверждает manufacturer; hostname не используется для detection,
model/serial не выводятся из vendor. Без подтверждения Vendor остаётся пустым.
Source/strength evidence сохраняются. Verified MikroTik prefix подтверждён
[официальной SNMP документацией](https://help.mikrotik.com/docs/spaces/ROS/pages/8978519/SNMP).

## Capabilities и отсутствие данных

Linux объявляет `system.read`, `inventory.read`, `cpu.read`, `memory.read`,
`interfaces.read`, `telemetry.read`. P0 Linux scalar index/MTU/admin fields
при отсутствии sysfs metadata имеют 0/false и oper=unknown; nil metadata
не превращает их в доказательство отключения. Optional DMI/core IDs/PSI/speed
могут отсутствовать. Cross-build подтверждает компиляцию, не OpenWrt RAM/flash
или native UCI/ubus support.

SNMP объявляет наблюдённые `system.read`, `inventory.read`, `interfaces.read`,
`telemetry.read`, `topology.lldp`. ENTITY-MIB optional: только фактические
components, device model/serial лишь при единственной chassis. sysUpTime —
uptime management subsystem; отсутствие явно отмечено. HC counters
предпочитаются; unavailable поля сохраняют availability=false. Fallback packet
суммы width=0 не получают rate. Counter32 rates консервативны и могут отсутствовать
даже при доступном counter. SNMPv3 и SNMP SET не реализованы.

RouterOS объявляет наблюдённые `system.read`, `inventory.read`, `interfaces.read`,
`telemetry.read`, `topology.read`. `interfaces.enable/disable` требуют
per-device opt-in и подтверждённых user/group `read/write/api/rest-api` прав;
отсутствующая/hidden/RADIUS-only metadata оставляет unsupported. Commands
policy default-deny и protected aliases/dependencies применяются дополнительно.
Unknown/partial dependency metadata не позволяет mutation. Services/firewall
writes, configuration snapshots и rollback отсутствуют.

LLDP topology сохраняет raw IDs/subtypes, unresolved local port и partial/failed
snapshots. RouterOS neighbor MAC не гарантирует LLDP chassis ID, CDP/MNDP/общий
bridge segment не доказывают один физический кабель. Received LLDP TTL
не предоставляется MIB; collector TTL явно обозначен как assumption.

## Дальнейшие этапы и обновление матрицы

Native OpenWrt, проверенные vendor profiles, logs/audit/cursors,
firewall/services/SIP/PBX и Simulator остаются P2. Windows LocalPlatform,
SNMPv3, прочие протоколы по необходимости, durable queue/journal/enrollment
и rollback — отдельные последующие этапы.

Hardware integration должен запускаться отдельно от обычной suite. После live
проверки указываются конкретные model/firmware, доступные operations и способ
проверки; один fixture или cross-build не подтверждают весь vendor.
Фактическая validation история — [validation](validation.md).
