# InfraPulse Agent: архитектура и план

Дата review: 2026-10-04. Module path — `InfraPulseAgent`, минимальная версия Go — 1.27. Архитектура отделяет сбор данных и выполнение разрешённых действий от транспортного протокола.

## Границы продукта

Agent — локальный сборщик и удалённый multi-vendor collector с исходящим соединением. Backend, Web UI, AI, историческое хранилище и отчёты сюда не входят. Mock Controller и Simulator — исключительно development-компоненты. Реальные данные не подменяются synthetic данными. В Simulator каждый sample/event имеет `source=simulated`.

Cross-platform означает ОС запуска Agent. Multi-vendor означает работу с удалёнными устройствами через сетевые протоколы без установки Agent на них. Local и Collector modes можно включать независимо и одновременно.

## Компоненты

| Компонент | Ответственность |
|---|---|
| Agent Core | Lifecycle, identity/session, registry устройств, wiring jobs, transport supervision, graceful shutdown. Не разбирает MIB и vendor JSON. |
| Local Platform | Реальные данные ОС. Linux использует /proc, /sys и системные API; OpenWrt переиспользует Linux counters, добавляет ubus/UCI/procd/fw4. Windows пока unsupported. |
| Remote Device Driver | Нормализация данных одного remote device, capability discovery, optional collectors/actions. Не читает YAML и не выбирает транспорт Backend. |
| Protocol Adapter | SNMP wire client, HTTPS RouterOS client; deadlines, cancellation, response/output limits. Не определяет policy действий. |
| Vendor Profile | Проверенные detection evidence, sysObjectID prefixes, metadata и дополнительные MIB mappings. Наличие profile не включает write capability. |
| Capability Model | Granular `system.read`, `health.read`, `inventory.read`, `interfaces.read`, `interfaces.enable/disable`, `topology.lldp`, `logs.read`, `telephony.read` и т.д. Status: supported/unsupported/unavailable + reason. |
| Scheduler | Один dispatcher, ограниченное число workers, интервалы/timeout, отсутствие перекрытия одного job, error isolation, cancellation. |
| Telemetry Pipeline | Timestamped snapshots, counters, корректные deltas/rates, health signals, sanitizer, bounded outbox. |
| Log Pipeline | Source adapters, bounded lines/batches, persistent cursors, rotation/truncate/recreate, sanitizer, explicit delivery semantics. |
| Topology Pipeline | LLDP/CDP/RouterOS neighbor observations, mapping портов, evidence/source/confidence, TTL и полные/частичные snapshots. |
| Command Pipeline | Typed decode → validation → target/capability → policy/protected resources → dedup → execution → verification → result. |
| Security Layer | mTLS, credentials by reference, enrollment extension, data sanitizer, redacted errors, limits; никаких shell actions. |

## Package structure

```text
cmd/{infrapulse-agent,infrapulse-mock-controller,infrapulse-simulator}
api/proto/infrapulse/v1/       # source contracts
api/gen/infrapulse/v1/         # generated Go, committed
internal/agent/               # orchestration, state, registry
internal/config/              # strict YAML + validation
internal/identity/            # persistent UUID + launch UUID
internal/model/               # transport-independent domain values
internal/platform/            # local interface and factory
internal/platform/{linux,openwrt}/
internal/driver/               # optional remote interfaces
internal/driver/{snmp,routeros}/
internal/driver/profiles/      # six profiles in one package initially
internal/protocol/{snmp,routeros}/
internal/scheduler/
internal/telemetry/            # counters/rates; health aggregates stay here initially
internal/logs/                 # cursors and source adapters
internal/telephony/
internal/commands/             # policy in same package initially
internal/credentials/
internal/sanitize/
internal/queue/
internal/transport/            # interface
internal/transport/grpc/       # mapping, TLS, stream
internal/version/
configs/ deployments/ docs/ tests/
```

Packages создаются по появлению работающего кода. Отдельные inventory/topology/events/health/security/logging packages не нужны для нескольких типов и функций; модели живут в model, orchestration — в agent, slog настраивается в entrypoint. SSH/NETCONF/gNMI и Windows не создаются как пустые fake drivers. FirewallProvider и ServiceManager добавляются вместе с первыми реальными providers.

## Основные интерфейсы (целевой контракт)

