# Протокол Agent ↔ Controller

Реальный P0-контракт находится в [agent.proto](../api/proto/infrapulse/v1/agent.proto).
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
| events | 16 | EventBatch, oneof payload |
| command_result | 18 | CommandResult, oneof payload |
| error | 20 | ErrorReport, oneof payload |

В одном envelope ровно один payload. Нормализованная Go-модель дополнительно
проверяется перед encoding: отсутствие и несколько payload являются ошибкой.
Невалидные timestamps, неизвестные enums, nil-элементы и бесконечные/NaN
метрики отклоняются. Базовый transport использует 4 MiB при незаданном лимите;
Agent передаёт `queue.max_message_bytes` (пример/default config: 1 MiB),
Mock Controller в P0 использует 1 MiB. Допустимый configurable диапазон
transport — 512 bytes–4 MiB; ограничение действует в обе стороны.

Номера 14, 15, 17, 19 пока свободны для будущих `TopologyUpdate`, `LogBatch`,
`CommandAck` и `HealthStatus`. Эти сообщения не реализованы в P0 и отсутствуют
в текущем oneof. Их появление не должно переименовывать или перенумеровывать
существующие поля. Удалённые в будущем поля нужно явно объявлять `reserved`;
занятый номер никогда не используется для другого смысла.

## Payloads

`AgentHello`: agent/session ID, name, build metadata, время запуска, локальный
`Device`, версия протокола. `Device` содержит identity/inventory, uptime,
boot ID, last seen, явно заданное физическое расположение и granular
capabilities. Неизвестные необязательные inventory fields остаются пустыми.
`Capability.supported=false` вместе с `reason` явно означает отсутствие
операции; наличие типа команды в protobuf само по себе не включает capability.

`Heartbeat`: timestamp, uptime, build metadata, health, remote device count,
число и суммарный размер queued messages, dropped telemetry/other counters.
Heartbeat отделён от telemetry. Health в P0 отражает ошибки collectors;
это не результат AI или полноценная оценка здоровья инфраструктуры.

`TelemetryBatch.samples` — repeated `TelemetrySample`. Sample содержит
device ID, observed/collected times, source, boot ID, uptime, CPU, memory,
interfaces и небольшие типизированные `Metric{name,value,unit}`. Generic metrics
не заменяют typed CPU/memory/network structures. P0 отправляет один sample на
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

`InventoryUpdate` содержит observed timestamp и список `Device`; `EventBatch`
содержит события со стабильным ID, type, device ID, timestamp, severity, message
и source. `ErrorReport` содержит component, code, message и timestamp. Секреты
очищаются в pipeline до помещения сообщения в outbox.

Времена событий представлены `google.protobuf.Timestamp` и преобразуются в UTC.
`observed_at` — момент измерения/наблюдения, `collected_at` — получение sample,
`sent_at` — текущая попытка отправки. Задержка в outbox не изменяет исходные
времена. `BuildInfo.build_time` является строковой build metadata из ldflags,
а не timestamp наблюдения инфраструктуры.

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
P0 не выполняет write actions. Корректная команда получает `unsupported` или
`rejected` с причиной после policy/capability checks; protocol malformed command
может завершить stream, не выполняя действие. Execution и idempotent application
будут добавлены вместе с работающими P1 drivers.

## Доставка и backpressure

P0 outbox ограничен числом сообщений, общим размером и размером одного envelope.
Он хранит immutable serialized copies в RAM и соблюдает приоритеты. При
переполнении разрешённый drop учитывается в heartbeat queue counters. Polling
не ждёт медленный Controller, а sender не создаёт неограниченное число workers.

Это best-effort delivery: успешный gRPC `Send` не является подтверждением
сохранения сообщения на Backend. Очередь не переживает restart Agent. Ошибка
отправки после извлечения из outbox также может потерять сообщение, включая
CommandResult; dropped queue counters описывают drops очереди и не подтверждают
доставку по сети. Следовательно, P0 не обещает exactly-once, durable command
results или надёжную доставку логов. Receiver Backend должен использовать
message/event IDs для deduplication при будущих retries. Для production log
cursors и durable command results потребуется persistent outbox + явный
application acknowledgement; это отдельная будущая реализация.

## TLS и enrollment

В `production` Agent проверяет CA и hostname/SAN Controller и предъявляет
client certificate. Server требует сертификат, подписанный доверенным client
CA. Используются TLS ≥ 1.2 и обычная verification; `InsecureSkipVerify` не
включается. CA, certificate и private key заданы файлами. Explicit `development`
допускает plaintext только для loopback address, включая localhost.

Enrollment RPC, one-time token exchange, выдача/rotation сертификатов и
связывание certificate identity с конкретным `agent_id` не реализованы в P0.
Сертификаты provisioning выполняется вне Agent; будущий Backend обязан отдельно
авторизовать certificate principal и проверить принадлежность Agent ID.
Mock Controller даже с mTLS остаётся development-компонентом и не является
production системой регистрации, авторизации или хранения данных.
