# Backend-managed configuration — Phase 7

Collector получает полные snapshots monitoring Targets через существующий
исходящий gRPC/mTLS stream. Добавление, изменение, disable/remove и смена polling
interval не требуют перезапуска процесса. Backend B9 должен реализовать свою
часть [wire contract](backend-managed-config-contract.md); UI/API и production
Backend в этом репозитории не реализованы. Mock E2E проверяет development
Controller, а не Backend/PostgreSQL.

## Владение конфигурацией

Локальный strict YAML остаётся конфигурацией администратора. Collector никогда
не переписывает этот файл snapshot из Backend. `devices` и
`telephony.endpoints` — static local Targets. Snapshot управляет только отдельным
набором Backend-managed Targets; полный пустой `targets` удаляет только этот
набор. Raw Target ID должен быть уникален во всём локальном/managed наборе:
совпадение с local ID отклоняет всю ревизию, даже при другом протоколе.

Backend может изменять read-only SNMP, RouterOS, SIP, Redfish и IPMI connections,
`enabled`, metadata, polling и credential/CA references в пределах local policy.
Он не получает права менять Collector identity, Backend endpoint/trust,
certificates/private keys, enrollment, state/outbox paths, NTP, Syslog listener,
command policy, protected resources, service setup или права ОС. В snapshot нет
raw YAML, shell, произвольного API/CLI/Ansible или mutation options.

## Opt-in и локальная policy

Remote management по умолчанию disabled. Local Targets продолжают работать
с прежними настройками; автоматической миграции нет. `enabled: true` допустим
только с `security.mode: production`; development plaintext не становится
аутентифицированным каналом управления.

Минимальная administrator-owned секция:

```yaml
remote_management:
  enabled: true
  allowed_protocols: [snmp, routeros, sip, redfish, ipmi]
  allowed_credential_refs: [switch-read, bmc-read]
  allowed_destinations: [192.0.2.0/24, 2001:db8:10::/64]
  max_managed_targets: 128
```

Именованные `credentials` уже должны существовать в local YAML и environment
service user. Пустой allowlist credential refs не разрешает ни одного
authenticated Target; SIP остаётся fixed unauthenticated OPTIONS и требует
пустого `credential_ref`. Optional `tls_ca_refs` связывает public key reference
с локальным CA path; Backend передаёт только key, например `bmc-ca`. Paths
разрешаются относительно local YAML и не передаются Backend.

Полный пример с production placeholders и durable outbox —
[config.managed.example.yaml](../configs/config.managed.example.yaml).
`validate-config` проверяет schema/policy; для настоящего `run` нужны реальные
CA/cert/key, credentials, storage и подходящий Backend.

| Локальный параметр | Default | Допустимый диапазон |
|---|---|---|
| `enabled` | false | явный local opt-in |
| `allowed_protocols` | все пять протоколов | subset без повторов; `[]` запрещает все |
| `allowed_credential_refs` | пусто | до 1024 существующих refs |
| `allowed_destinations` | пусто | до 128 CIDRs; пусто — без дополнительного CIDR ограничения |
| `tls_ca_refs` | пусто | до 128 local key/path pairs |
| `max_managed_targets` | 128 | 1–1024, включая disabled |
| `max_config_bytes` | 262144 | 1024–1048576 |
| `min_polling_interval` | 1s | 10ms–24h |
| `min_hardware_interval` | 10s | от `min_polling_interval` до 24h |
| `apply_timeout` | 10s | 100ms–1m |

`max_config_bytes` ограничивает normalized domain JSON; transport отдельно
ограничивает protobuf snapshot 1 MiB. Это разные representations: snapshot
может пройти wire limit и получить `payload_limit` по local policy.
Existing scheduler/remote concurrency, response budgets и adapter security
limits остаются локальными. Snapshot не увеличивает их.
Переиспользуемый validator также ограничивает общий набор local + managed:
до 1024 non-SIP devices и до 128 SIP endpoints. `max_managed_targets` не
увеличивает эти существующие лимиты.

При настроенных CIDRs Collector проверяет literal IP или весь DNS answer
(1–16 адресов). Любой адрес вне allowlist отклоняет snapshot. Один разрешённый
numeric IP фиксируется для adapter: повторного DNS lookup при poll нет.
HTTPS/SIP TLS продолжают проверять исходное hostname/SAN/SNI; HTTP redirects
и environment proxy отключены. Pin пересчитывается при следующей ревизии
или restart restore, не при каждом poll. Когда CIDR allowlist пуст, дополнительный
DNS pin не применяется; destination permissions принадлежат администратору.
LLDP/CDP/MNDP management addresses не создают Targets автоматически.

## Snapshots, revisions и digest

