# Redfish: аппаратный inventory, health и sensors

Phase 4 добавляет read-only DMTF Redfish adapter для существующего HTTPS API
BMC. Collector опрашивает серверное оборудование без установки своего агента
на сервер или внутрь BMC. Используются стандартные schemas, без обязательных
Dell/HPE OEM extensions. Управление питанием здесь не реализовано. Явный legacy
IPMI target добавлен в [Phase 5](ipmi.md); Redfish остаётся предпочтительным
протоколом, автоматического переключения между ними нет.

Нужен BMC с Redfish ServiceRoot, доступный по verified HTTPS, и отдельная
monitoring account с правами чтения inventory/status/sensors. Для session auth
учётная запись также должна разрешать создание и удаление собственной session.
Право Login обычно необходимо; конкретный role/privilege mapping настраивается
по документации BMC. Права ConfigureComponents, ConfigureManager и изменения
accounts/firmware для Collector не требуются.

## Configuration

Используется существующая секция `devices`, а не новый формат targets:

```yaml
credentials:
  server-bmc:
    username_env: INFRAPULSE_REDFISH_USER
    password_env: INFRAPULSE_REDFISH_PASSWORD
devices:
  - id: server-bmc-01
    driver: redfish
    address: https://bmc.example.net
    credential_ref: server-bmc
    tls:
      ca_file: /etc/infrapulse/certs/bmc-ca.pem
    polling:
      medium_interval: 30s
      inventory_interval: 5m
      timeout: 30s
    redfish:
      auth: session
      max_resources: 128
      max_pages: 8
      max_requests: 512
      request_timeout: 5s
```

`address` — HTTPS origin без userinfo, query, fragment и resource path.
Discovery начинается с `/redfish/v1/`; фиксированный `/Systems/1` не используется.
CA path разрешается относительно YAML, если путь относительный. Без `ca_file`
используется системный trust store. Custom CA добавляет сертификаты к системному
trust store; проверка hostname/IP SAN остаётся обязательной. Для IP endpoint
сертификат должен содержать соответствующий IP SAN.

Значения credentials читает existing `credentials.Provider`; пароль не входит
в YAML, Target JSON, SourceRef, inventory или diagnostics. Secrets передаются
через environment variables, указанные в credential reference. Example с
production mTLS для Controller — [config.redfish.example.yaml](../configs/config.redfish.example.yaml).

Default health/sensor cadence — `medium_interval: 30s`, inventory — `5m`,
total poll timeout — `30s`. Fast/topology intervals для Redfish отключены;
их явное ненулевое значение отклоняется. Опрашивать BMC каждые три секунды
не требуется. Existing bounded scheduler ограничивает concurrency targets;
внутри одного BMC запросы последовательные, concurrent polls не перекрываются.
Inventory обновляет bounded discovery/read plan. Hardware polling использует
этот план для fresh status/sensor GET без повторного обхода collections и
EthernetInterfaces; при отсутствии плана сначала выполняется discovery.
Каждый poll имеет собственный session lifecycle; новый runtime или background
polling goroutines не создаются.

Для устройств без working SessionService можно явно задать `auth: basic`.
Это только verified HTTPS, автоматического fallback нет. Development fixtures
помечаются `redfish.provenance: simulated` при `security.mode: development`;
production запрещает такой режим. Default provenance — `real`.

## Client, authentication и read-only policy

Pinned client — `github.com/stmcginnis/gofish v0.27.0`, BSD-3-Clause, Go 1.21
minimum. Dependency tree библиотеки не добавляет сторонних runtime modules;
Collector сохраняет Go 1.27 и CGO0 builds. Gofish client/session и schemas
используются только в `internal/driver/redfish`; domain models не импортируют
library types. Автоматические collection helpers библиотеки заменены bounded
enumeration: они могут создавать goroutines по числу members. Standard modern
Fan и 64-bit Drive capacity имеют узкие documented mappings, поскольку
library Fan helper/`*int` capacity неудобны для modern schemas/32-bit collectors.

