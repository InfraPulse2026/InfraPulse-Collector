# Enrollment и жизненный цикл сертификатов

P3 реализует Agent-side enrollment и renewal foundation. Production CA service,
tenancy authorization, revocation/CRL/OCSP и server CA rollover в repository
не входят. `infrapulse-mock-enrollment` — отдельный development HTTPS endpoint;
он не превращает Mock Controller в production Backend.

## Identity и доверие

Persistent UUID из `agent.state_dir/agent-id` связывается с единственным URI SAN:

```text
spiffe://infrapulse/agent/<agent UUID>
```

Это локальный certificate identity contract; SPIFFE Workload API/SPIRE,
federation и attestation не реализованы. Common Name не заменяет URI SAN.
Agent проверяет identity своего manual certificate перед запуском; Mock
Controller после verified mTLS сопоставляет URI с `AgentHello.agent_id`.
Доверенный certificate с другим Agent ID отклоняется. Production Backend
должен повторять эту проверку и отдельно контролировать tenant authorization.

Bootstrap enrollment service всегда HTTPS с CA и hostname/SAN verification.
`security.enrollment.ca_file` задаёт доверие именно enrollment HTTPS server;
`server_name` позволяет явно задать проверяемое имя. Redirects, HTTP proxy
из environment, plaintext и insecure TLS fallback отсутствуют.

Ответ enrollment service содержит client certificate и CA PEM. Certificate
проверяется на цепочку, clientAuth EKU, срок действия, URI identity и совпадение
с локально созданным private key. Возвращённая CA доверена через authenticated
bootstrap service. Если `security.ca_file` задан, он независимо задаёт доверие
Controller; иначе используется CA из enrollment response. Это не автоматическое
обнаружение нового Controller и не доверие произвольной CA из сети.

## Конфигурация

```yaml
agent:
  state_dir: /var/lib/infrapulse-agent
controller:
  address: controller.example:9443
security:
  mode: production
  server_name: controller.example
  # Необязательно, если Controller использует CA из enrollment response:
  ca_file: /etc/infrapulse/controller-ca.pem
  renew_before: 168h
  enrollment:
    enabled: true
    url: https://enrollment.example:9444
    ca_file: /etc/infrapulse/enrollment-ca.pem
    server_name: enrollment.example
    token_env: INFRAPULSE_ENROLLMENT_TOKEN
    timeout: 10s
```

В enrollment mode `cert_file`/`key_file` отсутствуют: Agent управляет одной
certificate bundle. `token_env` — имя environment variable, не literal token.
Token читает Agent только при первоначальном enrollment. После успешной
публикации bundle оператор может удалить variable; restart и renewal её
не используют. Environment service user остаётся sensitive storage.

Manual mode сохраняется с `ca_file`, `cert_file`, `key_file`. Старые manual
certificates P0–P2 без указанного URI SAN требуют перевыпуска. Сохраняйте
`agent-id`; новый UUID несовместим с сертификатом старой identity. Manual
certificates перечитываются при каждом reconnect, автоматическое enrollment
renewal для manual mode не запускается.

## Flow и private key

```text
local persistent Agent UUID
→ local ECDSA P-256 key generation
→ locally signed PKCS#10 CSR with Agent URI
→ POST /enroll (AgentID + CSR, Bearer one-time token)
→ validate certificate/key/CA/identity
→ atomic local bundle publication
→ ordinary verified mTLS Controller connection
```

HTTP request JSON содержит только `agent_id` и base64-encoded CSR PEM (`csr`);
response JSON — base64-encoded PEM `certificate` и `ca`. Private key не входит
в request, response, protobuf, diagnostics или logs. Errors имеют безопасные
категории без token, response body и underlying network payload.

Agent хранит `state_dir/pki/client-bundle.json`: version, Agent ID, certificate,
private key и CA в одном bounded файле ≤128 KiB. JSON base64 — encoding,
не encryption. На Unix directory имеет 0700, temporary/final file — 0600;
restart отклоняет bundle с group/other permission bits. TPM/HSM и encryption
at rest отсутствуют. Оператор защищает service account, filesystem, backups
и доступ к памяти процесса. Windows development build не заменяет Unix
permissions Windows ACL и не является production Windows Agent.

