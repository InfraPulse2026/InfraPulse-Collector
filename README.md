# InfraPulse Collector / Edge Gateway

InfraPulse Collector — Go-компонент NetOps Control Plane InfraPulse для работы
в management/local network. Он опрашивает сетевые устройства через SNMP v2c/v3
и RouterOS HTTPS REST, опрашивает hardware через Redfish HTTPS и IPMI LANplus,
собирает LLDP/CDP/MNDP observations и SIP health,
передаёт telemetry, inventory, topology, logs и события в Controller/Backend.
Установка Collector на каждый server не требуется: один gateway может
обслуживать набор configured targets без установки нашего ПО на них.

Существующие Zabbix и Prometheus могут оставаться источниками мониторинга
для Backend; Ansible относится к Backend/Automation layer, Grafana — к
дополнительному visualization layer. Их API clients, TSDB, asset merging,
корреляция и отчёты не входят в Collector. Локальные Linux/OpenWrt metrics
предназначены для standalone/fallback mode и наблюдения самого edge-хоста.

Production Backend, Web UI,
AI и историческое хранилище находятся вне этого репозитория. Для разработки
и демонстрации обмена данными поставляются Mock Controller и отдельный Simulator.
Исторические module path `InfraPulseAgent`, binary `infrapulse-agent`, YAML
секция `agent` и protocol names `Agent*` сохранены для совместимости.

## Возможности

- Локальная системная информация Linux: hostname, ОС, kernel, architecture,
  uptime и доступные сведения об оборудовании.
- CPU utilization и load average; RAM, swap и memory PSI при наличии источника.
- Сетевые интерфейсы, IP-адреса, счётчики traffic, packets, errors и drops;
  расчёт traffic rates между измерениями.
- Постоянный Agent ID и отдельный Session ID для каждого запуска.
- Передача inventory, heartbeat и событий Agent.
- Generic SNMP v2c/v3 read-only: SYSTEM, ENTITY-MIB при доступности, IF-MIB/IF-X-MIB
  и LLDP-MIB с сохранением исходных идентификаторов и evidence.
- Redfish read-only: несколько ComputerSystems/Chassis/Managers на BMC,
  hardware inventory/health и modern/legacy temperature/fan/PSU metrics;
  verified HTTPS, session auth и bounded polling через existing durable delivery.
- IPMI v2.0 LANplus read-only для явно заданных legacy BMC: Get Device ID,
  FRU, SDR Full/Compact sensors, temperature/fan/voltage/current/power и chassis
  status через те же hardware models и delivery. UDP/623, cipher 17 либо явно 3.
- RouterOS HTTPS REST: identity/resource/routerboard, interfaces и IP-адреса,
  neighbor discovery с фактическим LLDP/CDP/MNDP source.
- Ограниченный remote polling, offline после трёх последовательных ошибок связи
  и события переходов состояния.
- Настраиваемый интервал оперативной local/SNMP/RouterOS telemetry,
  по умолчанию 3 секунды; hardware polling использует отдельные интервалы.
- Исходящее bidirectional gRPC-соединение, production mTLS и reconnect.
- Memory или bounded durable outbox со stable IDs, negotiated application ACK,
  restart/replay и diagnostics. Durable delivery — at-least-once для retained
  messages; exactly-once не заявляется.
- Persistent command execution journal, durable results и typed RouterOS
  interface snapshots/rollback с теми же policy/protection/verification checks.
- Agent-side enrollment через verified HTTPS, локальный private key/CSR,
  certificate identity и renewal/rotation foundation.
- Типизированные RouterOS `interface.enable`/`interface.disable` с policy,
  защитой management interfaces и проверкой состояния после изменения.
  По умолчанию команды отключены.
- Development Mock Controller для наблюдения данных и проверки команд.
- Native OpenWrt: release/board, logical networks, procd services и read-only
  UCI firewall configuration; Linux CPU/RAM/interfaces collectors переиспользуются.
- Cisco/Huawei/MikroTik/Juniper/Eltex/QTECH detection profiles с подтверждёнными
  enterprise prefixes и Generic SNMP fallback.
- Ограниченный file log pipeline для rsyslog/common logs и auditd, sanitizer,
  persistent cursors и обработка rotation/truncate/recreate.
- Opt-in remote Syslog TCP/TLS Receiver: RFC5424, базовый RFC3164,
  octet-counting/LF compatibility, managed source mapping и auditd-over-syslog
  через существующий LogBatch/durable outbox. Receiver по умолчанию отключён.
