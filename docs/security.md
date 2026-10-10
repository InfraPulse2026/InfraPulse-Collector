# Безопасность InfraPulse Agent

## Backend-managed configuration Phase 7

Remote management disabled по умолчанию; opt-in требует production mTLS,
accepted Hello и negotiated `managed_config_v1`. Snapshot bound к persistent
Collector ID и current Session ID. Typed full snapshot управляет только отдельным
monitoring Target namespace; local YAML, identity, PKI/Backend trust/endpoint,
NTP, Syslog keys/listeners, state/outbox paths, deployment privileges, protected
resources и command authorization остаются local administrator-owned.
Нет raw YAML, arbitrary commands/API paths, mutation или insecure TLS/IPMI flags.

Local protocol/credential/CA/CIDR/resource allowlists нельзя расширить snapshot.
CredentialProvider проверяет существующие environment refs; values не входят
в wire/state/diagnostics. CA reference — local key, не incoming path. При CIDR
policy весь bounded DNS answer проверяется, adapter получает approved numeric
pin без второго resolution; HTTPS/SIP TLS сохраняют hostname verification.
Redirects/proxy и unsafe adapters запрещены. Нет Targets из discovery addresses.

Unknown desired fields/oversize/malformed durations отклоняются до apply.
Prepared adapters и изменения scheduler ограничены; одна active transition и
один pending snapshot. Revision/digest предотвращают stale overwrite/reuse;
checksum/identity/permissions защищают LKG consistency, но digest/checksum не
заменяют mTLS/ACL. Corrupt/denied restore не запускает managed Targets и не
останавливает local monitoring. Outbox/SourceRef/journal сохраняются при removal.
[Policy/lifecycle/recovery](managed-configuration.md),
[wire trust contract](backend-managed-config-contract.md).

## Identity/topology Phase 6

Protocol identifiers являются недоверенными данными. Normalization проверяет
UTF-8, control characters, размеры и MAC/UUID/IP syntax; invalid/rejected evidence
не становится matching key. Не генерируются отсутствующие serial/UUID, не
удаляются ведущие нули serial и не объединяются разные subjects по hostname/IP.
Facts ограничены суммарно 4096 legacy+extended на owner и 64 на сочетание полного
SourceRef/SubjectID/SubjectKind; management
addresses — 16/neighbor, capabilities — 16/list, raw discovery IDs — 255 bytes в
adapters (legacy wire limit 1024), management raw address — 31 bytes.
Точные bounds и diagnostic semantics — [Phase 6](identity-topology.md).

Advertised identifiers не интерпретируются как paths, shell, SQL или URLs для
запросов. LLDP/CDP/MNDP management addresses не создают Targets, credentials или
подключения: опрос возможен только для явно configured Target. Hardware
containment/management relationship не становится физическим network cable;
Collector не выполняет global Asset merge или cross-collector resolution.

Sanitizer переводит redacted identity value в `rejected` diagnostic. Если known
secret встречается в source/subject/resource key, envelope отклоняется с безопасной
ошибкой до outbox, поскольку переписывание ключа исказило бы identity. Это проверка
configured values и известных patterns, а не гарантия обнаружения любого secret.
Для source resource/relationship URI также проверяется стандартное percent-decoded
значение; сам resource key не переписывается и не используется для нового запроса.
Типы `ip`/`interface_name` и invalid/rejected diagnostics передаются только в
новых Device/Telemetry `extended_identity_evidence`; old Backend не получает их
в legacy matching lists. Backend Phase 6 должен сохранить оба typed списка.

Wire validation, protobuf encoding и message-size check выполняются до durable
admission. Malformed/oversized envelope получает diagnostic и не блокирует
delivery retained records. Automatic topology batch splitting отсутствует;
действуют existing queue limits (default 1 MiB), ACK/replay и PKI. Simulated
provenance сохраняется явно; TLS peer identity Syslog остаётся отдельно от
заявленного HOSTNAME. [Backend contract](backend-identity-topology-contract.md).

## IPMI fallback Phase 5