```go
type Platform interface {
    Identity(context.Context) (model.Device, error)
    Collect(context.Context) (model.TelemetrySample, error)
    Capabilities() []model.Capability
}

// Базовый driver мал. Optional interfaces не вынуждают реализовать всё.
type DeviceDriver interface {
    Probe(context.Context) (model.Device, error)
    Capabilities() []model.Capability
    Close() error
}
type TelemetryCollector interface {
    Collect(context.Context) (model.TelemetrySample, error)
}
type InventoryCollector interface {
    Inventory(context.Context) (model.DeviceInventory, error)
}
type TopologyCollector interface {
    Neighbors(context.Context) ([]model.Neighbor, error)
}
type CommandExecutor interface {
    Execute(context.Context, model.Command) model.CommandResult
}
type CredentialProvider interface {
    Get(context.Context, string) (Credential, error)
}
type Transport interface {
    Connect(context.Context) (Session, error)
}
type Session interface {
    Send(model.Envelope) error
    Receive() (model.ControllerMessage, error)
    Close() error
}
type DataSanitizer interface {
    Sanitize(model.Envelope) model.Envelope
}
type Job struct {
    Name string
    Interval, Timeout time.Duration
    Collect func(context.Context) error
}
```

Session lifetime определяется переданным Connect context. Один sender и один receiver на stream; Close отменяет блокирующие операции. Deadline отдельного Send реализуется отменой session watchdog, без неограниченного создания goroutines.

Protocol adapters конкретны там, где одна implementation. Узкие fakeable client interfaces определяются у потребителей для тестов. Providers получают адрес/options и credentials из composition root, не из YAML.

## Нормализованная модель

`AgentInfo` содержит persistent agentID, sessionID текущего запуска, name, hostname, version/commit/buildTime, platform и capabilities. `AgentHealth` содержит состояние collectors/transport/outbox и recent errors с ограниченным размером.

`Device`: стабильный ID, name/hostname, managementAddress, vendor/product/model, type (server/workstation/router/switch/firewall/gateway/access-point/voice-gateway/pbx/hypervisor/unknown), serial/hardwareRevision/softwareVersion/firmwareVersion, OS/architecture, chassisID, uptime, driver, capabilities, online, lastSeen. Неизвестные значения отсутствуют, а не выдумываются. Local device ID выводится из persistent agentID; remote ID задаётся config/Backend и не меняется при переименовании.

`PhysicalLocation`: site/datacenter/building/room/rack/rackUnit/position/comment. Только config/Backend metadata.

`TelemetrySample`: deviceID, observedAt, collectedAt, source, uptime/boot identity, optional CPUStats/MemoryStats, interface counters and derived rates, metrics и health signals. Нет CPU=0, когда CPU не собран: presence/validity отделяют ноль от отсутствия. Counters — uint64. Units и metric names стабильны.

`CPUStats`: logical/physical cores, model, optional usagePercent, load1/5/15. `MemoryStats`: total/used/available/free/swapTotal/swapUsed, optional PSI. `Interface`: stable identity/index/name/MAC/MTU/admin/oper/speed/addresses, RX/TX bytes/packets/errors/drops, optional rates. `StorageStats`, `Route`, `DNSInfo`, `ServiceInfo`, `FirewallRule` добавляются как реальные collectors.

`Neighbor` хранит raw identifiers/subtypes и resolution evidence. `TopologyEdge` содержит localDeviceID/localInterface/remoteDeviceIdentifier/remoteChassisID/remoteInterface/protocol/observedAt/source/confidence и optional VLAN/network. Неподтверждённый remote остаётся unresolved node, не ошибочно объединяется по hostname.

`LogEvent`: eventID, timestamp/collectedAt, deviceID/hostname/source, facility/severity/program/message, bounded structured fields, cursor/sequence. AuditEvent — отдельный source с raw record и common fields. `TelephonyHealth`: только реально наблюдаемые values, SIP OPTIONS availability/latency отдельно от PBX call statistics.

`Command`: commandID, targetDeviceID, action enum/typed parameters, requestedAt, timeout, initiator human/ai_suggested/automation, idempotencyKey. `CommandResult`: status, startedAt/finishedAt, changed, errorCode/safe errorMessage, typed verification result. Unsupported actions не исполняются.

## Protobuf и transport

Versioned package `infrapulse.v1`, proto3, generated official Go stubs. Timestamps — google.protobuf.Timestamp, duration — Duration, UTC. Transport mapping единственное место зависимости domain models от wire representation. Core не импортирует grpc/protobuf.