- SIP OPTIONS UDP/TCP/TLS: availability, latency, failure threshold и state
  events; TLS проверяет CA/SAN/SNI, Digest authentication пока отсутствует.
- Development Simulator с deterministic scenarios/trends и `source=simulated`.
- Корпоративный NTP через стандартную службу ОС: отдельный Linux deployment
  helper для chrony/systemd-timesyncd и read-only status при запуске Collector.

## Как это работает

### Local Mode

Collector собирает данные Linux/OpenWrt-хоста, на котором запущен, и сам
устанавливает соединение с Controller. Этот standalone/fallback mode можно
совместить с remote collection. Входящий management port на хосте не нужен.

```text
Local host
    │
    ▼
InfraPulse Collector
    │
    │ outbound gRPC / mTLS
    ▼
InfraPulse Controller / Backend
```

### Collector Mode

Collector Mode опрашивает targets из `devices` без установки Collector на них.
`local.enabled: false` отключает сбор хоста; remote polling и heartbeat продолжают
работать. Local Mode и Collector Mode можно использовать одновременно.

Configured Target идентифицирует подключение/опрос и не является окончательным
Backend Asset. Источник, provenance, identity evidence и время наблюдения
нормализуются до delivery layer. Границы компонентов описаны в
[архитектуре](docs/architecture.md), модель Collector Core — в
[collector-core](docs/collector-core.md).

## Требования

- Для сборки — Go 1.27 или новее, согласно `go.mod`, и доступ к Go modules
  при первой загрузке зависимостей.
- Для локального сбора — Linux с доступными `/proc` и `/sys`.
  Текущие read-only collectors не требуют root при наличии прав чтения
  системных источников и записи в каталог состояния.
- Для передачи данных — доступный Controller с совместимым протоколом
  либо development Mock Controller.
- Для production-соединения — trusted CA и manual сертификат Agent/private key
  либо verified Agent-side enrollment с bootstrap CA и one-time token.
- `protoc` не требуется для обычной сборки: generated Go sources находятся
  в репозитории. Установка сервиса требует Linux с systemd и прав администратора.

## Быстрый старт

Команды выполняются из корня клонированного репозитория на Linux.

1. Соберите оба бинарника:

   ```sh
   mkdir -p bin
   go build -o bin/infrapulse-agent ./cmd/infrapulse-agent
   go build -o bin/infrapulse-mock-controller ./cmd/infrapulse-mock-controller
   ```

2. Запустите Mock Controller в первом терминале:

   ```sh
   ./bin/infrapulse-mock-controller
   ```

3. Во втором терминале проверьте example config и запустите Agent:

   ```sh
   ./bin/infrapulse-agent validate-config --config configs/config.example.yaml
   ./bin/infrapulse-agent run --config configs/config.example.yaml
   ```

Example config использует development-соединение на loopback. В выводе Mock
Controller появятся сведения об Agent, inventory, heartbeat и telemetry.
Оперативные метрики поступают примерно каждые 3 секунды. CPU utilization
и traffic rates отсутствуют на первом sample, пока нет предыдущего измерения.

Остановить процессы можно через `Ctrl+C`. Для проверки reconnect остановите
Mock Controller и запустите его снова: Agent восстановит соединение автоматически.

## Сборка

Обычная сборка:

```sh
mkdir -p bin
go build -o bin/infrapulse-agent ./cmd/infrapulse-agent
go build -o bin/infrapulse-mock-controller ./cmd/infrapulse-mock-controller
```

В shell-среде с Make доступен `make build`. На Windows в PowerShell:

```powershell
./scripts/build.ps1
```

Script создаёт `bin/infrapulse-agent.exe`, `bin/infrapulse-mock-controller.exe`
и отдельный `bin/infrapulse-simulator.exe`.
Development Mock Enrollment также собирается как `infrapulse-mock-enrollment`.
Если Go не находится в `PATH`, используйте параметр `-GoPath <path-to-go>`.

### Cross-compilation

Пример для Linux arm64 из POSIX shell:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/infrapulse-agent-linux-arm64 ./cmd/infrapulse-agent
```

Для набора Linux targets доступны `make cross` и `scripts/build.ps1 -Cross`.
Они собирают Agent для amd64, arm64, arm, mips и mipsle. Подбирайте CPU/float
параметры под целевое устройство; наличие target не означает совместимость
с любой OpenWrt firmware. Подробности платформ — в
[матрице поддержки](docs/vendor-support.md).

## Конфигурация

Конфигурация задаётся YAML-файлом. Неизвестные поля и некорректные значения
отклоняются. Рабочий минимальный пример для локального demo, сохраняемый
в каталоге `configs`:

```yaml
agent:
  name: collector-01
  state_dir: ../state
