# IPMI fallback: аппаратный мониторинг legacy BMC

Phase 5 добавляет явно выбранный read-only IPMI target в существующий Collector.
Redfish остаётся предпочтительным hardware protocol: IPMI нужен для серверов,
у которых Redfish отсутствует или непригоден для нужного чтения. Collector
работает клиентом BMC, без установки своего агента на сервер и без сканирования
management network. Ошибка HTTPS/TLS или authentication Redfish не запускает
IPMI автоматически. Каждый endpoint настраивается отдельным target.

Используется IPMI v2.0 / LANplus / RMCP+ поверх UDP, default port 623.
HTTPS origin или TCP endpoint здесь не подходят. Collector не открывает IPMI
listener для внешних клиентов. Reference BMC server используется только в tests.

## Конфигурация и BMC requirements

Используется существующая YAML schema `credentials` и `devices`:

```yaml
credentials:
  legacy-server-bmc:
    username_env: INFRAPULSE_IPMI_USER
    password_env: INFRAPULSE_IPMI_PASSWORD
devices:
  - id: legacy-server-bmc-01
    driver: ipmi
    address: 10.10.20.31
    credential_ref: legacy-server-bmc
    polling:
      medium_interval: 30s
      inventory_interval: 5m
      timeout: 30s
    ipmi:
      port: 623
      cipher_suite: 17
      privilege: user
      request_timeout: 2s
      retries: 0
      max_session_duration: 30s
      sdr_cache_ttl: 5m
```

Полный deployment также задаёт existing Controller mTLS и durable outbox.
Пароль не входит в YAML; значения получает существующий `credentials.Provider`
по ссылке и environment names. IPMI reference требует именно username/password;
SNMP community/auth/privacy references отклоняются. Для LANplus username
ограничен 16 bytes, пароль — 20 bytes; пустые credentials отклоняются.

`address` содержит host/IP без scheme, port, brackets, userinfo или resource
path; порт задаётся только в `ipmi.port`. Duplicate target IDs, неизвестные YAML
fields, неверные ссылки и bounds отклоняются strict config validation.
`port` и `cipher_suite` имеют presence: пропуск задаёт default, явный `0`
отклоняется. `enable_mutations: true`, Redfish TLS options и SNMP options
несовместимы с IPMI target. Nonzero `fast_interval` и `topology_interval`
отклоняются: стандартная 3s telemetry SNMP/RouterOS не меняется.

Нужен BMC с LANplus и выбранным допустимым cipher suite, доступным через
management network. Monitoring account должна разрешать Get Device ID,
Get Chassis Status, Get SDR Repository Info / Get SDR / Reserve SDR Repository,
Get FRU Inventory Area Info / Read FRU Data и Get Sensor Reading. Default
privilege — `user`; `operator` разрешён только как явная настройка для BMC,
где чтение требует этого уровня. `administrator`, `callback`, null privilege
и автоматическое повышение прав не поддерживаются. Настройка `operator`
не добавляет write capability адаптеру.

| Поле | Default | Допустимый диапазон |
|---|---|---|
| `port` | 623 | 1–65535 |
| `cipher_suite` | 17 | 17 или явно выбранный 3 |
| `privilege` | user | user / operator |
| `request_timeout` | 2s | 10ms–30s |
| `retries` | 0 | 0–3 дополнительных попытки |
| `max_session_duration` | 30s | 10ms–5m |
| `max_operations` | 1024 | 1–8192 |
| `max_sdr_records` | 128 | 1–1024, не больше `remote.max_rows` |
| `max_sdr_bytes` | 128 KiB | 260 bytes–1 MiB |
| `max_fru_records` | 16 | 1–128, не больше `remote.max_rows` |
| `max_fru_bytes` | 64 KiB | 8 bytes–64 KiB на FRU device |
| `max_poll_bytes` | 1 MiB | не меньше FRU/SDR budget и `min(remote.max_response_bytes, 4096)`, не больше 8 MiB |
| `sdr_cache_ttl` | 5m | 1s–1h |
| `provenance` | real | real; simulated только в development |

`medium_interval` и `inventory_interval` используют общие polling bounds:
10ms–24h при явной настройке, defaults 30s/5m. Практический начальный диапазон
для BMC — health 30–60s и inventory 5–15m. Total target `polling.timeout`
ограничен 5m; effective request/session timeout также ограничен remaining
poll context. Короткие intervals подходят для isolated fixtures, а не для
массового production polling медленных BMC.

## Библиотека и RMCP+ security