```proto
service AgentService {
  rpc Connect(stream AgentEnvelope) returns (stream ControllerEnvelope);
  // Enrollment отдельным unary RPC после появления реального CA workflow.
}
message AgentEnvelope {
  string message_id = 1;
  string agent_id = 2;
  string session_id = 3;
  uint64 sequence = 4;
  google.protobuf.Timestamp sent_at = 5;
  oneof payload {
    AgentHello hello = 10;
    Heartbeat heartbeat = 11;
    TelemetryBatch telemetry = 12;
    InventoryUpdate inventory = 13;
    TopologyUpdate topology = 14;
    LogBatch logs = 15;
    EventBatch events = 16;
    CommandAck command_ack = 17;
    CommandResult command_result = 18;
    HealthStatus health = 19;
    ErrorReport error = 20;
  }
}
message ControllerEnvelope {
  oneof payload {
    HelloAck hello_ack = 1;
    Command command = 2;
  }
}
message Command {
  string command_id = 1;
  string target_device_id = 2;
  google.protobuf.Timestamp requested_at = 3;
  google.protobuf.Duration timeout = 4;
  Initiator initiator = 5;
  string idempotency_key = 6;
  oneof action {
    InterfaceAction interface_action = 10; // explicit enable/disable enum
    ServiceAction service_action = 11;     // start/stop/restart enum
  }
}
```

Не все перечисленные payload collectors появляются в P0. P0 wire schema реализует hello/ack, heartbeat, telemetry, inventory, events/error и безопасный unsupported command result; остальные message shapes документированы и добавляются совместимо в P1/P2. Никаких google.protobuf.Any для metrics/actions. Необязательные scalar fields — optional; metadata maps допустимы только для bounded labels/structured fields. Удалённые field numbers резервируются.

Hello — первое сообщение каждой session. Controller подтверждает protocol version. До acknowledgement обычный outbox не отправляется. Production controller связывает agentID с сертификатной identity; Mock не является production авторизацией.

P0 доставка best effort: отправка gRPC не означает durable acceptance Backend. Возможны loss/duplicate при disconnect/restart, Backend должен deduplicate по messageID. Durable ACK/replay, persistence command results и disk queue — отдельный P3 контракт, не обещание P0.

## Multi-vendor SNMP

Единственный Generic SNMP driver читает SYSTEM/IF-MIB/IF-X-MIB/LLDP-MIB/при наличии ENTITY-MIB. Protocol layer отвечает за BER/UDP/versions/walk; driver — за нормализацию. Cisco/Huawei/MikroTik/Juniper/Eltex/QTECH profiles добавляют проверенные detection prefixes и metadata. Vendor-specific OID добавляется только с подтверждённой MIB и fixture/test. SNMPv2c read-only в P1, SNMPv3 auth/privacy options предусмотрены; capability unsupported до реализации.

Vendor detection: explicit configured hint сохраняется отдельно → sysObjectID longest verified prefix → sysDescr как слабое evidence → unknown. Configured profile не считается автоматическим обнаружением. PEN/OEM неоднозначность допускается; конфликт evidence показывается в diagnostics. Модель и serial не выводятся из названия вендора. Profile не объявляет LLDP поддержанным без успешного probe MIB.

## Topology

LLDP remote table индексируется timeMark/localPort/remIndex. lldpLocPortNum не всегда ifIndex; mapping строится через local port ID/subtype, ifName/ifDescr/MAC, при необходимости BRIDGE-MIB. Raw IDs/subtypes сохраняются. Отсутствующие OID и ошибки walk дают partial snapshot, не удаление всех edges.

RouterOS neighbor discovery может возвращать LLDP/CDP/MNDP observations: source сохраняется фактическим. ARP/ND/FDB — evidence логической смежности; не объявляется физическим кабелем. TopologyUpdate имеет observedAt и признак complete/partial; link removals только после complete collection и TTL. Dedup/reconciliation графа между collectors — ответственность Backend.

## Fast pipeline и ограничение ресурсов

Default fast=3s, medium=30s, inventory/topology=5m, heartbeat=10s. Все configurable. Fast сбор CPU/RAM/counters/oper/errors/drops; полные inventory/topology не отправляются каждые 3s. Если job не успевает — пропускается повторный запуск, не копятся задачи. Remote pool имеет общую concurrency и per-device exclusion; offline после трёх последовательных connectivity failures, not unsupported OID.

