# Контракт Backend B9: managed Collector configuration v1

Это handoff для независимой реализации Backend Stage9F / B9. Collector Phase 7
реализует описанный протокол; наличие этого документа не означает, что
production Backend B9, UI/API или PostgreSQL integration уже реализованы.
Действующий источник wire schema —
[agent.proto](../api/proto/infrapulse/v1/agent.proto). Operational policy и restart
поведение описаны в [managed configuration](managed-configuration.md).

## Существующий stream и negotiation

Используется прежний RPC `/infrapulse.v1.AgentService/Connect`, инициируемый
Collector, с `protocol_version=1`. Нового management service/listener нет.
Existing identity/topology/hardware/log fields, command protocol и номера
остальных fields сохранены.

| Message | Новое поле | Номер | Смысл |
|---|---|---:|---|
| `AgentHello` | `managed_config_v1` | 9 | Collector реализует этот contract |
| `AgentHello` | `remote_management_enabled` | 10 | local opt-in; capability не означает permission |
| `AgentHello` | `applied_config` | 11 | реально active applied state |
| `HelloAck` | `managed_config_v1` | 4 | Backend подтверждает поддержку |
| `ControllerEnvelope` oneof | `desired_config` | 4 | `DesiredConfigSnapshot` |
| `AgentEnvelope` oneof | `config_status` | 21 | `ConfigApplyStatus` |
| `Heartbeat` | `managed_config` | 8 | последний runtime apply result при наличии |

Backend сначала проверяет client mTLS chain и URI identity
`spiffe://infrapulse/agent/<agent_id>`, ownership/tenant и свои permissions,
затем принимает matching Hello/Session ID. Collector проверяет Backend CA/SAN
и собственный client certificate. Snapshot принимается после accepted HelloAck
только при verified production TLS, взаимном `managed_config_v1`, local opt-in
и совпадающих `collector_id`/`session_id` с этой session. Persistent Collector UUID
сам по себе не authentication. Development loopback plaintext не разрешает
managed configuration.

Старый peer с отсутствующим bool означает отсутствие поддержки. Backend не
посылает snapshot старому Collector и не кодирует его в `Command`. Старый
Backend может продолжать существующий observation stream; новое управление
не включается молча. Для будущих config schema extensions потребуется отдельная
negotiated version/capability: desired v1 отклоняет unknown fields рекурсивно,
включая envelope и protocol options.

## Full snapshot messages

`DesiredConfigSnapshot`:

| Поле | № | Тип / требование |
|---|---:|---|
| `collector_id` | 1 | string, exact persistent Collector UUID |
| `session_id` | 2 | string, exact текущий Hello session UUID; обязателен live |
| `revision` | 3 | uint64, 1–18446744073709551615 |
| `configuration_digest` | 4 | string, 64 lower-case hex SHA-256 characters |
| `issued_at` | 5 | optional Timestamp; не используется как authorization/TTL |
| `targets` | 6 | repeated `ManagedTargetSpec`, полный managed набор |

Пустой `targets` — valid desired managed empty set. Удалённый из snapshot Target
удаляется из monitoring только после успешного apply. Local Targets не входят
в snapshot, не удаляются и не заменяются. Совпадение raw Target ID с любым local
`devices.id`/`telephony.endpoints.id` отклоняет ревизию целиком.

`ManagedTargetSpec`:

| Поле | № | Тип / смысл |
|---|---:|---|
| `target_id` | 1 | string, 1–128 ASCII `[A-Za-z0-9_.-]`, уникальный raw ID |
| `protocol` | 2 | string: `snmp`, `routeros`, `sip`, `redfish`, `ipmi` |
| `enabled` | 3 | bool; отсутствующее значение false, активность задавать явно |
| `endpoint` | 4 | typed по выбранному protocol, без embedded credentials |
| `credential_ref` | 5 | local allowlisted reference; SIP требует пусто |
| `name` | 6 | string metadata |
| `vendor_hint` | 7 | string, слабый configured hint, не capability/доказательство vendor |
| `location` | 8 | existing `PhysicalLocation`, optional |
| `polling` | 9 | `ManagedTargetPolling`, optional fixed defaults ниже |
| `ca_ref` | 10 | key в local `remote_management.tls_ca_refs`, не path |
| `snmp` | 20 | oneof `ManagedSNMPOptions` |
| `sip` | 21 | oneof `ManagedSIPOptions` |
| `redfish` | 22 | oneof `ManagedRedfishOptions` |
| `ipmi` | 23 | oneof `ManagedIPMIOptions` |

