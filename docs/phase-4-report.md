# Phase 4 — Redfish hardware monitoring: отчёт

Дата проверки: 2026-10-09. Реализация находится в рабочем дереве `main`.
Этот отчёт охватывает Collector/Edge Gateway, development Mock Controller и
локальные fixtures. Реальный BMC и production Backend/PostgreSQL не проверялись.
Phase 4 read-only adapter реализован и проверен на локальных HTTPS fixtures и
официальных DMTF tools. Mandatory regression/race, serial integration, Linux
suites, cross-build и protobuf checks прошли. Phase 5 и IPMI не реализованы.

## 1. Baseline

До изменений проверены `git status`, `git branch --show-current`,
`git log -5 --oneline`, `go version`, `go env` и существующий код.
Основной baseline — чистый `main` на `443b8d7`, Go 1.27.1, Windows/amd64.
Сохранён также baseline стабильной Phase 2 на `3b9b0ba`.

| Команда на исходном main | Результат | Время |
|---|---|---:|
| `gofmt -l` для всех Go sources | FAIL: 27 файлов с CRLF; отдельный LF-normalized frozen snapshot: 208 файлов, 0 unformatted, PASS | 0,135 s |
| `go build ./...` | PASS | 2,267 s |
| `go vet ./...` | PASS | 0,824 s |
| `go test -count=1 ./...` | PASS | 11,666 s |
| `go test -race -count=1 ./...` | PASS | 37,962 s |
| `go mod verify` | PASS | 0,370 s |

Дополнительный baseline: `go test -count=1 -tags=integration ./...`, native/cross
build scripts, protobuf generation/hash verification, проверка всех девяти
YAML examples и 16 Linux test suites в WSL — PASS. Эти результаты относятся
к исходной версии, а не заменяют проверки Redfish. Логи сохранены в ignored
`.cache/phase4-main-baseline` и `.cache/phase4-phase2-baseline`.

## 2. Git branch и worktree

По уточнению пользователя работа выполнена прямо в `main`, где Phase 3 уже
merged. HEAD остался `443b8d7` (`Merge pull request #1 from
InfraPulse2026/feat/phase-3-syslog-receiver`). Первоначальный неиспользованный
чистый worktree Phase 2 и его ветка удалены после этого уточнения; пользовательские
изменения не удалялись. Commit, push, merge, rebase, reset, clean и stash
для реализации Phase 4 не выполнялись. Изменения остаются unstaged.

Phase 4 использует generic models/metadata helpers текущего main. Redfish
adapter не зависит от syslog receiver, syslog config или log messages.

## 3. Интеграция с Collector

`Agent.New` → compile-time `driver.Registry`/factory → `remote.NewAdapters` →
existing bounded scheduler → normalized observations → `Agent.push` → sanitizer
→ existing durable outbox → gRPC → application ACK.

Добавлены optional `InventoryBatchReader` и `HardwareReader`. Старые
`InventoryReader`, `TelemetryReader` и SNMP/RouterOS interfaces сохранены.
Target обозначает подключение к endpoint; observed Devices — отдельные
обнаруженные ресурсы. Redfish не создаёт собственный runtime или delivery queue.

## 4. Client/library

Выбран и pinned `github.com/stmcginnis/gofish v0.27.0`: BSD-3-Clause,
minimum Go 1.21, runtime dependencies библиотеки — standard library.
Gofish client и typed schemas находятся только в `internal/driver/redfish`.
Domain models и protobuf не импортируют library-specific types.

Изучены официальные DMTF specification/schema index и current schemas,
включая modern и legacy thermal/power resources. Collection helpers библиотеки
не используются, поскольку их pagination/concurrency не соответствует
требуемым bounds. Enumeration выполняется последовательно по стандартным
links. Узкие mappings modern Fan и 64-bit capacity документированы:
они обходят ограничения library helper/`*int` на 32-bit platforms.