Pinned dependency —
`github.com/bougou/go-ipmi v0.9.2-0.20261009051248-b3670df57943`,
commit `b3670df57943`. Актуальный client import — `pkg/client`; старое root API
v0.8 не используется. Выбран upstream commit с context-aware UDP I/O,
exchange serialization и bounded concurrent `Close`, отсутствовавшими в
предыдущем stable snapshot. Библиотека остаётся v0.x с изменяемым public API;
обновление требует повторного security/protocol review. Runtime использует
native Go client; `ipmitool`/FreeIPMI не являются runtime dependencies.

| Suite | Authentication | Integrity | Confidentiality | Policy |
|---|---|---|---|---|
| 17 | RAKP HMAC-SHA256 | HMAC-SHA256-128 | AES-CBC-128 | Default |
| 3 | RAKP HMAC-SHA1 | HMAC-SHA1-96 | AES-CBC-128 | Только явно выбранная совместимость |
| 0 / прочие | — | — | — | Отклоняются |

Передаётся один выбранный suite. Ответ BMC с другими algorithms,
неаутентифицированным или незашифрованным operational payload отклоняется;
silent downgrade отсутствует. Unsupported suite возвращает ошибку вместо
перехода к suite 0, null auth или IPMI v1.5 session. LANplus discovery включает
стандартный unauthenticated Get Channel Authentication Capabilities preflight
в legacy framing; он не активирует v1.5 session и не разрешает operational
commands без RMCP+ authentication/integrity/encryption.

Адаптер дополняет client узким packet gate, поскольку pinned receive path
библиотеки не проверяет все необходимые invariants. Gate проверяет выбранные
algorithms, RAKP authentication, session IDs, monotonic inbound sequence,
request/response correlation, integrity HMAC, AES framing/padding и IPMB
checksums до library decoder. Console session ID генерируется через
`crypto/rand`. Unrelated/duplicate datagrams пропускаются в ограниченном
количестве: максимум 16 за чтение; превышение завершает exchange ошибкой.
Полный IPMI stack не продублирован: framing, commands и crypto primitives
принадлежат библиотеке, workaround находится внутри `internal/driver/ipmi`.

Nonzero BMC `Kg` не поддерживается: отдельного secret reference для него нет,
такой authentication mode отклоняется. Username, пароль, RAKP transcript,
session keys и raw packets не входят в SourceRef, inventory, telemetry,
protobuf или diagnostics; library debug отключён, errors сведены к безопасным
категориям. Cipher 17 не устраняет ограничения самого IPMI/RAKP и firmware BMC:
нужны management segmentation, отдельная monitoring account и уникальный
сложный пароль. Collector не меняет LAN/users/cipher configuration BMC.

## Общие hardware contracts

IPMI адаптер напрямую реализует existing `InventoryBatchReader`,
`HardwareReader` и typed probe. Используются `model.Device`,
`InventoryComponent`, `HardwareInventory`, `HardwareStatus`,
`ComponentHealth`, `Metric`, `IdentityEvidence`, `ObservationMetadata`
и `ResourceRelationship`. Capability set содержит только `inventory.read`,
`health.read` и `telemetry.read`. Domain packages не импортируют go-ipmi types.
Второго hardware health engine или inventory format нет.

Get Device ID создаёт observed Manager/BMC с `Vendor=unknown` и numeric
Manufacturer ID / Product ID evidence. Device ID BMC не становится server
serial. Device revision, firmware major/minor BCD, IPMI version и Additional
Device Support проверяются; BMC firmware не подменяет BIOS version. Numeric
Manufacturer ID сохраняется без неподтверждённого vendor-name mapping.

Get Chassis Status создаёт scoped Chassis observation и management relationship
`BMC_MANAGES_CHASSIS` / `MANAGED_BY_BMC`. `PowerState=On/Off` отделён от health:
выключенный сервер сам по себе не является failure. Power fault, power control
fault, overload, drive fault и cooling fault дают `critical`; intrusion —
`warning`, если stronger fault отсутствует. Успешный chassis status без этих
fault flags даёт `ok`. Missing status остаётся unknown; failed network poll
не является hardware Critical.

## FRU inventory

FRU IDs берутся из стандартного FRU Device Locator SDR и optional built-in
FRU 0 при соответствующем Get Device ID support bit. Несколько FRU records
дают scoped components одного BMC target. Присутствие нескольких serials
не доказывает несколько отдельных серверов.

