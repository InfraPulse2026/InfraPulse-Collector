# Развёртывание Collector для интеграционных испытаний

Phase 8A проверяет код и isolated fixtures. Этот guide не является утверждением
production-ready: настоящие Backend/PostgreSQL, BMC, switches и OpenWrt должны
пройти [отдельную проверку](real-hardware-validation.md).

## Платформы и привилегии

Collector CGO_ENABLED=0 собирается для Linux amd64/arm64/arm GOARM=7/mips/mipsle
hardfloat. Runtime здесь проверен на Linux amd64 в WSL; Windows amd64 используется
для hosting с local.enabled=false и development tests. Windows LocalPlatform
не реализован. Архитектура сборки не подтверждает ABI/RAM/flash конкретного роутера.

Один экземпляр — отдельный UUID, state directory, outbox и process Session ID.
Не копируйте identity с Collector A на B. Нормализация общего оборудования между
двумя Collectors принадлежит Backend. Протоколы и ограничения —
[support matrix](vendor-support.md); сетевые mutations default-disabled.

Remote SNMP/RouterOS/SIP/Redfish/IPMI, outbound gRPC и Syslog TCP/6514 не требуют
root. Local Linux `/proc`/`/sys` нужны на чтение; audit/log files требуют отдельного
read ACL/group, а state — записи. Corporate NTP apply — отдельная административная
операция; daemon не должен получать root ради неё. Не включайте команды на
реальном оборудовании при первоначальном read-only acceptance.

## Linux/systemd

Команды этого раздела выполняет администратор на согласованном test host.
В development environment они не запускались.

```sh
sudo useradd --system --user-group --home-dir /var/lib/infrapulse-agent --shell /usr/sbin/nologin infrapulse
sudo install -d -m 0750 -o root -g infrapulse /etc/infrapulse
sudo install -d -m 0700 -o infrapulse -g infrapulse /var/lib/infrapulse-agent
sudo install -m 0755 ./infrapulse-agent /usr/local/bin/infrapulse-agent
sudo install -m 0644 deployments/systemd/infrapulse-agent.service /etc/systemd/system/infrapulse-agent.service
```

Сначала подготовьте config/CA/certificate/private key, затем установите с правами
root:infrapulse 0640. YAML `agent.state_dir` должен быть абсолютным
`/var/lib/infrapulse-agent`; все credential/trust/log paths также абсолютные.
Для production выберите security.mode=production, verified CA/SAN, UUID-bound
client cert/key либо [enrollment](enrollment.md). На новом private state команда
`sudo -u infrapulse /usr/local/bin/infrapulse-agent diagnostics --config /etc/infrapulse/config.yaml`
создаёт/читает persistent UUID до запуска daemon. Для manual certificate issuer
должен поместить именно этот UUID в URI SAN `spiffe://infrapulse/agent/<UUID>`.
Production [managed example](../configs/config.managed.example.yaml) требует
замены endpoint и PKI paths. [P3](../configs/config.p3.example.yaml) — development
loopback template с durable queue и SNMPv3, а не готовый production TLS config.
Отдельные adapter examples:
[Syslog](../configs/config.syslog.example.yaml),
[Redfish](../configs/config.redfish.example.yaml), [IPMI](../configs/config.ipmi.example.yaml).

```sh
sudo -u infrapulse /usr/local/bin/infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
sudo systemd-analyze verify /etc/systemd/system/infrapulse-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now infrapulse-agent
sudo systemctl status infrapulse-agent
sudo journalctl -u infrapulse-agent --since '-5 minutes'
```