controller:
  address: 127.0.0.1:9443
security:
  mode: development
local:
  enabled: true
telemetry:
  fast_interval: 3s
  inventory_interval: 5m
  heartbeat_interval: 10s
commands:
  enabled: false
devices: []
```

Неуказанные параметры получают значения по умолчанию. `security.mode`
по умолчанию — `production`, поэтому development-режим нужно выбрать явно.
Относительные пути `agent.state_dir` и TLS-файлов разрешаются относительно
каталога YAML, а не текущей рабочей директории.

Основные разделы:

| Раздел | Назначение |
|---|---|
| `agent` | Имя Agent и каталог постоянного состояния |
| `controller` | Адрес Controller и сетевые timeout/reconnect параметры |
| `security` | Режим соединения, CA, сертификат, private key и имя сервера |
| `local` | Включение локального сбора, platform auto/linux/openwrt |
| `time_sync` | Opt-in corporate NTP servers для системной службы ОС; apply выполняется отдельно от monitoring |
| `telemetry` | Интервалы fast, inventory, heartbeat и timeout сбора |
| `queue` | Memory/durable, message/payload/file limits, retention, batching и ACK fallback |
| `scheduler` | Число одновременно выполняемых collectors |
| `logging` | Уровень логирования: debug, info, warn, error |
| `location` | Физическое расположение, заданное оператором |
| `commands` | Включение typed actions и protected resources |
| `devices` | Стабильные ID, адреса, SNMP/RouterOS/Redfish/IPMI driver и настройки опроса |
| `credentials` | Именованные ссылки на environment variables с credentials |
| `remote` | Concurrency, лимиты responses/rows и срок актуальности topology |
| `logs` | Configured file sources, read/line/batch limits и polling |
| `syslog_receiver` | Opt-in TLS/TCP listeners, sender mappings, trust policy и bounded admission в durable outbox |
| `telephony` | Stable SIP endpoints и bounded probe concurrency |

Готовые примеры:

- [config.example.yaml](configs/config.example.yaml) — Linux local collection.
- [config.collector.example.yaml](configs/config.collector.example.yaml) —
  development-соединение без локального сбора, в том числе на Windows.
- [config.remote.example.yaml](configs/config.remote.example.yaml) — remote
  SNMP/RouterOS collection с credentials из environment.
- [config.redfish.example.yaml](configs/config.redfish.example.yaml) — production
  read-only BMC hardware collection, custom CA и environment credential references.
- [config.p2.example.yaml](configs/config.p2.example.yaml) — OpenWrt auto mode,
  configurable logs/audit и SIP OPTIONS.
- [config.p3.example.yaml](configs/config.p3.example.yaml) — durable delivery,
  SNMPv3 authPriv и SIP TLS.
- [config.enrollment.example.yaml](configs/config.enrollment.example.yaml) —
  production Agent-side enrollment с bootstrap CA и token environment reference.
- [config.time-sync.example.yaml](configs/config.time-sync.example.yaml) —
  corporate NTP и отдельный deployment apply без локального сбора.
- [config.ipmi.example.yaml](configs/config.ipmi.example.yaml) — explicit IPMI
  LANplus target с provider credentials, USER privilege и durable delivery.

Для production замените адрес и параметры безопасности:

```yaml
controller:
  address: controller.example.net:9443
security:
  mode: production
  ca_file: /etc/infrapulse/ca.pem
  cert_file: /etc/infrapulse/agent.pem
  key_file: /etc/infrapulse/agent-key.pem
  server_name: controller.example.net
```

Manual certificates должны содержать URI SAN
`spiffe://infrapulse/agent/<persistent-Agent-UUID>`; старые сертификаты без этой
привязки необходимо перевыпустить. Альтернатива — Agent-side enrollment с
локальным key/CSR и one-time token. Renewal публикует новую certificate bundle
атомарно; неудача сохраняет действующую bundle. Подробнее —
[enrollment](docs/enrollment.md) и [безопасность](docs/security.md).

Поддерживаемые environment overrides:

| Переменная | Поле конфигурации |
|---|---|
| `INFRAPULSE_STATE_DIR` | `agent.state_dir` |
| `INFRAPULSE_CA_FILE` | `security.ca_file` |
| `INFRAPULSE_CERT_FILE` | `security.cert_file` |
| `INFRAPULSE_KEY_FILE` | `security.key_file` |