Rates = delta/actual elapsed monotonic time. Первый sample не имеет rate. Reboot, ifCounterDiscontinuityTime, reset и смена interface identity сбрасывают baseline. Для 32-bit overflow нужен known width и правдоподобная граница скорости; неоднозначный reset/overflow даёт unavailable rate. CPU usage — delta cumulative busy/total без двойного учёта guest. Trends/rolling stats только математические, не AI.

Outbox ограничен количеством сообщений, retained payload bytes и размером каждого сообщения. Приоритеты: results → critical events → important logs → inventory/topology → heartbeat → telemetry. Telemetry drops oldest; счётчики drop по class видны в heartbeat. Резерв для results; при невозможности сохранить result новые commands отклоняются до action. Max devices/interfaces/lines/labels/response sizes дополнительно ограничивают working set. Один sender/receiver, bounded command workers, bounded recent errors.

Send может блокироваться gRPC flow control. Watchdog отменяет session после configured send_timeout; exponential backoff с jitter 1s→2s→…max. Reset после hello handshake. Collector jobs продолжаются offline. Очередь P0 в памяти и теряется при restart; persistence — P3.

## Logs, commands, security

Logs P2: open file inode/device identity + offset; bounded read, restart cursor; rotation дочитывает старый descriptor и затем новый, truncate сбрасывает offset. Persistence через temp+fsync+atomic rename. Cursor committed after enqueue означает best effort, после Backend durable ACK — at least once. Journald использует native cursor, если доступен adapter; не читается весь журнал заново. Audit records отдельно, correlation на Backend.

Commands default-deny и bounded concurrency. InterfaceAction принимает stable ID/name, не CLI/API endpoint. Parameter validation, stale requestedAt, timeout cap, target/capability/policy проверяются до action. Protected_devices/interfaces/services проверяются по каноническим resolved identifiers: .id/name/index/MAC aliases нормализуются driver. Все disruptive operations запрещены protected resources; management interface нужно явно отметить, auto detection дополнительная защита, а не гарантия маршрута. Config example включает protect list и отключённые write commands. Idempotency fingerprint включает target/action/params; одна key с другими параметрами отклоняется. Duplicate in progress не запускает action повторно. P0 любые mutations unsupported.

RouterOS P1: HTTPS REST, GET resource/identity/interfaces/addresses/routes/neighbors; PATCH существующего interface .id с disabled=true/false; read-after-write verification. Credentials Basic Auth только HTTPS, custom CA, никакого production InsecureSkipVerify. Перед execution повторно проверяются имя/.id и protected aliases. Timeout после mutation может означать uncertain state: result failed/timeout с verification evidence, автоматического слепого rollback нет.

AI только на Backend: evidence → recommendation → human approval → typed Command. Initiator не меняет trust и не обходит policy. У Agent нет LLM client и произвольного shell. Snapshot/Apply/Verify/Rollback extension позже, rollback unsupported пока нет реальной реализации.

Production transport — TLS >=1.2 и обязательный client certificate/CA/private key (mTLS). Development plaintext только explicit mode и loopback controller/listener; remote plaintext в P0 запрещён, отдельного opt-in нет. Enrollment пока documented extension, one-time token не превращается в готовый сертификат без CA workflow. CredentialProvider в P1: config/env references → позже Vault/Backend; секреты не сериализуются в diagnostics/errors/logs. DataSanitizer очищает известные секретные поля перед outbox; free-text redaction имеет ограничения и не заменяет policy Backend для cloud AI.

Shutdown: stop command admission, cancel scheduler/polls/session, wait bounded, persist cursors/state, close clients. Volatile queue нельзя обещать полностью flush при offline controller.

## Dependencies

| Dependency | Зачем и почему | Альтернативы/этап |
|---|---|---|
| google.golang.org/grpc | Официальный maintained Go gRPC runtime, HTTP/2 streaming/TLS/context. | ConnectRPC — иной контракт; standard net/http не заменяет gRPC. P0. |
| google.golang.org/protobuf | Официальные generated codecs/types, совместимость Backend. | Никакого handwritten protobuf. P0. |
| go.yaml.in/yaml/v3 | Strict KnownFields decoder, официальный maintained YAML organization fork, pure Go. | Старый gopkg.in/yaml.v3 upstream объявлял unmaintained; JSON не удовлетворяет YAML requirement. P0. |
| github.com/google/uuid | Mature UUID generation/parsing; не писать RFC implementation. | gofrs/uuid допустим, но отдельной причины нет. P0. |
| github.com/gosnmp/gosnmp | Mature pure Go SNMP v2c/v3, Get/BulkWalk. | Не реализовывать BER/SNMP самостоятельно. P1. |
| golang.org/x/sys | Только при реальной необходимости Linux syscalls/netlink/statfs. | /proc/sys + stdlib для P0. P1/P2. |

