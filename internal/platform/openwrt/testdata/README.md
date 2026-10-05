# OpenWrt schema fixtures

Эти fixtures построены по schemas официальных OpenWrt implementations.
Имена, адреса, PID, board и release revision специально заданы для tests:
это не dump физического router и не hardware verification.

Источники schemas, просмотренные 2026-10-05:

- `board.json`: [procd system_board](https://github.com/openwrt/procd/blob/master/system.c).
- `networks.json`: [netifd_dump_status/netifd_handle_dump](https://github.com/openwrt/netifd/blob/master/ubus.c).
- `services.json`: [service_dump](https://github.com/openwrt/procd/blob/master/service/service.c)
  и [instance_dump](https://github.com/openwrt/procd/blob/master/service/instance.c).
- `firewall.json`: [rpcd UCI JSON](https://github.com/openwrt/rpcd/blob/master/uci.c)
  и [fw4 parse_zone/parse_forwarding/parse_rule/parse_redirect](https://github.com/openwrt/firewall4/blob/master/root/usr/share/ucode/fw4.uc).
- `openwrt_release`: [официальный release template](https://github.com/openwrt/openwrt/blob/main/package/base-files/files/etc/openwrt_release).

Secret-bearing `command`, `env`, `data` и неизвестные UCI options включены
специально для проверки, что они не передаются в normalized inventory.
