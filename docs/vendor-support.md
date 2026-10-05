# Поддержка платформ и производителей

Матрица отражает текущую реализацию и фактически выполненные проверки.
Cross-platform описывает ОС запуска Agent; multi-vendor — удалённые устройства,
опрашиваемые через protocol drivers. Наличие profile, config hint или action
в protobuf само по себе не объявляет capability.

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
| Native OpenWrt | Linux collector плюс board/release evidence, netifd logical networks, procd services, UCI firewall configuration read | Official implementation schema fixtures; timeout/cancellation/partial availability и настоящий wire encode/decode | **OpenWrt runtime unverified**: доступная WSL среда не содержит OpenWrt release/ubus | OpenWrt router/firmware и RAM/flash consumption не проверены |
| Linux system logs | Configurable regular files, syslog/rsyslog/raw parsing, sanitizer, bounded batches, persistent cursors, rotation/truncate/recreate | Temp files, burst/rotation/restart/errors/cancellation | WSL Linux amd64: настоящие file reads, rotation, cursor restart и gRPC delivery | Production rsyslog deployment не проверен; paths задаются оператором |
| auditd file source | Sanitized raw records, exact event ID/time/type, известные uid/auid/pid/exe/syscall/success/result; bounded correlation внутри batch | Audit syntax fixtures и Agent/Mock delivery | Linux file pipeline выполнен; содержимое audit records в tests — fixtures | Реальный audit daemon/audit rules не проверены |
| SIP OPTIONS | Fixed unauthenticated UDP/TCP probe, matching final response, availability/latency/error category/state transitions | Локальные UDP/TCP SIP servers: 2xx, 401/407, 404, 5xx, timeout/malformed/cancel/recovery | WSL Agent/Mock с локальным UDP SIP fixture | Внешний PBX, authentication, TLS и media не проверены/не реализованы |
| Generic SNMP v2c | SYSTEM, optional ENTITY inventory, IF/IF-X counters/rates и LLDP-MIB observations | Standard MIB mapping fixtures и локальный UDP SNMP server | Linux amd64 protocol/E2E tests с test server | Реальное SNMP оборудование не проверено; SNMP SET/SNMPv3 unsupported |
| RouterOS HTTPS REST | Identity/resource/RouterBOARD, interfaces/IP/counters/rates, neighbor observations, typed interface enable/disable с checks/readback | JSON fixtures, локальный HTTPS server с test CA и Agent/Mock E2E | Linux amd64 protocol/E2E tests с test server | MikroTik hardware/firmware не проверены; fixture не является hardware validation |
| Development Simulator | Отдельный executable, deterministic scenarios/trends, ordinary normalized envelopes через InfraPulse gRPC, `source=simulated` | Determinism/scenario/transport tests, Simulator ↔ Mock gRPC E2E | WSL Linux amd64 suite и отдельные Windows amd64 CLI processes Simulator/Mock | Synthetic demonstration не подтверждает оборудование или production collectors |
| Local Windows | LocalPlatform **unsupported**; возможно collector-only hosting с `local.enabled: false` | Windows suite проверяет portable core/protocol и file fixtures | Windows test/collector hosting не является Windows local monitoring | Windows LocalPlatform относится к P3 |

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
ложной capability. Vendor-specific health/inventory OID не реализованы.
Model, serial, software и firmware остаются стандартным ENTITY-MIB fallback
при доступности однозначной chassis; неизвестные значения не угадываются.

Configured hint сохраняется отдельным слабым evidence и не подтверждает vendor.
P1 literal MikroTik `sysDescr` fallback сохранён с `confidence=low`. Hostname
не используется для detection. Primary evidence и scope каждого profile
описаны в [vendor profiles](vendor-profiles.md).

## Availability и границы функций

Linux объявляет `system.read`, `inventory.read`, `cpu.read`, `memory.read`,
`interfaces.read`, `telemetry.read`. При configured file sources добавляются
`logs.read` и для auditd — `audit.read`; unavailable source виден в diagnostics.
File-based logs не включают journald API, network syslog listener, OpenWrt logd/
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

Telephony объявляет `telephony.options`/`telephony.health` для configured probes.
2xx означает успешный OPTIONS; высокая latency даёт degraded. 401/407 означает
reachable и authentication_required, остальные non-2xx — отдельный error
category. SIP failure меняет только telephony state, не remote device online
и не interface state. REGISTER/calls/RTP/media/PBX statistics/SIP TLS отсутствуют.
Подробнее — [telephony](telephony.md).

Generic SNMP объявляет только наблюдённые standard reads. ENTITY optional;
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
Services/firewall writes, configuration snapshots и rollback отсутствуют.

Topology сохраняет raw IDs/subtypes, evidence, unresolved mapping и partial/
failed snapshots. LLDP/CDP/MNDP observation и общий bridge segment не гарантируют
один физический кабель. LLDP expiry содержит явную collector assumption;
ARP не используется как physical topology evidence. Подробнее — [topology](topology.md).

Persistent log cursors переживают обычный restart, но outbox остаётся в RAM.
Advance выполняется после admission `LogBatch`; crash, queue eviction и Send
error могут потерять уже прочитанные logs, а crash до cursor persistence может
дать replay. Durable delivery/ACK и exactly-once отсутствуют. Подробные
rotation/санитизации/delivery semantics — [logs](logs.md).

Windows LocalPlatform, SNMPv3, durable outbox/journal, production enrollment,
certificate workflow и rollback остаются вне P2. Backend, Web UI, LLM/AI analysis,
database и reports в этот репозиторий не входят.

Hardware tests запускаются отдельно от обычной suite. После live проверки
матрица должна называть конкретные model/firmware, доступные operations и
источник evidence; fixture или cross-build не подтверждают весь vendor.