CLI flag, slog, crypto/tls, net/http, os/signal, contexts — standard library. Cobra/Viper/ORM/event bus не нужны. Protoc и protoc-gen-go/grpc — pinned build tools, не runtime dependencies. Versions фиксируются в go.mod/go.sum после разрешения. Runtime dependencies pure Go; portability подтверждается cross-build, а не декларацией. На маленьких OpenWrt остаётся риск RAM/flash/certificate store и vendor Go architecture support.

Primary references: [gRPC Go](https://grpc.io/docs/languages/go/basics/), [flow control](https://grpc.io/docs/guides/flow-control/), [protobuf presence/versioning](https://protobuf.dev/programming-guides/proto3/), [YAML maintained fork](https://github.com/yaml/go-yaml), [UUID](https://github.com/google/uuid), [GoSNMP](https://github.com/gosnmp/gosnmp), [IF-MIB RFC2863](https://www.rfc-editor.org/rfc/rfc2863), [RouterOS REST](https://help.mikrotik.com/docs/spaces/ROS/pages/47579162/REST+API).

## Риски и проверки

1. Multi-vendor MIB differences: fixtures, unsupported/partial semantics, реальные devices перед заявлением подтверждённой поддержки.
2. LLDP port/chassis mapping и OEM PEN: raw evidence, no hostname-only merging, partial topology expiry.
3. Counters/reset/overflow: actual elapsed time, discontinuity markers, unit tests и bounded state.
4. Command disconnect/timeout: protected IDs, default-deny, verification, bounded dedup; no exactly-once claims.
5. Controller backpressure: byte/message limits, send cancellation, reconnect tests с реальным stream.
6. Linux/OpenWrt variability: fixture /proc/sys tests + Linux live smoke отдельно. Cross-build не доказывает runtime.
7. Identity/cursor crash safety: exclusive creation, validation corruption, sync; ошибки persistence не создают новый ID молча.
8. Log storms и sensitive data: line/batch limits, cursor delivery documentation, sanitizer tests, secret-free diagnostics.
9. Production PKI/enrollment: test CA mTLS positive/negative tests, certificate identity authorization — будущий Backend.

## Implementation plan

| Этап | Работа | Критерий завершения |
|---|---|---|
| P0.1 | Bootstrap, strict config/defaults/validation, version/CLI, identity/models | config/identity tests, build/vet/test |
| P0.2 | Linux identity/CPU/RAM/network; unsupported local platform fallback | fixture mapping tests, linux amd64/arm64 cross-build |
| P0.3 | Controlled scheduler, bounded priority queue, counters/rates, backoff | cancellation/timeout/non-overlap/drop/reset tests |
| P0.4 | Versioned protobuf, generated stubs, TLS/mTLS transport, hello/heartbeat | real bidi stream test, TLS rejection test |
| P0.5 | Agent wiring, Mock Controller, graceful shutdown/diagnostics | end-to-end hello→telemetry→heartbeat, outage→reconnect; no hardware required |
| P0.6 | README/docs, config, systemd, build scripts/CI | gofmt/build/vet/test/race where toolchain permits; runtime limitations stated |
| P1 | Generic SNMP v2c/interfaces/IF-X rates/inventory/LLDP; RouterOS HTTPS mappings; remote registry, online threshold; typed interface actions/policy/protection | fixtures + simulated protocol servers, e2e commands protected rejection and disable/enable; real hardware tests separately tagged |
| P2 | Native OpenWrt, six verified profiles, logs/audit/cursors, firewall read/services, SIP OPTIONS, Simulator scenarios/trends | rotation/restart tests, pure simulated source labels, Linux/OpenWrt live integration and telephony fixture |
| P3 | Windows local, SNMPv3, NETCONF/gNMI as justified, durable outbox/results, enrollment, config snapshots/rollback, advanced telephony | feature-specific verified capabilities/security/compatibility tests |

P0 не заявляет OpenWrt-native, SNMP, RouterOS, physical topology, logs, PBX статистику или write actions готовыми. Они следуют после рабочего P0. Требуемые vendor/protocol/policy/log unit tests добавляются вместе с implementation, без fake production collectors.