| FRU area | Доступные нормализованные сведения |
|---|---|
| Chassis | Chassis type, part number, serial |
| Board | Manufacturer, product name, serial, part number |
| Product | Manufacturer, name, part/model number, version, serial, asset tag |

Стандартная Chassis Info Area не содержит manufacturer: этот field не
синтезируется из Board/Product или numeric Manufacturer ID BMC. FRU serials
принадлежат своим component subjects; chassis/board/product serial не
подменяет System serial. CPU model/cores, RAM/DIMM capacity/vendor и drive
serial/capacity не заявляются без source evidence. System GUID отдельно не
читается; RAKP GUID используется только для authentication, не выдаётся за
UUID физического сервера.

Чтение ограничено размером и числом устройств; поддержаны byte/word addressing,
bounded chunks и partial-read checks. Common header, area lengths/offsets,
overlap, terminator/padding и checksums проверяются до typed area decoder.
Повреждённая area не отменяет валидную sibling area. Empty/missing FRU остаётся
пустым; отсутствующие поля не заполняются нулями или случайным текстом.

Text decoding поддерживает BCD+, packed 6-bit ASCII и определённые FRU
language encodings; binary/unspecified strings остаются отсутствующими.
Invalid lengths, unsupported encoding, control characters, malformed UTF-16
и replacement characters отклоняются. OEM multirecord и произвольные custom
fields не превращаются в универсальный JSON payload.

## SDR, readings и health

Enumeration поддерживает Full Sensor Record, Compact Sensor Record и
FRU Device Locator. Проверяются record header/version/type length, ID/next ID,
iteration count, total bytes и repository snapshot consistency. Cyclic traversal,
несовпадение record ID и truncated record дают partial/error, без бесконечного
обхода. Максимальный record имеет 5-byte header и 255-byte body.
Compact shared sensors разворачиваются с явным sensor/entity numbering,
а одинаковые names не объединяются.

Bounded cache живёт в target lifecycle: health poll использует сохранённый
SDR только до TTL и при неизменных repository count/add/erase metadata.
Inventory выполняет refresh. Device-ID fingerprint change, repository change,
истечение TTL или failed discovery сбрасывают cache; stale metadata не
используется бесконечно. После restart cache строится заново.

Operational Get Sensor Reading выполняется последовательно в одной session.
Отдельно различаются analog threshold, discrete, unavailable reading,
disabled scanning и unsupported conversion. Remote IPMB bridging не
поддерживается: доступны sensors самого BMC с owner `0x20`, channel 0 и
допустимым LUN. Unsupported owner/channel даёт unknown вместо запроса
неверному controller.

Analog conversion использует SDR signed raw format, M/B coefficients,
R/B exponents и поддержанную linearization. Raw byte не является готовой
температурой/RPM/напряжением. Missing/unsupported conversion или nonfinite
result не создаёт numeric metric. Health threshold evidence берётся из
actual reading flags и SDR reading mask; установленный discrete bit
интерпретируется по event/sensor type.

Library conversion используется только для standard linearization 0–11 и
threshold Full Sensor records с analog reading. Nonlinear/OEM conversion с
динамическими reading factors, rate/modifier units и неподдержанные units
не создают metric. Numeric fan value не возникает из одного discrete state.

| SDR units/type | Metric / normalized unit | Дополнительная проверка |
|---|---|---|
| Temperature + Degrees C/F/K | `hardware.temperature.celsius` / `Cel` | F/K преобразуются в Celsius; значение не ниже −273.15°C |
| Fan + RPM | `hardware.fan.rpm` / `RPM` | Неотрицательное значение |
| Fan + percentage | `hardware.fan.percent` / `%` | 0–100; не RPM |
| Percentage другого analog sensor | `hardware.sensor.percent` / `%` | 0–100, unspecified base unit |
| Voltage + Volts | `hardware.voltage.volts` / `V` | Подтверждённый voltage sensor type |
| Current + Amps | `hardware.current.amperes` / `A` | Подтверждённый current sensor type |
| Watts | `hardware.power.watts` / `W` | Source unit именно Watts |

Metric содержит ResourceID, ComponentID, unit, observation и hardware status.
Temperature/RPM/power semantics совместимы с Phase 4; SourceRef отличает
протоколы. Voltage/current используют тот же existing typed `Metric` contract.
Нельзя вычислять PSU wattage из несвязанных sensors, приравнивать power Off
к Critical или превращать unavailable reading в 0°C.

