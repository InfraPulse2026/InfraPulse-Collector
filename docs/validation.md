# Проверка P0

Дата: 2026-10-04. Проверенный SDK: `go1.27.1 windows/amd64`. Проверка runtime выполнена отдельно в WSL Linux amd64, kernel `6.18.33.2-microsoft-standard-WSL2`. Приведённые результаты относятся к реализации P0; последующие изменения README, документации и Git-исключений не изменяли Go-код, protobuf, зависимости или runtime-конфигурацию. Полный набор Go/runtime проверок после этих документационных изменений повторно не запускался.

## Реальный Linux end-to-end

Agent и Mock Controller собраны для `linux/amd64` с `CGO_ENABLED=0` и запущены на loopback `127.0.0.1:19443`. Использовались explicit development mode, local collection, fast interval 3s, heartbeat 2s и inventory 30s. Telemetry не генерировалась Simulator.

| Проверка | Результат |
|---|---|
| AgentHello | Реальный Linux hostname/kernel/architecture/boot identity; driver `local-linux` |
| Fast telemetry | 8 samples, UTC observedAt от 09:50:29.651Z до 09:50:50.650Z; интервалы приблизительно 3s |
| CPU | Первый utilization отсутствует; затем 7 delta samples; 12 logical CPUs |
| RAM | Реальный total 16,647,843,840 bytes; available/used/free/swap получены из procfs |
| Interfaces | 2 реальных interfaces; counters/sysfs metadata/IP addresses |
| Traffic rates | 7 samples с derived rates после первого baseline |
| Heartbeat / inventory | 11 heartbeat, 1 inventory update |
| Typed command | Mock отправил `interface.disable` для `eth-test`; два `CommandResult`: `rejected`, `policy_denied`, `changed=false` |
| Outage / reconnect | Mock остановлен SIGTERM, Agent продолжил работу; 5 retries с jitter/backoff, затем восстановил connection после старта Mock |
| Identity | Agent ID и session ID совпали до и после reconnect в рамках одного запуска |
| Shutdown | Agent и оба экземпляра Mock завершились после SIGTERM с exit code 0 |

Retry delays этой проверки: примерно 197ms, 217ms, 698ms, 812ms, 1943ms при configured initial=200ms и max=2s. Параллельный scheduler продолжал собирать samples при недоступном controller. Проверка не доказывает durable delivery: P0 outbox остаётся volatile/best effort.

Временные артефакты проверки не включены в репозиторий.

## Linux collector tests и cross-build

Обычные tests используют isolated procfs/sysfs fixtures и не требуют Linux, root или сетевого оборудования. Проверены CPU guest accounting/delta/reset/reboot, memory available/fallback/overflow, interface mapping, missing optional metadata, malformed sources, resource limits, cancellation и concurrent collection. `gofmt`, package tests и `go vet ./internal/platform/...` прошли.

Integration binary собран командой `go test -c -tags=integration -o .cache/linux-platform-tests ./internal/platform/linux` для Linux amd64 и выполнен в WSL. `TestRealLinuxCollector` и все fixture tests прошли на реальных `/proc`, `/sys` и interface addresses. Integration test не требует root:

```sh
go test -tags=integration ./internal/platform/linux
```

Финальная сборка через `scripts/build.ps1 -Cross` с выбранным Go SDK успешно собрала полный Agent с CGO disabled для пяти architectures. Отдельно успешно собраны полные Mock Controller binaries для amd64 и arm64. Это builds всех runtime dependencies, а не только platform packages. Бинарные артефакты не включены в репозиторий:

| Binary | Target | Размер, bytes |
|---|---|---:|
| infrapulse-agent-linux-amd64 | linux/amd64 | 19,048,010 |
| infrapulse-agent-linux-arm64 | linux/arm64 | 17,846,356 |
| infrapulse-agent-linux-arm | linux/arm | 18,035,999 |
| infrapulse-agent-linux-mips | linux/mips | 19,684,136 |
| infrapulse-agent-linux-mipsle | linux/mipsle | 19,684,092 |
| infrapulse-mock-controller-linux-amd64 | linux/amd64 | 18,255,279 |
| infrapulse-mock-controller-linux-arm64 | linux/arm64 | 17,120,913 |

