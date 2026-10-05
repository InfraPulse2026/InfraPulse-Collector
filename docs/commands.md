# Durable commands, snapshots и rollback

P3 сохраняет типизированные `interface.enable`/`interface.disable` RouterOS и
добавляет `resource.rollback`. Команда rollback содержит только `device ID` и
Agent-owned `snapshot ID`. Произвольные JSON, REST paths, CLI, XML, shell и
configuration payloads не принимаются.

## Pipeline

```text
decode → validation → outbox result reservation → persistent command admission
  → target/capability/policy → canonical interface + dependency protection
  → fresh RouterOS version/identity → persistent snapshot + executing state
  → PATCH once → read-after-write verification → persistent result envelope
  → outbox → Controller ACK → acknowledged journal state
```

Один device exclusion переиспользует registry gate: polling и mutation одного
устройства не выполняются параллельно. Перед PATCH Driver повторно читает
canonical identity, admin state и bridge/bond/VLAN dependencies. Все initiators
проходят одинаковые проверки. Смена state/identity после snapshot запрещает
запись; произвольного executor нет.

## Persistent execution journal

При `commands.enabled: true` journal хранится в
`agent.state_dir/commands/safety-state.json` даже с `queue.mode: memory`.
Если оператор выключил commands после restart, существующий journal продолжает
загружаться для доставки pending results; включение mutations для этого не
требуется. Пустой journal при disabled commands не создаётся.
State directory и файл на Unix имеют permissions `0700` и `0600`; это обычная
filesystem protection, а не encryption/TPM/HSM. `safety-state.lock` обеспечивает
single writer через advisory `flock` на Linux/OpenWrt и `LockFileEx` для
development builds Windows. Agent с недоступным lock/storage продолжает
monitoring, но не начинает mutations.

Journal сохраняет command ID, idempotency key aliases, fingerprint typed action,
device/resource/operation, received/created/updated timestamps, execution state,
verification state, result и result envelope со стабильным message ID.
Повтор command ID или idempotency key с другими параметрами отклоняется.
Повтор completed command получает сохранённый result; mutation не повторяется.
Read-only historical lookup проверяет typed shape и fingerprint, но не требует
свежего исходного `requested_at` или включённой текущей policy для уже
сохранённого результата: этот ответ описывает прошлую execution, а не разрешение
новой mutation. Unhealthy journal не принимает новые commands и не доставляет
непроверенный state, но его ошибка не разрывает обычный transport на telemetry ACK.
Новая identity alias, найденная по уже известному idempotency key, синхронно
привязывается к historical record перед ответом; это metadata update без external
execution. Поэтому последующий повтор alias ID после restart без key всё равно
deduplicates. Отказ persist binding отклоняет ответ admission и не начинает
mutation. Lookup уже известных IDs/keys сам не пишет state.

Admission резервирует в persistent record ограниченное место для будущего result.
Если даже outbox result reservation невозможна, command не допускается к
execution и текущая session завершается с ошибкой admission. При полностью
заполненном storage нельзя обещать durable доставку нового rejection; ранее
принятые pending results сохраняются. Controller может повторить command после
освобождения capacity; retained journal исключает повтор уже выполненной mutation.
`executing` и snapshot публикуются одной synced atomic replacement перед внешней
операцией. Result сохраняется до outbox admission. На редких command transitions
выполняется `fsync`; frequent telemetry использует отдельную outbox strategy.
Запись сначала создаётся в единственном `.safety-state.tmp`, файл синхронизируется,
заменяет основной state, затем на Linux синхронизируется каталог. Checksum SHA-256
и strict schema обнаруживают повреждение; checksum не является защитой от
злоумышленника с правом записи state. Повреждённый state не сбрасывается и не
удаляется автоматически. Directory и staging file symlinks запрещены.

Размеры и retention задаются отдельно от telemetry:

```yaml
commands:
  enabled: false
  store:
    journal_max_entries: 256
    journal_max_bytes: 4194304
    journal_retention: 168h
    snapshot_max_entries: 256
    snapshot_max_bytes: 2097152
    snapshot_retention: 24h
```

Это defaults. Atomic replacement временно требует места примерно для двух
ограниченных state files; резервирование capacity не обещает физическую квоту
на CoW/filesystem и не исключает внешнее заполнение диска. Если запись после
mutation внезапно становится невозможной, Agent не повторяет mutation, а
recovery использует ранее сохранённый `executing` и возвращает `uncertain`.
Для constrained OpenWrt лимиты следует уменьшить под persistent storage и flash
budget; постоянные mutations требуют persistent state даже при RAM telemetry.

