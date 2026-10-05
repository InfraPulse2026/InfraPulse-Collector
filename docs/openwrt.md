# Native OpenWrt

Native OpenWrt — LocalPlatform того же InfraPulse Agent. CPU, memory, uptime,
kernel, interface counters, sysfs metadata и IP-адреса собираются существующим
Linux collector. OpenWrt-specific inventory обновляется в inventory job,
отдельно от fast telemetry.

Выбор через `local.platform`:

```yaml
local:
  enabled: true
  platform: auto
```

`auto` выбирает Native OpenWrt при `DISTRIB_ID=OpenWrt` в `/etc/openwrt_release`
или `ID=openwrt` в `/etc/os-release`; иначе используется Linux. `linux`
принудительно выбирает общий Linux collector. `openwrt` требует указанного
distribution evidence. Hostname и наличие ubus сами по себе не подтверждают ОС.

OpenWrt inventory использует только фиксированные typed operations:

| Источник | Нормализованные данные |
|---|---|
| `/etc/openwrt_release`, `/etc/os-release` | Distribution, software release, firmware revision, target/architecture evidence |
| `/bin/ubus -S call system board {}` | Hostname, kernel, board/model, release, rootfs evidence |
| `/bin/ubus -S call network.interface dump {}` | Logical network name, protocol, device, up/pending и addresses |
| `/bin/ubus -S call service list {"verbose":false}` | Registered procd services, instances, running/stopped/unknown и PID |
| `/bin/ubus -S call uci get {"config":"firewall"}` | UCI zones, networks, forwardings, rules и redirects |

Agent вызывает absolute binary без shell, внешних arguments и Controller-defined
commands. Каждый read ограничен context timeout; optional reads делят общий
deadline inventory job. stdout ограничен 1 MiB, stderr — 4 KiB; arrays/maps —
1024 entries, известные string fields — 1024 bytes. Количество service instances
ограничено 1024 на snapshot. Timeout/malformed/unavailable source записывается
как unresolved evidence с фиксированной категорией, без raw process output.
Недоступность optional sources сохраняет обычный Linux collector.

`services.read` и `firewall.read` объявляются supported только после успешного
соответствующего inventory read. При отказе они становятся unavailable; CPU,
memory и interfaces capabilities сохраняются. `health.read`, service mutation
и firewall mutation не объявляются.

Service state определяется только `instances.running`. Любая явно running
instance даёт `running`; все явно stopped instances дают `stopped`; отсутствие
instances или неполные сведения дают `unknown`. Это учитывает одноразовые init
services. procd `autostart` не используется как признак boot enablement.
`enabled=true` сохраняется только при подтверждённой `S??<service>` symlink
из `/etc/rc.d` на существующий `/etc/init.d/<service>`; отсутствие ссылки остаётся
unknown, поскольку Agent не исполняет init scripts для определения START/STOP.
Service `command`, `env`, `data` не включаются в inventory.

Firewall snapshot описывает **configured UCI evidence**, не active nftables
ruleset. Zones/rules/forwardings/redirects сохраняют configured `enabled`;
отсутствующее значение использует подтверждённый fw4 default `1`. Snapshot-level
enabled остаётся unknown. Отсутствующие fields сохраняются пустыми/unknown.
Неподдерживаемые sections (например, includes/NAT/ipsets) дают `partial`;
невозможный read или malformed schema — `failed`, чтобы не заявлять полное
исчезновение конфигурации. Неизвестные UCI options и raw config не передаются.
При отсутствии rpcd UCI object firewall unavailable. `fw4`, `nft` и UCI mutations
не вызываются.

Schemas подтверждены primary implementations:

- [procd system_board](https://github.com/openwrt/procd/blob/master/system.c);
- [procd service_dump](https://github.com/openwrt/procd/blob/master/service/service.c)
  и [instance_dump](https://github.com/openwrt/procd/blob/master/service/instance.c);
- [netifd network dump](https://github.com/openwrt/netifd/blob/master/ubus.c);
- [rpcd UCI JSON mapping](https://github.com/openwrt/rpcd/blob/master/uci.c);
- [fw4 configuration parsing](https://github.com/openwrt/firewall4/blob/master/root/usr/share/ucode/fw4.uc);
- [OpenWrt rc.common boot semantics](https://github.com/openwrt/openwrt/blob/main/package/base-files/files/etc/rc.common);
- [ubus installation path](https://github.com/openwrt/openwrt/blob/main/package/system/ubus/Makefile).

Unit tests используют schema fixtures, isolated release files и существующие
Linux procfs/sysfs fixtures. Они проверяют detection, board/release, network,
services/firewall, timeout/cancellation, bounds, partial availability, Linux
fallback и encode/decode настоящего gRPC wire contract. Fixtures не являются
hardware dump. OpenWrt runtime/hardware validation пока не выполнена;
cross-compilation не доказывает startup и потребление RAM на конкретной firmware.

P3 regression дополнительно проверяет повторные inventory snapshots после
исчезновения procd service, смены UCI port/enabled и удаления sections. Новый
read не изменяет прежний normalized snapshot и не сохраняет stale service/rule
в cache. Успешный пустой UCI read даёт `complete`; RPC permission failure и
malformed UCI дают `failed`, неподдерживаемый include — `partial`. Ошибка ubus
с secret-bearing text не попадает в evidence. Linux telemetry сохраняется.
Эти проверки остаются schema fixtures; реальные rpcd/procd/fw4 runtime и
ограничения RAM/flash на конкретном роутере не подтверждены.

SNMPv3 Collector Mode использует USM cryptography. Неограниченный global
password cache GoSNMP отключён; password-to-key вычисляется при новых exchanges.
Это расходует CPU, поэтому fast polling/concurrency на слабом MIPS/ARM нужно
настраивать по measurement конкретного устройства. Cross-build подтверждает
portability binary, но не performance. Для ограниченного flash допустим memory
outbox; durable state рекомендуется размещать на подходящем persistent storage
с ограничениями bytes/messages/retention и batching.
