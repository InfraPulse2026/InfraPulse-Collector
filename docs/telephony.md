# SIP OPTIONS health

Agent отправляет фиксированный unauthenticated SIP `OPTIONS` probe по UDP или
TCP. Request формируется внутри Agent, а config задаёт только typed endpoint
settings. SIP credentials, REGISTER, calls, RTP, media, TLS и PBX statistics
не реализованы. Обычные tests используют локальный protocol fixture server.

```yaml
telephony:
  concurrency: 2
  endpoints:
    - id: office-pbx
      address: 192.0.2.20
      port: 5060
      transport: udp
      timeout: 2s
      interval: 30s
      failure_threshold: 3
      latency_threshold: 250ms
      from_user: infrapulse
      # to_user: health
      # domain: pbx.example.net
```

Defaults: UDP, port 5060, timeout 2s, interval 30s, failure threshold 3,
latency threshold 250ms. Endpoint ID стабилен и уникален в config. Domain
меняет SIP URI, но сетевой destination задаётся address/port. Поддерживается
обычный DNS lookup hostname; SIP SRV discovery и redirect follow отсутствуют.
Ни credentials, ни request/response bodies не выводятся в diagnostics.

Normalized health содержит target ID, network address, transport, observedAt,
reachable, optional response code/latency, consecutiveFailures, state и error
category. Latency включает resolve/connect и ожидание matching final response.
`reachable=true` означает проверенный final SIP response с теми же Call-ID,
CSeq/OPTIONS и Via branch. Случайный UDP packet не является доказательством.

| Результат probe | Reachable | State / error category |
|---|---|---|
| 2xx в пределах latency threshold | true | up |
| 2xx выше latency threshold | true | degraded / high_latency |
| 401/407 | true | degraded / authentication_required |
| 3xx | true | degraded / redirect |
| Остальные 4xx, включая 404 | true | degraded / request_rejected |
| 5xx | true | degraded / server_error |
| 6xx | true | degraded / global_failure |
| DNS, connect, timeout, malformed SIP | false | degraded, затем down после threshold; отдельная error category |

Consecutive failures считает неуспешные OPTIONS: отсутствие корректного final
response и non-2xx. Любой 2xx сбрасывает этот счётчик; высокая latency отдельно
сохраняет degraded. Счётчик и state хранятся в ограниченной RAM state endpoint,
при restart начинаются заново. 401/407 не превращаются в device offline.
Cancellation scheduler/shutdown сохраняет предыдущий state и не увеличивает
failures. События telephony.up/down/degraded появляются только при изменении
уже наблюдённого состояния; первый sample не создаёт transition event.

UDP использует ограниченные Timer E retransmissions по
[RFC 3261](https://www.rfc-editor.org/rfc/rfc3261.html): 500ms с удвоением до 4s,
в пределах timeout. TCP использует Content-Length framing. Лимит response —
32 KiB, максимум 64 UDP reads / 8 TCP messages на probe; сети обслуживаются
с context deadline и закрываются при cancellation. Все config URI components
проверяются на допустимые символы; произвольные headers/payloads недоступны.

Probe подтверждает только response выбранного SIP endpoint. Это не проверка
качества звонков, авторизации, регистрации, RTP или состояния всех PBX services.
Hardware/PBX runtime validation отсутствует; UDP/TCP fixture verification
включает 200/401/407/404/5xx, latency, timeout, malformed frames, cancellation,
retransmission, transitions и recovery.
