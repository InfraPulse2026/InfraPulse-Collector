# Доставка и durable outbox

P3 поддерживает `queue.mode: memory` и `durable`. Default сохраняет memory для
совместимости development configs; production рекомендуется durable на надёжном
локальном writable volume. Каталог состояния нельзя размещать в ephemeral
container layer. SQLite/CGO, отдельный database server и telemetry history здесь
не используются.

## Storage

Outbox — отдельный `state_dir/outbox/outbox.db`, embedded pure-Go
[bbolt v1.5.0](https://github.com/etcd-io/bbolt/releases/tag/v1.5.0).
Библиотека поддерживается etcd-io и предоставляет serialized ACID transactions,
synced commits и две checksum-protected metadata pages. Agent дополнительно
проверяет schema, ID index, page consistency и SHA-256 metadata/payload каждого
record. Checksums обнаруживают повреждение, но не защищают от владельца state.
Повреждённая DB сохраняется для явного recovery, автоматического reset нет.
Single-writer open ждёт освобождения filesystem lock не более одной секунды;
это bounded startup wait, а не retry команды или удалённой mutation.

Runtime dependencies остаются pure Go. bbolt mmap зависит от архитектуры/endian;
DB не следует переносить между разными endian targets. File size ограничен
`max_storage_bytes` через bbolt `MaxSize`; payload capacity отдельно ограничена
messages/bytes/message size. Page/index/JSON overhead требует запаса сверх
payload limit. RAM ограничивается configured payloads/transient record reads и
маленьким in-flight set; OS управляет mmap page cache. Файл переиспользует free
pages, но автоматически не уменьшается. Online compaction отсутствует. При
исчерпании file cap может понадобиться обслуживание остановленного Agent;
не удаляйте pending records для освобождения места.

```yaml
queue:
  mode: durable
  max_messages: 256
  max_bytes: 8388608
  max_message_bytes: 1048576
  max_storage_bytes: 67108864
  retention: 24h
  batch_delay: 50ms
  batch_size: 64
  max_inflight: 16
  ack_timeout: 30s
  ack_fallback: reject
```

Concurrent admissions и ACK opportunistically объединяются через bbolt Batch.
Каждый успешный Push возвращается после synced transaction, `NoSync` не
используется. Одно telemetry сообщение может содержать много interface samples;
отдельного fsync на каждую метрику нет. Изолированная batch всё равно требует
sync: безопасность durable admission важнее скрытого окна потерь. На слабом
OpenWrt flash выбирайте memory, writable external storage либо более редкий
polling; batched admission не устраняет flash wear. Размеры и CPU/RAM/flash
consumption конкретной firmware требуют runtime измерения.

Unix directory/file permissions — 0700/0600, существующие permissions
ужесточаются; symlink directory/DB отклоняются. Новый DB path публикуется с
directory sync на Unix. Power-loss guarantees зависят от filesystem/device,
который действительно выполняет sync; аппаратная power-loss validation не
выполнялась. Disk corruption/unavailable storage при startup переводит
monitoring в явно degraded memory mode и блокирует mutations; исходный durable
state сохраняется. Ошибки во время работы отражаются в health/diagnostics.

## ACK/replay

```text
enqueue → synced local durable admission → Send → Controller application acceptance
        → ACK(message_id) → synced local deletion
```

`message_id` создаётся до admission и сохраняется между reconnect/restart.
Replay меняет session/sequence/sent_at текущей попытки, сохраняя message ID и
исходные observed/collected timestamps. Pop в durable mode только берёт
ограниченную lease; запись остаётся до ACK. После disconnect leases сбрасываются.
ACK timeout вызывает bounded reconnect/backoff. Duplicate/unknown ACK harmless;
ACK не порождает Event на каждое сообщение.

AgentHello `application_ack=8` и HelloAck `application_ack=3` согласуют поддержку.
ControllerEnvelope `message_ack=3` содержит accepted ID. Mock ACK следует после
decode/identity validation и успешного observer/output; это development receipt,
а не production durable Backend. Он не сохраняет dedup DB. Production Backend
обязан атомарно принимать/deduplicate сообщения по `(agent_id,message_id)` и
подтверждать принятие только после своего application commit.

Semantics — **at-least-once для locally admitted, retained messages**. Duplicate
delivery возможна, including crash после Controller acceptance до local ACK
deletion. Exactly-once не заявляется. Неограниченное хранение не обещается:
retention/eviction могут терять low/medium priority данные, что видно по counters.

Если peer не поддерживает ACK, durable default `reject` прекращает exchange и
сохраняет bounded records; collectors продолжают admission/eviction в пределах
лимитов. Operator может явно выбрать `ack_fallback: send`: deletion после Send
даёт best effort, без обещания обработки Controller. Fallback виден в warning и
diagnostics. Memory mode сохраняет прежний best effort для telemetry; journal
CommandResult при ACK peer всё равно ждёт ACK. Legacy memory peer подтверждает
result journal после Send, что также является best effort.

## Приоритеты и logs

Durable priorities: CommandResult/reservation — protected; security/audit/events —
high; inventory/topology/logs — medium; heartbeat/SIP — ниже; frequent telemetry —
low. Eviction выбирает самый низкий допустимый priority и самый старый record.
Rejected admission не удаляет уже принятые записи. CommandResult не вытесняются
и не удаляются по telemetry retention. Переполнение только results блокирует
новую command admission. Memory сохраняет P2 log priority для изоляции bursts.
Retention применяется при новых admissions (Agent heartbeat регулярно создаёт
admission), а snapshots/journal имеют собственную retention.

Filesystem cursor, synced outbox admission и Controller ACK — разные состояния.
Log cursor advances после успешного Push всей batch. В durable mode batch уже
переживает restart; ACK только удаляет её из outbox и не управляет file cursor.
Crash между admission и cursor persistence может повторить records в другом
envelope; Backend дополнительно использует log record IDs. Последующая eviction
может потерять уже прочитанные logs. При недоступном/corrupted cursor reader
сохраняет fail-closed поведение P2.

Command journal/result/snapshot recovery описаны в [commands](commands.md),
wire additions — в [protocol](protocol.md). Disk/runtime files остаются вне Git.
