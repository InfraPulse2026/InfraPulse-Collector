# Безопасность InfraPulse Agent

Agent сам открывает исходящий gRPC bidirectional stream; входящего management
listener нет. Remote integrations — SNMP v2c read-only и RouterOS HTTPS REST.
Mock Controller — development endpoint, не production authorization Backend.

## Controller transport и identity

`security.mode: production` требует CA, certificate/private key. Agent проверяет
цепочку CA и hostname/SAN Controller, Mock server проверяет client certificate.
TLS ≥ 1.2, `InsecureSkipVerify` и silent plaintext fallback отсутствуют.
`development` явно допускает plaintext только на localhost/loopback и не
аутентифицирует участников. Remote RouterOS transport при этом всё равно HTTPS.

Сертификаты provisioning выполняется оператором. Enrollment/rotation/revocation
workflow и certificate-to-AgentID/tenancy authorization не реализованы.
Persistent UUID сам по себе не credential; mTLS удостоверяет владение доверенным
сертификатом, а не принадлежность заявленного ID конкретному tenant.

Agent ID хранится в `state_dir/agent-id`, session ID создаётся на запуск.
Повреждённый identity file вызывает ошибку. State directory/file используют
0700/0600 там, где ОС поддерживает permissions; на Windows это не заменяет ACL.
State/config/CA/private keys должны быть доступны service user и защищены
от изменения посторонними пользователями.

## Remote credentials

YAML содержит только именованные environment references:
`credentials.<ref>.community_env` либо `username_env`/`password_env`.
`devices[].credential_ref` выбирает reference. `credentials.Provider` допускает
замену provider; Vault/Backend secret distribution сейчас отсутствуют.
Environment читается на request, пустая/отсутствующая variable означает
unavailable. Literal community/password в schema отсутствуют. Credentials
не включаются в protobuf/domain Device/diagnostics; JSON сериализация
credential fields и fmt String/GoString представление скрывают значения.

SNMP использует только v2c read-only UDP. Community передаётся по сети без
шифрования; нужны доверенная management network, ограничение source addresses
и read-only access на оборудовании. SNMPv3 отсутствует. GoSNMP debug logger
отключён; protocol errors заменяются безопасными категориями и не включают
community/packet dump. Unsupported OID не считается connectivity failure.

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
ENABLE/DISABLE. Он не задаёт endpoint/path, HTTP method, arbitrary JSON, CLI,
argv или shell. Local/SNMP writes и service actions unsupported. Human,
ai_suggested и automation проходят одинаковые проверки; initiator не является
доказательством отдельного human approval.

Agent перед action резервирует 4096 bytes и слот для CommandResult в bounded
RAM outbox. При полной очереди или меньшем max_message_bytes execution
не запускается; session закрывается. Accepted results не вытесняются.
Reservation защищает сохранение результата в RAM, не delivery на Backend.

Validation ограничивает identifiers 128 bytes, timeout 10ms–1m, requested time
допустимым окном. Execution дополнительно проверяет expiry и ограничивает
context deadline. Command receiver выполняет один command за раз; registry
gate сериализует его с polls этого устройства. Dedup cache ограничен 256
commands/results, проверяет command ID/idempotency key и fingerprint
параметров; конфликт identity отклоняется, in-flight command не запускается
повторно. Cache volatile: после restart или eviction повтор может выполниться
снова. Persistent execution journal и exactly-once отсутствуют.

Порядок исполнения: validation → target/deadline → observed capability →
commands policy/protected device → canonical interface resolution → protection
по aliases/dependencies → повторная capability/deadline/permission проверка →
PATCH → read-after-write GET. `commands.enabled` по умолчанию false, per-device
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
uncertain timeout. Ни success, ни rollback в таком случае не предполагаются;
configuration snapshots/rollback отсутствуют.

## Bounds, diagnostics и delivery

