# Поддержка платформ и производителей

Матрица отражает текущую реализацию и фактически выполненные проверки.
Cross-platform описывает ОС запуска Collector; multi-vendor — удалённые устройства,
опрашиваемые через protocol drivers. Наличие profile, config hint или action
в protobuf само по себе не объявляет capability.

Collector / Edge Gateway сохраняет существующие protocols и capability checks.
SourceRef и identity evidence обозначают происхождение данных; vendor profile
не является source или подтверждением hardware identity. Collector Core
подготавливает adapter registration для будущих sources, не добавляя их
capabilities автоматически. [Domain model](collector-core.md).

## Уровни подтверждения

- **Generic SNMP compatible** — устройство предоставляет необходимые standard
  SYSTEM/ENTITY/IF/IF-X/LLDP MIB; availability проверяется при опросе. Это не
  подтверждение всех моделей и firmware производителя.
- **Fixture verified** — mapping/schema и protocol behavior проверены fixtures
  и локальными test servers. Schema/registry fixtures не являются captures
  реального оборудования.
- **Runtime verified** — указанная программа или collector выполнены в
  названной ОС. Запуск test server в Linux не подтверждает firmware устройства.
- **Hardware verified** — проверены конкретные устройство, model, firmware и
  operations. В текущей validation такого подтверждения сетевого оборудования нет.
- **Unsupported** — функция не реализована или запрещена выбранным driver.
- **Unavailable** — реализованный read не дал достоверных данных на текущем
  источнике: missing file, permissions, timeout, optional MIB/ubus object.
  Unavailable не является измеренным нулём или доказательством stopped/offline.

## Реализованные компоненты

