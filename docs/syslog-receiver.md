# Remote Syslog / Audit Receiver

Phase 3 подключает существующие Linux logging mechanisms к InfraPulse Collector:
на сервере достаточно rsyslog и, для аудита, штатного audisp-syslog plugin.
Устанавливать InfraPulse Agent на каждый Linux-сервер не требуется.
Receiver — источник существующего LogBatch pipeline, а не отдельное хранилище.
По умолчанию он отключён и не открывает sockets или фоновые workers.

```text
Linux servers: syslog applications → rsyslog ───────────┐
Linux audit: auditd → audisp-syslog → rsyslog ──────────┤
                                                      ▼
                             Collector TCP/TLS listener
                             → bounded framing / parser
                             → source + time attribution
                             → LogRecord / optional AuditRecord
Local files → existing reader / cursors ──────────────┤
                                                      ▼
                             LogBatch → existing Agent.push
                             → durable outbox → outbound gRPC
                             → Backend application ACK / replay
```

Production Backend storage/search, PostgreSQL, REST filtering, incidents и AI
не входят в этот этап. Mock Controller подтверждает development ingest, а не
production Backend commit.

## RFC support

| Стандарт | Реализованный scope |
|---|---|
| [RFC 5424](https://www.rfc-editor.org/rfc/rfc5424) | VERSION=1, PRI/facility/severity, timezone timestamp/NILVALUE, HOSTNAME/APP-NAME/PROCID/MSGID, structured-data и MSG; raw сохраняется после sanitizer |
| [RFC 5425](https://www.rfc-editor.org/rfc/rfc5425) | TLS/TCP и обязательное octet-counting framing; TLS ≥ 1.2 и стандартная crypto/tls certificate validation |
| [RFC 6587](https://www.rfc-editor.org/rfc/rfc6587) | Явный plain TCP compatibility: octet-counting либо LF; mode выбирается в YAML, автоматического переключения нет |
| [RFC 3164](https://www.rfc-editor.org/rfc/rfc3164) | Базовый `<PRI>MMM DD HH:MM:SS HOST TAG: MESSAGE`; timestamp без года/timezone не превращается в абсолютный момент |

Это scope поддержки выбранных formats/transports, а не заявление выполнения
каждого требования полной RFC certification. UDP, RELP, NUL-delimited framing
и отдельный auditd network protocol отсутствуют. TLS всегда использует
`MSG-LEN SP SYSLOG-MSG`; границы TCP Read/TLS records не считаются границами
сообщений. MSG-LEN означает число octets, а не Unicode characters.
В octet-counting допустим embedded LF; LF compatibility разбивает сообщения
по LF и поэтому не подходит для multiline messages.

Parser сохраняет structured-data целиком, включая escapes и пробелы внутри
quoted values. HEADER NILVALUE означает неизвестное поле. Неизвестный формат
или malformed syslog header сохраняется как raw fallback с parse status;
неподтверждённые header/time fields не выдумываются. Malformed framing и
oversized message закрывают только соединение нарушителя.

## Конфигурация Collector

Включение receiver требует `queue.mode: durable` и `ack_fallback: reject`.
Пример фрагмента production config:

```yaml
queue:
  mode: durable
  ack_fallback: reject
  max_message_bytes: 1048576
logs:
  max_batch_bytes: 262144
syslog_receiver:
  enabled: true
  unknown_source_policy: reject
  tls:
    enabled: true
    listen: "0.0.0.0:6514"
    cert_file: /etc/infrapulse/syslog/server.crt
    key_file: /etc/infrapulse/syslog/server.key
    client_auth: require_and_verify
    client_ca_file: /etc/infrapulse/syslog/clients-ca.crt
  tcp:
    enabled: false
    listen: "127.0.0.1:1514"
    framing: octet_counting
    allow_non_loopback: false
  allowed_peers:
    - "10.20.0.0/16"
  sources:
    - id: linux-01
      tls_identity: "dns:linux-01.company.example"
      peer_cidrs: ["10.20.10.15/32"]
      audit_apps: [audispd, audisp-syslog]
      provenance: real
  limits:
    max_message_bytes: 8192
    max_connections: 64
    max_connections_per_source: 8
    read_timeout: 10s
    idle_timeout: 60s
    admission_timeout: 2s
```

Относительные TLS paths разрешаются относительно YAML. Listener addresses —
literal IPv4/IPv6 и port (`0.0.0.0`/`::` для wildcard); DNS bind names,
invalid port/address, пересекающиеся
listeners, отсутствующие cert/key, несовместимые trust options и неверные
limits отклоняются. TLS listener имеет отдельную PKI boundary: не копируйте
Collector→Backend client identity certificate в syslog server config.
Bind/certificate/storage startup failure не включает insecure fallback.

`client_auth: require_and_verify` проверяет client certificate chain, validity
и client-auth usage через настроенный CA. `tls_identity` сопоставляется с
проверенным DNS SAN (`dns:`) либо URI SAN (`uri:`), а не Common Name или
syslog HOSTNAME. При `client_auth: none` Collector аутентифицируется перед
sender, но sender перед Collector не аутентифицирован: требуется explicit
`allowed_peers`, а attribution выполняется по настроенным peer CIDRs.

Plain TCP включается только явно. Для loopback development отключите TLS
listener и включите `tcp.enabled: true`; framing задайте `octet_counting`
или `lf`. Public plain TCP дополнительно требует `allow_non_loopback: true`
и ограниченный `allowed_peers`. Шифрования и authenticated peer identity
у plain TCP нет. Receiver не открывает UDP port.

## Source identity

Listener configuration отделена от source mappings: один listener принимает
много серверов. На каждый incoming hostname не создаётся persistent Target.

| Сведение | Значение и доверие |
|---|---|
| `LogRecord.host` | Заявленный HOSTNAME; sender claim, не authentication |
| `SyslogMetadata.peer_ip` | Реальный transport peer address, возможен relay/NAT |
| `SyslogMetadata.tls_identity` | Configured verified SAN mapping либо fingerprint client leaf certificate |
| `SyslogMetadata.authenticated` | Проверен client certificate; у plain/server-only TLS false |
| `SyslogMetadata.managed` | Установлен configured logical source mapping |
| `Observation.SourceRef` | type=syslog либо auditd для parsed audit, CollectorID, TargetID=`syslog:<id>`, protocol=syslog-tls/syslog-tcp, InstanceID=`<id>`, configured provenance |
| `LogRecord.source_id` | Configured source ID; не Global Asset ID |

Если SAN и peer CIDRs заданы вместе, применяются оба ограничения. Ambiguous
mapping отклоняется. Peer IP не доказывает identity машины за relay/NAT;
один relay может пересылать claims нескольких originators. Global Asset
Resolution и сопоставление заявленных origin hostnames выполняет Backend.

`unknown_source_policy: reject` — default. Явный `unmanaged` сохраняет
ограниченные observations без configured TargetID, с InstanceID=`peer:<ip>`
и `managed=false`. Legacy `device_id=syslog:unmanaged` — source label для
совместимости, не созданный Asset. Unmanaged payload не получает audit
classification только по тексту сообщения. Cardinality ограничена active
connections; permanent registry на каждый connection/hostname отсутствует.

Реальные mappings имеют `provenance: real`; isolated synthetic sender можно
явно отметить `simulated`. TLS certificate не превращает synthetic fixture
в реальный production event. Legacy `source=simulated` сохраняется при delivery.

## Время

Используются существующие SourceRef/ObservationTime, без новой temporal model:

| Поле | Remote syslog semantics |
|---|---|
| source_timestamp / original_timestamp | Абсолютный timestamp RFC5424 с explicit timezone; NILVALUE/invalid/legacy — unknown |
| observed_at | Когда Collector получил полное сообщение |
| collected_at | Когда Collector зафиксировал normalized observation |
| clock_quality | unknown; timezone не доказывает synchronized source clock |
| clock_offset / uncertainty | unknown; не вычисляются из arrival delay |
| received_at | Ставит Backend; Collector его не создаёт |

Raw RFC3164 timestamp сохраняется в syslog metadata. Год/timezone не infer,
trusted timezone option в Phase 3 не добавлена. AuditRecord.EventID сохраняет
kernel audit timestamp/serial отдельно: syslog source_timestamp не заменяется
audit timestamp. Outbox delay/replay не переписывает исходные времена.
Phase 2 NTP дисциплинирует системные часы стандартной службой ОС;
Receiver не корректирует source timestamps вручную через offset.

## Подключение rsyslog

1. Выдайте sender certificate с client-auth usage и SAN, соответствующим
   `sources[].tls_identity`. CA Collector server должен доверяться sender.
   Server certificate должен содержать DNS SAN имени, проверяемого sender.
2. Установите штатный rsyslog TLS module вашей ОС (например, GnuTLS module).
   Создайте private writable spool directory и защитите sender private key.
3. Адаптируйте [rsyslog-forwarding.example.conf](../configs/rsyslog-forwarding.example.conf):
   Collector address, CA/cert/key paths, spool location и corporate peer name.
   Добавьте фрагмент в существующую rsyslog configuration, объединяя уже
   заданные global TLS defaults. Существующие local actions можно сохранить.
4. Проверьте полную конфигурацию до reload:

   ```sh
   rsyslogd -v
   sudo rsyslogd -N1
   ```

5. Проверьте Collector config и только затем перезапустите его сервис;
   reload rsyslog выполните стандартным способом вашей ОС. Отправьте test
   message через штатный logger и проверьте `syslog_receiver` diagnostics
   и ingest Controller:

   ```sh
   logger -t infrapulse-check "remote syslog connection check"
   /usr/local/bin/infrapulse-agent diagnostics --config /etc/infrapulse/config.yaml
   ```

Пример использует `omfwd`, RFC5424 template, `TCP_Framing="octet-counted"`,
GnuTLS `x509/name` с проверкой peer и disk-assisted action queue с retry.
`RSYSLOG_SyslogRFC5424Format` не добавляет LF; для старой версии можно использовать
`RSYSLOG_SyslogProtocol23Format`, учитывая LF внутри octet-counted payload.
Синтаксис зависит от доступной версии/module; фактическая проверка example
указана в [validation](validation.md).
Настройки основаны на официальных [omfwd](https://docs.rsyslog.com/doc/configuration/modules/omfwd.html),
[templates](https://docs.rsyslog.com/doc/configuration/templates.html) и
[queues](https://docs.rsyslog.com/doc/concepts/queues.html).

Disk-assisted queue имеет bounded disk usage и volatile RAM portion. Retry
сохраняет возможность reconnect при offline Collector, но finite capacity,
crash/power loss и отсутствие per-record application ACK не дают lossless
или exactly-once guarantees. Отдельную sender queue InfraPulse не создаёт.

## auditd → syslog

Используется штатный `audisp-syslog` из Linux audit-userspace. Не включайте
auditd remote protocol и не устанавливайте InfraPulse dispatcher.

Расположение зависит от версии/дистрибутива: текущий auditd ищет plugins в
`plugin_dir`, default `/etc/audit/plugins.d`; старые audispd deployments
используют `/etc/audisp/plugins.d`. Найдите supplied `syslog.conf` по package
manifest и фактическому `plugin_dir`, а binary — через package manifest/
`command -v audisp-syslog`. Путь из примера ниже заменяется на installed binary:

```ini
active = yes
path = /sbin/audisp-syslog
type = always
args = LOG_INFO LOG_AUTHPRIV
format = string
```

Не добавляйте `interpret`: upstream plugin тогда меняет canonical audit
header на locale datetime и может потерять serial; интерпретированные
user-controlled поля также усложняют безопасный parsing. При заданной facility
plugin использует syslog tag `audispd`, поэтому этот APP-NAME должен присутствовать
в `audit_apps`. Default LOG_USER/version packaging могут дать другой tag;
проверьте реальный output перед mapping. Настройте `name_format` в auditd.conf
для useful node claim, если это соответствует политике предприятия; claim не
заменяет transport authentication. [Upstream plugin](https://github.com/linux-audit/audit-userspace/blob/master/audisp/plugins/syslog/audisp-syslog.c),
[plugin options](https://github.com/linux-audit/audit-userspace/blob/master/audisp/plugins/syslog/audisp-syslog.8),
[auditd plugin_dir](https://github.com/linux-audit/audit-userspace/blob/master/docs/auditd.conf.5).

Audit normalization включается только для managed source и configured
`audit_apps`, после успешного syslog parsing. Требуется canonical
`type=<record_type> msg=audit(<epoch>[.<fraction>]:<serial>):`;
одно `type=` не является audit evidence. SYSCALL, EXECVE, PATH, USER_AUTH,
USER_LOGIN и другие canonical record types используют существующий parser.
Malformed/неподдерживаемый audit текст остаётся обычным syslog LogRecord.

```text
<14>1 2026-10-08T12:00:00Z linux-01 app 23 ID47 [example@32473 key="a b"] message
<86>1 2026-10-08T12:00:01+03:00 linux-01 audispd - - - type=SYSCALL msg=audit(1791450001.123:42): uid=1000 auid=1000 pid=23 syscall=59 success=yes exit=0
<14>Oct  8 12:00:00 linux-01 app[23]: legacy message
```

Это examples/fixtures, не утверждение появления реальных audit events.
Все records сохраняются отдельно: EventID позволяет future Backend aggregation,
но не доказывает полноту event и не вызывает destructive grouping.

## Limits, failure behavior и delivery

Default limits — 8192 bytes/message, 64 connections, 8 connections/source,
10s/frame read, 60s idle, 2s admission wait. Prefix и declared size проверяются
до allocation, partial frame имеет deadline. Connections ограничены до TLS
handshake; дополнительно ограничивается IP и mapped source. На каждое bounded
connection приходится worker; goroutine на message и unbounded input queue нет.
Один record admission использует существующий LogBatch и Agent.push.

JSON expansion, duplicated raw/message, metadata и sanitizer учитываются в
config budget: `14 * max_message_bytes + 8192 <= logs.max_batch_bytes`,
`logs.max_batch_bytes + 1024 <= queue.max_message_bytes`. На devices с малым RAM
уменьшайте connection limits; max message должен оставаться пригодным для
RFC5425 minimum 2048 bytes. OpenWrt receiver остаётся disabled по умолчанию.

При full durable outbox Receiver удерживает только текущий bounded record,
прекращает читать следующие frames и делает retry каждые 50ms до admission_timeout.
Storage/unavailable errors закрывают connection сразу; истечение admission
timeout тоже закрывает connection и увеличивает safe diagnostics
failure counters. Successful network Read не считается durable admission.
Никакой syslog application ACK не отправляется. Sender retry policy принадлежит
rsyslog; resend/dedup между sender и Collector не гарантируются.

| Сбой | Поведение |
|---|---|
| Bind/cert-key/config error | Receiver startup failure, без fallback |
| Invalid/expired/missing mTLS client certificate | Handshake rejection, другие clients продолжают работу |
| Unknown/ambiguous source | Reject по default policy, либо explicit unmanaged |
| Invalid framing/size, disconnect mid-frame | Connection закрывается, partial payload не admission |
| Idle/slow client | Deadline, release connection resources |
| Malformed syslog/audit | Sanitized raw fallback, без invented fields/audit identity |
| Outbox full/storage failure | Stop reading; full — bounded retry, storage failure — immediate connection close |
| Backend offline | Admitted retained records остаются в durable outbox в пределах retention/capacity |
| Collector restart | Retained messages replay со stable message IDs через existing ACK delivery |
| Shutdown | Stop accept, cancel/close active sockets, wait bounded processing |

Receiver diagnostics содержит counts/error categories, не payload/certificate/
private key dumps. Raw проходит существующий best-effort sanitizer;
конфиденциальные audit paths/usernames/UID всё равно могут присутствовать.
Logs никогда не управляют NTP/config/commands/credentials и не исполняются.

Гарантии разделены: sender→Collector имеет TCP/TLS transport без application
ACK, поэтому возможны loss/duplicates; Collector→Backend использует существующие
synced durable admission, stable IDs и ACK/replay, at-least-once для retained
messages. Retention/priority eviction допускает documented loss; parsed audit
использует high priority, ordinary durable logs — medium. Exactly-once нет.
Local file cursors/rotation продолжают работать независимо от network source.

## Deployment

Открывайте 6514/TCP только approved sender subnets в management network и
только после explicit enable. Не публикуйте listener в Internet. Применяйте
host firewall/security groups, service user, отдельные syslog PKI files,
persistent writable state и storage monitoring. Port 6514 не требует root.
Container deployment требует explicit published port и persistent state;
network namespace определяет доступность listener. До изменения service unit
проверьте существующие sandbox/network restrictions.
Практические deployment границы — [deployment](deployment.md),
[security](security.md), [delivery](delivery.md).

Tests, actual sender checks, regression, cross-build и неподтверждённые live
auditd/production Backend ограничения отражаются в [validation](validation.md).
Фикстуры и cross-build не являются production throughput/OpenWrt certification.