Default authentication: ServiceRoot links → SessionService → POST Sessions →
X-Auth-Token → GET resources → DELETE собственной session. На 401 разрешена
одна reauthentication за poll; invalid login и 403 не дают бесконечных retry.
Session cleanup использует отдельный bounded context даже при cancellation.
Own session Location/token запоминаются до разбора response body: malformed
или oversized успешный login response остаётся ошибкой, но разрешает cleanup
созданной session по проверенной same-origin ссылке. Cleanup ограничен одной
секундой либо меньшим configured request timeout.
Новый poll не сохраняет истёкший token между runs. Если BMC недоступен во время
logout, удаление session не гарантируется: дальнейшее освобождение зависит от
таймаута BMC, поэтому account/session budget требует настройки на оборудовании.

Допустимы GET/HEAD и фиксированные session creation/logout requests. Adapter
не exposes `CommandExecutor`, generic Redfish Actions, power control, BIOS,
SecureBoot, firmware, accounts, storage или network writes. Наличие
`PowerState` не предоставляет command capability. `enable_mutations: true`
отклоняется config validation.

## Discovery и несколько ресурсов

ServiceRoot `Systems`, `Chassis`, `Managers` links задают collections. Members,
`Members@odata.nextLink` и `@odata.id` разрешаются только в том же HTTPS origin
и `/redfish/v1` service. Optional collections могут отсутствовать. Обход
не является recursive crawl всего API: поддерживаются известные resource
relationships и явно заданные resource types.

`InventoryBatchReader` — additive reader: один endpoint даёт bounded
`InventoryUpdate.Devices`. Старый `InventoryReader` SNMP/RouterOS по-прежнему
возвращает один Device. Hardware telemetry использует additive
`HardwareReader` с несколькими typed samples. Каждый ComputerSystem,
Chassis и Manager — отдельный observed Device. Для совместимости Device.Type
остаётся `unknown`; role задаёт typed `Hardware.ResourceType` (`ComputerSystem`,
`Chassis`, `Manager`). CPU/DIMM/Drive/PSU/Fan — scoped components.

Target ID `remote:server-bmc-01` обозначает подключение, а observed Device ID
`redfish-<hash>` вычисляется из configured ID, resource type и canonical URI.
Порядок members, время poll, display name и missing serial не меняют ID.
Изменение URI или Target ID меняет этот scoped ID; окончательный Asset identity
определяет Backend. Serial/UUID не генерируются при отсутствии properties.

Реальные `Links` сохраняют system↔chassis, manager→system и manager→chassis
отношения в `HardwareInventory.Relations`. Shared chassis допускается.
Это management/component evidence; BMC management link не является LLDP cable.
Collector не объединяет Manager, System и Chassis в physical Asset.

## Inventory fields

| Resource | Нормализованные доступные поля |
|---|---|
| ComputerSystem | Manufacturer, Model, SerialNumber, UUID, SKU, PartNumber, BiosVersion, PowerState, Status; Processors/Memory/Storage/Drives и network metadata через предоставленные ссылки |
| Chassis | Manufacturer, Model, SerialNumber, PartNumber, ChassisType, AssetTag, PhysicalSecurity intrusion state, Status |
| Manager | Manufacturer, Model, ManagerType, FirmwareVersion, Status, network address metadata при доступности EthernetInterfaces |
| Processor | Manufacturer/model, cores/threads/socket, Status |
| Memory | CapacityMiB→bytes, manufacturer/location/type/ECC description, Status |
| Storage/Drive | Model/serial, capacity bytes, protocol/media type/part number, Status |
| Fan/PSU | Identity/part number/status и available metrics; absent reading остаётся отсутствующим |

BIOSVersion System и FirmwareVersion Manager различаются. Chassis serial не
подменяет System serial. CPU inventory/health не является CPU utilization,
memory capacity/health не является RAM usage. Отсутствующие optional
properties не дают fabricated defaults. Неподдерживаемые OEM fields, network
configuration и complete firmware UpdateService inventory не обходятся.

## Health и metrics

| Redfish property/value | Normalized behavior |
|---|---|
| `Status.Health=OK/Warning/Critical` | `ok/warning/critical` |
| Missing/unknown Health | `unknown` |
| `Status.HealthRollup` | Независимый aggregate field с тем же mapping; не заменяет own Health |
| `Status.State` | Исходный enum, например Enabled, Disabled, Absent, UnavailableOffline, StandbyOffline, Updating; absent→`unknown` |
| `PowerState=Off` | Отдельное свойство питания, не автоматический Critical |

Health-only components входят в `TelemetrySample.HardwareHealth`; отсутствие
reading не превращается в нулевой metric.

