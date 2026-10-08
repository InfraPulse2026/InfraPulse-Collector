# Deployment Collector / Edge Gateway

Linux installation с отдельным service user и существующим systemd unit
описана в [README](../README.md#установка-на-linux). Collector-only mode
`local.enabled: false` обслуживает SNMP/RouterOS/SIP/remote topology и opt-in
remote Syslog без установки InfraPulse Agent на каждый source server.
Production Backend/CA services остаются отдельными компонентами.

## State и права

Используйте persistent writable `agent.state_dir` с private permissions
(Unix0700/0600), достаточным disk space и working fsync. Identity, durable
outbox, command journal/snapshots и local file cursors сохраняются при update.
Не удаляйте state для устранения corruption: existing fail-closed recovery
требует явного operator action. Durable limits/retention/priority eviction
ограничивают сохранение logs; durability не заменяет capacity planning.

Непривилегированный service user должен читать config, CA/cert/key и explicitly
configured local log files и записывать state. Remote Syslog не требует
доступа к source-server filesystem или исполнения shell. Existing
[systemd unit](../deployments/systemd/infrapulse-agent.service) использует
ProtectSystem=strict, StateDirectory и TimeoutStopSec=10s; сохраняйте отдельный
writable state path. Syslog port6514 не требует CAP_NET_BIND_SERVICE/root.

## Network logging

Receiver disabled по умолчанию. Для enable настройте отдельные syslog server
certificate/private key/client CA, approved source mapping и firewall.
Certificate должен иметь SAN имени, проверяемого rsyslog sender; mTLS sender
certificate — client-auth usage и SAN из configured mapping. Logging PKI и
Collector→Backend identity находятся в разных security boundaries.

Разрешайте 6514/TCP только approved sender subnets в management network;
firewall/security group rule принадлежит администратору. Public insecure
listener не включается автоматически. Plain TCP требует explicit compatibility
configuration; loopback — default, public mode дополнительно требует CIDR
allowlist и allow_non_loopback. UDP logging port не нужен.

Перед запуском выполните от имени service user:

```sh
/usr/local/bin/infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
```

Enabled receiver требует `queue.mode: durable` и `ack_fallback: reject`.
Cert/key/CA/config errors отклоняются до открытия sockets; listener bind error
останавливает startup без silent fallback. Backend offline не препятствует
bounded durable admission; unavailable durable storage не становится
успешным memory admission remote logs. Certificate replacement listener
требует restart Collector; Backend enrollment renewal не обновляет listener.

Sender использует штатный rsyslog omfwd с disk-assisted action queue, retry и
проверкой Collector TLS identity. Auditd подключается через installed
audisp-syslog plugin по фактическому plugin_dir вашей версии Linux. Setup,
test messages, source attribution и failure behavior —
[syslog-receiver](syslog-receiver.md).

## Linux/OpenWrt/container bounds

Для bounded listener выбирайте connection/message/timeouts по ресурсам edge.
Defaults64 connections/8 per source/message8192 — configurable limits, а не
доказанный hardware budget. OpenWrt receiver по умолчанию disabled;
durable remote logging требует подходящего external/persistent storage.
Учитывайте flash wear и RAM/page cache. Cross-build Linux amd64/arm64/arm/mips/
mipsle не подтверждает firmware runtime или production throughput.

Container требует persistent state volume, explicit published TCP listener
port и trusted cert/key mounts с правами service user. Host metrics/logs
требуют отдельного осознанного mounts/permissions review: обычный container
namespace не является full host evidence. Production container runtime
deployment этим этапом не подтверждён; Docker не обязателен.

Corporate NTP на Backend/Collector/source hosts настраивается стандартной
службой ОС. Collector использует Phase 2 [time-sync](time-sync.md), не
исправляет received log timestamps вручную и не выполняет remote NTP commands.
Source clocks остаются отдельным trust domain.

Фактические rsyslog fixtures, regression/cross-build и ограничения проверки —
[validation](validation.md). Mock ACK подтверждает development receipt;
production Backend должен durable commit/deduplicate перед application ACK.
