# Phase 8B: проверка на реальной инфраструктуре

Статус Phase8B — PARTIAL. Linux amd64 proc/sys/file/network runtime выполнен
в WSL; systemd/procd installation, настоящее оборудование и production-like
Backend/PostgreSQL требуют отдельного исполнения. Ни один Mock/fixture не
заменяет hardware evidence. До записи actual result пункты ниже NOT EXECUTED;
в текущей среде отсутствующие endpoints отмечены NOT AVAILABLE в отчёте.

## Evidence и границы

Для каждого сценария сохраняйте date/time, оператор, host/firmware/version,
Collector version/SHA256/UUID/session, sanitized config diff, exact commands,
duration/load, expected/actual, PASS/FAIL/NOT EXECUTED/NOT AVAILABLE и limitations.
Серийные номера/адреса редактируются для внешнего отчёта. Secrets/raw audit/private
keys не прикладываются. Write tests только с отдельным разрешением; базовый
acceptance read-only. Нельзя менять development host NTP/firewall или production
services ради failure injection.

## Linux

На выделенном test host установите [systemd deployment](production-deployment.md).
От service user проверьте config/PKI/read sources и private state write. Запустите,
запишите UUID/session и native CPU/memory/interfaces; проверьте NTP status без apply.
Измеряйте `/proc/<pid>/status`, `/proc/<pid>/fd`, `ps`, filesystem space, queue
diagnostics, startup и SIGTERM latency на 10/50 утверждённых read-only targets.

Restart должен сохранить UUID/LKG/message IDs, изменить process Session ID и
восстановить ACK/replay. Service stop должен уложиться в10s без оставшихся sockets.
Backup/recovery — только stopped state. Backend outage создаётся в isolated test
network/proxy, не глобальным firewall на development host. Подтвердите resource
stabilization после50 polls/50 reconnects/повторных startup-stop.

## OpenWrt

Выберите armv7/mips/mipsle/arm64 binary по board/ABI/endian/FPU firmware. Проверьте
RAM/flash/mount permanence до установки; cross-build этого не доказал. Установите
[procd script](../deployments/openwrt/infrapulse-agent.init) на отдельный разрешённый
роутер; permissions и ubus read ACL принадлежат администратору.

Проверьте start/stop/restart, SIGTERM<=10s, respawn при согласованном test process
failure, controlled reboot, тот же UUID, LKG restore и retained replay/ACK.
После reboot state mount должен существовать до startup. Измерьте RSS/heap/CPU/
FD с10/50 targets, syslog burst и Backend outage, overlay writes/flash growth.
`logread` должен содержать fixed error categories без secrets/payload dump.
`time-sync-status` проверяет sysntpd/UCI; active daemon не доказывает synchronized.
Не применяйте UCI/NTP/network/interface mutations этим checklist.

## Устройства и источники

| Test | Setup / exact local entry point | Expected pass evidence |
|---|---|---|
| SNMPv2c/v3 | утверждённый read-only account; [remote example](../configs/config.remote.example.yaml), [v3 guide](snmpv3.md); `probe-device --config <yaml> <id>` затем normal run | фактические SYSTEM/ENTITY/IF counters; v3 auth/priv; documented vendor prefix, корректные reset/rates; никаких SET |
| RouterOS | read-only HTTPS REST account + verified CA; approved neighbor sources | inventory/interfaces/metrics, canonical IDs, LLDP/CDP/MNDP с raw facts; hardware/firmware записаны; writes0 |
| SIP | отдельный PBX test endpoint, OPTIONS account policy | real response/latency/status, degradation/recovery; no REGISTER/media calls |
| Redfish | actual BMC verified HTTPS/custom CA, read-only role | multi-resource inventory/health/sensors, partial optional sources; balanced sessions либо документированный expiry если logout недоступен; no Actions/writes |
| IPMI | actual BMC LANplus with read-only role; [IPMI guide](ipmi.md) | cipher17 либо явно3, RAKP/integrity, FRU/SDR/sensors; unsupported sensors честно partial; socket cleanup, no power/SEL/SOL/v1.5 |
| Syslog | real rsyslog configured TLS/mTLS with local disk-assisted action queue; [sender setup](syslog-receiver.md) | peer/cert source attribution, RFC5424/5425 framing, queue pressure/reconnect/replay; sender HOSTNAME не меняет trusted source |
| auditd | real auditd + installed audisp-syslog forwarding, approved test read-only event | event ID/type/time/uid fields, correct audit-derived marker/source; sanitizer; no guarantee arbitrary raw secrets removed |
| NTP | corporate chrony/timesyncd/sysntpd managed by administrator | actual service status and synchronization evidence where available; monitoring continues if status unavailable; host clocks не изменяются Collector |

Для LLDP неизвестный neighbor остаётся unresolved. Failed/partial poll не означает
удаление всех links; ARP не кабель, BMC management relation не L2 link. Не
выполняйте локальный global Asset merging. Сохраните исходные subtype/raw ID,
TTL/expiry и source timestamps с real provenance.

## Backend и отказоустойчивость

Используйте [Collector→Backend E2E](collector-backend-e2e.md). Обязательны real
gRPC→durable PostgreSQL→REST, ACK после commit, replay после restart, production
B9 DesiredConfig→ApplyStatus и две независимые identities/outboxes/revisions.
Wrong UUID/session/CA/expired cert должны fail-closed. Временный Backend outage
не удаляет protected/retained records за пределами документированных capacity/
retention правил. Exactly-once end-to-end не заявляется.

Disk-full/unclean power failure — лишь отдельная expendable test VM/storage;
нельзя заполнить общий host disk. Сначала воспроизводимые automated fixtures.
На hardware explicitly mark результат и observed loss/duplicates. Финальный
8B PASS требует фактической evidence; статус8A COMPLETE не заполняет эту таблицу.
