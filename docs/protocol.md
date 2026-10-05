# Протокол Agent ↔ Controller

Действующий контракт находится в [agent.proto](../api/proto/infrapulse/v1/agent.proto).
Сгенерированные Go-файлы находятся в `api/gen/infrapulse/v1`: их создали
официальные protoc 33.5, protoc-gen-go v1.36.12 и protoc-gen-go-grpc v1.6.0.
Для обычной сборки protoc не требуется. Команды повторной генерации и закреплённые
версии описаны в [api/proto/README.md](../api/proto/README.md).

## Соединение и handshake

Agent инициирует исходящий HTTP/2 gRPC stream:

```protobuf
package infrapulse.v1;
service AgentService {
  rpc Connect(stream AgentEnvelope) returns (stream ControllerEnvelope);
}
```

Полное имя RPC: `/infrapulse.v1.AgentService/Connect`. Agent не открывает
входящий management port. После открытия stream он отправляет `AgentHello`;
Controller отвечает `HelloAck{accepted: true, protocol_version: 1}`. До этого
подтверждения Agent не начинает отправлять сообщения из outbox. Отказ, другая
версия, команда вместо acknowledgement или истечение timeout закрывают session.

Каждая session использует один последовательный sender и один receiver. Эти
две операции могут работать одновременно; два конкурентных `Send` или два
`Receive` на одном stream не поддерживаются. Transport не создаёт goroutine на
каждую операцию. Context, переданный в `Connect`, управляет всей жизнью session,
поэтому после handshake он остаётся активным. `Close` отменяет этот context,
закрывает gRPC connection и разблокирует сетевые операции; повторный `Close`
безопасен. Core отдельно ограничивает handshake и отправку watchdog timeout.

При разрыве Agent закрывает старую session, делает exponential backoff с jitter
и открывает новую. Успешный handshake сбрасывает backoff. Persistent `agent_id`
сохраняется после restart; `session_id` относится к запуску процесса и не
меняется при reconnect в этом запуске.

## AgentEnvelope v1

| Поле | Номер | Тип |
|---|---:|---|
| message_id | 1 | string, стабильный UUID сообщения |
| agent_id | 2 | string, persistent UUID Agent |
| session_id | 3 | string, UUID запуска |
| sequence | 4 | uint64, порядковый номер попытки отправки в этом запуске |
| sent_at | 5 | google.protobuf.Timestamp |
| hello | 10 | AgentHello, oneof payload |
| heartbeat | 11 | Heartbeat, oneof payload |
| telemetry | 12 | TelemetryBatch, oneof payload |
| inventory | 13 | InventoryUpdate, oneof payload |
| topology | 14 | TopologyUpdate, oneof payload |
| events | 16 | EventBatch, oneof payload |
| command_result | 18 | CommandResult, oneof payload |
| error | 20 | ErrorReport, oneof payload |

В одном envelope ровно один payload. Нормализованная Go-модель дополнительно
проверяется перед encoding: отсутствие и несколько payload являются ошибкой.
Невалидные timestamps, неизвестные enums, nil-элементы и бесконечные/NaN
метрики отклоняются. Базовый transport использует 4 MiB при незаданном лимите;
Agent передаёт `queue.max_message_bytes` (пример/default config: 1 MiB),
Mock Controller использует 1 MiB. Допустимый configurable диапазон
transport — 512 bytes–4 MiB; ограничение действует в обе стороны.

Номер 14 использован для `TopologyUpdate` согласно исходному protocol plan.
Номера 15, 17, 19 остаются свободны для `LogBatch`, `CommandAck`, `HealthStatus`.
Их появление не должно переименовывать или перенумеровывать
существующие поля. Удалённые в будущем поля нужно явно объявлять `reserved`;
занятый номер никогда не используется для другого смысла.

## Payloads

`AgentHello`: agent/session ID, name, build metadata, время запуска, локальный
`Device`, версия протокола. `Device` содержит identity/inventory, uptime,
boot ID, last seen, явно заданное физическое расположение и granular
capabilities. Неизвестные необязательные inventory fields остаются пустыми.
`Capability.supported=false` вместе с `reason` явно означает отсутствие
операции; наличие типа команды в protobuf само по себе не включает capability.

Remote additions `Device.system_description=24`, `sys_object_id=25`,
`system_location=26`, `system_contact=27`, `vendor_hint=28`,
`vendor_evidence=29`, `components=30`, `uptime_available=31`, `uptime_source=32`
сохраняют подтверждённые SYSTEM/ENTITY/API сведения. `Evidence` содержит source,
value и confidence (`high`, `medium`, `low`, `unresolved`). Configured hint
не является подтверждением vendor. Components сохраняют собственные ID/parent
и class/model/serial/revisions. PhysicalLocation не выводится из system location.
Credentials и credential references отсутствуют в Device/wire models.