ACK journal независим от удаления outbox record. Crash между outbox deletion и
journal ACK может повторно доставить result с тем же message ID; Controller
должен deduplicate. Exactly-once для RouterOS не обещается.
Если Controller не поддерживает ACK и выбран разрешённый best-effort fallback,
освобождение queue после Send также пополняет её следующими recovered results:
pending results сверх queue capacity не требуют reconnect для доставки.

## Crash recovery

| Последняя durable граница | Recovery |
|---|---|
| `reserved`, execution boundary ещё не сохранён | `rejected / execution_interrupted`, без mutation |
| `executing`, включая crash до/после PATCH или verification | `uncertain / mutation_uncertain`, без автоматического retry |
| Result сохранён, ACK отсутствует | Replay сохранённого result envelope/message ID |
| `acknowledged` | Result delivery не повторяется при обычном запуске; duplicate command остаётся deduplicated |

`uncertain` отражает отсутствие достоверно сохранённого исхода внешней операции.
Read-only operator verification разрешена отдельно; автоматический PATCH после
restart не выполняется. Доставка и execution — независимые состояния:
`reserved/executing/succeeded/failed/rejected/unsupported/uncertain` и
`not_ready/result_pending_ack/acknowledged`. Wire использует существующие статусы
result; неопределённый исход — `status=timeout`, `uncertain=true`,
`error_code=mutation_uncertain`.

Только acknowledged entries старше journal retention удаляются при новом
admission. Pending results никогда не вытесняются ради других commands/telemetry.
Если journal заполнен, новые mutations отклоняются. Deduplication имеет явно
ограниченный retention horizon: после удаления старого acknowledged record
новая команда с той же identity не имеет бессрочной гарантии deduplication.

## Configuration snapshots

Snapshot содержит только RouterOS interface admin state: snapshot ID, device ID,
canonical `.id`, name/MAC/alias/description, dependency aliases, previous и
expected `admin_up`, source, observation/expiry timestamps, related command ID,
Driver/software/schema version и local generation. Secrets и полный device
configuration не сохраняются. После verification snapshot отмечается verified.
Snapshot без verified success не пригоден для rollback.

`CommandResult.snapshot_id` сообщает созданный snapshot. Срок годности default
24 часа. Expired snapshots удаляются при subsequent state admissions, кроме
связанных с ещё не acknowledged result; все counts/bytes всё равно bounded.
Создание snapshot при заполненном/недоступном storage закрывает путь к PATCH.

## Typed rollback

`resource.rollback` восстанавливает только предыдущий RouterOS interface
`admin_up`. Проверяются target, capability, policy, protected device, текущие
canonical IDs/aliases/dependencies, ownership, schema и свежая software version,
retention, verified status и текущее expected state. Snapshot должен оставаться
последним для canonical resource. Более новая reserved mutation помечает старый
snapshot superseded; удаление нового snapshot не оживляет старый после ABA.

Перед rollback создаётся новый snapshot текущего состояния, затем выполняется
тот же fixed PATCH и read-after-write verification. Успешный rollback consumes
исходный snapshot. Failed/uncertain writes не считаются успешными. Повтор самого
rollback command deduplicates journal, а новый command по consumed/superseded
snapshot получает conflict. Protected management interface нельзя изменить
rollback-командой в обход обычной защиты.

Snapshots не дают full backup, multi-resource transaction, rollback любых
RouterOS settings или защиты от всех внешних параллельных изменений устройства.
Typed read-before-write снижает риск, но удалённый API не предоставляет атомарный
CAS вместе с локальным state transaction.

## Проверки

Unit/HTTPS fixtures покрывают journal admission/results/ACK/restart/dedup,
reserved/executing/result/ACK crash boundaries, corruption/truncation/checksum,
limits/retention и storage failure before mutation. RouterOS HTTPS schema fixtures
проверяют snapshot persistence, rollback success, ownership/resource/version/state
conflicts, protected aliases, rejected/uncertain write, malformed write response
с успешным safe readback и duplicate rollback. Это fixtures, не RouterOS hardware
validation и не destructive power-loss/flash endurance testing.

Отдельный `go test -tags integration ./internal/commands` запускает subprocess
и принудительно завершает его после admission, перед simulated mutation, после
simulated mutation/verification, после persisted result/ACK и при partial staging
write. Проверяется восстановление journal, освобождение OS file lock, сохранение
message ID и отсутствие повторной simulated mutation. Ошибки `ENOSPC`, `EROFS`
и permission denied проверяются отдельными filesystem fault fixtures; реальный
production disk не заполняется и read-only mount не создаётся.