### Удалённые устройства

Назначьте каждому устройству постоянный `id`, который сохраняется при смене
IP или имени. Например, для SNMP:

```yaml
credentials:
  switch-snmp:
    community_env: INFRAPULSE_SNMP_COMMUNITY
devices:
  - id: switch-01
    name: Access switch
    address: 192.0.2.10
    driver: snmp
    credential_ref: switch-snmp
    snmp:
      port: 161
      retries: 1
    polling:
      timeout: 5s
    location:
      datacenter: dc-1
      room: network
      rack: rack-01
      rack_unit: 12
```

Задайте `INFRAPULSE_SNMP_COMMUNITY` в окружении процесса Agent. Значение
community в YAML не записывается. На устройстве разрешите read-only SNMP v2c
и доступ с адреса collector. SNMPv2c не шифрует community; используйте доверенную
management network. Пустая или отсутствующая environment variable делает
credential недоступным. `vendor_hint` — необязательная подсказка, которая сама
по себе не подтверждает производителя. `location` задаётся оператором;
`sysLocation` не превращается автоматически в datacenter/room/rack.

Для RouterOS 7 включите `www-ssl`, настройте серверный сертификат и разрешите
REST доступ выбранному пользователю. В config указывается HTTPS origin:

```yaml
credentials:
  routeros-main:
    username_env: INFRAPULSE_ROUTEROS_USERNAME
    password_env: INFRAPULSE_ROUTEROS_PASSWORD
devices:
  - id: router-01
    name: Edge router
    address: https://router.example.net
    driver: routeros
    credential_ref: routeros-main
    tls:
      ca_file: ../state/routeros-ca.pem
    polling:
      timeout: 5s
    enable_mutations: false
```

Установите обе environment variables до запуска. `tls.ca_file` задаёт доверенные
CA именно этого устройства; если поле отсутствует, используется системное
хранилище CA. Имя или IP в адресе должны соответствовать сертификату. URL
не содержит `/rest`, credentials, query или fragment. HTTP и отключение TLS
verification не поддерживаются даже при development-соединении с Controller.

Настройки `polling.fast_interval`, `medium_interval`, `inventory_interval`,
`topology_interval` и `timeout` переопределяют общие интервалы SNMP/RouterOS.
По умолчанию fast — 3s, medium — 30s, inventory/topology — 5m. Полные inventory
и neighbor tables опрашиваются отдельно от fast counters. Общий лимит
одновременных remote jobs задаётся `remote.concurrency` (default 4), лимиты
таблиц и ответов — `remote.max_rows` и `remote.max_response_bytes`.
SNMP limit применяется также к числу varbind/table cells, поэтому большая
таблица может достичь его раньше, чем число interfaces достигнет max_rows.
После первого bounded interface baseline SNMP fast polling использует Get
по известным indexes. Новые interfaces обнаруживаются на следующем inventory
refresh; исчезнувшие известные indexes удаляются при недоступном ifIndex.

Redfish и IPMI используют `medium_interval` для hardware health/metrics и
`inventory_interval` для inventory; отдельного fast/topology polling у них нет.
IPMI выбирается только явно через `driver: ipmi`, с username/password references,
host/IP в `address` и отдельным UDP port. Конфигурация и security policy —
[Redfish](docs/redfish.md), [IPMI](docs/ipmi.md).

Перед запуском можно выполнить ограниченный read-only probe:

```sh
./bin/infrapulse-agent validate-config --config configs/config.remote.example.yaml
./bin/infrapulse-agent probe-device --config configs/config.remote.example.yaml switch-01
```

Probe проверяет configured driver без подключения к Controller. Для полного
сбора telemetry/topology запустите `run`. Отсутствующие MIB/JSON fields остаются
недоступными; первый sample и сброс счётчиков не имеют traffic rates. При
неизвестном uptime или неоднозначном 32-bit wrap rates также отсутствуют.

## Запуск

### OpenWrt, logs и SIP

`local.platform: auto` (default) выбирает Native OpenWrt по `ID=openwrt` либо
`DISTRIB_ID=OpenWrt`; `linux` явно оставляет generic collector. Явный `openwrt`
также требует distribution evidence. `/bin/ubus` читает system board, netifd,
procd и UCI firewall. Без optional источника остаются Linux measurements;
`services.read`/`firewall.read` отмечают фактическую доступность. Подробнее —
[OpenWrt](docs/openwrt.md). Runtime конкретной firmware ещё не проверен.

