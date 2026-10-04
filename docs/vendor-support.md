# Поддержка платформ и производителей

Документ отражает текущий P0. Наличие архитектурного плана, поля конфигурации или типа команды не означает работающую интеграцию. Реализован локальный Linux collector; удалённые SNMP/RouterOS drivers и vendor profiles относятся к следующему этапу.

## Текущее состояние

| Платформа / vendor | Detection | Inventory | Interfaces | Telemetry | Physical topology | Commands |
|---|---|---|---|---|---|---|
| Local Linux | Runtime OS, hostname, os-release, kernel | OS/version/architecture/uptime/boot ID; DMI vendor/model/serial/revision при доступности | Реальные counters, index/name/MAC/MTU/admin/oper/speed/IP | CPU/load, RAM/swap, optional PSI, traffic rates | Unsupported | Unsupported; mutations не выполняются |
| Local OpenWrt | Native adapter planned P2 | Native ubus/UCI inventory planned P2 | Общий Linux путь потенциально применим; runtime на OpenWrt не проверен | Общий Linux путь потенциально применим; native adapter не реализован | Unsupported | Unsupported |
| Local Windows | Unsupported; collector-only hosting архитектурно допустим | Unsupported | Unsupported | Unsupported | Unsupported | Unsupported |
| Generic SNMP | Planned P1 | Planned P1 | Planned P1 | Planned P1 | LLDP planned P1 | Read-only target; write unsupported |
| MikroTik RouterOS | HTTPS REST planned P1 | Planned P1 | Planned P1 | Planned P1 | Neighbor discovery planned P1 | Typed interface enable/disable planned P1 |
| Cisco | SNMP profile planned | Standard MIB collection planned | Planned | Planned | LLDP where available, planned | Unsupported |
| Huawei | SNMP profile planned | Standard MIB collection planned | Planned | Planned | LLDP where available, planned | Unsupported |
| Juniper | SNMP profile planned | Standard MIB collection planned | Planned | Planned | LLDP where available, planned | Unsupported |
| Eltex | SNMP profile planned | Standard MIB collection planned | Planned | Planned | LLDP where available, planned | Unsupported |
| QTECH | SNMP profile planned | Standard MIB collection planned | Planned | Planned | LLDP where available, planned | Unsupported |

MikroTik также может использовать общий SNMP driver после его реализации. Сейчас ни один из шести remote vendors не объявляется поддержанным production collector. Mock Controller получает реальные локальные данные; Simulator и synthetic vendor demonstrations пока не реализованы.

## Linux capabilities и ограничения

Локальная implementation объявляет `system.read`, `inventory.read`, `cpu.read`, `memory.read`, `interfaces.read`, `telemetry.read`. Поддержка write, services, firewall, logs, telephony и topology не объявляется. Agent не превращает наличие Linux в доказательство типа оборудования: device type остаётся `unknown`, если надёжной классификации нет.

Необязательные DMI поля могут отсутствовать или требовать привилегий. Physical core count отсутствует, если cpuinfo не содержит физических/core IDs. Первый CPU sample, reboot и уменьшение counters не имеют utilization. PSI и interface speed доступны только когда соответствующий kernel/sysfs источник возвращает данные. Если старый kernel не предоставляет MemAvailable, используется оценка `free + buffers + cached + SReclaimable - Shmem`, ограниченная размером RAM; это оценка reclaimable memory, а не отдельный sensor.

Текущая scalar модель interface index/MTU/admin state не имеет presence flags: при отсутствии sysfs metadata index/MTU равны 0, admin state false, oper state `unknown`. Consumers не должны трактовать это как подтверждённую команду отключения. Расширение presence для этих полей потребуется вместе с remote mappings.

Linux collector проверен с fixtures без root/network и на реальных `/proc`, `/sys` и interface addresses в WSL Linux amd64. Компиляция `linux/amd64`, `linux/arm64`, `linux/arm`, `linux/mips`, `linux/mipsle` проверена отдельно. Cross-build не подтверждает работу конкретной прошивки OpenWrt или ресурсные ограничения маршрутизатора.

## Как будет расширяться SNMP поддержка

Один protocol adapter реализует SNMP exchange, deadlines и bounded responses. Один Generic SNMP driver нормализует стандартные SYSTEM, IF-MIB/IF-X-MIB и LLDP данные. Новому manufacturer не требуется копия driver: profile добавляет только подтверждённое detection evidence и реально нужные mappings.

Vendor detection будет использовать проверенные sysObjectID prefixes, затем слабое sysDescr evidence. Явно выбранный profile — конфигурационный hint, а не автоматически подтверждённый manufacturer. Без достоверного evidence vendor остаётся unknown. Serial/model не выводятся из названия производителя.

Каждая capability будет объявляться по реализованной функции и фактической доступности данных. Отсутствующая MIB или API operation даёт unsupported/unavailable с причиной. Vendor-specific OID, endpoint или CLI добавляется только после проверки документации и mapping tests. SNMPv2c — первый read-only этап; SNMPv3 auth/privacy и дополнительные протоколы требуют отдельной реализации.

## Критерий обновления matrix

После появления driver строка обновляется только с перечислением конкретных работающих функций, протокола/версии и способа проверки: fixtures, simulator, virtual device или реальное оборудование. Проверка на одном model не гарантирует все семейства и firmware vendor. Реальные hardware tests остаются отдельно tagged и не требуются для обычного `go test ./...`.