Snapshot содержит Collector ID, текущий Session ID, положительный `uint64`
revision, SHA-256 digest и полный managed Target set. `issued_at` необязателен
и не является authentication/freshness проверкой. Digest вычисляется по fixed
v1 normalization независимо от local telemetry defaults; target order не влияет.
Точный язык-независимый encoding и test vector находятся в
[Backend contract](backend-managed-config-contract.md#canonical-digest-v1).
В digest входят references, но не resolved secrets; revision/session/time
доставки исключены. Digest проверяет consistency, mTLS проверяет peer.

Новая аутентифицированная ревизия сохраняет persistent highest-seen watermark
и terminal result, в том числе при validation rejection. Пример: applied 11,
rejected 12, applied 13; новая 11 после 13 отклоняется как stale. Same revision
с другим content/digest даёт conflict. Повтор still-active applied ревизии с
тем же content/digest возвращает APPLIED без повторного запуска Targets;
это допустимо и после более высокой rejected ревизии. Повтор последней
REJECTED/FAILED ревизии возвращает её terminal status; исправления требуют
новой, большей revision.

Новая ревизия с неизменным Target сохраняет его runnable instance. Изменённый
endpoint/options/interval/DNS pin заменяет соответствующий instance. Revision
`18446744073709551615` допустима, increment не выполняется и wrap к нулю
невозможен; дальнейшее продвижение требует administrative recovery.

## Apply и Target lifecycle

Последовательность: validate full snapshot/policy → resolve refs и prepare
adapters → prepare scheduler changes → cancel/drain изменённые jobs → опубликовать
новый registry с paused новыми jobs → persist last-known-good → commit jobs →
report APPLIED. Подготовка не проверяет постоянную доступность оборудования:
offline device может быть корректным Target и затем иметь connectivity errors.

ADD создаёт adapter/job. UPDATE останавливает старый poll через bounded context
и подготавливает замену. DISABLE сохраняет definition, но не создаёт adapter/job.
REMOVE останавливает poll и освобождает adapter. Static local jobs и unchanged
managed jobs сохраняются. Remote Registry использует существующий scheduler;
второго managed scheduler нет. Один worker применяет одну ревизию; буфер
вмещает ещё один snapshot. Burst сверх capacity закрывает session для retry
после reconnect, не уничтожая LKG/outbox.

Validation/prepare failure сохраняет текущий набор. Runtime/persistence failure
возвращает FAILED и выполняет bounded compensating recovery предыдущих entries/
jobs. Новые jobs не стартуют до успешной публикации persistent state. Если
recovery storage не удался, diagnostics отражает `storage_degraded`; success
не объявляется. Это компенсируемый runtime transition и atomic file publication,
а не OS transaction или защита от всех filesystem/power-loss failures.

При подготовке одновременно существуют прежние adapters и до
`max_managed_targets` новых; managed peak ограничен двумя наборами этого размера.
Local adapters не входят в этот managed budget. Все polls используют прежние
worker/time/response limits. Polling hardware по умолчанию health 30s/inventory
5m, а не network fast 3s.

Удаление Target не удаляет observations из durable outbox, не меняет original
SourceRef и не удаляет historical Backend data. ACK/replay и command journal
работают своим прежним путём. Config apply status не является equipment command.

## Last-known-good, restart и offline

Файл `agent.state_dir/managed-config.json` хранит один normalized LKG snapshot,
applied metadata, highest revision/fingerprint и terminal result. Secret values
не сохраняются; session/issued_at удалены из LKG. File format version, SHA-256
checksum, Collector identity и revision metadata проверяются при чтении.
Regular file/size/permissions проверяются; symlinks отклоняются. Публикация:
0600 temporary file → file fsync → rename → directory fsync на Unix.
Windows mode bits не заменяют administrator ACL. Размер ограничен
`min(4 MiB, 4 * max_config_bytes + 8192)`; history не накапливается.

При restart до соединения с Backend Collector повторно проверяет LKG против
текущего local YAML, credential availability и CIDRs, затем восстанавливает
разрешённый набор. Backend offline не мешает polling. При disabled management,
corrupt state или новой запрещающей policy managed Targets не активируются;
local monitoring продолжает работать. Restore применяется целиком, частичного
обхода policy нет. Diagnostics отличает сохранённую `last_known_good_revision`
от реально active `applied.revision`. После denied restore повтор сохранённой
APPLIED ревизии возвращает FAILED `applied_state_inactive`, active revision 0;
сохранённый факт успеха не выдаётся за работающий runtime.

Recovery — отдельное действие администратора: остановить Collector, сохранить
копию state и согласовать с Backend новую revision baseline; исправить local
policy/credentials/permissions либо осознанно архивировать managed state для
новой истории. Не менять `agent-id`, PKI/outbox/journal и не удалять весь
`state_dir`. После reset Backend обязан передать согласованный новый snapshot;
автоматического rollback/epoch reset через remote config нет.

## Status и diagnostics

Wire lifecycle: RECEIVED → VALIDATING → APPLIED / REJECTED / FAILED. Authentication,
identity или local opt-in denial может сразу дать REJECTED. Intermediate statuses
не означают успешную validation; только APPLIED подтверждает activation и state
publication. `applied_revision`/`applied_digest` в любом result описывают реально
active LKG. Safe error code/message не содержит config/credentials/endpoints.

Statuses идут через existing bounded outbox. MessageACK подтверждает receipt
конкретного envelope, не применение конфигурации. Outbox saturation может
помешать admission status; Backend повторяет snapshot, а Collector распознаёт
duplicate. Hello всегда объявляет implementation capability, local opt-in и
active applied state; Heartbeat передаёт последний runtime result при наличии.

`diagnostics.managed_configuration` содержит enabled, applied revision/digest/
time/count, desired revision, last apply status/time, rejected revision,
sanitized error code, managed count, stored LKG revision, last successful sync
и storage degraded. Credential values и полный desired config не выводятся.

## Границы проверки

Unit/security/runtime fixtures и authenticated Mock E2E описаны в
[validation](validation.md) и [Phase 7 report](phase-7-report.md). Реальные
vendor devices, production Backend B9/PostgreSQL, OpenWrt firmware/RAM/flash и
power-loss durability этими fixtures не подтверждаются. Изменение local policy,
credentials references/CA paths, PKI, NTP или Syslog listener требует local
deployment и обычного restart/setup; этот этап реализует hot reload managed
monitoring Targets. Phase 8 автоматически не выполняется.