IPMI target выбирается явно: Redfish TLS/authentication failure не запускает
другой протокол. Используется только IPMI v2.0 LANplus по исходящему UDP/623
(порт configurable), без management listener. Cipher 17 default, cipher 3
допускается только explicit; оба требуют RAKP authentication, session HMAC и
AES-CBC-128. Cipher 0, null/no-auth/no-integrity/no-encryption, автоматический
downgrade и IPMI v1.5 sessions запрещены. Стандартный unauthenticated capability
preflight в legacy framing не активирует v1.5 session. USER default,
OPERATOR — explicit escalation;
ADMINISTRATOR не предлагается. BMC account должен иметь права чтения.

Pinned pure-Go go-ipmi используется только внутри adapter. Из-за отсутствующих
receive checks upstream добавлен ограниченный gate: exact algorithms/session
IDs, RAKP/HMAC, encrypted/authenticated flags, inbound sequence/correlation,
AES/IPMB framing и checksums проверяются до library decoder. Library crypto и
packet construction переиспользуются; собственный IPMI stack не создаётся.
Повторы/посторонние responses имеют конечный discard budget. Credentials,
RAKP transcript и session keys никогда не включаются в diagnostics/payloads.

Read-only typed methods исключают arbitrary raw IPMI, power/chassis controls,
user/LAN/FRU/SDR/threshold writes, SEL clear и firmware update. Создание/закрытие
собственной сессии, session privilege и SDR read reservation — protocol lifecycle.
Размеры/число records, session/poll durations, packets/retries и cache ограничены;
частичное повреждение optional inventory/sensor сохраняет валидные observations.
IPMI security не заменяет изолированную management network: RAKP позволяет
offline password guessing, поэтому нужны отдельные сильные credentials и ACL.
Nonzero/отдельный BMC `Kg`, IPMB bridging и OEM commands не поддержаны.
[Полный contract и состояние проверок](ipmi.md).

## Redfish hardware collection

Redfish Phase 4 разрешает только GET/HEAD и фиксированный lifecycle собственной
SessionService session (POST создания, DELETE logout). Power control, generic
Actions, BIOS/firmware/SecureBoot/accounts/storage/network writes отсутствуют;
`enable_mutations: true` для Redfish отклоняется. Monitoring account должна иметь
minimum read/login privileges по документации своего BMC.

Endpoint — HTTPS origin; TLS ≥1.2 с системным trust store и optional custom CA,
добавленным к нему. Hostname/IP SAN verification обязательна, insecure mode и
plaintext fallback отсутствуют. Session auth default, Basic auth требует
explicit `redfish.auth: basic` и той же verification. CredentialProvider читает
environment refs только при poll. Session token/password/Authorization/cookies
и raw BMC responses не попадают в diagnostics, SourceRef или inventory.

Response/request/resource/page/time budgets, последовательные requests внутри
Target, same-origin service links и запрет redirects предотвращают forwarding
credentials на чужие hosts и unlimited traversal. Retry 401/429/503 ограничен;
Retry-After сохраняет cooldown, logout выполняется в bounded cleanup. Если BMC
не отвечает на logout, session expiration зависит от BMC. Environment HTTP
proxy для BMC отключён. Simulated provenance разрешён только в development;
production default — real. [Configuration, exact limits и gaps](redfish.md).

## Существующий transport и локальные источники

Agent сам открывает исходящий gRPC bidirectional stream; входящего management
API нет. Opt-in Syslog Receiver открывает отдельный logging listener, disabled
по умолчанию. Remote integrations — SNMP v2c/v3 read-only, RouterOS HTTPS REST
и fixed SIP OPTIONS UDP/TCP/TLS.
Mock Controller — development endpoint, не production authorization Backend.

## Controller transport и identity

`security.mode: production` требует verified mTLS: manual CA/certificate/private
key либо явно настроенный enrollment. Agent проверяет цепочку CA и hostname/SAN
Controller, Mock server проверяет client certificate.
TLS ≥ 1.2, `InsecureSkipVerify` и silent plaintext fallback отсутствуют.
`development` явно допускает plaintext только на localhost/loopback и не
аутентифицирует участников. Remote RouterOS transport при этом всё равно HTTPS.