`Heartbeat`: timestamp, uptime, build metadata, health, remote device count,
число и суммарный размер queued messages, dropped telemetry/other counters.
Heartbeat отделён от telemetry. Health отражает ошибки collectors;
это не результат AI или полноценная оценка здоровья инфраструктуры.

`TelemetryBatch.samples` — repeated `TelemetrySample`. Sample содержит
device ID, observed/collected times, source, boot ID, uptime, CPU, memory,
interfaces и небольшие типизированные `Metric{name,value,unit}`. Linux использует
typed CPU/memory structures; RouterOS resource CPU/RAM values передаются metrics,
без выдуманных Linux load/swap полей. Agent отправляет один sample на
envelope. Нормализованный `DecodeEnvelope` Mock Controller принимает ровно один
sample: multi-sample batch отклоняется, а не сокращается до первого. Будущий
Backend должен обрабатывать полный repeated batch непосредственно из protobuf.

CPU usage, memory pressure и interface speed — optional scalars. Измеренный
ноль сохраняет presence и отличается от недоступного значения. Message presence
для interface rates отличает рассчитанные нулевые rates от первого sample или
counter reset, когда rate ещё не определён. Counters — unsigned 64-bit
накопительные значения; rates — double в единицах за секунду. Адреса интерфейсов
передаются строками CIDR. Synthetic source допустим только как `simulated` в
development simulator/tests.

`Interface` дополнен `description=12`, `alias=13`, optional `type=14`,
`metadata=15`, `counter_metadata=16`. Metadata различает отсутствие
index/MTU/admin status. Counter metadata содержит mask доступности каждого
counter, width (`32`, `64`, `0` — unknown/aggregate) и optional discontinuity
ticks. Число с availability=false нельзя считать измеренным нулём.
Nil metadata сохраняет P0 contract: все counters доступны и 64-bit.
`InterfaceRates.available=9` позволяет передать частично определённые rates;
nil mask сохраняет P0 semantics всех rates. Unknown-width aggregate не получает
derived rate. `TelemetrySample.uptime_available=11` и `uptime_source=12`
различают реальный uptime и его отсутствие; у P0 payload эти поля отсутствуют.
SNMP `sysUpTime` относится к management subsystem, не обязательно к ОС.

`InventoryUpdate` содержит observed timestamp и список `Device`; `EventBatch`
содержит события со стабильным ID, type, device ID, timestamp, severity, message
и source. `ErrorReport` содержит component, code, message и timestamp. Секреты
очищаются в pipeline до помещения сообщения в outbox.

Времена событий представлены `google.protobuf.Timestamp` и преобразуются в UTC.
`observed_at` — момент измерения/наблюдения, `collected_at` — получение sample,
`sent_at` — текущая попытка отправки. Задержка в outbox не изменяет исходные
времена. `BuildInfo.build_time` является строковой build metadata из ldflags,
а не timestamp наблюдения инфраструктуры.

## TopologyUpdate

`AgentEnvelope.topology=14` содержит один snapshot одного configured устройства:

| Поле | Номер | Смысл |
|---|---:|---|
| device_id | 1 | Stable configured device ID |
| observed_at / collected_at | 2 / 3 | UTC observation и collection time |
| source | 4 | Фактический collector source |
| completeness | 5 | Enum COMPLETE/PARTIAL/FAILED |
| ttl_seconds | 6 | Collector expiry assumption, не advertised LLDP TTL |
| expiry_assumption | 7 | Описание предположения срока актуальности |
| observations | 8 | Repeated Neighbor |

Complete empty snapshot допустим и отличается от failed snapshot. Partial/failed
не означает удаления прежних links. Backend выполняет reconciliation; Agent
не объединяет неизвестные remote devices по hostname и не передаёт глобальный
graph как доказанную истину.

`Neighbor` сохраняет local device/interface ID/name, raw local port number/bytes
и subtype; raw remote chassis/port bytes и subtypes; remote system name/port
description; discovery protocol (`LLDP`, `CDP`, `MNDP`, `UNKNOWN`), source,
observed/optional expiry timestamps, evidence и confidence. Subtype 0 означает
unknown, LLDP subtypes 1–7 сохраняют исходный смысл. LocalPortNumber не ifIndex.
Отсутствующий LocalInterfaceID остаётся unresolved. RouterOS neighbor MAC имеет
unknown chassis subtype, поскольку API не гарантирует, что это LLDP chassis ID.
ARP/ND/FDB не кодируются как physical discovery protocol.