Размеры относятся к обычной сборке без stripping; они могут изменяться после правок. `go version -m` подтверждает Go 1.27.1 и CGO_ENABLED=0. Проверенный ARM build использует GOARM=7, MIPS builds — GOMIPS=hardfloat. Для конкретного устройства требуется подобрать CPU/float target; compiled hardfloat binary нельзя автоматически считать подходящим для любого OpenWrt маршрутизатора.

Agent/Mock runtime этой проверки — Linux amd64. Cross-build дополнительных architectures подтверждает компиляцию, но не поддержку конкретной OpenWrt firmware, наличие CA store, доступность native interfaces или приемлемое RAM/flash потребление. Native OpenWrt adapter остаётся P2. После переработки README повторно проверены все его относительные ссылки: указанные файлы существуют.

## Race detector на Windows

Первоначальный SDK запускал tests с CGO disabled. Для race probe получен portable [официальный LLVM MinGW release 20260922](https://github.com/mstorsjo/llvm-mingw/releases/tag/20260922), asset `llvm-mingw-20260922-ucrt-x86_64.zip`, LLVM 23.1.2. Архив расположен только в ignored `.tools`; системная установка и постоянное изменение PATH не выполнялись. Published GitHub SHA256 проверен:

```text
e3ad77d117a4bea19a7a3b333341824d79a5a371004a10e25b8504e7b3047666
```

Compiler возвращает полный путь `libsynchronization.a`, соответствуя [требованиям Go race detector на Windows](https://go.dev/doc/articles/race_detector). Первоначальный platform probe и финальный **`go test -race ./...` прошли** с exit code 0, включая Agent end-to-end и transport mTLS tests. Проверка выполнена для Windows amd64 с `CGO_ENABLED=1` и `CC`, указывающим на подготовленный C compiler.

Race detector проверяет только выполненные paths. Linux runtime integration и Windows race tests дополняют друг друга; один запуск не доказывает отсутствие любых будущих races или корректность ещё не реализованных drivers.

## Финальные quality gates

После завершения всех P0 изменений успешно выполнены:

- `gofmt -l` по всем Go sources: список пуст;
- `go build ./...`, `go vet ./...`, `go test ./...`;
- `go test -race ./...` с portable compiler;
- `go mod verify`: all modules verified;
- CLI `version`, `validate-config` для обоих example YAML и безопасный `diagnostics`;
- повторная protobuf generation: SHA256 обоих generated sources не изменился.

Обычная suite проверяет config/defaults/strict YAML/resource limits, identity persistence/corruption/concurrent creation, scheduler timeout/non-overlap/error isolation, bounded queue/priority/results retention, counter reset/rates, typed validation/capability/policy/protection и sanitizer. Transport tests проверяют protobuf presence/roundtrips/malformed inputs, TCP bidi, cancellation и mTLS positive/negative cases (CA/SAN/client certificates).

Agent tests используют локальный TCP Mock и test-only fixtures с source=simulated: hello/inventory/telemetry/rates/heartbeat/typed rejection, Controller restart с reconnect, offline queue bounds, slow-reader backpressure и graceful shutdown. Они не требуют real network equipment, root или внешнего Controller.

## Что не проверялось этим demo

Remote SNMP/RouterOS, шесть vendor profiles, LLDP/topology, native OpenWrt, logs/auditd, services/firewall actions, SIP/PBX statistics и Simulator не реализованы в P0. Поэтому эта проверка подтверждает foundation: реальные Linux measurements, outbound stream, typed rejection, intervals, reconnect и shutdown. Она не является подтверждением полного hackathon E2E с remote equipment.