Источники: [DMTF Redfish](https://redfish.dmtf.org/),
[DSP0266 1.20.1](https://redfish.dmtf.org/schemas/DSP0266_1.20.1.html),
[актуальный schema index](https://redfish.dmtf.org/redfish/schema_index),
[gofish v0.27.0](https://github.com/stmcginnis/gofish/tree/v0.27.0).

## 5. Configuration contract

Используется существующий strict YAML contract `devices`, а не новая секция
`remote_devices`. Production example —
[config.redfish.example.yaml](../configs/config.redfish.example.yaml).

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

`address` — HTTPS origin без userinfo/query/fragment/resource path.
Existing CredentialProvider разрешает reference через environment variables.
Fast/topology polling отключён; явное ненулевое значение отклоняется.
Simulated provenance разрешён только явно в development configuration.
Mutation-enabled Redfish target отклоняется.

## 6. Authentication и TLS

Default — SessionService discovery, POST Sessions, private `X-Auth-Token`,
authenticated GET, bounded DELETE собственной session. При 401 разрешена
одна reauthentication за poll. 403/invalid login не запускают бесконечный retry.
Cleanup использует отдельный bounded context при cancellation. Explicit
`auth: basic` допускается только поверх проверенного HTTPS, без automatic fallback.

TLS certificate verification обязательна: system trust store либо custom CA,
hostname/IP SAN verification, minimum TLS 1.2. Custom CA добавляется к системным
корням; relative path разрешается от YAML directory. Insecure mode отсутствует.
Password, token, Authorization и cookies не передаются в observations/Backend.
Недоступный BMC во время logout не гарантирует удаление session: остаётся
server-side session expiry, что требует проверки account/session budget на BMC.

## 7. Resource discovery

Первый GET — `/redfish/v1/`. Systems, Chassis, Managers и SessionService
обнаруживаются через реальные ServiceRoot links. Поддерживаются Members,
`@odata.id`, pagination `Members@odata.nextLink`, optional absent collections.
Фиксированный `/Systems/1` не предполагается. Обход ограничен известными
resource types и relationships, не является recursive crawl всего service.

## 8. Multi-resource inventory

Один endpoint возвращает bounded `InventoryUpdate.Devices`: несколько
ComputerSystems, Chassis и Managers. CPU, DIMM, Drive, Fan и PSU представлены
scoped components. Старые single-device SNMP/RouterOS readers не изменены.
Registry хранит configured endpoint state отдельно от observed Devices.

Snapshot completeness — `complete`, `partial` или `failed`. Optional missing
resource и отдельный malformed sensor сохраняют валидные observations.
Partial snapshot сохраняет ранее известные inventory facts; auth/network/TLS
failure не превращается в успешный target poll. Cache и admission bounded.

## 9. Stable identities и relationships

Observed ID — `redfish-<hash>` от configured Target ID, resource type и
canonical resource URI. Порядок collection members, имя, timestamp и missing
serial не участвуют в ID. Изменение Target ID или URI меняет scoped ID.
Hardware UUID, serial и part number сохраняются только при наличии исходных
полей; fabricated identities отсутствуют.

System↔Chassis, Manager→System и Manager→Chassis relationships сохраняются
только из реальных Links. Shared Chassis поддерживается. Manager не объединяется
с ComputerSystem; management relation не выдаётся за LLDP physical cable.
Backend Asset Resolution не реализован.

## 10. Hardware inventory

| Resource | Поля при наличии |
|---|---|
| ComputerSystem | Manufacturer, Model, SerialNumber, UUID, SKU, PartNumber, BiosVersion, PowerState, Status, linked processors/memory/storage/network metadata |
| Chassis | Manufacturer, Model, SerialNumber, PartNumber, ChassisType, AssetTag, PhysicalSecurity, Status |
| Manager | Manufacturer, Model, ManagerType, FirmwareVersion, Status, linked network addresses |
| Processor | Manufacturer/model, cores/threads/socket, Status |
| Memory | CapacityMiB → bytes, manufacturer, location, type, ECC description, Status |
| Storage/Drive | Identity, model/serial, capacity bytes, protocol/media type/part number, Status |
| Fan/PSU | Identity, available part number/presence/state/health, linked readings |

BIOS firmware и BMC firmware различаются. System serial не подменяется Chassis
serial, hardware UUID не подменяется ID Manager. Отсутствующие optional fields
остаются unavailable. Full firmware UpdateService и MemoryMetrics error counters
не реализованы.

## 11. Health mapping

`Status.Health=OK/Warning/Critical` → `ok/warning/critical`.
Missing/unknown → `unknown`. HealthRollup сохраняется независимо, с тем же
mapping, и не заменяет own Health. State хранится отдельно как исходный enum
(Enabled, Disabled, Absent, UnavailableOffline и другие); missing → unknown.
`PowerState=Off` не означает hardware Critical. Health-only components
передаются без fabricated zero metrics.

## 12. Sensors и metrics

| Источник | Нормализованная metric |
|---|---|
| Sensor Temperature/Cel, legacy Thermal.Temperatures, EnvironmentMetrics/ThermalMetrics | `hardware.temperature.celsius`, `Cel` |
| Modern Fan/Sensor или legacy Fan с RPM/`{rev}/min` | `hardware.fan.rpm`, `RPM` |
| Fan с Percent/% | `hardware.fan.percent`, `%` |
| PowerSupplyMetrics InputPowerWatts/OutputPowerWatts | `hardware.power.input.watts` / `hardware.power.output.watts`, `W` |
| Sensor/EnvironmentMetrics/legacy Power readings | `hardware.power.watts`, `W` |

Modern Sensor/ThermalSubsystem/Fan/PowerSubsystem/PowerSupply/PowerSupplyMetrics
и legacy Thermal/Power выбираются по доступным links. Unknown units не
конвертируются. Missing reading не создаёт fake metric; health при наличии
сохраняется. CPU health не является CPU utilization, capacity DIMM — RAM usage.

## 13. Provenance и IdentityEvidence

SourceRef содержит `type=redfish`, `protocol=redfish`, runtime CollectorID,
TargetID и bounded resource-relative ResourceID. Nested component/metric health
имеет source/observation metadata, соответствующие своему resource.
IdentityEvidence сохраняет доступные manufacturer/model/serial/UUID facts и
их источник. Настоящее устройство по умолчанию `real`; tests/mockups явно
`simulated`. Sanitizer не заменяет missing identities fake redaction values.

## 14. Temporal semantics

ObservedAt и CollectedAt — UTC время реального collection в existing temporal
model. Standard reading timestamp сохраняется optional SourceTimestamp,
если он имеется и корректно распарсен. BMC DateTime не доказывает clock sync;
offset/uncertainty остаются unknown. Corporate NTP Phase 2 продолжает
синхронизировать часы ОС; ручной offset и BMC time correction отсутствуют.
Backend-owned received_at не задаётся Collector.

## 15. Protobuf compatibility

Добавлены generic typed HardwareInventory, HardwareStatus, ComponentHealth,
ResourceRelationship и IdentityEvidence; existing Device, InventoryComponent,
Metric, TelemetrySample, InventoryUpdate и SourceRef получили additive fields.
Field numbers не переиспользованы, RPC/protocol version сохранены.
Device.Type остаётся existing UNKNOWN; resource role задан Hardware.ResourceType,
что избегает новых enum values для older strict Backend.

Generic source/observation mapping вынесен в transport helper, доступный без
syslog coupling. Raw JSON blob не заменяет typed inventory. Older Backend может
игнорировать additions; полное сохранение hardware/relations/source и
multi-resource inventory требует реализации этих fields в Backend.
Backend код в этой фазе не менялся. См. [protocol.md](protocol.md).

## 16. Durable delivery

Adapter отдаёт normalized models через existing sink и `Agent.push`, без
прямого gRPC доступа. Sanitizer, immutable envelopes, stable message IDs,
durable replay, queue limits, application ACK и dedup semantics сохранены.
Local Agent E2E проверяет backend offline → retained outbox → Collector restart
→ replay тех же IDs → Mock Controller ACK → очистка подтверждённых сообщений.
Delivery остаётся at-least-once; exactly-once не заявляется.

## 17. Security и limits

Default bounds: 128 resources/components, 8 pages/collection, 512 requests/poll,
1 MiB response, 8 MiB aggregate response budget/poll, request timeout 5s,
poll timeout 30s, request concurrency 1. Config ceilings: resources≤1024 и
remote.max_rows, pages≤64, requests≤4096, request timeout≤30s, total≤5m.
Existing scheduler ограничивает target workers и исключает overlapping jobs.
Target failures имеют bounded backoff/jitter; 429/503 учитывают Retry-After.

Cross-origin links/redirects, userinfo, credential query, encoded path traversal,
cyclic pagination и превышение bounds отклоняются. Environment proxy для BMC
не используется. HTTP dumps/raw sensitive bodies не логируются. Credentials
и session tokens редактируются до normalized output, включая nested fields.

Read-only policy разрешает GET/HEAD, session creation и logout собственной
session. Power/BIOS/firmware/account/SecureBoot/storage/network writes,
generic action executor и Redfish CommandExecutor отсутствуют.

## 18. Unit, integration, security и failure tests

Добавлены tests config defaults/validation/credential reference/custom CA,
registry construction, multi-resource batches, IDs/evidence, clone ownership,
typed wire roundtrip/validation и sanitizer. Adapter tests покрывают ServiceRoot,
несколько Systems/Chassis/Managers, optional missing fields, CPU/DIMM/Drive/PSU
health, modern/legacy sensors, Celsius/RPM/percent, timestamps и provenance.

Security/failure fixtures покрывают valid/unknown/expired/mismatched TLS,
invalid credentials, 401 reauthentication, 403, cancellation, offline isolation,
timeouts, 429/503 cooldown, malformed/oversized responses, malicious links,
external redirects, cycles, page/resource/request limits и secret redaction.
Дополнительные tests проверяют typed wire bounds для optional strings,
relationships, generated resource fragments и unavailable zero source timestamp:
частичная observation должна оставаться encodable для durable replay.
Session lifecycle и failure cleanup проверяются HTTPS httptest, без production BMC.

Промежуточный полный прогон выявил security test FAIL для percent-encoded
credential в `@odata.id`. Это не исходный baseline failure. Причина исправлена;
окончательный полный unit/race rerun после исправления — PASS.

## 19. DMTF Mockup/Emulator E2E

Использованы official tools:
[Redfish Mockup Server](https://github.com/DMTF/Redfish-Mockup-Server)
`23d21f9a3610f6b6fbb71ffc941296b159a524d2` и
[Redfish Interface Emulator](https://github.com/DMTF/Redfish-Interface-Emulator)
`5f51d8ac48e46665a2a85242434f125aa7b25172`.
Python/venv/tool files находятся в ignored `.tools`, не runtime dependency Collector.

Команда opt-in E2E (endpoint/CA/auth задаются environment variables):

```text
go test -count=1 -tags=integration -run '^TestDMTFRedfishServiceCollectorDurableACK$' -v ./internal/agent
```

После final code freeze повторены три официальных E2E:

| Fixture/tool | Результат | Observations |
|---|---|---|
| Modern public-rackmount1, Mockup Server | PASS, 4,451 s | 1 System + 1 Chassis + 1 Manager; complete; 43 metrics на System и 6 на Chassis |
| Legacy, Interface Emulator | PASS, 3,235 s | 1 System + 1 Chassis + 1 Manager; partial; 41 metrics |
| Derived DMTF multi, Mockup Server | PASS, 4,606 s | 2 Systems + 2 Chassis + 1 Manager; complete; 43 metrics на каждом System и 6 на каждом Chassis |

Исходный System: manufacturer Contoso, modern model 3500/legacy 3500RX,
serial 437XR1138R2, UUID 38947555-7742-3448-3784-823347823834,
PowerState On, health ok. BIOS и BMC firmware сохранены раздельно.
Проверены повторные stable IDs и existing pipeline/Mock Controller application
ACK. Производные данные второго System явно synthetic/simulated.
Логи — `.cache/phase4-final/dmtf-{modern,legacy,multi}-e2e.log`.

Static tools не подтверждают working production authentication/session model.
Fixture/Mock Controller E2E не является Backend/PostgreSQL E2E или hardware validation.

## 20. Full regression и race

После final code freeze выполнены:

| Команда | Результат | Время |
|---|---|---:|
| `gofmt -l` всех Go sources | PASS, список пуст | 0,097 s |
| `go build ./...` | PASS | 1,901 s |
| `go vet ./...` | PASS | 0,584 s |
| `go test -count=1 ./...` | PASS | 11,640 s |
| `go test -race -count=1 ./...` | PASS | 36,152 s |
| `go mod verify` | PASS | 0,387 s |

Redfish adapter race suite в первом полном прогоне прошла. В Agent E2E второй
consumer вызывал `queue.Pop` одновременно с offline transport и его
`ResetInflight`: ожидание telemetry могло голодать до deadline, хотя тест должен
был проверять retained outbox. Тест исправлен: чтение после остановки A и
reopen B перед запуском B. Production runtime для этого исправления не менялся.
Targeted Redfish E2E race repeat `-count=3` — PASS 3/3, balanced sessions 6/6.

Queue failures относятся к Windows `t.TempDir`/`RemoveAll` (`directory is not
empty`); isolated queue repeat — PASS. Full sequential race repeat ещё имел
ошибку теста Redfish и прежний cleanup `DIAGNOSTICS.JSON.tmp` в Agent reject
fixture. Windows test cleanup helper исправлен: bounded 2s retry только для
`ERROR_DIR_NOT_EMPTY`; настоящий locked-file error не маскируется, что проверено
отдельным test. Targeted helper race repeat `-count=20` — PASS. Production
Agent shutdown для этого исправления не менялся. Последний полный mandatory
прогон после test fixes — PASS, включая race без reported data races.
Первые FAIL не скрываются: сохранены `race-first.log`, `race-serial-retry.log`,
`race-queue-retry.log` и final `race.log` в `.cache/phase4-final`.

Дополнительные команды уже выполнены:

| Команда | Результат |
|---|---|
| `go test -count=1 -tags=integration ./...` | Первичный прогон PASS; повторные Windows full attempts FAIL в existing cleanup, описано ниже |
| `go test -p 1 -count=1 -tags=integration ./...` | Финальный PASS, все tests; opt-in external fixture tests без environment skipped |
| `./scripts/build.ps1 -GoPath C:\Users\kiril\sdk\go1.27.1\bin\go.exe` | PASS, 9,649 s |
| `./scripts/build.ps1 -Cross -GoPath C:\Users\kiril\sdk\go1.27.1\bin\go.exe` | PASS, 51,208 s |
| Pinned protoc/protoc-gen-go/protoc-gen-go-grpc generation в temporary output, SHA256 comparison | PASS, оба generated файла совпали |
| `infrapulse-agent.exe validate-config --config <каждый configs/*.yaml>` | PASS, все 10 examples |
| `go test -c -tags=integration` и запуск test binaries в WSL docker-desktop | PASS, все 17 package suites |
| Linux `infrapulse-agent-linux-amd64 time-sync-status --config configs/config.time-sync.example.yaml` | PASS; provider unavailable без systemctl, synchronization не заявлена |

Regression охватывает existing Linux/OpenWrt, SNMP v2c/v3, RouterOS,
LLDP/CDP/MNDP, SIP, local logs/audit/syslog, NTP, scheduler, PKI, queue,
ACK/replay, commands/journal/snapshots/rollback и Simulator. Linux WSL suites,
proto generation/hash check и validate-config examples выполнены отдельно.
Windows integration retries после final freeze встретили existing logs/queue
`RemoveAll` cleanup failures. Isolated logs повтор прошёл; full integration с
последовательными package IO (`-p 1`) прошёл без исключения tests.
Исходные FAIL сохранены отдельно; Redfish E2E и adapter failures в этом
финальном integration случае не обнаружены.

Linux default telemetry intervals в последнем Agent suite:
3,000764552 s и 3,000588381 s.
Local fixture Redfish Agent E2E в Linux suite прошёл: 4 inventory resources,
verified HTTPS, offline/restart/ACK и balanced sessions 4/4.
Ни одного live network appliance test этот отчёт не подразумевает.

## 21. Cross-build

Existing `scripts/build.ps1 -Cross` выполнен с `CGO_ENABLED=0` для Collector
linux/amd64, linux/arm64, linux/arm GOARM=7, linux/mips и linux/mipsle
(MIPS hardfloat): PASS. Native build и companion Mock/Simulator builds — PASS.
Размеры текущего прогона приведены ниже; причина SDK footprint подтверждена
финальным read-only code/symbol review.

| Collector platform | Baseline bytes | Redfish bytes | Изменение |
|---|---:|---:|---:|
| linux/amd64 | 22 936 550 | 35 447 995 | +54,55% |
| linux/arm64 | 21 410 686 | 33 362 805 | +55,82% |
| linux/arm GOARM=7 | 21 539 544 | 33 265 787 | +54,44% |
| linux/mips | 23 675 000 | 35 785 247 | +51,15% |
| linux/mipsle | 23 674 046 | 35 786 581 | +51,16% |

Cross-build не подтверждает runtime на OpenWrt firmware или её RAM/flash budget.

## 22. Resource usage

Исходный synthetic scheduler workload: 32 fake SNMP targets, 4 workers,
25ms cadence/2ms collection delay/2s run. Concurrency peak 4, goroutines
1→6→1, queue peak 32 messages/31 048 bytes, total allocations 2 771 456 bytes.
Это baseline ограничения Collector, а не production Redfish throughput.

Финальный Redfish benchmark: 50 health polls, duration 247,1 ms,
retained heap delta 108 640 bytes, goroutine delta 0, request concurrency peak 1,
23 planned resources, inventory JSON 19 742 bytes, largest sample JSON
11 338 bytes/9 metrics. Test ограничивает retained heap/goroutines и проверяет
balanced sessions — PASS.

Повторный scheduler workload сохранил peak concurrency 4, goroutines 1→6→1,
queue 32/29 298 bytes; TotalAlloc 2 743 064 bytes (baseline 2 771 456).
`BenchmarkObserveTelemetry`: baseline 385,3 ns/op, 1056 B/op, 8 allocs/op;
после изменения 458,2 ns/op, 1152 B/op, 8 allocs/op. Это единичные synthetic
measurements, чувствительные к загрузке хоста. Рост binary 51–56% требует
учёта при deploy на ограниченное оборудование; он не объявляется приемлемым
production flash/RAM budget. Mockup numbers не являются production sizing.

Read-only ELF/symbol analysis связывает рост с обязательным graph
`gofish.Client.ConnectContext` → ServiceRoot UnmarshalJSON и library schemas/
reflection: 16 070 gofish symbols, 14 567 functions, 223 resource UnmarshalJSON
types. Non-strippable growth: `.gopclntab` +4 216 701 bytes, `.text` +2 306 272,
`.go.type` +1 502 968. Removable symbols/debug: `.strtab` +2 642 341,
`.symtab` +394 848 и debug sections +1 343 696 bytes. Заменить только adapter
DTO недостаточно, поскольку client connection сам входит в этот graph.
Сохранён mature SDK с измеренным cost; собственный fork/protocol rewrite и
lightweight build tag не добавлены. Paired stripped comparison приведён ниже.
Flash budget маленького OpenWrt устройства этим этапом не подтверждён.

Matched stripped linux/amd64 builds с `CGO_ENABLED=0`, `-trimpath`,
`-buildvcs=false`, `-ldflags='-s -w'`:

| Baseline bytes | Redfish bytes | Delta |
|---:|---:|---:|
| 15 732 896 | 23 855 264 | +8 122 368 bytes (+51,63%) |

Команда для обоих source snapshots:

```text
go build -trimpath -buildvcs=false -ldflags="-s -w" -o <output> ./cmd/infrapulse-agent
```

Stripping уменьшает абсолютный размер, но не устраняет основной SDK cost.
Лог — `.cache/phase4-final/stripped-binary-comparison.json`.

## 23. Changed files

Tracked modifications и new source/config/docs files проверены через
`git diff --name-only` и `git ls-files --others --exclude-standard`:

```text
README.md
api/gen/infrapulse/v1/agent.pb.go
api/proto/infrapulse/v1/agent.proto
configs/config.redfish.example.yaml
docs/architecture.md
docs/collector-core.md
docs/phase-4-report.md
docs/protocol.md
docs/redfish.md
docs/security.md
docs/validation.md
docs/vendor-support.md
go.mod
go.sum
internal/agent/redfish_dmtf_integration_test.go
internal/agent/redfish_e2e_test.go
internal/agent/remote.go
internal/agent/tempdir_windows_test.go
internal/config/config.go
internal/config/redfish.go
internal/config/redfish_test.go
internal/config/target.go
internal/driver/driver.go
internal/driver/redfish/adapter.go
internal/driver/redfish/adapter_bounds_test.go
internal/driver/redfish/client.go
internal/driver/redfish/redfish_test.go
internal/driver/redfish/session_failure_test.go
internal/model/clone.go
internal/model/hardware.go
internal/model/hardware_test.go
internal/model/identity_evidence.go
internal/model/model.go
internal/model/observation.go
internal/model/observe.go
internal/model/remote.go
internal/remote/batch.go
internal/remote/batch_test.go
internal/remote/registry.go
internal/sanitize/hardware.go
internal/sanitize/hardware_test.go
internal/sanitize/sanitize.go
internal/transport/grpc/hardware_mapping.go
internal/transport/grpc/hardware_mapping_test.go
internal/transport/grpc/log_metadata.go
internal/transport/grpc/log_metadata_test.go
internal/transport/grpc/mapping.go
internal/transport/grpc/observation_metadata.go
internal/transport/grpc/p2_mapping.go
```

Existing syslog files с одним изменением
CRLF/LF не имеют functional diff и не включены в список Redfish изменений.
Ignored caches, tools, generated test binaries, credentials и fixtures server
certificates не являются source changes.

## 24. Known limitations

- Реальные BMC, Dell/HPE hardware и vendor-specific differences не проверены.
- Production Backend/PostgreSQL/REST E2E отсутствует; typed additions требуют
  Backend persistence support для полного hardware/source/relationship хранения.
- Static DMTF tools не проверяют production credentials/session lifecycle.
- OEM extensions, full UpdateService firmware enumeration, MemoryMetrics ECC
  error counters и произвольный traversal Redfish API отсутствуют.
- Logout при недоступном BMC зависит от server-side expiry; BMC session limits
  и read-only role проверяются на реальном оборудовании.
- URI/Target ID задают scoped resource identity; перенос/смена URI требует
  Backend Entity Resolution, которого Collector не выполняет.
- Cross-build не доказывает OpenWrt RAM/flash/runtime пригодность.
- Default package-parallel Windows integration встретил existing directory
  cleanup failures; финальный полный integration PASS получен с `-p 1`.
- Размер Collector с SDK существенно вырос; library reflection/schema graph
  остаётся в binary. Deployment на маленький OpenWrt требует отдельного sizing.
- IPMI, power control и remediation operations не реализованы.

## 25. Readiness for Phase 5

Phase 4 формирует bounded read-only hardware observation path через existing
Collector delivery. Phase 5 IPMI fallback не начиналась. Для неё потребуется
отдельное решение по client, transport/auth, normalization, failure policy,
security и fixture/hardware validation. Финальные результаты Phase 4 сохранены
в [validation record](validation.md) и этом отчёте. Foundation готов для отдельной
следующей разработки; production hardware readiness не заявляется.