| Source state/evidence | `HardwareStatus` mapping |
|---|---|
| Reading unavailable / scanning disabled / invalid response | `health=unknown`, отдельный availability/validity state, без metric |
| Unsupported conversion/units | `state=conversion_unavailable`, numeric metric отсутствует; подтверждённая threshold health сохраняется |
| Threshold LNC / UNC, bits 0/3 | `warning` |
| Threshold LCR / LNR / UCR / UNR, bits 1/2/4/5 | `critical` |
| Threshold flags отсутствуют, есть implemented threshold mask | `ok` |
| Нет sufficient health evidence | `unknown` |
| Generic predictive failure / limit exceeded / performance lags, event types 04/05/06 | Asserted offset 1 → `warning`; offset 0 → `ok`; ambiguous combination → `unknown` |
| Generic severity, event type 07 | Offsets 2/3/5/6 → `critical`, 1/4 → `warning`, 0 → `ok` |
| Generic presence, event type 08 | Offset 0 → `Absent`, offset 1 → `Present`; health остаётся unknown |
| Generic enabled, event type 09 | Offset 0 → `Disabled`, offset 1 → `Enabled`; health остаётся unknown |
| Generic redundancy, event type 0B | Offset 5 → `critical`, offsets 1/2/3/4/6/7 → `warning`, один offset 0 → `ok` |
| Sensor-specific event type 6F | Ниже перечислены поддержанные маски; остальные types/unknown asserted bits → `unknown` |

