# InfraPulse Agent

InfraPulse Agent — Go-компонент многовендорной NetOps Platform InfraPulse для
сбора данных инфраструктуры и передачи их в Controller/Backend. В текущей версии
Agent собирает данные локального Linux-хоста, передаёт telemetry, inventory
и events и принимает только типизированные управляющие команды.

Архитектура предусматривает работу collector для удалённых сетевых устройств;
удалённые integrations пока не поддерживаются. Production Backend, Web UI,
AI и историческое хранилище находятся вне этого репозитория. Для разработки
и демонстрации обмена данными поставляется Mock Controller.

## Возможности

- Локальная системная информация Linux: hostname, ОС, kernel, architecture,
  uptime и доступные сведения об оборудовании.
- CPU utilization и load average; RAM, swap и memory PSI при наличии источника.
- Сетевые интерфейсы, IP-адреса, счётчики traffic, packets, errors и drops;
  расчёт traffic rates между измерениями.
- Постоянный Agent ID и отдельный Session ID для каждого запуска.
- Передача inventory, heartbeat и событий Agent.
- Настраиваемый интервал оперативной telemetry, по умолчанию 3 секунды.
- Исходящее bidirectional gRPC-соединение, production mTLS и reconnect.
- Ограниченная очередь отправки и диагностика состояния Agent.
- Типизированный протокол команд с validation и policy checks. Изменяющие
  действия сейчас возвращают `rejected` или `unsupported` и не исполняются.
- Development Mock Controller для наблюдения данных и проверки команд.

## Как это работает

### Local Mode

Agent собирает данные системы, на которой запущен, и сам устанавливает
соединение с Controller. Входящий management port на хосте Agent не нужен.

```text
Local host
    │
    ▼
InfraPulse Agent
    │
    │ outbound gRPC / mTLS
    ▼
InfraPulse Controller / Backend
```

### Collector Mode

Collector Mode предназначен для опроса удалённых устройств через сетевые
протоколы без установки Agent на каждое устройство. В текущем коде SNMP/REST
drivers отсутствуют. Режим `local.enabled: false` позволяет запустить обмен
с Controller и heartbeat без локального сбора; он не включает remote polling.

Границы компонентов и внутренние механизмы описаны в
[архитектуре](docs/architecture.md).

## Требования

- Для сборки — Go 1.27 или новее, согласно `go.mod`, и доступ к Go modules
  при первой загрузке зависимостей.
- Для локального сбора — Linux с доступными `/proc` и `/sys`.
  Текущие read-only collectors не требуют root при наличии прав чтения
  системных источников и записи в каталог состояния.
- Для передачи данных — доступный Controller с совместимым протоколом
  либо development Mock Controller.
- Для production-соединения — CA, сертификат Agent и private key.
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

Script создаёт `bin/infrapulse-agent.exe` и `bin/infrapulse-mock-controller.exe`.
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
| `local` | Включение локального сбора |
| `telemetry` | Интервалы fast, inventory, heartbeat и timeout сбора |
| `queue` | Лимиты количества и размера сообщений |
| `scheduler` | Число одновременно выполняемых collectors |
| `logging` | Уровень логирования: debug, info, warn, error |
| `location` | Физическое расположение, заданное оператором |
| `commands` | Policy и protected resources; исполнители действий отсутствуют |
| `devices` | Настройки remote devices; сейчас допустим запуск только с `[]` |

Готовые примеры:

- [config.example.yaml](configs/config.example.yaml) — Linux local collection.
- [config.collector.example.yaml](configs/config.collector.example.yaml) —
  development-соединение без локального сбора, в том числе на Windows.

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

Сертификаты подготавливаются отдельно; файлы должны быть доступны пользователю
Agent. Подробнее — [безопасность](docs/security.md).

Поддерживаемые environment overrides:

| Переменная | Поле конфигурации |
|---|---|
| `INFRAPULSE_STATE_DIR` | `agent.state_dir` |
| `INFRAPULSE_CA_FILE` | `security.ca_file` |
| `INFRAPULSE_CERT_FILE` | `security.cert_file` |
| `INFRAPULSE_KEY_FILE` | `security.key_file` |

## Запуск

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
| `probe-device` | Команда предусмотрена, но сейчас возвращает unsupported |

Общий параметр — `--config <file>`; без него используется
`configs/config.example.yaml`. Команда `version` не требует config.
`validate-config` не проверяет доступность Controller и содержимое TLS-файлов.

```sh
./bin/infrapulse-agent version
./bin/infrapulse-agent probe-device --config configs/config.example.yaml router-01
```

Для `probe-device` параметры должны предшествовать ID устройства.

## Development Mock Controller

Mock Controller предназначен только для разработки и тестирования Agent.
Он не является production Backend и не предоставляет историческое хранилище.

```sh
./bin/infrapulse-mock-controller --address 127.0.0.1:9443 --security-mode development
```

Controller принимает AgentHello, heartbeat, telemetry, inventory, events
и CommandResult и выводит их JSON-строками. Для проверки typed command
остановите обычный экземпляр и запустите его с параметрами:

```sh
./bin/infrapulse-mock-controller --command interface.disable --interface eth-test
```

По умолчанию команда адресуется локальному устройству подключившегося Agent;
другой ID задаётся `--target`. Поддерживаются сообщения `interface.enable`,
`interface.disable`, `service.start`, `service.stop`, `service.restart`.
Для service-команды ресурс задаётся `--service`.

В текущей версии Agent не исполняет эти действия: с example config возвращается
`rejected` по policy, разрешённая policy не включает отсутствующий исполнитель.
Результат содержит `changed=false`.

Для соединения с mTLS Mock поддерживает параметры `--security-mode production`,
`--ca-file`, `--cert-file`, `--key-file`. Требования к сертификатам и протоколу —
в [security](docs/security.md) и [protocol](docs/protocol.md).

## Безопасность

- Agent сам инициирует исходящее соединение; inbound management listener отсутствует.
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
- **Windows:** локальный сбор отсутствует. Для development-соединения используйте
  `configs/config.collector.example.yaml` с `local.enabled: false`.
- **OpenWrt:** native integrations отсутствуют; совместимость конкретной
  архитектуры и firmware требует отдельной проверки.
- **Remote devices:** SNMP, RouterOS и другие remote integrations пока
  не поддерживаются. Физическая topology сейчас не собирается.

Подробная [матрица платформ, vendors и capabilities](docs/vendor-support.md).

## Разработка и проверки

Из корня репозитория:

```sh
go fmt ./...
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

`go fmt ./...` применяет gofmt к packages проекта. Для race detector необходимы
поддерживаемый C compiler и включённый CGO. Обычные tests не требуют root
или реального сетевого оборудования.

Отдельный integration test локального collector запускается на Linux:

```sh
go test -tags=integration ./internal/platform/linux
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
- [Протокол](docs/protocol.md) — обмен Agent ↔ Controller и правила доставки.
- [Безопасность](docs/security.md) — mTLS, команды, секреты и ограничения.
- [Topology discovery](docs/topology.md) — физические связи и источники evidence.
- [Матрица поддержки](docs/vendor-support.md) — платформы, vendors и capabilities.
- [Результаты проверок](docs/validation.md) — проведённые validation и cross-build.

## Текущие ограничения

Очередь хранится только в RAM: restart теряет её содержимое, а разрыв соединения
может потерять отправляемое сообщение. Она не заменяет durable storage.
Удалённые integrations, Windows local collection и исполнение управляющих
действий отсутствуют. Enrollment и автоматический выпуск сертификатов
не поддерживаются; сертификаты устанавливаются отдельно.