Mapping проверяет enums, timestamps, размер raw IDs/evidence/observation lists
и наличие metadata masks; unknown width numbers отклоняются до integer
conversion. Номера и типы существующих P0 fields сохранены.

## ControllerEnvelope и typed commands

`ControllerEnvelope` содержит oneof: `hello_ack=1` или `command=2`.
`HelloAck` содержит `accepted=1`, `protocol_version=2`.

`Command` содержит `id=1`, `target_device_id=2`, `requested_at=3`,
`timeout=4` (`google.protobuf.Duration`), `initiator=5`, `idempotency_key=6`
и ровно одну typed action:

- `interface=10`: interface ID и enum `ENABLE`/`DISABLE`;
- `service=11`: service name и enum `START`/`STOP`/`RESTART`.

Initiator enum: `HUMAN`, `AI_SUGGESTED`, `AUTOMATION`. Значение `UNSPECIFIED`,
неизвестные enum numbers, пустая action/resource, отсутствующий requested time,
неположительный timeout и Duration, не помещающийся в Go `time.Duration`,
отклоняются. Transport выполняет structural validation; core дополнительно
проверяет target, срок, capability, policy и protected resources. Все initiators
проходят одинаковые проверки. Shell string, argv и arbitrary CLI отсутствуют.

`CommandResult` содержит command ID, typed status, started/finished times,
changed, error code/message. Statuses: `ACCEPTED`, `RUNNING`, `SUCCESS`, `FAILED`,
`REJECTED`, `TIMEOUT`, `UNSUPPORTED`. Времена начала/окончания имеют message
presence: они могут отсутствовать до соответствующего этапа действия.
`CommandResult.verification=8` содержит canonical InterfaceVerification:
interface ID/name, expected admin state, optional observed state/time,
verified/changed/uncertain и bounded safe evidence. `uncertain=9` у результата
явно отражает неподтверждённый outcome после отправки mutation. Status остаётся
`TIMEOUT`/`FAILED`, а не автоматически `SUCCESS`; fake rollback не выполняется.
Измеренный admin=false сохраняет optional presence. Verification с
verified=true требует совпадающего наблюдения и uncertain=false.

Реальные writes выполняются только RouterOS interface executor после capability,
default-deny policy и canonical protection checks. Local/SNMP и service actions
остаются unsupported. Protocol malformed command может завершить stream.
Command deadline ограничивается requested timestamp/timeout и временем admission.
Все initiators проходят одинаковые checks.

## Доставка и backpressure

Outbox ограничен числом сообщений, общим размером и размером одного envelope.
Он хранит immutable serialized copies в RAM и соблюдает приоритеты. При
переполнении разрешённый drop учитывается в heartbeat queue counters. Polling
не ждёт медленный Controller, а sender не создаёт неограниченное число workers.

Это best-effort delivery: успешный gRPC `Send` не является подтверждением
сохранения сообщения на Backend. Очередь не переживает restart Agent. Ошибка
отправки после извлечения из outbox также может потерять сообщение, включая
CommandResult; dropped queue counters описывают drops очереди и не подтверждают
доставку по сети. Следовательно, Agent не обещает exactly-once, durable command
results или надёжную доставку логов. Receiver Backend должен использовать
message/event IDs для deduplication при будущих retries. Для production log
cursors и durable command results потребуется persistent outbox + явный
application acknowledgement; это отдельная будущая реализация.

До execution Agent резервирует 4096 bytes/один message slot под result.
При недостаточной capacity или `max_message_bytes < 4096` команда не исполняется,
session закрывается; reservation гарантирует RAM admission, не доставку.
Dedup cache ограничен 256 commands/results, сверяет command ID/idempotency key
и fingerprint параметров, повторно выдаёт завершённый result с актуальным
command ID. Cache теряется при restart/eviction, persistent journal отсутствует.

## TLS и enrollment

В `production` Agent проверяет CA и hostname/SAN Controller и предъявляет
client certificate. Server требует сертификат, подписанный доверенным client
CA. Используются TLS ≥ 1.2 и обычная verification; `InsecureSkipVerify` не
включается. CA, certificate и private key заданы файлами. Explicit `development`
допускает plaintext только для loopback address, включая localhost.

Enrollment RPC, one-time token exchange, выдача/rotation сертификатов и
связывание certificate identity с конкретным `agent_id` не реализованы.
Сертификаты provisioning выполняется вне Agent; будущий Backend обязан отдельно
авторизовать certificate principal и проверить принадлежность Agent ID.
Mock Controller даже с mTLS остаётся development-компонентом и не является
production системой регистрации, авторизации или хранения данных.
