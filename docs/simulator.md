# Development Simulator

`infrapulse-simulator` — отдельный development binary. Он генерирует
deterministic normalized envelopes с явным `source=simulated`; production
Agent и реальные Linux/OpenWrt/SNMP/RouterOS collectors его не используют.
Все simulated devices имеют `driver=simulated` и отдельные ID. Protobuf device
type остаётся обычным `switch`: synthetic origin передаётся отдельным source.

```sh
go build -o bin/infrapulse-simulator ./cmd/infrapulse-simulator

# JSON lines в stdout, без подключения к Controller.
./bin/infrapulse-simulator --scenario cpu-increase --duration 10s \
  --interval 1s --seed 42 --devices 4 --output json

# В другом терминале: ./bin/infrapulse-mock-controller
./bin/infrapulse-simulator --config configs/config.simulator.example.yaml \
  --scenario all --duration 10s --interval 1s --seed 42 --devices 4 \
  --output controller
```

CLI задаёт `--scenario`, `--duration`, `--interval`, `--seed`, `--devices`,
`--output json|controller`, `--config` и optional `--start` RFC3339.
Defaults: healthy, 30s, 1s, seed 42, четыре devices, JSON output. Duration
ограничена 24h, interval — 10ms..1h, device count — 1..64. Один log batch
содержит не более 129 records. Генерация идёт с заданным runtime interval,
а не бесконтрольно заполняет stdout/Controller.

| Scenario | Наблюдаемый результат |
|---|---|
| healthy | Нормальная infrastructure, interfaces, topology, logs и SIP |
| cpu-increase | CPU 20 → 30 → 45 → 60 → 80 → 95%, затем plateau |
| memory-pressure | RAM used 30 → 40 → 55 → 70 → 85 → 95% и memory pressure |
| traffic-increase | Interface traffic 100k → 200k → 400k → 800k → 1.6m → 3.2m bytes/s |
| interface-errors | RX errors 0 → 3 → 12 → 50 → 200, затем plateau |
| interface-flap | Interface down/up на steps 2/3 и 4/5 |
| device-recovery | Первый device offline на steps 2..4, recovery на step 5 |
| topology-change | Links появляются на step 2 и исчезают на step 5 |
| log-burst | До 128 system log records на steps 2..4 |
| audit-activity | Явно simulated denied SYSCALL audit records на steps 2..4 |
| sip-latency | SIP latency 20 → 50 → 120 → 400ms, затем timeout |
| sip-recovery | SIP timeout на steps 2..4, recovery на step 5 |
| all | Сочетание сценариев; topology links на steps 5..7 после device recovery; SIP degradation/outage на steps 3..6 и recovery на step 7 |

Step — один interval с первым sample на step 0. Чтобы увидеть полный composite
scenario со всеми recovery/disappearance transitions, задайте duration не менее
девяти intervals. На каждом step отправляются
inventory, online telemetry, topology, logs и telephony health. Transition events
появляются при фактической смене deterministic scenario state. Audit records
содержат нормализованные факты; Simulator не выполняет AI/security inference.

Фиксированные scenario/seed/device count/interval/start дают одинаковые
normalized payloads. Default logical start — `2026-01-01T00:00:00Z`; для demo
текущей временной шкалы задайте `--start`. Session UUID, message UUID и SentAt
относятся к реальному transport lifecycle и могут отличаться между запусками.

Base YAML использует существующий strict config loader для controller/security
и RAM queue limits. Identity всегда хранится в дочернем `simulated` state
directory, даже если передана конфигурация обычного Agent. Это исключает
совместное использование production Agent ID. Local/remote collectors из
base config не запускаются. Example включает development plaintext только на
loopback; при production transport действуют mTLS правила и URI SAN binding
к отдельному persistent Simulator UUID. Manual certificate настоящего Agent
с другой identity не подходит; Simulator не запускает Agent enrollment/renewal
manager.

Controller output использует тот же protobuf/gRPC protocol-v1 stream,
AgentHello/HelloAck и increasing sequence. Runner переиспользует RAM queue и
bounded exponential backoff, закрывает session при cancellation, имеет одного
sender и receiver. Command responses всегда unsupported: Simulator не выполняет
device mutations. Production Agent не зависит от Simulator.

Delivery best effort: queue eviction, Pop/Send error или process crash могут
потерять данные. После duration Runner пытается отправить остаток RAM queue в
течение ограниченного drain timeout; успешный Send не является application ACK.
Для gRPC sender завершает поток через CloseSend и ждёт peer EOF в пределах этого
timeout, чтобы immediate cancellation не отбросила buffered transport frames.
Это завершение stream, а не подтверждение durable application delivery.
Отсутствующий Controller приводит к понятной ошибке завершения drain, а не
подтверждению доставки. Durable delivery и exactly-once не реализованы.

Unit tests проверяют deterministic payloads, trends, limits и simulated markers
после wire roundtrip для каждого scenario. Local gRPC E2E проверяет настоящий
Simulator → Mock Controller stream, включая logs/audit/telephony. Reconnect и
shutdown проверены дополнительными transport fixtures. Эти проверки относятся
к development Simulator, а не к hardware/network device support.