| Компонент | Фактические функции | Fixture verified | Runtime verified | Hardware / внешние системы |
|---|---|---|---|---|
| Local Linux | Identity, OS/kernel/architecture/uptime, optional DMI, CPU/load, RAM/swap/optional PSI, interfaces/IP/counters/rates | Isolated procfs/sysfs fixtures | WSL Linux amd64: реальные `/proc`, `/sys`, interface addresses и Agent/Mock stream | WSL runtime; сетевое оборудование не требуется и не проверялось |
| Corporate NTP host setup | Linux chrony/timesyncd deployment apply; bounded read-only status; OpenWrt status и administrator UCI setup | Isolated provider/config/ownership/idempotency fixtures; результаты — в Phase 2 validation | Live chrony/timesyncd synchronization и OpenWrt runtime не подтверждены | NTP удалённых устройств и Backend protocol не реализуются; [границы](time-sync.md) |
| Native OpenWrt | Linux collector плюс board/release evidence, netifd logical networks, procd services, UCI firewall configuration read | Official implementation schema fixtures; timeout/cancellation/partial availability и настоящий wire encode/decode | **OpenWrt runtime unverified**: доступная WSL среда не содержит OpenWrt release/ubus | OpenWrt router/firmware и RAM/flash consumption не проверены |
| Linux system logs | Configurable regular files, syslog/rsyslog/raw parsing, sanitizer, bounded batches, persistent cursors, rotation/truncate/recreate | Temp files, burst/rotation/restart/errors/cancellation | WSL Linux amd64: настоящие file reads, rotation, cursor restart и gRPC delivery | Production rsyslog deployment не проверен; paths задаются оператором |
| Remote Syslog/Audit Receiver | Opt-in TLS/mTLS RFC5425, RFC5424, базовый RFC3164, explicit TCP octet-counting/LF, source mapping, existing LogBatch/durable delivery | Parser/framing/TLS/limits/admission fixtures; exact commands в validation | Isolated rsyslog 8.2512.0 sender в WSL, TLS/TCP → Collector → development Mock; synthetic provenance | UDP/RELP отсутствуют; live auditd dispatcher и production Backend не проверены; sender→Collector без application ACK |
| auditd file source | Sanitized raw records, exact event ID/time/type, известные uid/auid/pid/exe/syscall/success/result; bounded correlation внутри batch | Audit syntax fixtures и Agent/Mock delivery | Linux file pipeline выполнен; содержимое audit records в tests — fixtures | Реальный audit daemon/audit rules не проверены |
| SIP OPTIONS | Fixed unauthenticated UDP/TCP/TLS probe, matching final response, availability/latency/error category/state transitions; TLS CA/name verification и SNI | Локальные SIP/TLS servers: 2xx, 401/407, 404, 5xx, timeout/malformed/cancel/recovery, wrong CA/name/expired certificate | WSL Agent/Mock с локальным UDP SIP fixture; результаты остальных P3 Linux fixtures — в validation | Внешний PBX и media не проверены; Digest/REGISTER/media не реализованы |
| Generic SNMP v2c/v3 | Один adapter: SYSTEM, optional ENTITY inventory, IF/IF-X counters/rates и LLDP-MIB; v3 USM authPriv/authNoPriv | Standard MIB fixtures, локальный UDP SNMP server, настоящий BER/USM exchange, discovery/reconnect/invalid credentials/redaction | Linux amd64 protocol/E2E tests с test server; это не switch runtime | Реальное SNMP оборудование не проверено; SNMP SET unsupported |
| RouterOS HTTPS REST | Identity/resource/RouterBOARD, interfaces/IP/counters/rates, neighbor observations, typed interface enable/disable, persistent interface snapshots и typed rollback с checks/readback | JSON/HTTPS fixtures с test CA, snapshot/restart/rollback/protection/conflict/uncertain write и Agent/Mock E2E | Linux amd64 protocol/E2E tests с test server | MikroTik hardware/firmware не проверены; fixture не является hardware validation |
| Redfish HTTPS read-only | Multi-System/Chassis/Manager discovery, hardware/component inventory/status, modern Sensor/ThermalSubsystem/PowerSubsystem и legacy Thermal/Power; verified TLS и session/basic auth | Bounded HTTPS schema/auth/TLS/error fixtures, multi-resource Collector durable restart/ACK; official DMTF Mockup Server/Interface Emulator → Mock E2E | Python DMTF tools реально выполнены с custom CA и simulated provenance; cross-build не означает BMC/OpenWrt runtime | Настоящие BMC/Dell/HPE и production Backend/PostgreSQL не проверены; OEM/actions отсутствуют; IPMI — отдельный explicit adapter; [contract](redfish.md) |
| Durable delivery | Memory либо bounded bbolt outbox, stable message IDs, negotiated application ACK, replay/reconnect | Temp storage, checksums/schema/corruption/limits/retention, process-crash boundaries и gRPC ACK fixtures | Development suite; конкретная Linux runtime validation описана отдельно | Нет power-loss/flash endurance validation; Mock receipt не является production Backend commit |
| IPMI v2 LANplus read-only | Explicit UDP targets; Device ID, multiple FRU areas, bounded Full/Compact SDR, analog/discrete sensors, chassis; общие hardware models | Reference BMC + FRU/SDR/sensor fixtures; corrupt/auth/replay/loss/security/limits; mixed Redfish/IPMI и durable restart/ACK, full unit/race PASS | Native UDP reference-BMC suites выполнены Windows/WSL Linux amd64; resource probe в одном process с BMC/helper | Production BMC/OpenWrt unverified; suites 17/3, USER/OPERATOR; no v1.5/OEM/IPMB bridging/power control; [IPMI](ipmi.md), [проверки](validation.md) |
| Command safety state | Bounded persistent journal/results, dedup/recovery без автоматического retry uncertain mutation, RouterOS interface snapshots | Atomic state/storage faults, restart, journal/ACK/replay и RouterOS HTTPS rollback fixtures | Development suite; конкретная Linux runtime validation описана отдельно | External mutation exactly-once не заявляется; полное configuration backup отсутствует |
| Agent PKI | Local ECDSA key/CSR, HTTPS enrollment, URI Agent identity binding, mTLS renewal/atomic rotation | Local CA/HTTPS/gRPC fixtures: invalid/expired/reused token, wrong identity/CA, renewal/reconnect и failed renewal | Agent ↔ development Mock E2E; среда и команды — в validation | Production CA service, tenancy, revocation, TPM/HSM не реализованы |
| Development Simulator | Отдельный executable, deterministic scenarios/trends, ordinary normalized envelopes через InfraPulse gRPC, `source=simulated` | Determinism/scenario/transport tests, Simulator ↔ Mock gRPC E2E | WSL Linux amd64 suite и отдельные Windows amd64 CLI processes Simulator/Mock | Synthetic demonstration не подтверждает оборудование или production collectors |
| Local Windows | LocalPlatform **unsupported**; development tools продолжают собираться | Windows suite проверяет portable core/protocol и file fixtures | Windows tests не являются Windows local monitoring validation | Windows LocalPlatform — post-MVP/backlog, не критерий готовности P3 |

Полные результаты, команды, cross-build targets и способ runtime-проверки
приведены в [validation](validation.md). Поддержка architecture при компиляции
не доказывает runtime совместимость конкретной OpenWrt firmware или приемлемое
потребление RAM/flash. Production Agent не импортирует Simulator.

## Шесть vendor profiles

Profiles являются detection-only правилами над Generic SNMP, а не шестью
отдельными drivers. Enterprise roots подтверждены IANA PEN registry; matching
учитывает границы OID arcs. Namespace evidence не доказывает подлинность
оборудования и не предоставляет дополнительных capabilities.