Strict YAML ограничен 1 MiB. Config ограничивает devices, rows, response bytes,
worker counts, message/outbox bytes. Remote polling имеет отдельный bounded
scheduler, timeout и per-device exclusion; network cancellation разблокирует
SNMP/HTTP. Не используются unlimited goroutines/tables/buffers.

Diagnostics выбирает безопасную сводку и normalized devices/collector states;
полный config и credentials не печатаются. Remote errors в registry/scheduler
сводятся к безопасным категориям. `DataSanitizer` дополнительно очищает известные
password/token/community/authorization patterns в free-text errors/events и
command results. Это ограниченная redaction: arbitrary device inventory text
или бинарные identifiers не гарантированно очищены от всех возможных secrets.

Outbox, reservations и dedup хранятся в RAM. Restart теряет очередь; Send после
Pop может потерять telemetry/topology/result, успешный gRPC Send не означает
commit на Backend. Delivery ACK/durable outbox/journal/enrollment относятся
к следующим этапам. Agent не содержит LLM client и не отправляет данные AI
напрямую; Backend должен отдельно решать допустимость дальнейшей передачи.

## Logs, audit и persistent state

Configured file sources читаются только как regular files, с limits строки,
read budget, batch/JSON bytes, tracked identities и scanned directory entries.
Отдельный worker pool и низкий queue priority защищают telemetry от log storm.
Diagnostics хранит source status/counters/offset и safe error category, не raw
log messages. Права на syslog/audit files и state directory задаёт оператор;
ordinary tests используют temp directories без root.

Перед parsing/enqueue применяется sanitizer известных полей/Authorization
Bearer/Basic, literal values configured SNMP community/RouterOS password и
private-key blocks. PEM suppression state сохраняется в cursor для restart.
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
Agent не сбрасывает его молча. Это read position, не transport durability:
RAM loss/eviction/Send error остаются возможны. Detailed semantics — [logs](logs.md).

## OpenWrt typed access

OpenWrt определяется release evidence; Linux CPU/RAM/interfaces остаются прежними.
Закрытый enum операций вызывает только absolute `/bin/ubus` и literal arguments
system board/network.interface dump/service list/uci get firewall. Controller
не задаёт binary/argv/API endpoint; `sh -c`, init script execution, UCI/nft mutation
отсутствуют. Procd command/env/data и unknown raw UCI options не публикуются.
Stdout/stderr bounded; сообщения ошибок не включают содержимое ответов.
UCI snapshot не подтверждает active firewall; службы/firewall read-only.

## SIP и Simulator

SIP endpoint config validated against CRLF/URI injection, транспорт только
UDP/TCP. Agent самостоятельно формирует fixed OPTIONS с уникальными transaction
IDs, сопоставляет Call-ID/CSeq/Via branch, ограничивает messages/response bytes
и время. Raw packets/network errors не логируются. 401/407 не обходятся через
REGISTER; SIP authentication/TLS/media отсутствуют. UDP/TCP не обеспечивают
шифрование SIP; probe не содержит passwords/tokens.

Simulator — отдельный development binary. Production Agent не импортирует
его packages и не подменяет collectors synthetic fallback. Synthetic nested
samples/devices/neighbors/logs/health/events имеют `source=simulated`; будущий
Backend должен сохранять marker. Simulator подключается через обычный verified
transport; development plaintext остаётся только loopback. Он не реализует
command mutation executor и не является production Agent/hardware validation.

## Primary documentation

[MikroTik REST API](https://manual.mikrotik.com/docs/developer-guides/rest-api/),
[user/group policy](https://manual.mikrotik.com/docs/authentication-authorization-accounting/user/),
[interface properties](https://manual.mikrotik.com/docs/cli-reference/interface/),
[bridge ports](https://manual.mikrotik.com/docs/cli-reference/interface/bridge/port/),
[bonding](https://help.mikrotik.com/docs/spaces/ROS/pages/8323193/Bonding),
[VLAN dependencies](https://help.mikrotik.com/docs/spaces/ROS/pages/19136718/Layer2+misconfiguration).
