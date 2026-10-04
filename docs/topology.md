# Физическая топология

Физическая карта — обязательный результат следующего этапа InfraPulse Agent. Текущий P0 передаёт локальную inventory и interface telemetry. Topology collector, normalized edges, `TopologyUpdate` protobuf payload и graph reconciliation ещё не реализованы. `topology_interval` зарезервирован в конфигурации; его наличие не означает сбор LLDP. Mock Controller P0 не показывает обнаруженные связи.

## Целевой pipeline P1

```text
SNMP LLDP / RouterOS neighbor observations
                    |
                    v
     bounded protocol collection + timestamp
                    |
                    v
   raw identifiers/subtypes + local port mapping
                    |
                    v
 normalized nodes/edges + evidence + completeness
                    |
                    v
   sanitizer -> bounded outbox -> outbound gRPC
                    |
                    v
       Backend reconciliation and graph UI
```

Agent собирает evidence; Backend связывает observations разных collectors, хранит историю и строит graph. Для UI не требуется изменение конкретных vendor drivers.

## Целевой normalized edge

Следующие поля — проектируемый контракт, а не существующий P0 protobuf message:

| Поле | Смысл |
|---|---|
| localDeviceID | Стабильный ID устройства из Agent identity или config/Backend |
| localInterface | Канонический локальный interface ID плюс raw port evidence |
| remoteDeviceIdentifier | Известный remote ID либо unresolved advertised identifier |
| remoteChassisID | Chassis identifier с исходным subtype/bytes |
| remoteInterface | Advertised remote port ID с subtype |
| discoveryProtocol | Реально наблюдаемый LLDP/CDP/MNDP либо иной protocol |
| observedAt / collectedAt | UTC время observation и сбора |
| source | `lldp`, `cdp`, `routeros-neighbor`, `arp`, `fdb`, `manual`, `simulated` |
| confidence / evidence | Качество привязки и основание вывода о связи |
| optional VLAN/network | Данные логического сегмента, когда они доступны |

Raw chassis/port subtypes сохраняются: MAC, network address и locally assigned identifier нельзя сравнивать как одну неразличимую строку. Device hostname не является глобальным уникальным ID. Remote, который ещё не сопоставлен с inventory, остаётся unresolved node.

## Источники и корректность

LLDP — основной планируемый источник физического соседства через LLDP-MIB. Local LLDP port number не обязан совпадать с ifIndex. Driver должен сопоставить local port ID/subtype с interface name/description/MAC и сохранять unresolved mapping, если уверенного соответствия нет. Индексы remote table должны разбираться целиком, чтобы разные neighbors и port observations не объединялись случайно.

RouterOS neighbor discovery будет сохранять фактический protocol и raw interface evidence. MNDP или discovery на общем bridged сегменте могут указывать доступность устройства в сегменте без доказательства одного прямого кабеля. Такие observations нельзя автоматически выдавать за достоверную point-to-point физическую связь.

ARP/ND и forwarding/MAC tables пригодны для дополнительных логических relationships. ARP entry сам по себе не доказывает физическое соединение. Источник и confidence всегда сопровождают observation; manual и simulated edges остаются явно помеченными.

## Обновления, ошибки и lifetime

Сбор topology работает по отдельному configurable slow interval, по умолчанию 5 минут. Он не повторяет полный neighbor walk каждые 3 секунды. Responses, nodes/edges и parallel device requests ограничиваются по размеру и concurrency.

Будущий `TopologyUpdate` должен различать полный успешный snapshot и partial observation. Timeout, unsupported MIB или ошибка части walk не означают исчезновение всех предыдущих links. Link removal определяется успешными полными observations и expiry/TTL; timestamps сохраняются для работы с устаревшими данными. Backend дедуплицирует оба направления одной связи и сопоставляет remote identifiers с inventory.

PhysicalLocation задаётся config/Backend. Agent не выводит room/rack из LLDP system description и не придумывает отсутствующие значения.

## Демонстрация и тесты

Цель P1: MikroTik router → Eltex switch → server/AP. Реальные LLDP/neighbor observations показываются Mock Controller текстово: local device/port, remote chassis/port, protocol, timestamp и confidence. При отсутствии оборудования development Simulator может формировать такой graph только с `source=simulated`; production collector не использует synthetic neighbors.

Обязательные mapping tests следующего этапа: localPortNum≠ifIndex, chassis/port subtypes, несколько neighbors на port, неизвестный remote, partial walk, empty complete snapshot, TTL/removal и ошибочная попытка считать ARP физической связью. Hardware integration tests требуют явного запуска. Web graph и логическая topology относятся к будущему Backend; P0 их не реализует.