Unit задаёт User/Group, StateDirectory=0700, WorkingDirectory, UMask=0077,
ProtectSystem=strict, ProtectHome, PrivateTmp, NoNewPrivileges, защиты kernel
modules/tunables/control groups, RestrictSUIDSGID и пустой CapabilityBoundingSet.
StateDirectory остаётся writable; `/etc` и binary — read-only. Restart=on-failure
с паузой5s, TimeoutStopSec=10s; runtime ждёт components до5s. Fixed adapter I/O
обязан соблюдать cancellation. Unit не выполняет NTP apply и команды устройств.
Семантика directives: [primary systemd source](https://github.com/systemd/systemd/blob/main/man/systemd.exec.xml).

Настраиваемые environment credentials задаются в закрытом service drop-in:

```ini
[Service]
EnvironmentFile=/etc/infrapulse/credentials.env
```

Файл root:infrapulse 0640, без вывода содержимого в diagnostics/issue reports.
Не помещайте secrets в argv или Git. Если намеренно нужен listener ниже1024,
администратор отдельно задаёт CapabilityBoundingSet=CAP_NET_BIND_SERVICE и
AmbientCapabilities=CAP_NET_BIND_SERVICE в drop-in; обычный6514 этого не требует.
ProtectHome требует перенести cert/log/state из home в служебные каталоги.
PrivateTmp скрывает host `/tmp`; sources оттуда не подходят для production logs.

## OpenWrt/procd

Поставляется [init script](../deployments/openwrt/infrapulse-agent.init), но live
procd runtime не проверен. `user`, `group`, `respawn`, `stdout/stderr`, `file` и
`term_timeout` сверены с [procd shell API](https://github.com/openwrt/openwrt/blob/main/package/system/procd/files/procd.sh)
и [instance implementation](https://github.com/openwrt/procd/blob/master/service/instance.c).
Проверьте поддержку на вашей firmware до enable.

Администратор выбирает корректный arch/ABI, создаёт service account/group
`infrapulse` средствами своей firmware и private persistent state. Установка:

```sh
install -m 0755 infrapulse-agent /usr/bin/infrapulse-agent
mkdir -p /etc/infrapulse /var/lib/infrapulse-agent
chown infrapulse:infrapulse /var/lib/infrapulse-agent
chmod 0700 /var/lib/infrapulse-agent
cp deployments/openwrt/infrapulse-agent.init /etc/init.d/infrapulse-agent
chmod 0755 /etc/init.d/infrapulse-agent
/usr/bin/infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
/etc/init.d/infrapulse-agent start
logread -e infrapulse
```

Эта команда validate проверяет YAML, затем отдельно проверьте доступ config/PKI/
state именно от имени infrapulse перед start. `/var` на OpenWrt часто volatile:
замените YAML state_dir на реально persistent mount/overlay/external storage,
например `/mnt/collector-state`, создайте его с0700 и владельцем service user.
Не используйте `/tmp` для identity/LKG/outbox. Script не создаёт пользователя,
state, credentials и не меняет UCI. Нужна доступность mount до START=95.
Enable после успешной ручной проверки: `/etc/init.d/infrapulse-agent enable`.

Collector-only mode непривилегирован. Native OpenWrt optional ubus/system/uci
reads требуют разрешений firmware: определите read-only ACL/permissions; если
недоступны, diagnostics честно показывает unavailable. Не переходите на root
автоматически ради optional inventory. Sysntpd принадлежит администратору;
[existing OpenWrt/NTP guide](openwrt.md) описывает ограничения synchronization.

## Ресурсы и network policy

Проверенные числа и workload — [Phase8 report](phase-8-report.md). Это samples
одного development host, не минимальные системные требования/SLA. Измеряйте
RSS/page cache, heap, GC/CPU, descriptors, volume/fd capacity на целевой машине.
Go GOMEMLIMIT — soft runtime budget, не hard RSS ceiling; слишком малое значение
может увеличить GC/CPU. Для syslog-heavy edge используйте persistent storage
с подходящим endurance; RAM не заменяет диск/retention planning.

По умолчанию `queue.mode: memory`: для сохранения данных после restart явно
задайте `queue.mode: durable` и private persistent `agent.state_dir`.
Defaults: outbox256 messages/8MiB payload/1MiB envelope; durable storage64MiB
при этих defaults; retain24h; inflight16; ACK timeout30s. Это limits, не capacity
claims. Hardware medium default30s, inventory/topology5m; SNMP/RouterOS fast3s.
При Backend outage queue bounded: logs/telemetry/обычные inventory/topology/events
могут вытесняться по priority/retention; только принятые CommandResult/reservations
priority0 защищены от вытеснения. Admission failure должен быть виден оператору.
Устойчивость retained data зависит от исправного synced filesystem и Backend ACK
после durable commit/dedup. [Полная delivery model](delivery.md).

Firewall задаёт администратор: outbound Backend gRPC, management addresses,
SNMP161/UDP, IPMI623/UDP, HTTPS, SIP configured ports; inbound Syslog только
approved senders. TLS private files должны быть regular и bounded (symlink к
regular CA допустим); FIFO/device не используется как trust source. Redfish
не следует cross-origin references, IPMI не допускает cipher0/v1.5 fallback.

## Upgrade, backup и recovery

1. Сверьте SHA256, version, commit/dirty marker и architecture до установки.
2. Запишите active managed revision/digest, identity, queue/health diagnostics.
3. Остановите только согласованный Collector (`systemctl stop` либо procd stop),
   дождитесь exit и отсутствия его sockets. Не копируйте открытую bbolt DB обычным cp.
4. Архивируйте весь private state вместе: identity, outbox, command journal/
   snapshots, cursors, managed-config.json, PKI bundle; config/credentials отдельно.
   Backup содержит чувствительные данные: закрытые права и защищённое хранение.
5. Установите новый binary через staged regular file + rename на том же filesystem,
   сохраните previous binary; config/state не заменяются examples.
6. Запустите; проверьте тот же UUID, новый Session ID, restore/revision,
   reconnect/replay/ACK, queue/health и source attribution.

Rollback binary возможен лишь при совместимой версии persistent/wire state.
Не удаляйте identity/outbox/LKG для обхода corruption или stale revisions.
Corrupt state сначала сохраняется для анализа при остановленном процессе;
explicit administrative recovery согласуется с Backend revision/dedup history.
Durable unavailable не означает durable-success: telemetry может работать в
degraded memory fallback, mutations блокируются, remote Syslog admission fails.
Physical power-loss/flash durability не доказаны fixtures.

## Diagnostics и troubleshooting

`infrapulse-agent diagnostics --config /etc/infrapulse/config.yaml` читает persisted
snapshot (и создаёт identity при первом вызове на новом state); `observed_at`
может быть устаревшим после exit. Не запускайте второй
`run` с тем же state. Snapshot/Heartbeat содержат identity/build, connection,
queue/storage/replay/inflight/drops, job runs/skips/errors, active Targets,
Syslog status, time_sync, PKI и managed stored/active revision/status.
Heartbeat поступает только при reachable Backend; отсутствие связи само по себе
не останавливает polling. Receiver не dumps raw input в diagnostics.

При проблеме последовательно проверьте права/regular trust files, CA/SAN/expiry,
network DNS/connectivity, local protocol/CIDR/reference policy, disk/queue limits,
restored managed status и revision. FAILED restore нельзя читать как APPLIED;
старый snapshot/revision не заменяет новый. Admission/eviction и hardware
optional partial errors отличаются от transport failure. Safe support bundle
содержит version, sanitized diagnostics и aggregate counters, без полного YAML,
credentials, private key, сырого Syslog или telemetry dumps.