Добавьте только реально существующие файлы:

```yaml
logs:
  sources:
    - id: system
      path: /var/log/syslog
      format: syslog
    - id: audit
      path: /var/log/audit/audit.log
      format: auditd
telephony:
  endpoints:
    - id: pbx-main
      address: 192.0.2.20
      port: 5060
      transport: udp
      timeout: 2s
      interval: 30s
      failure_threshold: 3
      latency_threshold: 250ms
```

Paths разрешаются относительно YAML; missing/permission-denied source виден
в diagnostics. Agent не требует одновременно syslog и messages. Для OpenWrt
`logread`/RAM ring buffer в file pipeline не входит. Cursor продвигается после
local admission; в durable режиме это synced disk admission, в memory — RAM
best effort. Controller ACK отдельно удаляет outbox record. Правила rotation, восстановления
corrupted state и audit correlation описаны в [logs](docs/logs.md).

SIP probe самостоятельно формирует unauthenticated OPTIONS. UDP/TCP/TLS поддержаны;
401/407 означают reachable/authentication required, а не успешный probe.
REGISTER, Digest credentials и media отсутствуют. Подробнее —
[telephony](docs/telephony.md).

### Agent

```sh
./bin/infrapulse-agent validate-config --config configs/config.example.yaml
./bin/infrapulse-agent run --config configs/config.example.yaml
```

Agent загружает конфигурацию, использует постоянный Agent ID, запускает
collectors и открывает исходящее соединение. После handshake начинается
передача данных Controller. При временном обрыве выполняется reconnect;
локальный сбор продолжается в пределах ограниченной очереди.

Для просмотра диагностики в другом терминале:

```sh
./bin/infrapulse-agent diagnostics --config configs/config.example.yaml
```

Проверяйте состояние соединения, collectors и очереди, а также время
`observed_at`: diagnostics показывает сохранённый snapshot, который может
устареть после остановки Agent. Логи Agent выводятся в stderr, данные Mock
Controller — в stdout.

## CLI

| Команда | Назначение |
|---|---|
| `run` | Запуск Agent |
| `version` | Версия и сведения о сборке |
| `validate-config` | Проверка структуры и значений конфигурации |
| `diagnostics` | Конфигурационная сводка и последний snapshot состояния |
| `probe-device` | Ограниченный read-only probe configured remote device |

Общий параметр — `--config <file>`; без него используется
`configs/config.example.yaml`. Команда `version` не требует config.
`validate-config` не проверяет доступность Controller и содержимое TLS-файлов.

```sh
./bin/infrapulse-agent version
./bin/infrapulse-agent probe-device --config configs/config.remote.example.yaml switch-01
```

Для `probe-device` параметры должны предшествовать ID устройства.

## Development Mock Controller

Mock Controller предназначен только для разработки и тестирования Agent.
Он не является production Backend и не предоставляет историческое хранилище.

```sh
./bin/infrapulse-mock-controller --address 127.0.0.1:9443 --security-mode development
```

Controller принимает AgentHello, heartbeat, telemetry, inventory, topology,
logs, telephony, events и CommandResult и выводит sanitized JSON-строками. Для проверки typed command
остановите обычный экземпляр и запустите его с параметрами:

```sh
./bin/infrapulse-mock-controller --command interface.disable --interface eth-test
```

По умолчанию команда адресуется локальному устройству подключившегося Agent;
другой ID задаётся `--target`. Поддерживаются сообщения `interface.enable`,
`interface.disable`, `service.start`, `service.stop`, `service.restart`.
Для service-команды ресурс задаётся `--service`.

Для RouterOS разрешение действий требует одновременно `commands.enabled: true`
и `enable_mutations: true` у устройства. Agent дополнительно проверяет реальные
права пользователя по `/user` и `/user/group`; если metadata недоступна,
write capability остаётся unsupported. Требуются права `read`, `write`, `api`
и `rest-api`. SNMP и локальный Linux остаются read-only; service actions
возвращают unsupported.

Management interface обязательно включите в protected list перед разрешением
команд:

```yaml
commands:
  enabled: true
  protected_interfaces: [ether1]
```

Protection проверяется по текущему `.id`, имени, default-name/MAC и связанным
bridge/bond/VLAN interfaces. При недоступных dependency tables команда
отклоняется. Защита действует на все устройства для перечисленных aliases.
Agent не определяет management path автоматически. Для разрешённой команды
Mock использует stable target ID из config:

```sh
./bin/infrapulse-mock-controller --command interface.disable --target router-01 --interface ether2
```

Результат включает `changed` и read-after-write verification. Timeout после
отправки изменения может вернуть `uncertain=true`; автоматического rollback
нет. Journal и snapshots сохраняются при restart: `reserved` без execution
boundary восстанавливается как rejected, `executing` — как uncertain без
повторной mutation. Для admission команды лимит
`queue.max_message_bytes` должен быть не меньше 4096 bytes и в очереди должно
хватать места для результата.

Result содержит `snapshot_id` для поддерживаемой interface mutation. Typed rollback:

```sh
./bin/infrapulse-mock-controller --command resource.rollback --target router-01 --snapshot <snapshot-id>
```

Agent проверяет snapshot ownership/retention/identity/version/latest generation,
текущее expected state и protected dependency closure. Stale/conflicting snapshot
отклоняется. Успех требует read-after-write verification. Это восстановление
одного interface admin state, не full device backup. [Подробности](docs/commands.md).

Для соединения с mTLS Mock поддерживает параметры `--security-mode production`,
`--ca-file`, `--cert-file`, `--key-file`. Требования к сертификатам и протоколу —
в [security](docs/security.md) и [protocol](docs/protocol.md).

## Development Simulator

```sh
go build -o bin/infrapulse-simulator ./cmd/infrapulse-simulator
./bin/infrapulse-simulator --scenario cpu-increase --duration 30s --interval 3s --seed 42 --devices 3 --output json
./bin/infrapulse-simulator --scenario all --duration 30s --interval 1s --config configs/config.simulator.example.yaml --output controller
```

Simulator — отдельный development component с обычным InfraPulse gRPC contract.
Каждый synthetic sample/event/log/inventory device помечен `source=simulated`.
Production Agent его не импортирует. Доступны CPU/RAM/traffic/error trends,
flap/offline/recovery, topology, log/audit и SIP scenarios. Подробные параметры
и limits — [Simulator](docs/simulator.md).

## Безопасность

- Collector сам инициирует gRPC-соединение; remote Syslog listener открывается
  только при `syslog_receiver.enabled: true` и не является management API.
- Production transport использует mTLS с проверкой сертификатов.
- Plaintext разрешён только в явном development-режиме на loopback.
- Arbitrary remote shell отсутствует; управляющие команды типизированы.
- Секреты не должны попадать в логи и metadata. Очистка известных секретных
  полей не гарантирует обнаружение всех возможных секретов.
- Agent напрямую не обращается к LLM.

Полная модель безопасности и её ограничения описаны в
[docs/security.md](docs/security.md).

## Установка на Linux

Инструкция предназначена для Linux с systemd. Сначала соберите бинарник
или получите сборку для архитектуры целевого хоста.

Если пользователя `infrapulse` ещё нет, создайте системного пользователя
и одноимённую группу:

```sh
sudo useradd --system --user-group --no-create-home --home-dir /var/lib/infrapulse-agent infrapulse
```

Разместите бинарник, config и каталог состояния:

```sh
sudo install -m 0755 bin/infrapulse-agent /usr/local/bin/infrapulse-agent
sudo install -d -m 0750 -o root -g infrapulse /etc/infrapulse
sudo install -m 0640 -o root -g infrapulse configs/config.example.yaml /etc/infrapulse/config.yaml
sudo install -d -m 0700 -o infrapulse -g infrapulse /var/lib/infrapulse-agent
```

В `/etc/infrapulse/config.yaml` установите
`agent.state_dir: /var/lib/infrapulse-agent`. Для production настройте адрес
Controller, `security.mode: production` и TLS-файлы, как в разделе конфигурации.
Private key должен читаться пользователем `infrapulse` и быть недоступен
посторонним пользователям.

Установите существующий [systemd unit](deployments/systemd/infrapulse-agent.service)
и запустите сервис:

```sh
sudo -u infrapulse /usr/local/bin/infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
sudo cp deployments/systemd/infrapulse-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now infrapulse-agent
sudo systemctl status infrapulse-agent
```

Unit запускает процесс от пользователя и группы `infrapulse`; каталог состояния
должен быть доступен им для записи. Для просмотра логов:

```sh
sudo journalctl -u infrapulse-agent -n 50 --no-pager
```

## Обновление

Подготовьте новый бинарник для целевой архитектуры, затем остановите сервис,
замените бинарник, проверьте конфигурацию и запустите сервис:

```sh
sudo systemctl stop infrapulse-agent
sudo install -m 0755 bin/infrapulse-agent /usr/local/bin/infrapulse-agent
sudo -u infrapulse /usr/local/bin/infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
sudo systemctl start infrapulse-agent
sudo systemctl status infrapulse-agent
```

Сохраняйте каталог состояния, чтобы Agent ID не изменился.

## Поддержка платформ и устройств

- **Linux:** поддерживается локальный read-only collector; основные targets —
  amd64 и arm64.
- **Windows:** LocalPlatform сознательно остаётся post-MVP/backlog и не является
  критерием P3. Для development-соединения используйте
  `configs/config.collector.example.yaml` с `local.enabled: false`.
- **OpenWrt:** native read-only adapter реализован и проверен schema fixtures;
  совместимость конкретной архитектуры и firmware требует runtime проверки.
- **Remote devices:** Generic SNMP v2c/v3 и RouterOS HTTPS REST реализованы;
  mappings проверяются fixtures и локальными protocol servers. Поддержка
  конкретных моделей/firmware на реальном оборудовании отдельно не подтверждена.
  LLDP передаётся как observations с evidence, а reconciliation графа выполняет
  Backend. Шесть detection profiles используют подтверждённые prefixes;
  product-specific health mappings отсутствуют. SNMPv3 расширяет security mode
  существующего adapter; mappings остаются общими. [SNMPv3 config](docs/snmpv3.md).
- **Hardware BMC:** Redfish HTTPS и explicit IPMI LANplus используют общие
  hardware contracts. Реальные BMC/vendor firmware пока не проверены;
  границы fixtures и validation — [Redfish](docs/redfish.md), [IPMI](docs/ipmi.md).

Подробная [матрица платформ, vendors и capabilities](docs/vendor-support.md).

## Разработка и проверки

Из корня репозитория:

```sh
go fmt ./...
go build ./...
go vet ./...
go test ./...
go test -race ./...
go mod verify
```

`go fmt ./...` применяет gofmt к packages проекта. Для race detector необходимы
поддерживаемый C compiler и включённый CGO. Обычные tests не требуют root
или реального сетевого оборудования.

Отдельный integration test локального collector запускается на Linux:

```sh
go test -tags=integration ./internal/platform/linux
go test -tags=integration ./internal/agent -run TestLiveLinuxLogsTelemetrySIPAndRotation
go test -tags=integration ./internal/queue ./internal/commands
```

Доступны также `make check` и `make race`. Конкретные результаты проведённых
проверок приведены в [docs/validation.md](docs/validation.md).

## Генерация protobuf

Обычная сборка использует готовые Go sources из `api/gen`; regeneration
нужна при изменении protobuf-контракта.

В PowerShell первоначальная подготовка инструментов и генерация:

```powershell
./scripts/generate-proto.ps1 -Bootstrap
```

Для последующих запусков используйте `./scripts/generate-proto.ps1`.
На Linux с установленным protoc нужной версии:

```sh
sh scripts/generate-proto.sh
```

Версии инструментов и порядок генерации описаны в
[api/proto/README.md](api/proto/README.md).

## Документация

- [Архитектура](docs/architecture.md) — компоненты, модели и внутренние pipelines.
- [IPMI fallback](docs/ipmi.md) — LANplus, cipher policy, FRU/SDR и hardware
  mapping, ограничения legacy BMC и результаты проверки Phase 5.
- [Phase 5 report](docs/phase-5-report.md) — baseline, protocol/security fixtures,
  shared hardware pipeline, resource measurements и фактические ограничения.
- [Collector Core](docs/collector-core.md) — Target/Source, identity evidence,
  temporal semantics и границы совместимости Phase 1.
- [Корпоративный NTP](docs/time-sync.md) — конфигурация chrony/timesyncd,
  права, idempotency, status и ручной OpenWrt setup.
- [Remote Syslog/Audit Receiver](docs/syslog-receiver.md) — подключение Linux
  серверов через rsyslog/audisp-syslog без InfraPulse Agent на каждом сервере,
  TLS, source identity, limits и delivery boundaries.
- [Deployment](docs/deployment.md) — persistent state, listener/firewall и
  ограничения Linux/OpenWrt/container установки.
- [Протокол](docs/protocol.md) — обмен Agent ↔ Controller и правила доставки.
- [Durable delivery](docs/delivery.md), [journal/snapshots/rollback](docs/commands.md),
  [enrollment/PKI](docs/enrollment.md), [SNMPv3](docs/snmpv3.md).