Публикация: temporary file → chmod → Write → file Sync → Close → atomic Rename
→ directory Sync на Unix. Один файл исключает рассогласование новой key и
старого certificate. Повреждённая bundle не удаляется и не заменяется молча;
нужно явное operator recovery. Crash до публикации может оставить temporary
файл, который не используется как рабочий сертификат.

Enrollment token single-use: если service уже выдал certificate, а Agent умер
до local publication, автоматический повтор тем же token отклоняется.
Оператор выпускает новый token. Это намеренная fail-closed semantics,
а не resumable enrollment transaction.

## Renewal и rotation

Agent проверяет expiry раз в минуту. Default `renew_before` — семь дней;
effective advance window ограничен одной третью certificate lifetime, чтобы
короткий certificate не инициировал issuance на каждом scheduler tick.

Renewal создаёт новый локальный ECDSA P-256 key и CSR. `POST /renew`
аутентифицируется старым verified client certificate; token отсутствует.
Service проверяет binding current certificate identity и новой CSR identity.
Успешная validation и atomic publication заменяют TLS snapshot; активная
Controller session отменяется и reconnect загружает новый certificate.
Requests ограничены timeout, responses и retry frequency; unlimited loops нет.

Failed request, response validation или отказ записи сохраняют текущий
certificate/key. Старый действующий certificate продолжает работать. После
directory Sync error публикация может иметь неопределённую crash persistence,
но рабочий файл всегда содержит полную matching bundle; Agent не заявляет
гарантию filesystem, которую не подтвердил Sync.

Истёкший certificate не обеспечивает renewal через mTLS. При долгом offline
после expiration нужен operator enrollment recovery с новым token. Automated
revocation и recovery через expired credential отсутствуют.

Diagnostics содержит только enrollment/renewal states, expiresAt, remaining
lifetime, last successful renewal текущего процесса и sanitized last error.

## Development Mock Enrollment

Сначала получите persistent Agent ID через `diagnostics --config <path>`.
Установите token environment variable у Agent и Mock; не передавайте значение
token через CLI. Пример запуска с существующими development CA/server TLS
files (CA signing key доступен только Mock process):

```sh
./bin/infrapulse-mock-enrollment \
  --address 127.0.0.1:9444 \
  --ca-file /secure/dev-ca.pem --ca-key-file /secure/dev-ca-key.pem \
  --cert-file /secure/enrollment-server.pem --key-file /secure/enrollment-server-key.pem \
  --agent-id <persistent-UUID> --token-env INFRAPULSE_ENROLLMENT_TOKEN \
  --token-lifetime 15m --cert-lifetime 720h
```

Mock listener допускает только literal loopback IP. TLS server certificate
должен соответствовать hostname/SAN из Agent config. Mock хранит bounded
набор token hashes; используемый CLI поддерживает один заранее привязанный
token. Invalid, expired, reused и wrong-Agent token отклоняются. Renewal
требует verified client certificate подписанный signing CA.

Tokens и replay state Mock хранятся только в RAM; restart Mock с тем же
environment token заново объявляет его неиспользованным. Для production
service нужен durable one-time token registry, tenant authorization,
auditing/rate limits, CA key protection и revocation. Mock CA key никогда
не передаётся Agent.

## Проверки

Unit/local TLS fixtures проверяют CSR/key binding, enrollment/restart, invalid/
expired/reused token, wrong CA/identity/EKU, expiration, malformed/oversized
response, redirect rejection, timeout/cancellation, corrupted state,
renewal key rotation, failed renewal и injected storage failure.

Local E2E запускает настоящий Agent и Mock Controller: issued certificate
используется для mTLS, после renewal/restart reconnect получает новый serial,
а trusted certificate с другим claimed Agent ID отвергается. Tests не требуют
production CA, root или Internet. Production CA service/hardware protection
не проверены.

Primary references: [Go x509 verification](https://pkg.go.dev/crypto/x509#Certificate.Verify),
[Go TLS Config](https://pkg.go.dev/crypto/tls#Config),
[PKCS#10 CSR, RFC 2986](https://www.rfc-editor.org/rfc/rfc2986).