| Standard resource/property | Metric / unit |
|---|---|
| Sensor Temperature с units Cel; legacy Thermal.Temperatures.ReadingCelsius; EnvironmentMetrics/ThermalMetrics readings | `hardware.temperature.celsius` / `Cel`; unknown/другие units не преобразуются |
| Modern Fan/Sensor и Thermal.Fans с RPM/`{rev}/min` | `hardware.fan.rpm` / `RPM` |
| Fan с percent/Percent | `hardware.fan.percent` / `%`, без преобразования в RPM |
| PowerSubsystem/PowerSupplyMetrics InputPowerWatts/OutputPowerWatts | `hardware.power.input.watts`, `hardware.power.output.watts` / `W` |
| Sensor/EnvironmentMetrics/legacy power readings | `hardware.power.watts` / `W` |

Metric сохраняет resource/component identity, unit, health/status и observation
metadata. Modern Sensor, ThermalSubsystem/Fan, PowerSubsystem/PowerSupply/
PowerSupplyMetrics, EnvironmentMetrics и legacy Thermal/Power поддерживаются
по реально предоставленным links. Unknown units или missing reading дают
health/evidence при наличии, без fabricated temperature/RPM/power.

## Source, время и доставка

SourceRef: `type=redfish`, `protocol=redfish`, runtime CollectorID,
TargetID и resource-relative `ResourceID`. IdentityEvidence содержит только
имеющиеся serial/hardware UUID/manufacturer/model/component facts и their
source. Provenance `simulated` сохраняется для fixtures в nested values и
legacy `source=simulated`; настоящий BMC по умолчанию `real`.

ObservedAt/CollectedAt используют время фактического collection в existing
Phase 1 temporal model. Достоверный standard reading timestamp сохраняется
отдельным optional SourceTimestamp; BMC DateTime не объявляет clocks synced.
Clock offset/uncertainty остаются unknown, NTP offset вручную не прибавляется.
Corporate NTP Phase 2 синхронизирует часы ОС обычной системной службой.
Backend received_at остаётся Backend-owned. BMC time correction отсутствует.
Invalid/zero/out-of-range `Sensor.ReadingTime` не передаётся как
SourceTimestamp; сохраняются реальные ObservedAt/CollectedAt без выдуманного
source time.

Flow: Redfish adapter → normalized models → remote.Registry → Agent.push →
sanitizer → existing immutable durable outbox → gRPC → application ACK.
Отдельной queue, sender или непосредственного доступа adapter к gRPC нет.
Retained delivery остаётся at-least-once с existing stable message IDs,
retention/priority/size limits и replay; exactly-once не заявляется.

Wire extensions — protocol-neutral HardwareInventory/HardwareStatus/
ComponentHealth/ResourceRelationship и source/identity metadata на existing
Device/Inventory/Telemetry/Metric. Field numbers/RPC/protocol version сохранены.
DeviceType использует существующий UNKNOWN, без новых enum values. Старый
Backend может принять прежние typed fields и игнорировать additions; полное
multi-resource hardware/source storage требует поддержки этих новых fields.
Backend/PostgreSQL changes и их E2E здесь не выполняются. [Wire contract](protocol.md).

## Limits, failures и partial results

Default bounds: 128 resources/components, 8 pages per collection, 512 HTTP
requests per poll, 5s request timeout, 30s poll timeout; response bytes берутся
из `remote.max_response_bytes` (default 1 MiB). Суммарное buffered response
budget — 8 MiB на poll. Config ceilings: resources≤1024 и remote.max_rows,
pages≤64, requests≤4096, request timeout≤30s, total timeout≤5m.
Request concurrency внутри Target — 1. Login/retries входят в request budget;
bounded logout разрешён после его исчерпания, чтобы очистить созданные sessions.

Optional inventory text ограничен 1024 bytes на property, State — 128 bytes;
слишком длинное optional поле подавляется, State становится `unknown`, а
snapshot отмечается partial с сохранением остальных валидных facts.
Resource/component URI вместе с legacy fragment identity ограничен 2048 bytes.
Relations ограничены 1024 на resource. Malformed явно предоставленная link
отличается от absent/null optional link и не считается доказательством
исчезновения component.