Client certificate содержит единственный URI SAN
`spiffe://infrapulse/agent/<persistent UUID>`. Agent проверяет свой manual
certificate, а Mock Controller сопоставляет verified certificate identity
с claimed `AgentHello.agent_id`. Trusted certificate другого Agent отклоняется;
Common Name не заменяет binding. Этот contract не реализует SPIFFE attestation
или tenancy authorization. Production Backend обязан отдельно проверять tenant
и разрешения; persistent UUID сам по себе не credential.

Agent-side enrollment генерирует локальный ECDSA P-256 private key и CSR.
Bootstrap service проверяется через HTTPS CA/SAN; отправляются только CSR,
Agent ID и one-time token из environment reference. Private key остаётся на
Agent. Token после успешной публикации bundle больше не нужен для restart
и renewal, не становится permanent credential и не включается в diagnostics.
Development Mock проверяет invalid/expired/reused token и привязывает его к
Agent ID; token registry Mock volatile и не является production service.

Bundle `state_dir/pki/client-bundle.json` хранит matching key/cert/CA как encoding
без encryption, Unix directory/file 0700/0600. Temporary file, file Sync,
atomic Rename и directory Sync публикуют bundle целиком. Renewal создаёт новый
local key/CSR, использует старый mTLS certificate, проверяет ответ, заменяет
bundle и reconnects Controller. Failed renewal сохраняет рабочий certificate.
Expiration после долгого offline требует operator recovery. TPM/HSM,
hardware-backed keys, revocation/CRL/OCSP и server CA rollover не реализованы.
Подробности, manual certificate migration и Mock boundaries —
[enrollment](enrollment.md).

Agent ID хранится в `state_dir/agent-id`, session ID создаётся на запуск.
Повреждённый identity file вызывает ошибку. State directory/file используют
0700/0600 там, где ОС поддерживает permissions; на Windows это не заменяет ACL.
State/config/CA/private keys должны быть доступны service user и защищены
от изменения посторонними пользователями.

## Corporate NTP и права deployment

`time_sync` читается только из trusted local YAML. NTP addresses не получаются
из SNMP/LLDP/Syslog, remote Device fields или Controller commands. Phase 7
Backend-managed configuration также не содержит `time_sync`: NTP остаётся
local administrator-owned. Серверы проходят bounded IPv4/IPv6/DNS
validation; shell fragments, URLs, ports и process arguments в этом contract
не допускаются. Public NTP fallback не задаётся.

Linux `apply-time-sync` — отдельное административное действие с правами записи
system config и управления выбранной существующей NTP-службой. Оно ограничено
InfraPulse-managed fragment/drop-in; main administrator config не заменяется.
Active conflict между NTP clients приводит к отказу apply: helper не отключает
службы, не устанавливает пакеты и не запускает новый client вместо администратора.
OpenWrt global UCI section не принадлежит InfraPulse, поэтому automatic UCI
mutation отсутствует; setup и rollback выполняет администратор.

Обычный `run` выполняет read-only status check с конечным timeout. Недоступность
service/status, permission denied и ещё не достигнутая synchronization дают
warning/diagnostic, без panic и остановки SNMP/RouterOS/SIP monitoring. Service
user сохраняет прежние права; systemd sandbox не ослабляется ради NTP setup.
Collector не исполняет NTP protocol, не корректирует timestamps вручную и не
управляет NTP удалённых устройств. Ownership и rollback —
[time-sync](time-sync.md).

## Remote credentials

YAML содержит только именованные environment references:
`credentials.<ref>.community_env`, `username_env`/`password_env` либо SNMPv3
`username_env`/`auth_password_env`/`privacy_password_env`.
`devices[].credential_ref` выбирает reference. `credentials.Provider` допускает
замену provider; Vault/Backend secret distribution сейчас отсутствуют.
Environment читается на request, пустая/отсутствующая variable означает
unavailable. Literal community/password в schema отсутствуют. Credentials
не включаются в protobuf/domain Device/diagnostics; JSON сериализация
credential fields и fmt String/GoString представление скрывают значения.