| Profile | Подтверждённый sysObjectID namespace | Fixture verification | Hardware verification |
|---|---|---|---|
| Cisco | `.1.3.6.1.4.1.9` — [IANA PEN 9](https://www.iana.org/assignments/enterprise-numbers/?page=1) | Registry semantics, positive/negative/missing/malformed/unknown product и Generic fallback | Не выполнена |
| Huawei | `.1.3.6.1.4.1.2011` — [IANA PEN 2011](https://www.iana.org/assignments/enterprise-numbers/?page=21) | Registry semantics, positive/negative/missing/malformed/unknown product и Generic fallback | Не выполнена |
| MikroTik | `.1.3.6.1.4.1.14988` — [IANA PEN 14988](https://www.iana.org/assignments/enterprise-numbers/?page=150) | Registry semantics и Generic fallback; RouterOS REST проверяется отдельно protocol fixtures | Не выполнена |
| Juniper | `.1.3.6.1.4.1.2636` — [IANA PEN 2636](https://www.iana.org/assignments/enterprise-numbers/?page=27) | Registry semantics, positive/negative/missing/malformed/unknown product и Generic fallback | Не выполнена |
| Eltex | `.1.3.6.1.4.1.35265` — [IANA PEN 35265](https://www.iana.org/assignments/enterprise-numbers/?page=353) | Registry semantics, positive/negative/missing/malformed/unknown product и Generic fallback | Не выполнена |
| QTECH | `.1.3.6.1.4.1.27514` — [IANA PEN 27514](https://www.iana.org/assignments/enterprise-numbers/?page=276) | Registry semantics, positive/negative/missing/malformed/unknown product и Generic fallback | Не выполнена |

Fixtures содержат подтверждённые registry semantics и специально заданные
test values; это не captures устройств. У всех profiles проверено отсутствие
ложной capability. Vendor-specific health/inventory OID не реализованы: P3
не добавляет mappings без официальной MIB, понятной semantics и воспроизводимой
fixture.
Model, serial, software и firmware остаются стандартным ENTITY-MIB fallback
при доступности однозначной chassis; неизвестные значения не угадываются.

Eltex и QTECH используют тот же стандартный MIB fallback. Наличие российского
vendor profile не заявляет CPU/temperature/fan/PSU/optics mappings и не снижает
требования к официальной MIB/evidence/fixtures.

Configured hint сохраняется отдельным слабым evidence и не подтверждает vendor.
P1 literal MikroTik `sysDescr` fallback сохранён с `confidence=low`. Hostname
не используется для detection. Primary evidence и scope каждого profile
описаны в [vendor profiles](vendor-profiles.md).

## Availability и границы функций

Linux объявляет `system.read`, `inventory.read`, `cpu.read`, `memory.read`,
`interfaces.read`, `telemetry.read`. При configured file sources добавляются
`logs.read` и для auditd — `audit.read`; unavailable source виден в diagnostics.
File-based logs не включают journald API, OpenWrt logd/
`logread` ring buffer и compressed archives. На OpenWrt требуется реальный
настроенный log file, если нужен этот путь сбора.

Native OpenWrt выбирается по distribution evidence, не hostname. При missing
ubus или malformed/timeout read обычные Linux measurements сохраняются;
`services.read` и `firewall.read` отражают успешный read соответствующего source.
При отсутствии rpcd UCI object firewall unavailable. Services сохраняют только
registered procd instances; отсутствие instances означает unknown.
`services.enabled=true` требует подтверждённой boot `S??` symlink; отсутствие
ссылки и procd `autostart` не доказывают disabled/enabled. Firewall inventory
отражает configured UCI sections, не проверенный active nftables ruleset;
snapshot-level enabled остаётся unknown. Service/firewall mutations и отдельный
`health.read` collector отсутствуют. Подробнее — [OpenWrt](openwrt.md).

Telephony объявляет `telephony.options`/`telephony.health` для configured probes,
а при TLS endpoint — `telephony.options.tls`. TLS проверяет trusted CA/server name
и использует SNI; production bypass отсутствует.
2xx означает успешный OPTIONS; высокая latency даёт degraded. 401/407 означает
reachable и authentication_required, остальные non-2xx — отдельный error
category. SIP failure меняет только telephony state, не remote device online
и не interface state. Digest/REGISTER/calls/RTP/media/PBX statistics отсутствуют.
Подробнее — [telephony](telephony.md).

Generic SNMP объявляет только наблюдённые standard reads. `v2c` сохраняет
compatibility default; явно выбранный `v3` использует default `authPriv`,
`SHA256`/`AES`. `authNoPriv` и legacy algorithms требуют явной настройки;
`noAuthNoPriv` не поддерживается. Все credentials идут через provider, mode
не добавляет business capability. Подробнее — [SNMPv3](snmpv3.md). ENTITY optional;
sysUpTime относится к management subsystem. HC counters предпочтительны,
недоступные поля сохраняют availability=false, неоднозначные Counter32 и
unknown-width aggregate rates могут отсутствовать. Новые interfaces обнаруживаются
на inventory refresh. Remote polling и ответы ограничены.

RouterOS REST ориентирован на RouterOS 7 с `www-ssl` и проверяемым server
certificate. Endpoints/properties зависят от version/device; missing fields
остаются unavailable. CPU/RAM передаются как реально возвращённые typed metrics.
Typed interface enable/disable требуют per-device opt-in, реальных permissions,
default-deny policy, canonical resource protection и read-after-write
verification. При incomplete permissions/dependency metadata writes unsupported.
Services/firewall writes отсутствуют. Snapshots сохраняют только предыдущий
RouterOS interface admin state; `resource.rollback` проходит те же permissions,
policy/protection, ownership/version/retention/current-state checks и verification.
При неопределённом исходе PATCH не повторяется автоматически. Подробнее —
[commands](commands.md).

Topology сохраняет raw IDs/subtypes, evidence, unresolved mapping и partial/
failed snapshots. LLDP/CDP/MNDP observation и общий bridge segment не гарантируют
один физический кабель. LLDP expiry содержит явную collector assumption;
ARP не используется как physical topology evidence. Подробнее — [topology](topology.md).

Phase 6 сохраняет `observed_l2_neighbor` direction, stable source-scoped
observation ID, local interface identity, LLDP management addresses/capabilities
и доступный RouterOS age. Один RouterOS neighbor с несколькими discovered-by
protocols даёт отдельные observations; native Cisco CDP reader отсутствует.
Unknown endpoint/port допустим. Source/time/real-or-simulated provenance проходят
общую durable delivery; subjects Redfish System/Chassis/Manager и IPMI BMC/FRU
не объединяются по IP/serial. Received TTL/explicit withdrawal не подтверждены
текущими readers. [Semantics и ограничения](identity-topology.md),
[Backend handoff](backend-identity-topology-contract.md).

Persistent log cursor advances после admission `LogBatch`, независимо от
Controller ACK. В memory mode admission остаётся volatile; в durable mode
batch сохранена synced transaction и переживает restart. Retention/eviction
могут потерять low/medium priority records; replay требует dedup envelope IDs
и log record IDs. Semantics — at-least-once для admitted retained records,
без exactly-once. Подробнее — [logs](logs.md) и [delivery](delivery.md).

Astra Linux, ALT Linux и RED OS используют тот же generic Linux collector,
без отдельных платформ. `/proc`, `/sys`, net interfaces и regular file logs
не требуют distro-specific API; policy/permissions, kernel и init/service
environment могут влиять на availability. Runtime этих дистрибутивов не
проверен: WSL Linux не заменяет такую validation.

Windows LocalPlatform остаётся post-MVP/backlog. NETCONF/gNMI deferred: для
P3 нет подтверждённой дополнительной функции и vendor fixture сверх SNMP/REST.
Production Backend/CA service, Web UI, LLM/AI analysis, historical database
и reports в этот репозиторий не входят.

## Sources и следующие этапы

| Source | Состояние |
|---|---|
| Remote Syslog/Audit receiver | Реализован opt-in в Phase 3 через existing logs/durable pipeline; sender setup и границы — [syslog-receiver](syslog-receiver.md) |
| Redfish | Реализован read-only в Phase 4; standard schema fixtures и официальные DMTF tools проверены. Hardware/vendor support требует отдельных live результатов; [Redfish](redfish.md) |
| IPMI | Phase 5: explicit v2.0 LANplus read-only targets, Device ID/FRU/SDR/sensors/chassis через hardware contracts Phase 4. Reference BMC и protocol fixtures не доказывают работу настоящего hardware; [contract и проверки](ipmi.md) |
| Identity/topology | Phase 6: scoped evidence и directed observations через существующие models/outbox; global Asset matching/topology остаются Backend. [Модель](identity-topology.md), [wire contract](backend-identity-topology-contract.md); фактические проверки — [validation](validation.md) |
| Collector ↔ Backend time exchange | Отложен. Optional offset/uncertainty и clock quality передаются в observation metadata, но наличие wire fields не означает измеренный offset. Corporate NTP Phase 2 использует службу ОС и не добавляет clock exchange |

Zabbix, Prometheus, Ansible, Grafana, TSDB и global Asset/entity resolution
относятся к Backend/Control Plane и не являются adapters Collector.

Hardware tests запускаются отдельно от обычной suite. После live проверки
матрица должна называть конкретные model/firmware, доступные operations и
источник evidence; fixture или cross-build не подтверждают весь vendor.