TLS ≥1.2, CA/SAN verification; insecure mode отсутствует. Cross-origin redirects
и links, userinfo, path traversal/encoded slashes и cyclic pagination
отклоняются. Credentials не пересылаются на другой host; environment HTTP
proxy для BMC не используется. Password/token/header/cookie/private-key и
полные raw BMC responses не попадают в normalized output/logs.

404 optional resource/malformed optional sensor дают `partial` с сохранением
валидных результатов. 401/403/network/TLS failures остаются failure, даже если
часть данных уже получена. 429/503 имеют bounded retry и cooldown с Retry-After;
последующие runs учитывают cooldown. Timeout/cancellation разблокируют poll.
Cooldown учитывает наиболее поздний valid Retry-After, включая response
повторного запроса. Ожидание GET retry отменяется через request context;
долгий Retry-After приводит к bounded failure/cooldown без немедленного retry.
Общие limits дают controlled error, а не unlimited traversal.
Partial snapshot не является основанием удалить прежний inventory.

## Проверка через официальные инструменты DMTF

Использованы [Redfish Mockup Server](https://github.com/DMTF/Redfish-Mockup-Server)
`23d21f9a3610f6b6fbb71ffc941296b159a524d2` и
[Redfish Interface Emulator](https://github.com/DMTF/Redfish-Interface-Emulator)
`5f51d8ac48e46665a2a85242434f125aa7b25172`, Python venv в ignored `.tools`.
Tools и Python dependencies не являются Collector runtime dependencies.
Для локального mockup с fixture certificate:

```text
python redfishMockupServer.py -H 127.0.0.1 -p 19443 -D public-rackmount1 --short-form --ssl --cert server.crt --key server.key
```

`public-rackmount1` входит в pinned Mockup Server. Использованный Interface
Emulator запускался в static Redfish mode с `MODE=Local`, `HTTPS=Enable`,
`STATIC=Enable`, `MOCKUPFOLDERS=["Redfish"]` и тем же локальным test certificate.
Для проверки binding Flask был ограничен `127.0.0.1`; исходный server без
debug по умолчанию слушает `0.0.0.0`, поэтому этот loopback wrapper относится
только к локальному test harness. Сертификат обязан содержать IP SAN для
`127.0.0.1` либо hostname SAN, соответствующий fixture endpoint.

Opt-in integration:

```sh
INFRAPULSE_REDFISH_FIXTURE_ENDPOINT=https://127.0.0.1:19443 \
INFRAPULSE_REDFISH_FIXTURE_CA=/path/to/server.crt \
INFRAPULSE_REDFISH_FIXTURE_AUTH=basic \
go test -count=1 -tags=integration -run '^TestDMTFRedfishServiceCollectorDurableACK$' -v ./internal/agent
```

Для fixture с несколькими ресурсами дополнительно задаются
`INFRAPULSE_REDFISH_FIXTURE_MIN_SYSTEMS=2` и
`INFRAPULSE_REDFISH_FIXTURE_MIN_CHASSIS=2`. В выполненном multi-resource опыте
второй System и Chassis созданы копированием DMTF mockup, с изменёнными URI,
коллекциями и явно synthetic identity. Это test data, а не ответы оборудования;
adapter не генерирует отсутствующие serial/UUID у реального BMC.

Integration принимает только loopback HTTPS и явно simulated provenance.
Official static tools не доказывают authentication настоящего BMC; session,
invalid credentials, reauthentication, TLS/error/limit tests используют local
HTTPS fixtures. Выполнены generic modern public-rackmount1, legacy Emulator и
derived DMTF fixture с двумя Systems/Chassis; exact results — [validation](validation.md).
Mock Controller ACK не является подтверждением production Backend storage.

Primary references: [DMTF Redfish](https://redfish.dmtf.org/),
[DSP0266 specification](https://redfish.dmtf.org/schemas/DSP0266_1.20.1.html),
[current schema index](https://redfish.dmtf.org/redfish/schema_index),
[gofish v0.27.0](https://github.com/stmcginnis/gofish/tree/v0.27.0).
Реальные BMC, Dell/HPE hardware, OpenWrt RAM/flash и Backend/PostgreSQL не
проверялись. MemoryMetrics ECC/error counters, OEM extensions и complete firmware
UpdateService enumeration не реализованы. Production sizing и vendor-specific differences требуют отдельной
hardware validation. Explicit IPMI fallback описан отдельно в [Phase 5](ipmi.md).