Generic SNMP adapter поддерживает v2c и v3 USM read-only UDP без автоматического
downgrade. V2c community передаётся без шифрования; нужны доверенная management
network, ограничение source addresses и read-only access. Явно выбранный v3
использует default `authPriv`, `SHA256`/`AES`; `authNoPriv` явно разрешает
authentication без confidentiality. `noAuthNoPriv` запрещён. MD5/SHA1/DES и
другие поддерживаемые GoSNMP algorithms выбираются только явно; конкретная
firmware compatibility не предполагается.

Auth/privacy passwords получает только CredentialProvider; plaintext fields
в device YAML, protobuf и diagnostics отсутствуют. GoSNMP debug logger
отключён, unbounded global password cache отключён. Credentials и localized
keys остаются в process memory; zeroization не заявляется. Protocol errors
заменяются безопасными категориями без username/secret/packet dump, incompatible
USM security-level response отклоняется. Unsupported OID не считается
connectivity failure. Algorithms/limits/fixtures — [SNMPv3](snmpv3.md).

RouterOS использует Basic Auth только по HTTPS. Config принимает origin без
embedded credentials, `/rest` path, query/fragment. TLS проверяет CA/SAN обычным
механизмом; device `tls.ca_file` задаёт отдельный PEM roots pool, при отсутствии
используется системное доверие. Root CA Controller и RouterOS независимы.
Custom verification callbacks и `InsecureSkipVerify` отвергаются. Redirects,
environment HTTP proxies и HTTPS→HTTP fallback отключены. HTTP error bodies,
URL и underlying transport errors не включаются в сообщения: ответ устройства
может содержать sensitive values.

Provider credentials существуют в памяти процесса и environment. Они не
заменяют защиту учётной записи сервиса или доступ к памяти процесса. Не
помещайте их в command line, Git, diagnostic metadata или management URL.

## Типизированные команды и admission

Controller передаёт только target device ID, interface stable ID/name и
ENABLE/DISABLE либо typed `resource.rollback` с snapshot ID. Он не задаёт
endpoint/path, HTTP method, arbitrary JSON, CLI,
argv или shell. Local/SNMP writes и service actions unsupported. Human,
ai_suggested и automation проходят одинаковые проверки; initiator не является
доказательством отдельного human approval.

Agent перед action резервирует 4096 bytes и слот для CommandResult в bounded
outbox, затем persistent command state и место будущего result в journal.
При невозможном admission, storage full/unavailable/corrupt или недостаточных
limits mutation не начинается. Accepted results не вытесняются ради telemetry.
Journal обязателен при `commands.enabled: true` даже с memory telemetry;
startup durable outbox failure переводит monitoring в degraded memory mode
и блокирует mutations. Capacity reservation не гарантирует физический disk
quota и не исключает последующее внешнее заполнение filesystem.

Validation ограничивает identifiers 128 bytes, timeout 10ms–1m, requested time
допустимым окном. Execution дополнительно проверяет expiry и ограничивает
context deadline. Command worker выполняет один command за раз; отдельный
receiver продолжает принимать ACK, backlog commands bounded. Registry gate
сериализует mutation с polls этого устройства. Persistent journal сохраняет
command ID/idempotency key/fingerprint, execution/verification states и result
envelope со stable message ID. Identity conflict отклоняется, повтор completed
command возвращает сохранённый result. Pending results не удаляются по retention;
dedup horizon ограничен retention acknowledged entries.

Порядок исполнения: validation → outbox reservation → persistent admission →
target/deadline/observed capability → policy/protected device → canonical
interface resolution → aliases/dependencies protection → fresh version/identity
и persistent snapshot/executing boundary → повторная capability/deadline/permission
проверка → PATCH once → read-after-write GET → persistent result → delivery/ACK.
`commands.enabled` по умолчанию false, per-device
`enable_mutations` также false. Наличие interface action в protobuf ничего
не разрешает.

