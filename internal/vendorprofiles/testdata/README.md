# Источники fixtures

`registry.json` сохраняет подтверждённые записи IANA PEN, просмотренные
2026-10-05. Это fixtures семантики реестра, а не SNMP captures оборудования.
Тестируется соответствие предприятия и граница OID prefix. Значения неизвестного
product suffix в тестах являются негативными/семантическими cases; они не
объявляются реальными моделями. Названия моделей, firmware и serial здесь
намеренно отсутствуют. Hardware validation не выполнена.

Для каждого производителя source указывает primary registry evidence.
Дополнительные официальные MIB/documentation ссылки приведены в
[`docs/vendor-profiles.md`](../../../docs/vendor-profiles.md).