- [Безопасность](docs/security.md) — mTLS, команды, секреты и ограничения.
- [Topology discovery](docs/topology.md) — физические связи и источники evidence.
- [Матрица поддержки](docs/vendor-support.md) — платформы, vendors и capabilities.
- [Результаты проверок](docs/validation.md) — проведённые validation и cross-build.
- [OpenWrt](docs/openwrt.md), [vendor profiles](docs/vendor-profiles.md),
  [logs/audit/cursors](docs/logs.md), [telephony](docs/telephony.md),
  [Simulator](docs/simulator.md).

## Текущие ограничения

Memory mode и explicit Send fallback остаются best effort. Durable mode требует
надёжного writable storage и ACK peer; retention/eviction могут терять данные
низких приоритетов. Pending CommandResult не вытесняются. At-least-once не даёт
exactly-once внешней mutation; uncertain outcome требует operator review.
Production Backend/dedup/CA authorization/revocation/CA rollover не реализованы.
Mock Enrollment token state хранится в RAM. Private keys защищены filesystem
permissions, без encryption at rest/TPM/HSM.
Remote Syslog/Audit Receiver принимает TLS/TCP через existing durable delivery;
UDP/RELP и Backend log storage/search отсутствуют. Redfish hardware adapter
реализован; actual BMC/vendor hardware и production Backend storage ещё не
проверены. Настройка, fields и limits — [Redfish](docs/redfish.md).
IPMI Phase 5 использует общий hardware pipeline; автоматического перехода с
Redfish, IPMI v1.5, IPMB bridging, OEM conversion и power control нет.
Реальные BMC и OpenWrt RAM/flash пока не проверены; [границы](docs/ipmi.md).
Collector ↔ Backend clock offset estimation также отложен; unknown offset
не считается нулём. Phase 2 передаёт corporate servers стандартной службе ОС
через отдельный deployment helper. Collector использует системное время,
не меняет часы удалённых устройств и не запускает собственный NTP service.

Windows LocalPlatform — post-MVP/backlog. Service/firewall writes, SIP Digest,
NETCONF/gNMI и vendor-specific health OID не реализованы. OpenWrt, Astra/ALT/RED OS
runtime и реальное сетевое оборудование/PBX не проверены. [Матрица](docs/vendor-support.md).

## Production deployment

Для корреляции metrics/events/logs/topology/SIP синхронизируйте Backend host,
все Collector hosts и network/server equipment, где это возможно, с одной
корпоративной NTP hierarchy. Например:

```yaml
time_sync:
  enabled: true
  servers:
    - ntp1.company.local
    - ntp2.company.local
```

По умолчанию `time_sync.enabled: false`; публичного fallback нет. Enabled
configuration требует 1–16 IPv4/IPv6/DNS servers. Сначала администратор
выбирает и запускает единственную системную NTP-службу, затем на Linux выполняет:

```sh
sudo /usr/local/bin/infrapulse-agent apply-time-sync --config /etc/infrapulse/config.yaml
/usr/local/bin/infrapulse-agent time-sync-status --config /etc/infrapulse/config.yaml
```

Helper не устанавливает пакеты и не переключает конкурирующие NTP clients.
Обычный `run` выполняет только ограниченный status read, предупреждает о
несинхронизированных часах и продолжает monitoring. Применение системного
config не требует изменения прав существующего service user. Chrony без
подключённого managed fragment требует administrator setup; OpenWrt настройка
выполняется администратором через UCI. Ownership, rollback и ограничения —
[time-sync](docs/time-sync.md). Backend NTP остаётся deployment requirement;
Collector не меняет Backend protocol и NTP config удалённых устройств.

Native Linux/OpenWrt запускается от отдельного service user с правами чтения
нужных источников и записи private state directory. Durable mode рекомендуется
на storage с достаточным свободным местом и working fsync; atomic journal
replacement временно требует места для двух bounded files. Для constrained
OpenWrt учитывайте flash wear, RAM и SNMPv3 key-localization CPU; допустим memory
mode с явно менее надёжной telemetry delivery.

Collector-only hosting (`local.enabled: false`) собирает SNMP/RouterOS/Redfish/SIP и
remote topology; opt-in receiver также принимает remote logs/audit. Его
настройка и rsyslog sender пример — [syslog-receiver](docs/syslog-receiver.md).
В контейнере нужен persistent state volume и CA trust. Host
monitoring/logs потребовали бы явных mounts и прав; обычный container namespace
не является full host evidence. Production container image/runtime deployment
не проверены; Docker не является обязательным способом установки.