Protocol block должен соответствовать `protocol`. RouterOS использует общие
endpoint/ref/polling/CA fields без отдельного options block. Отсутствующий
правильный block получает fixed v1 defaults. Все definitions, в том числе
disabled, валидируются и входят в target count. Исторический observation
SourceRef Target ID для managed Targets — `remote:<target_id>`; target_id не
является окончательным Backend Asset ID. Local SIP сохраняет прежний namespace.

`ManagedTargetPolling` field numbers: `fast_interval=1`, `medium_interval=2`,
`inventory_interval=3`, `topology_interval=4`, `timeout=5`, все Duration.
Unknown, malformed и overflowing Duration отклоняются, truncation/clamping нет.

| Protocol options | Wire fields с номерами |
|---|---|
| SNMP | `version=1`, `security_level=2`, `auth_protocol=3`, `privacy_protocol=4`, `context_name=5` strings; `port=6` uint32; `retries=7` int32 |
| SIP | `port=1` uint32; `transport=2`, `from_user=3`, `to_user=4`, `domain=5`, `server_name=6` strings; `failure_threshold=7` uint32; `latency_threshold=8` Duration |
| Redfish | `auth=1` string; `request_timeout=2` Duration; `max_resources=3`, `max_pages=4`, `max_requests=5` int32 |
| IPMI | `port=1`, `cipher_suite=2` optional uint32; `privilege=3` string; `retries=4` int32; `request_timeout=5`, `max_session_duration=6` Duration; `max_operations=7`, `max_sdr_records=8`, `max_sdr_bytes=9`, `max_fru_records=10`, `max_fru_bytes=11`, `max_poll_bytes=12` int32; `sdr_cache_ttl=13` Duration |

SNMP/SIP port должны помещаться в uint16; endpoint — host/IP, порт задаётся
options. RouterOS/Redfish endpoint — verified HTTPS origin без userinfo/query/
fragment/path кроме `/`. IPMI endpoint — host/IP, только LANplus. SIP endpoint
— host/IP, fixed OPTIONS UDP/TCP/TLS, произвольные headers/body запрещены.
SNMP остаётся read-only/bounded, SET и unlimited walk отсутствуют. Redfish TLS
verification обязательна; session/basic auth не разрешают Actions/mutations.
IPMI допускает cipher 17 либо явно 3, USER либо явно OPERATOR, без v1.5/unsafe
cipher/downgrade/OEM/power commands. Typed snapshot не содержит insecure flags,
provenance override, mutations, arbitrary paths, commands или secrets.

## Fixed v1 normalization

Backend вычисляет digest после следующих defaults; они не берутся из local
YAML. `0`/missing duration получает default только там, где указан в таблице.
Negative duration сохраняется при normalization и затем отклоняется validation.
Строки, endpoints, reference IDs и location не trim/lowercase/переписываются.
Пустой targets список нормализуется в пустой список, сортируется по raw ID.

| Protocol | Fast | Medium | Inventory | Topology | Timeout |
|---|---:|---:|---:|---:|---:|
| SNMP / RouterOS | 3s | 30s | 300s | 300s | 2s |
| Redfish / IPMI | 0 | 30s | 300s | 0 | 30s |
| SIP | 30s | 0 | 0 | 0 | 2s |

Для hardware fast/topology и для SIP medium/inventory/topology должны остаться
нулевыми: их explicit nonzero значения не являются поддерживаемой настройкой.
Optional metadata пусто, location strings пусто/rack_unit 0; `enabled` сохраняется
как передано без default true. Отсутствующий protocol block создаётся после
проверки соответствия oneof/protocol.

- SNMP: missing `version` → `v2c`, port 0 → 161, retries 0. Для v3 missing
  security level → `authPriv`, auth algorithm → `SHA256`; missing privacy → `AES`
  только при authPriv. Context name пусто. v2c не допускает v3 security fields;
  authNoPriv не допускает privacy algorithm. Supported legacy algorithms
  остаются explicit существующими SNMP options, см. [SNMPv3](snmpv3.md).