RouterOS write permission discovery читает metadata текущего authenticated
local user и его group через фиксированные GET `/rest/user` и `/rest/user/group`.
Необходимы положительные `read`, `write`, `api`, `rest-api`; недоступные,
неоднозначные или RADIUS-only account metadata оставляют capability false.
Permissions повторно проверяются непосредственно перед write. Серверная
RouterOS authorization остаётся обязательной.

## Management interfaces и verification

Оператор явно задаёт `commands.protected_interfaces` и при необходимости
`protected_devices`. Agent не определяет management path автоматически.
Protection учитывает текущий canonical `.id`, name/default-name/MAC и связанные
bridge/bond/VLAN interfaces. Driver читает dependency tables и консервативно
обходит связанный graph. Если metadata недоступна или неоднозначна, safe
resolution не подтверждён и mutation не выполняется. Это может блокировать
изменение sibling port общего bridge; безопасность management path имеет
приоритет. Protection не ограничивается строкой из команды Backend.

MAC aliases канонизируются через MAC parser, включая mixed case, dashed и
dotted notation. Перед PATCH dependency closure читается повторно; изменение
aliases/identity по сравнению с проверенным resolution прекращает mutation.

Driver перед PATCH повторно читает canonical `.id`, проверяет identity/name/MAC
и admin state. Dynamic interfaces не изменяются. Supported write меняет только
`disabled` boolean текущего interface record. Уже требуемое состояние даёт
verified success без PATCH и `changed=false`.

После каждого отправленного PATCH выполняется GET canonical record, включая
HTTP error и malformed/oversized ответ. Часть command deadline оставлена
для verification; после network timeout допускается безопасное чтение, но
PATCH не повторяется. Result содержит expected/observed state/time, verified,
changed и evidence. Если отправленная mutation не подтверждена, возвращается
uncertain timeout. Crash из persisted `executing` также восстанавливается как
`uncertain` без повторной mutation; crash до execution boundary даёт rejected
interrupted command. Result, сохранённый до crash, доставляется повторно со
своим stable message ID. Exactly-once для внешнего сетевого устройства не
заявляется.

## Snapshots и безопасный rollback

Snapshot сохраняется до поддерживаемой mutation и содержит только RouterOS
interface admin state, canonical ID/aliases/dependencies, owner device,
previous/expected state, observation/expiry, related command и schema/software
compatibility. Это bounded persistent state, не full device backup и не
telemetry history; credentials и произвольная конфигурация не сохраняются.

`resource.rollback` использует Agent-owned snapshot и тот же fixed PATCH.
Target/capability/policy/protected resources проверяются заново, включая resolved
aliases/dependencies. Wrong owner/resource, expired/unverified/consumed/superseded
snapshot, version mismatch и stale/conflicting current state запрещают write.
Более новая mutation делает прежний snapshot superseded; retention нового
snapshot не оживляет старый после ABA. Rollback создаёт snapshot текущего
состояния, требует read-after-write verification и не считает uncertain write
успехом. Remote API не даёт атомарного CAS с локальным journal; все внешние
parallel changes предотвратить невозможно. Детали — [commands](commands.md).

## Bounds, diagnostics и delivery

Strict YAML ограничен 1 MiB. Config ограничивает devices, rows, response bytes,
worker counts, message/outbox bytes. Remote polling имеет отдельный bounded
scheduler, timeout и per-device exclusion; network cancellation разблокирует
SNMP/HTTP. Не используются unlimited goroutines/tables/buffers.

Diagnostics выбирает безопасную сводку и normalized devices/collector states,
queue mode/messages/bytes/oldest pending/unacked/results, journal/snapshot
health/counts, certificate expiration/renewal и SNMP algorithms без credentials;
полный config и credentials не печатаются. Remote errors в registry/scheduler
сводятся к безопасным категориям. `DataSanitizer` дополнительно очищает известные
password/token/community/authorization patterns в free-text errors/events и
command results. Это ограниченная redaction: arbitrary device inventory text
или бинарные identifiers не гарантированно очищены от всех возможных secrets.