Exact bit masks sensor-specific type 6F приведены в hexadecimal; offsets
интерпретируются по [IPMI v2.0 E6, Table 42-3](https://www.intel.com/content/dam/www/public/us/en/documents/product-specifications/ipmi-v2-rev1-1-spec-errata-6-markup.pdf),
а не по самому факту asserted bit. Critical/warning — консервативная policy
Collector; presence, disabled, spare и операции в процессе остаются unknown:

| Sensor type | Critical mask | Warning mask | Non-health mask, health unknown |
|---|---:|---:|---:|
| Processor 07 | `087f` | `1400` | `0380` |
| Power Supply 08 | `001a` | `0064` | `0081` |
| Power Unit 09 | `0074` | `0080` | `000b` |
| Memory 0C | `048e` | `0221` | `0150` |
| Drive Slot 0D | `0042` | `0124` | `0099` |

PSU presence-only offset 0 даёт `Present`, standby offset 7 —
`StandbyOffline`; ни один не доказывает OK. Discrete fan поддерживает общие
event-state mappings без выдуманного RPM. CPU/DIMM/storage sensors дают
scoped component status/temperature при доступности, без fabricated
processor/memory/storage inventory. State сохраняет event type/asserted mask
для provenance; unknown/OEM meanings не угадываются.

## Identity, provenance и время

Target ID `remote:<configured-id>` обозначает connection. Observed IDs
`ipmi-<hash>` вычисляются из configured target, resource kind и stable resource
path. Sensor path включает owner/channel/LUN, sensor number, entity ID/instance;
array order, display name, время и случайный UUID в ID не участвуют. FRU
component path включает FRU ID и area kind. Sensor name collisions допустимы.
Изменение configured Target ID меняет scoped observation IDs.

SourceRef содержит `type=ipmi`, `protocol=ipmi`, TargetID, ResourceID и
provenance. Credentials или authentication fields не включаются. Production
observations имеют `real`; isolated fixtures задают `simulated` в development.
Registry сохраняет configured provenance также в connectivity/error events;
legacy wire Event.Source для таких fixtures равен `simulated`, при сохранении
IPMI/Redfish type/protocol в normalized SourceRef.
Полная event Observation metadata остаётся normalized/JSON-only: existing
Event protobuf переносит legacy source marker, без нового поля Phase 5.
Management IP даёт только weak evidence BMC; numeric manufacturer, FRU model
и component serial имеют отдельные subjects/confidence. Server, BMC, chassis
и FRU component identity не объединяются автоматически.

Тот же сервер может иметь Redfish и IPMI targets. Их Resource IDs и SourceRefs
различаются; существующие Redfish IDs не меняются. Одинаковый hostname/IP
не запускает скрытый merge. Backend выполняет последующее global entity
resolution на основании evidence. BMC management relationships не являются
physical cable/LLDP evidence; ARP также не доказывает физическую связь.

`observed_at`/`collected_at` задаёт существующий observation lifecycle в UTC;
`source_timestamp` отсутствует, если измерение не предоставляет достоверного
времени. Corporate NTP синхронизирует OS clock через Phase 2 mechanism.
IPMI clock synchronization, manual offsets и переписывание Syslog/Redfish
timestamps отсутствуют.

## Lifecycle, isolation и доставка

Global scheduler/`remote.concurrency` ограничивает concurrent targets; target
gate исключает пересекающиеся polls. Запросы к BMC последовательны. Session
живёт один вызов `InventoryBatch` или `Hardware`; все sensor requests этого
вызова переиспользуют её. Tick с inventory и health создаёт две последовательные
sessions, без overlap; следующий вызов создаёт свежую session. Effective
deadline ограничивает общий target poll и каждую session duration.
Cancellation/Close отменяют network I/O; cleanup собственного session/socket
использует отдельный context не более 1s либо меньший request timeout.
Если BMC недоступен при logout, его session expiry остаётся ответственностью
BMC; account/session budget настраивается по документации оборудования.

IPMI datagram bound равен `min(remote.max_response_bytes, 4096)`; allocation
не зависит от непроверенной длины BMC. Wire gate отдельно ограничивает число
transmissions и total bytes session, включая handshake/retries; FRU/SDR
enumeration также имеет свои item/byte/iteration bounds. Normalized devices
ограничены `remote.max_rows`, components/health/metrics — existing batch
budget `16 × remote.max_rows`. Outbox дополнительно ограничивает размер
serialized envelope. Один sensor может дать и health, и optional metric,
а один FRU device — несколько component areas.

Network retries конечны. Shared hardware batch scheduler применяет bounded
exponential cooldown с jitter до 5m после unavailable/connectivity failures;
cooldown не создаёт новые sessions и не повторяет authentication для каждого
sensor. Один offline BMC не останавливает Redfish/SNMP/RouterOS/SIP/Syslog.
Failed FRU или sensor сохраняет валидные observations остальных records.
Partial inventory не означает исчезновение всех ранее наблюдавшихся components.
Availability, power state, component health и measurement validity разделены.

Все данные проходят существующий
`Agent.push → Sanitizer → Durable Outbox → gRPC → Application ACK`.
Адаптер не отправляет данные в gRPC и не создаёт свою очередь. При
`queue.mode: durable` и `ack_fallback: reject` admitted records сохраняются
в bounded existing bbolt outbox; restart/reconnect replay использует тот же
message ID. Переполненная очередь ограничивает admission согласно текущей
policy. До admission сохранность не обещается; exactly-once не заявляется.
Backend должен deduplicate и ACK после своего persist. Legacy send fallback
сохраняет existing best-effort semantics.

Используются hardware wire models Phase 4 и Phase 3 log metadata; отдельного
IPMI sensor protobuf или Backend API нет. RPC signatures, ACK/replay и field
numbers не меняются. Production Backend/PostgreSQL в этом репозитории не
реализован: полное хранение hardware/source observations требует поддержки
existing additive contracts на принимающей стороне.

## Read-only и deployment

Typed boundary допускает DeviceID, ChassisStatus, RepositoryInfo, ReserveSDR,
SDR, FRUInfo, ReadFRU, SensorReading и session Close. SDR reservation служит
согласованному чтению, session privilege/close — transport lifecycle.
Packet gate допускает только соответствующие monitoring/session commands.
Адаптер не реализует `CommandExecutor` или raw arbitrary command API.
Chassis Control/Identify, power/reset/cycle, thresholds/LAN/users/password
changes, FRU/SDR write, SEL Clear и firmware updates запрещены.

Firewall должен разрешать исходящий UDP к выбранному BMC/port и ответный
traffic session; входящий wildcard listener Collector не нужен. BMC лучше
размещать в изолированной management VLAN; WAN exposure не требуется.
На Linux используется тот же production CGO0 binary и existing deployment
units. Native in-band `/dev/ipmi`, local Windows WMI, SOL и shell-out backend
библиотеки Collector не включает в monitoring flow.

## Проверки, размер и ограничения

Baseline Phase 5 выполнен 2026-10-09 на `main`, `d456fea`, до IPMI изменений:
`gofmt -l`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...`,
`go test -race -count=1 ./...` и `go mod verify` — PASS. Compiled Linux suites
17 packages, protobuf hash comparison, cross-builds и 10 YAML examples — PASS.
Windows `go test -count=1 -p=1 -tags=integration ./...` завершился FAIL из-за
TempDir cleanup в existing Syslog test; targeted повтор этого test прошёл.
Полный исходный FAIL не считается PASS после targeted повтора. Exact commands,
logs и baseline report сохранены в ignored `.cache/phase5-baseline/`.
Это baseline до IPMI, а не финальное подтверждение нового adapter.
Matched Linux amd64 baseline с `CGO_ENABLED=0`, `-trimpath -buildvcs=false`:
35 372 897 bytes unstripped и 23 855 264 bytes с `-ldflags='-s -w'`.
После code freeze финальные build/vet/unit/race, serial Windows integration,
18 compiled Linux suites, native/cross-build, все 11 YAML examples и pinned
protobuf SHA256 comparison — PASS. Source hashes: 242 files, 0 mismatches.
Exact commands и fixture distinctions — [validation.md](validation.md),
[Phase 5 report](phase-5-report.md).

Matched Linux amd64 Phase 5 CGO0 binary: 38 257 990 bytes unstripped,
25 948 320 bytes stripped. Рост против baseline: 2 885 093 bytes / 8,16%
и 2 093 056 bytes / 8,77% соответственно; stripping не устраняет SDK cost.
Native UDP fixture probe: 50 health polls, 78,7598 ms, retained heap +156 920
bytes, goroutine delta 0, socket peak 1, 51 opened/51 closed sockets,
0 active BMC sessions. Sample maximum 6071 bytes / 4 metrics / 6 health items,
inventory 11 703 bytes. BMC/helper работает в том же test process: heap,
allocations и latency не изолируют production Collector или hardware BMC.

Reference BMC upstream предназначен для development. Текущий pin содержит
Get Device ID/Chassis/FRU/SDR/sensor handlers и configurable mock HAL, но
default values и supported error scenarios не заменяют real hardware.
Repository helper `ipmitest` задаёт независимые FRU/SDR byte fixtures и
собственный sensor handler. Его test-only response-framing shim исправляет
upstream requester-address/LUN defect, пересчитывая checksums и authenticated
framing; production validation не ослабляется. Это reference BMC с fixture
дополнениями, а не неизменённая firmware/server validation.
Protocol fixtures проверили corrupt FRU/SDR, discrete flags, packet loss,
invalid authentication, session/replay и partial-result cases. В отчётах
разделяются reference-BMC, deterministic-fixture и production-hardware E2E.
Native LANplus suites используют USER. Explicit OPERATOR policy проверена
config tests; успешная native OPERATOR session отдельно не проверялась.

Targeted и финальные suites проверили mixed Redfish/IPMI на общем loopback address,
offline IPMI isolation и Mock application ACK; simulated marker сохранён
также у lifecycle events. External DMTF server не запускался повторно:
опциональный service E2E skipped без endpoint environment. Existing Redfish
fixtures и regression выполнены; это не новая hardware validation.

Не проверены реальные BMC/vendor firmware, production Backend/PostgreSQL,
OpenWrt RAM/flash и другие architectures runtime. IPMI v1.5 sessions,
nonzero Kg, IPMB bridging, OEM inventory, SEL ingestion, detailed CPU/RAM/
storage parity с Redfish и hardware writes отсутствуют. Cross-build сам
по себе не доказывает пригодность binary для конкретной OpenWrt firmware.
Dependency tree, binary growth и raw symbol sums измерены в финальном build;
production sizing и optional adapter build tags этим не реализованы.

## Официальные источники

- [IPMI v2.0, Revision 1.1, E6](https://www.intel.com/content/dam/www/public/us/en/documents/product-specifications/ipmi-v2-rev1-1-spec-errata-6-markup.pdf): RMCP+/RAKP, Device ID, Chassis/FRU/SDR/Sensor commands и units.
- [Platform Management FRU, v1.0 Revision 1.3](https://www.intel.com/content/dam/www/public/us/en/documents/specification-updates/ipmi-platform-mgt-fru-info-storage-def-v1-0-rev-1-3-spec-update.pdf): header, areas, checksums и text encoding.
- [go-ipmi repository](https://github.com/bougou/go-ipmi), [pinned commit](https://github.com/bougou/go-ipmi/commit/b3670df57943): client implementation и reference BMC.
- [Client documentation](https://bougou.github.io/go-ipmi/docs/client/), [Server documentation](https://bougou.github.io/go-ipmi/docs/server/), [Architecture](https://bougou.github.io/go-ipmi/docs/architecture/): актуальные packages и lifecycle.
- [ipmitool](https://github.com/ipmitool/ipmitool), [FreeIPMI](https://www.gnu.org/software/freeipmi/): независимые read-only средства сверки при наличии в test environment. CLI не является частью production collector.
- [Redfish contracts](redfish.md), [wire protocol](protocol.md), [security](security.md), [topology evidence](topology.md).