- SIP: transport пусто → udp; port 0 → 5061 для tls, иначе 5060; from_user пусто
  → `infrapulse`; failure_threshold 0 → 3; latency_threshold 0 → 250ms.
  to_user/domain/server_name остаются пустыми.
- Redfish: auth пусто → session; request_timeout 0 → 5s; max_resources/pages/
  requests 0 → 128/8/512.
- IPMI: absent port/cipher_suite → 623/17; explicit optional zero **не default**
  и отклоняется. Privilege пусто → user; retries 0; request_timeout 0 → 2s;
  max_session_duration 0 → 30s; max_operations 0 → 1024; max_sdr_records/bytes 0
  → 128/131072; max_fru_records/bytes 0 → 16/65536; max_poll_bytes 0 → 1048576;
  sdr_cache_ttl 0 → 300s.

## Canonical digest v1

Digest — SHA-256 canonical bytes, lower-case hex. Он не является подписью.
Collector ID и нормализованные Target definitions входят в digest. Revision,
session_id, issued_at и supplied configuration_digest **не входят**: тот же
desired content в новой ревизии/сессии сохраняет digest. Secrets и local paths
не представимы в этих данных, входят только references.

Encoding определён независимо от Go, protobuf serialization и JSON:

1. Записать literal ASCII `InfraPulse managed config v1\n` с одним LF.
2. String: unsigned big-endian uint64 длина **UTF-8 bytes**, затем сами bytes.
   Нет terminator, padding или Unicode normalization.