Outbox имеет `memory` и `durable` modes. Memory telemetry остаётся best effort.
Durable outbox использует pure-Go embedded bbolt, bounded payload/file limits,
retention, deterministic priority eviction и synced admissions. SHA-256 records,
schema/index/page checks обнаруживают corruption, но не защищают от владельца
state. Persistent state не encrypted; directory/file на Unix 0700/0600,
symlink storage paths отклоняются. Corrupted state не удаляется автоматически.
Journal/snapshots используют отдельный bounded checksummed atomic state file,
single-writer lock и собственную retention. Runtime state, keys/certificates
и diagnostics не должны попадать в Git или публичные support bundles.

Успешный gRPC Send не означает application commit. Negotiated ACK позволяет
удалить record только после accepted stable message ID; unacked retained records
replay после reconnect/restart. Semantics — at-least-once, не exactly-once.
Low/medium priority retention/eviction допускает documented loss; pending
CommandResult защищён. Legacy Controller по default durable policy отклоняется;
explicit `ack_fallback: send` включается оператором и виден в diagnostics,
его semantics best effort. Mock ACK подтверждает только development acceptance;
production Backend должен выполнять durable dedup/commit перед ACK.

Batching ограничивает commits, но не устраняет flash wear. Filesystem/device
sync, physical disk space, RAM/page cache и power-loss behavior требуют
deployment review; аппаратные power-loss/endurance guarantees не заявляются.
Agent не содержит LLM client и не отправляет данные AI напрямую; Backend
должен отдельно решать допустимость дальнейшей передачи. Storage/replay
подробно описаны в [delivery](delivery.md).

## Logs, audit и persistent state

Configured file sources читаются только как regular files, с limits строки,
read budget, batch/JSON bytes, tracked identities и scanned directory entries.
Отдельный worker pool и batch bounds ограничивают log storm. Memory mode
сохраняет низкий ordinary log priority; durable mode использует medium logs,
а parsed audit batches имеют high priority в обоих modes.
Diagnostics хранит source status/counters/offset и safe error category, не raw
log messages. Права на syslog/audit files и state directory задаёт оператор;
ordinary tests используют temp directories без root.

Перед parsing/enqueue применяется sanitizer известных полей/Authorization
Bearer/Basic, literal values configured SNMP community/auth/privacy passwords,
RouterOS password/enrollment token и private-key blocks. PEM suppression state
сохраняется в cursor для restart.
Oversized line передаётся целиком как redacted marker: обрезанный credential
может не совпасть с полной configured строкой. Значения environment обновляются
при sanitization; недоступный/необъявленный secret не гарантированно скрывается.
Это bounded best-effort redaction, не универсальное удаление sensitive data.

Audit raw record остаётся sanitized; normalizer разбирает только достоверные
поля и сохраняет exact EventID. Он не является SIEM и не делает security/AI
inference. Audit records всё равно могут содержать usernames/paths/UID и другую
конфиденциальную информацию; Backend контролирует дальнейшую обработку.

Cursor state защищён mode0700/0600 на Linux, strict validation и atomic
temp/sync/rename с directory sync. Corruption fail-closed требует явного recovery;
Agent не сбрасывает его молча. Это read position, отдельно от local admission
и Controller ACK: cursor advances после успешного Push всей batch. Durable
admission переживает restart; memory admission volatile. Retention/eviction
может потерять уже прочитанные logs, crash до cursor persistence — дать replay.
Detailed semantics — [logs](logs.md).

## OpenWrt typed access

OpenWrt определяется release evidence; Linux CPU/RAM/interfaces остаются прежними.
Закрытый enum операций вызывает только absolute `/bin/ubus` и literal arguments
system board/network.interface dump/service list/uci get firewall. Controller
не задаёт binary/argv/API endpoint; `sh -c`, init script execution, UCI/nft mutation
отсутствуют. Procd command/env/data и unknown raw UCI options не публикуются.
Stdout/stderr bounded; сообщения ошибок не включают содержимое ответов.
UCI snapshot не подтверждает active firewall; службы/firewall read-only.

## SIP и Simulator

