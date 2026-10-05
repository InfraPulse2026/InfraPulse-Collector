# Vendor profiles

Profiles Cisco, Huawei, MikroTik, Juniper, Eltex и QTECH определяют производителя
по проверенному enterprise prefix `sysObjectID`. Они переиспользуют Generic SNMP
driver. Сам profile не добавляет capability и не утверждает поддержку всех MIB
производителя. Configured hint остаётся отдельным слабым evidence.

| Profile | Enterprise prefix | Primary evidence |
|---|---|---|
| Cisco | `.1.3.6.1.4.1.9` | [IANA PEN 9](https://www.iana.org/assignments/enterprise-numbers/?page=1), [официальный CISCO-SMI](https://github.com/cisco/cisco-mibs/blob/main/v2/CISCO-SMI.my) |
| Huawei | `.1.3.6.1.4.1.2011` | [IANA PEN 2011](https://www.iana.org/assignments/enterprise-numbers/?page=21), [Huawei electronic labels](https://support.huawei.com/enterprise/en/doc/EDOC1100212308/a8bbda1/displaying-electronic-labels) |
| MikroTik | `.1.3.6.1.4.1.14988` | [IANA PEN 14988](https://www.iana.org/assignments/enterprise-numbers/?page=150), [официальная SNMP документация](https://help.mikrotik.com/docs/spaces/ROS/pages/8978519/SNMP) |
| Juniper | `.1.3.6.1.4.1.2636` | [IANA PEN 2636](https://www.iana.org/assignments/enterprise-numbers/?page=27), [официальная SNMP документация](https://www.juniper.net/documentation/us/en/software/junos/network-mgmt/topics/topic-map/snmp-mibs-supported-by-junos-os-and-junos-os-evolved.html) |
| Eltex | `.1.3.6.1.4.1.35265` | [IANA PEN 35265](https://www.iana.org/assignments/enterprise-numbers/?page=353), [Eltex SNMP documentation](https://docs.eltex-co.ru/pages/viewpage.action?pageId=597787525) |
| QTECH | `.1.3.6.1.4.1.27514` | [IANA PEN 27514](https://www.iana.org/assignments/enterprise-numbers/?page=276), [QTECH SNMP documentation](https://www.qtech.ru/support/wiki/113/2966/) |

Fixture verification проверяет семантику официального PEN registry: positive и
negative detection, границы arcs, malformed/missing OID, unknown product,
Generic SNMP fallback и отсутствие ложных capabilities. Fixtures не являются
captures оборудования; hardware validation ни для одного из шести profiles
не выполнена. Фактическая support matrix — [vendor-support](vendor-support.md).

Model, serial, firmware и software берутся из существующего standard ENTITY-MIB
mapping при наличии однозначной chassis. Неизвестный model остаётся пустым.
Vendor-specific health/inventory OID пока не опрашиваются: для безопасного
внедрения нужны подтверждённые product-specific schema и device captures.
P1 fallback по literal MikroTik в `sysDescr` сохранён с `confidence=low`.
Hostname не используется для определения производителя. Enterprise evidence
доказывает namespace, а не hardware authenticity или набор функций устройства.