3. Number: signed big-endian int64 (two's complement, 8 bytes). Duration:
   `seconds * 1000000000 + nanos`, также int64 nanoseconds.
4. Bool/presence: один byte `0x00` либо `0x01`.
5. Записать String collector_id, Number target count. Sorted Targets по
   target_id ascending (допустимые IDs ASCII).
6. Для каждого Target строго в следующем порядке:

```text
String target_id, name, protocol, endpoint, credential_ref, vendor_hint, ca_ref
Bool enabled
String location.site, datacenter, building, room, rack, position, comment
Number location.rack_unit
Number polling.fast_interval, medium_interval, inventory_interval,
       topology_interval, timeout                         # nanoseconds
Bool SNMP block present, SIP present, Redfish present, IPMI present
```

Затем payload каждого присутствующего block в порядке SNMP, SIP, Redfish, IPMI:

```text
SNMP:
  String version, security_level, auth_protocol, privacy_protocol, context_name
  Number port, retries
SIP:
  String transport, from_user, to_user, domain, server_name
  Number port, failure_threshold, latency_threshold       # last is nanoseconds
Redfish:
  String auth
  Number request_timeout, max_resources, max_pages, max_requests
IPMI:
  Number port, cipher_suite                               # normalized, present
  String privilege
  Number retries, request_timeout, max_session_duration, max_operations,
         max_sdr_records, max_sdr_bytes, max_fru_records, max_fru_bytes,
         max_poll_bytes, sdr_cache_ttl
```

Все Duration numbers в blocks — nanoseconds. Для valid normalized Target
присутствует один соответствующий block либо ни одного для RouterOS.
PhysicalLocation порядок в canonical encoding отличается от порядка proto
fields: rack_unit следует **после** position/comment. Backend не должен хешировать
serialized protobuf/JSON или internal Go state file.

Независимый [digest-v1.json](../internal/managedconfig/testdata/digest-v1.json)
содержит input, exact `canonical_hex` и SHA-256. Input использует domain key
`id`; в wire это `target_id`. Test input с одним SNMP Target и omitted defaults
даёт 332 bytes и digest
`5f1cf1a25a39ec033d81033706c417593c89d411521768fc00b4cb04c2b09bec`.
Encoder fixture использовал Python `struct.pack` big-endian независимо от Go;
Go contract test сравнивает bytes/hash. Backend B9 должен добавить свой test
по этому vector, включая missing/default blocks и duration conversion.

## Revisions и replay

Backend выделяет монотонные revisions отдельно для persistent Collector ID и
durably сохраняет history/baseline прежде, чем отправить snapshot. Нельзя
reuse revision для исправленного content. Текущий Session ID берётся из Hello
и обновляется после restart; он не меняет desired digest.

При повторной синхронизации Backend пересылает прежнюю revision, а не выделяет
новую на каждый heartbeat/reconnect. Duplicate не пишет LKG и не перезапускает
Targets; каждая новая terminal revision требует persistent watermark/state write.
Это особенно существенно для flash storage OpenWrt.

| Ситуация | Collector result / действие Backend |
|---|---|
| Новая revision больше watermark | validate/prepare/apply; terminal result и watermark persistent |
| Новая revision rejected/failed | active LKG сохранён; corrected snapshot только с большей revision |
| Same still-active applied revision/content | APPLIED с прежним applied_at; adapters/jobs не перезапускаются |
| Same applied content, другая большая revision | новая revision применяется; unchanged runtime instances могут переиспользоваться |
| Same latest rejected/failed revision/content | повтор terminal result; не автоматический retry failed transition |
| Same tracked applied/latest revision, different content/digest | REJECTED `revision_conflict` |
| Revision ниже highest-seen, не still-active applied | REJECTED `stale_revision` |
| Applied 2, rejected 3, duplicate 2 | APPLIED, потому что active 2 уже действительно работает |
| После denied restore duplicate stored APPLIED | FAILED `applied_state_inactive`, active applied_revision 0 |
| Revision 0 | REJECTED `invalid_revision` |
| uint64 maximum | допустим; не wrap/increment; дальнейшая history требует operator recovery |

Identity/unauthenticated/disabled denials не продвигают persistent watermark.
Malformed authenticated snapshot отклоняется; безопасная correlation может
не содержать invalid digest/ID. Collector хранит highest-seen fingerprint и
последний terminal result, а не unlimited revision history. Backend должен
хранить свою историю; восстановление потерянной Backend database не разрешает
Collector автоматический rollback. Administrative reset описан в
[recovery](managed-configuration.md#last-known-good-restart-и-offline).
В v1 нет remote epoch/reset operation.

## Status lifecycle и application ACK

`ConfigStatus`: UNSPECIFIED=0 (не terminal success), RECEIVED=1, VALIDATING=2,
APPLIED=3, REJECTED=4, FAILED=5. RECEIVED означает admission на обработку;
VALIDATING означает начало проверки, не её успешное завершение. APPLIED
подтверждает runtime commit и persistent LKG; REJECTED — snapshot/policy
denial; FAILED — apply/storage/runtime failure.

`ConfigApplyStatus` fields: collector_id=1, revision=2, configuration_digest=3,
status=4, applied_at=5, error_code=6, sanitized_error_message=7,
applied_revision=8, applied_digest=9. Error text ограничен 512 bytes, correlation
строки 128 bytes; invalid supplied digest может быть пустым. Applied timestamp
отсутствует у unsuccessful attempt; applied_revision/digest описывают реально
active state, а не desired status. `AppliedConfigState` fields: revision=1,
configuration_digest=2, status=3, applied_at=4, target_count=5; count включает
disabled definitions. Revision 0/digest пусто в Hello означает отсутствие
active managed applied state; positive empty-set revision допустима.

Status envelope проходит тот же bounded outbox и application ACK/replay.
`MessageACK.message_id` подтверждает Backend acceptance отдельного status или
observation envelope и не заменяет semantic ConfigApplyStatus. Backend должен
durable accept/deduplicate envelope до ACK. Повтор status допустим; delayed
RECEIVED/VALIDATING/старый terminal result не откатывает более новое known state.
ACK telemetry не означает APPLIED конфигурации, APPLIED не ACK retained telemetry.

При lost status Backend повторяет тот же snapshot с session_id текущего Hello.
Collector отвечает идемпотентно. Hello предоставляет active state после каждого
reconnect; Heartbeat сообщает последний runtime result при наличии. Restore
denial/storage degradation не скрываются за историческим persisted APPLIED.
Outbox saturation может сорвать status admission; retries/Hello reconciliation
нужны и при connected stream.

## Policy, persistence и ограничения

Local administrator выбирает allowed protocols/destinations/credential refs/CA
keys, target/payload/interval/apply budgets и existing concurrency. Backend
может узнать permitted refs из своего deployment inventory, но этот v1 contract
не предоставляет secret discovery или отдельный remote policy editor. Default
management disabled, allowed protocols пять, managed cap128, payload256 KiB,
min network1s/hardware10s, apply10s. В конфигурационном worker одна active operation
и один pending snapshot; overflow закрывает session для reconnect/retry.
Wire hard cap1 MiB/1024 Targets, local policy может быть строже.
Existing configuration validator дополнительно ограничивает local + managed
суммарно: 1024 non-SIP devices и 128 SIP endpoints. Backend должен учитывать
локальные Targets; `max_managed_targets` не заменяет эти общие лимиты.

Credentials provider проверяется при validation/preparation, resolved values
не сохраняются. SIP credential_ref запрещён. Custom CA берётся только по
approved local key. CIDR policy проверяет все DNS answers и pin одного numeric IP
до новой revision/restart; TLS hostname сохраняется. HTTP redirect/proxy bypass
и insecure adapters не разрешаются. Disabled Target также требует valid refs/
settings; отсутствующий credential отклоняет всю snapshot.

Collector хранит один identity-bound checksummed LKG и terminal watermark/result
в private atomic local file. Restart повторно валидирует local policy/refs/DNS
без Backend; denied restore не активирует частичный managed набор. Apply/recovery
не изменяет local config, command journal, outbox или historical SourceRef.
Это не distributed transaction/OS transaction, не power-loss proof и не exactly-once
equipment operation. Runtime atomicity включает paused replacement jobs до
publication; bounded recovery восстанавливает предыдущую runnable конфигурацию.

Stable error codes для Backend display/retry policy:

- Admission: `management_disabled`, `unauthenticated_delivery`, `identity_mismatch`,
  `invalid_revision`, `invalid_snapshot`, `digest_mismatch`, `revision_conflict`,
  `stale_revision`.
- Validation/policy: `invalid_protocol`, `invalid_protocol_options`,
  `invalid_target_id`, `local_target_conflict`, `protocol_forbidden`,
  `credential_forbidden`, `credential_unavailable`, `credential_incompatible`,
  `sip_auth_unsupported`, `ca_reference_forbidden`, `polling_interval_forbidden`,
  `invalid_polling_timeout`, `invalid_sip_options`, `invalid_target`,
  `invalid_target_settings`, `invalid_endpoint`, `target_limit`, `payload_limit`,
  `destination_unavailable`, `destination_limit`, `destination_forbidden`.
- Apply/restore: `adapter_prepare_failed`, `runtime_apply_failed`,
  `runtime_commit_failed`, `apply_cancelled`, `state_unavailable`,
  `state_write_failed`, `persistence_recovery_failed`, `restore_rejected`,
  `applied_state_inactive`. Corrupt local state также виден diagnostics как
  `state_corrupt`; storage degraded требует operator recovery.

Backend должен отображать unknown future code безопасно без silent success.
Messages не содержат raw adapter exceptions/config/secrets; не следует строить
decision parser на human-readable sanitized_error_message.

## Обязанности Backend B9 и development Mock

Backend B9 должен реализовать mTLS identity/tenant authorization, durable
revision issuance/history, canonical defaults/digest, typed full snapshots,
session binding, per-Collector delivery/retry, status/dedup/ACK, reconnect
reconciliation и operator workflow для policy errors/history loss. Область
credentials ограничена уже deployed local references; Secret delivery в этот
contract не входит. Два Collector получают разные identity-bound snapshots,
ownership/failover engine не добавлен в Collector.

Mock Controller имеет `SendConfig(collector_id, snapshot)`, bounded inbox8;
отправка после следующего envelope/heartbeat сохраняет единственного sender.
Static scenario advances при terminal result, может повторить snapshot,
отправить stale/invalid snapshot и один раз disconnect. CLI:

```text
infrapulse-mock-controller --security-mode production ...mTLS flags...
  --desired-config revision-1.json --desired-config revision-2.json
  --repeat-config --disconnect-after-config-status 2
```

Каждый файл — typed protojson `DesiredConfigSnapshot`, максимум 1 MiB. Unknown
JSON fields отвергаются. collector_id/digest вычислены заранее; session_id
можно опустить в fixture file, Mock заполняет его текущим Hello. Это fixture
convenience; настоящий Backend всегда передаёт exact session_id. Digest от
другого Collector ID не исправляется автоматически. Правильный production
Backend B9 E2E остаётся отдельной integration после реализации Backend; текущие
[проверки](validation.md) и [отчёт](phase-7-report.md) не утверждают её наличие.