SIP endpoint config validated against CRLF/URI injection, транспорт UDP/TCP/TLS.
Agent самостоятельно формирует fixed OPTIONS с уникальными transaction
IDs, сопоставляет Call-ID/CSeq/Via branch, ограничивает messages/response bytes
и время. Raw packets/network errors не логируются. 401/407 не обходятся через
REGISTER; Digest/SIP credentials/media отсутствуют. UDP/TCP не обеспечивают
шифрование SIP; probe не содержит passwords/tokens. TLS использует trusted
system/custom CA, server name verification/SNI, TLS ≥ 1.2 и context timeout/
cancellation. `InsecureSkipVerify` option отсутствует. Latency включает handshake;
raw TLS errors/certificates не публикуются. TLS обеспечивает transport security,
не подтверждает OPTIONS authentication или качество звонков. Подробности —
[telephony](telephony.md).

Simulator — отдельный development binary. Production Agent не импортирует
его packages и не подменяет collectors synthetic fallback. Synthetic nested
samples/devices/neighbors/logs/health/events имеют `source=simulated`; будущий
Backend должен сохранять marker. Simulator подключается через обычный verified
transport; development plaintext остаётся только loopback. Он не реализует
command mutation executor и не является production Agent/hardware validation.

## Remote Syslog / Audit Receiver

`syslog_receiver.enabled` default false: никаких ingress sockets/workers.
Enabled TLS listener использует отдельные server cert/key/client CA, TLS ≥1.2
и стандартный Go certificate validation без insecure callbacks/ciphers или
plaintext fallback. Backend Agent identity certificates не переиспользуются
автоматически для logging boundary. Client-auth default require_and_verify;
explicit server-only TLS требует approved peer CIDRs. Certificate rotation
listener требует operator restart; Backend enrollment Manager им не управляет.

Verified DNS/URI SAN сопоставляется с configured source mapping. Syslog
HOSTNAME/APP-NAME/node — недоверенные claims, не authenticated identity.
IP restriction ограничивает sender access, но не идентифицирует originator
за relay/NAT. Unknown source default reject; explicit unmanaged не создаёт
unbounded permanent Target/Asset entries и не включает audit classification.
Plain TCP default disabled/loopback; public compatibility требует explicit
allow_non_loopback и approved peer CIDRs, authentication у него отсутствует.

Connections ограничиваются до handshake; mapped source/IP имеют дополнительные
limits. Prefix length/size проверяются до allocation, read/idle/admission имеют
deadlines. Malformed client не прекращает accept других clients. Admission
использует только existing durable outbox; full останавливает чтение до bounded
retry timeout, storage error закрывает connection сразу. Memory degraded fallback не может
считаться durable remote-log admission. Receiver shutdown отменяет active I/O.

Syslog raw, structured-data и audit text проходят existing sanitizer перед
outbox; logs не исполняются и не изменяют commands/config/NTP/credentials.
Diagnostic error categories/counters не содержат payload/private key/certificate
dumps. Audit normalization требует managed mapping, approved plugin APP-NAME и
canonical audit header; одно type= не даёт audit trust. Audit usernames/paths
остаются потенциально sensitive даже после bounded best-effort redaction.

Sender→Collector TLS/TCP не имеет per-record application ACK. Read/transport ACK
не означает synced outbox admission, loss/duplicates при disconnect возможны.
Collector→Backend остаётся bounded retained at-least-once ACK/replay, без
exactly-once. Настройка rsyslog disk-assisted queue, firewall и ограничения —
[syslog-receiver](syslog-receiver.md), [deployment](deployment.md).

## Primary documentation

[MikroTik REST API](https://manual.mikrotik.com/docs/developer-guides/rest-api/),
[user/group policy](https://manual.mikrotik.com/docs/authentication-authorization-accounting/user/),
[interface properties](https://manual.mikrotik.com/docs/cli-reference/interface/),
[bridge ports](https://manual.mikrotik.com/docs/cli-reference/interface/bridge/port/),
[bonding](https://help.mikrotik.com/docs/spaces/ROS/pages/8323193/Bonding),
[VLAN dependencies](https://help.mikrotik.com/docs/spaces/ROS/pages/19136718/Layer2+misconfiguration).
