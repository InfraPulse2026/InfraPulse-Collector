# Phase 8 — final hardening, deployment и validation

Дата: 2026-10-11. Phase 8A: **COMPLETE** — automated code hardening/validation.
Phase 8B: **PARTIAL**. Linux amd64 runtime в WSL проверен; настоящие сетевые
устройства, BMC, OpenWrt и production Backend в этой среде недоступны.
Fixture tests не заменяют hardware validation.

## 1. Git state и base commit

До изменений выполнены `git status`, `git branch --show-current`,
`git branch -vv`, `git log -5 --oneline`. Фактическая база — чистый
`main` с HEAD `727c6e5 feat(agent): реализована управляемая конфигурация Collector в Phase 7`.
Tracking ref origin/main указывала на тот же commit; состояние remote не выдаётся
за проверенную публикацию. `61f4783` — parent Phase 6, а не фактический Phase 7 HEAD.

Создана отдельная ветка `feat/phase-8-hardening` от main. Изменения Phase 8
оставлены unstaged. Git add/commit/push/merge/rebase/reset/clean/stash/tag
не выполнялись. Production Backend source не изменён.

## 2. Phase 7 readiness

Все изменения Phase 7 уже сохранены в `727c6e5`; историческое замечание
«commit не выполнен» в предыдущем отчёте устарело. Typed snapshots,
managed_config_v1, authenticated mTLS, UUID/process Session ID binding, local
opt-in/policy, revision/digest, five-protocol lifecycle, LKG/status и durable
compatibility сохранены. Новая архитектура, scheduler, queue или protocol stack
не вводились.

## 3. Baseline до исправлений

Среда: Windows amd64, PowerShell, Go 1.27.1 из
`C:\Users\kiril\sdk\go1.27.1\bin\go.exe`, Intel i5-12400F. GOPATH,
GOMODCACHE и GOCACHE находятся в `.cache`. Normal/production build — CGO=0.
Race — CGO=1, portable LLVM-MinGW из `.tools/llvm-mingw-20260922-ucrt-x86_64`.
Linux binaries crosscompiled на Windows и исполнены в WSL docker-desktop
(workspace `/mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent`).
295 Go source hashes не изменились в ходе baseline.

| Exact command / check | Status | Duration / observed result |
|---|---|---|
| `gofmt -l <295 Go files>` | FAIL raw / PASS LF-normalized | CRLF checkout отличался от gofmt; слишком большой argv вызвал PowerShell StandardOutputEncoding error; batches of 30 LF copies дали unformatted=0 без изменения sources |
| `go build ./...` | PASS | 3.192s |
| `go vet ./...` | PASS | 1.325s |
| `go test -count=1 ./...` | PASS | 22.893s |
| `go test -race -count=1 ./...` | PASS | 51.278s |
| `go mod verify` | PASS | 0.446s |
| `go test -count=1 -p=1 -tags=integration ./...` | FAIL | 96.612s: queue crash/ACK assertions passed, затем TempDir cleanup ERROR_DIR_NOT_EMPTY; исправление test harness и negative locked-handle regression описаны ниже |
| `./scripts/build.ps1 -GoPath <SDK>` / `-Cross` | PASS | 5.324 / 18.904s |
| Pinned protoc scratch generation / SHA256 | FAIL raw / PASS exact LF equality | agent.pb.go CRLF; agent_grpc.pb.go raw equal; generated content и wire contract совпадают |
| `bin/infrapulse-agent.exe validate-config --config <each configs/*.yaml>` | PASS | 12 examples, 0.690s |
| `go test -c -tags=integration ./internal/<package>`; Linux binary `-test.v=true -test.count=1 -test.timeout=120s` | PASS | 23 Linux suites |
| `go test -count=1 -run '^TestPhase6AdaptersIdentityTopologyDurableReplayACK$' -v ./internal/agent` | PASS | 2.244s, real fixture networking/replay/ACK |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' ./cmd/infrapulse-agent` | PASS | 26,366,112 bytes; unstripped 38,809,254 |

Baseline resource fixture `TestResourceUsageBoundedIdentityTopologyAdmission`:
100 × 64 neighbors, 893.26ms, TotalAlloc 339,271,760 bytes; peak 5 messages /
951,005 bytes, JSON envelope 190,085 bytes, 95 intentional drops, goroutine delta=0.
Это нагрузка admission, не isolated daemon idle RSS. Полный сравнительный CPU/RSS
baseline Phase 7 отсутствует; улучшение этих показателей не заявляется.
Baseline raw logs, source hashes и результаты сохранены в `.cache/phase8-baseline`.
## 4. Карта аудита архитектуры

| Component | Responsibility → dependencies | Limits | Failure modes / tests / gaps |
|---|---|---|---|
| Agent/Core/Targets | identity/session, composition, lifecycle, normalized admission → config/platform/registry/scheduler/queue/transport | bounded workers; send/connect/ACK/apply deadlines; command/config pending1; recenterrors20×1024 | startup storage/policy failure, cancel, reconnect, queuepressure; full fixture E2E; same state never shared by simultaneous processes |
| Domain/time/identity/topology | source-scoped facts, independent clocks, raw IDs/subtypes/partial semantics → model | bounded facts/strings/rows; immutable clones | malformed timestamps/identifiers rejected; source/partial regression; global Asset resolution external |
| Remote registry/drivers | optional readers, one adapter/target, caches/capabilities/exclusion → protocol adapters/rate tracker | workers4default; per-target gate; row/batch/poll bounds; hardwarecooldown | offline threshold, timeout/partial/policy/removedpoll; lifecycle/adapters fixtures; realfirmware unavailable |
| Scheduler/Change | periodic work and paused/drained transitions → contexts/bounded channels | worker/work budget; one pending flag/job; changedcap1 | P1 startup starvation fixed;10/50/100 fairness/slowBMC/removal/race tests |
| Managed config | full set, policy/auth/session/digest/revision/LKG → atomic store/registry/provider | 1024targets model + local MaxConfigBytes/interval/CIDR/reference/apply limits | rapid/stale/conflict/canceled/drain/disk faults; truthful status; BackendB9 external |
| Credentials/sanitizer | named local env refs, known secret/literal redaction | finite configured map; bounded diagnostics | missing/rotated creds, echoedpassword/malformedURI; sanitizer is not exhaustive secret discovery |
| Durable outbox | synced bbolt admission/ACK/index/checksum/stableIDs/leases → bbolt1.5.0 | messages/payload/file/inflight/retention/batch bounds | offline/ACKloss/commiterror/corruption/pressure/crash/restart; physicalpowerloss external |
| Transport/PKI | one sender/receiver, verified mTLS/UUID, CSR/enrollment/rotation → grpc/protobuf/TLS | maxwire4MiB; boundedPEM/HTTP; timeout/backoff | halfopen/blockedSend/CA/SAN/expiry/rotation/replay; CRL/OCSP/revocation external gap |
| Commands/journal/rollback | typed validation/policy/protection/canonicalresolve/verification → durable reservation/journal | bounded resultstate/dedup; defaultdeny; fixedexecutors | crash/uncertainmutations/storagegate/protectedaliases; no realequipment mutations here |
| Local Linux/OpenWrt/NTP | proc/sys identity/counters; typed ubus; OS status → platform | boundedcommandoutput/timeouts; knownfixedoperations | unavailable optional evidence, parse/rotation/cancellation; liveprocd/NTPsync notavailable |
| File logs/Syslog | bounded filecursors + TCP/TLS ingress/parser/sourceattribution → existing LogBatch/outbox | frames2..64KiB; connections<=256; read/idle/admission deadlines | malformed/slow/oversize/queuepressure/TLS/source; senderhop has no applicationACK |
| Redfish/IPMI/SNMP/RouterOS/SIP | existing readonly adapters, protocol authentication/normalization | fixedoperations, response/traversal/session/retry/crypto bounds | security/timeout/fuzz/networkfixtures; pinneddeps unchanged; hardware outside8A |
| Simulator/Mock | synthetic scenarios/development peer + real wire | bounded queue/concurrency | simulated metadata/dedup/replay/digest compatibility; no productionBackend/database |
| Deployment/build/docs | existing matrix + package/checksum/leastprivilege/serviceguides | CGO0, no runtime services/dependencies added | releaseallowlist/sourcehashes/installmodes; systemd/procd live notavailable |

Read-only audits across all listed components сохранены до production edits.
Core по-прежнему не содержит protocol specifics или protobuf mapping.

## 5. Security findings

Подтверждён P1: FIFO/special files вместо TLS trust material могли блокировать
startup/reconnect до срабатывания сетевых timeout. Readers Syslog, SIP,
enrollment PKI, manual certificate identity и static gRPC проверяют regular-file
type/size перед Open, повторно проверяют handle и ограничивают чтение.
Administrator symlink → regular file сохранён. TLS verification, CA/SAN,
client authentication и UUID binding не ослаблены.

До fix actual FIFO subprocesses требовали external 2s watchdog (FAIL); после
fix самостоятельно возвращают bounded error (PASS). Positive regular symlink,
oversized file и wrong UUID tests прошли. Stat/Open не считается atomic защитой
от hostile local directory owner; administrator должен защищать trust paths.
Физическое зависание filesystem не проверено.

Review нового release script выявил recursive copy private configs/keys,
неверный Windows path separator и затем CRLF SHA256SUMS, непригодный для Linux
BusyBox. Исправлены allowlist, slash normalization и LF checksum output.
Исходный archive verify FAIL сохранён; пересобранные пакеты проверяются ниже.

## 6. Concurrency findings

Подтверждён P1 scheduler starvation: при одновременном startup 10/50/100 jobs,
workers=4, interval=1h, GOMAXPROCS=1 прежний dispatcher выполнял только 4 jobs
за 1s и переносил остальные на следующий полный interval (FAIL).

Минимальный fix: один coalesced pending flag на существующий bounded job,
fair cursor и сигнал completion через существующий channel(capacity=1).
Worker/work budgets не увеличены. После fix все targets обслужены, 50 saturated
jobs проходят по два раза с workers≤2; удаление pending jobs, cancellation и
hung BMC isolation PASS. Go не может принудительно остановить collector,
игнорирующий context; production adapters должны его соблюдать.

## 7. Durable outbox

Successful admission и application ACK возвращаются только после synced bbolt
transaction. ACK commit allocation failure воспроизведён настоящим bbolt MaxSize
limit с pinned read transactions: ErrStorage сохраняет record и lease.
Failed Push не меняет committed state. Restart replay сохраняет message ID,
последующий successful ACK удаляет record.

Проверены offline/send disconnect, lost/duplicate/delayed/out-of-order/unsent ACK,
lease timeout/reset, corruption/checksum/index, storage bounds, reservations,
graceful Close и subprocess termination после enqueue/send/ACK.
Count=20 означает 60 процессных crash boundaries, а не физическую power loss.
At-least-once относится к retained данным при соблюдении durable условий;
exactly-once не заявляется. Backend обязан commit/dedup до ACK.

## 8. Backpressure

8 producers × 128 mixed records при offline Backend: retained=64,
protected CommandResults=8, payload≈15.9KiB, file=262,144 bytes,
968 intentional drops (включая 256 logs). Accounting сохраняется; restart,
drain и ACK всех retained records PASS.

Stress сочетает Syslog и telemetry всех пяти adapters с общей outbox
128 messages/2MiB. Queue не растёт без ограничения. Только priority0
CommandResult/reservations защищены от eviction. Events priority1 могут
вытесняться равным priority; гарантия lossless events не вводилась.
Capacity/retention exhaustion приводит к видимому rejection/eviction.
Sender→Syslog receiver не имеет application ACK и не гарантирует lossless ingress.

## 9. Scheduler

Существующие интервалы, per-job no-overlap, worker bounds и dynamic pause/drain/
commit/rollback сохранены. Пропущенные периоды coalesce в один pending poll.
Slow/hung BMC не блокирует 50 healthy jobs. Syslog ingress работает отдельно.
Pending removal не исполняет callbacks, managed replacements не создают duplicate
workers. Scheduling jitter не обещан; существующие adapter retries bounded.

## 10. Managed configuration

Rapid concurrent revisions 10–13 дают сериализованные received/validating/
terminal statuses с truthful applied metadata, final active revision=13 и
единственным adapter. Pending wire updates bounded существующим механизмом.

ENOSPC до публикации, после реальной atomic publication и постоянный storage
failure сохраняют original active/LKG revision=1; replacement закрывается.
Неудачное recovery устанавливает degraded и блокирует следующий apply.
Cancellation во время drain реального poll не превращается в APPLIED.
Сохранены mTLS/local policy/UUID/session binding, duplicate/stale/conflict,
five-protocol add/update/disable/remove, LKG и policy change after restart.
Удаление Target не удаляет retained outbox records.

## 11. PKI/TLS

Wrong CA, hostname/SAN, expired certificates, UUID binding, enrollment/renewal,
rotation/reconnect, blocked Send/Receive Close и half-open cancellation проверены
существующими и новыми fixtures. 20 half-open handshakes × 3 и 50 streams × 3
не выявили неконтролируемого накопления ресурсов.

Production CA service, revocation/CRL/OCSP и CA rollover остаются внешними
integration gaps. Packet loss/slow/half-open/reset/response delay представлены
controlled sockets, timeouts и faults, а не изменением сети development host.
Не все возможные DNS/router failure patterns отдельно измерены.

## 12. Syslog/Audit

RFC5424/3164/5425/6587, octet counted framing, optional LF compatibility,
oversized/malformed/partial frames, slow clients, connection/CIDR/identity bounds,
admission pressure и secret redaction покрыты fixtures.

4 verified TLS senders + 2 slow clients, protected queue=8: четыре ожидаемых
admission failures; после освобождения capacity четыре durable logs проходят
restart/replay с теми же IDs и ACK. Peak connections=6.
50 TLS startup/stop cycles × 3 на Linux: FD 6→6, goroutines 2→6→2,
max shutdown 0.128–0.302ms. Raw logs в diagnostics не добавлялись.
Real rsyslog/auditd forwarding в текущей инфраструктуре NOT AVAILABLE.

## 13. Redfish

HTTPS enforcement, custom CA/name verification, same-origin references/redirects,
malicious @odata.id, bounded resources/pages/bytes/reauth, partial results и
session cleanup сохранены. Reference fuzz: 5s, 33,706 executions, PASS.
Stress использует production adapter с isolated verified HTTPS fixture;
writes=0, login/logout balanced. Real vendor BMC/firmware NOT AVAILABLE.

## 14. IPMI

Pinned go-ipmi `v0.9.2-0.20261009051248-b3670df57943` не изменён. Primary local
module source проверен на совместимость с packet guard. Cipher17 default,
cipher3 explicit; cipher0 и silent v1.5 downgrade запрещены.
RAKP/HMAC/integrity/session ID/sequence/replay/AES framing, read-only allowlist,
bounded retry/socket cleanup regression PASS.
Decoder/guard fuzz 5s: 38,664 / 33,447 executions, PASS.
Reference BMC — protocol fixture, не actual firmware validation.

## 15. Identity/Topology

Source/subject/origin/normalized+original value/timestamps/provenance сохранены.
Directed observations, raw IDs/subtypes, localPortNum≠ifIndex, TTL/expiry,
unknown neighbor, stable IDs и partial/failed semantics проверены.
Failed poll не подтверждает removal; ARP не означает cable evidence;
BMC management relation не является network link. Global Asset merging отсутствует.

Actual Agent admission → durable JSON → protobuf budget (Linux repeat):

| Neighbors | JSON bytes | protobuf bytes | Queue envelope limit | Result / push time |
|---|---:|---:|---:|---|
| 48 | 165,840 | 84,913 | 1,048,576 | PASS, 48 retained, 19.13ms |
| 256 | 882,408 | 452,138 | 1,048,576 | PASS, 256 retained, 55.97ms |
| 512 | 1,764,840 | 904,489 | 1,048,576 | REJECTED_TOO_LARGE, 0 retained, 112.30ms |
| 512 | 1,764,840 | 904,489 | 2,097,152 | PASS, 512 retained, 111.81ms |

Fixture содержит typical LLDP evidence и derived identity facts. Larger legal
strings/facts могут превысить budget при меньшем числе neighbors.
Типичные ≤256 observations прошли; systematic loss в этом workload не подтверждена,
поэтому automatic splitting не добавлено. Для dense topology оператор измеряет
JSON и protobuf, согласует max_message_bytes/max_bytes/storage/gRPC limits.
Silent truncation отсутствует; 512 требует 2MiB именно для этого fixture.

## 16. Time/NTP

source_timestamp, observed_at и collected_at сохранены; received_at принадлежит
Backend. Invalid timestamps, restart и unavailable OS synchronization fixtures
проходят. Linux time-sync-status baseline exit=0, available=false: live chrony/
timesyncd/corporate NTP недоступны. Polling не останавливается при unknown clock.
Системные часы/NTP development host не менялись; собственный NTP client не добавлен.

## 17. Simulator/provenance

Новый round-trip regression проверяет nine payloads: inventory, SNMP telemetry,
Redfish, IPMI, RouterOS, topology, SIP, lifecycle и Syslog. Actual Agent.push/
sanitizer, durable lease и envelope identity, protobuf encode/decode как у Mock,
ACK сохраняют Collector/Target IDs и simulated SourceRefs (counts 7/3/3/3/3/5/2/1/2).

Stress explicitly маркирует synthetic adapter data перед admission и проверяет
marker в Mock после wire decode. Review нашёл test-only inventory batch marker gap;
исправлен clone/mark. Production factory labels не менялись.
Some existing production-path wire fixtures сохраняют adapter-default real marker:
они изолированы и не являются hardware evidence.
Stress вызывает Manager.Apply(auth=true) напрямую; authenticated wire B9 покрыт
отдельными existing E2E, а не этим direct-apply workload.

## 18. Resource leaks и lifecycle

Repeated workloads: 100 Target add/poll/remove (100 opened/closed, goroutines 2→2),
20×32 existing lifecycle changes, 50 transport streams ×3, 20 half-open ×3,
50 Syslog cycles ×3, 60 subprocess crash boundaries и 20 Agent starts/stops.
Проверяется stabilization после close/GC, а не один snapshot.

Linux 20 durable startup/stop cycles (100ms idle each), PASS 2.16s:
same UUID, new Session ID; FD 6→6, goroutines 2→2; peak HeapAlloc=7,903,328,
HeapInuse=10,354,688, TotalAlloc=81,394,912 bytes / 293,352 allocations;
max New=14.27ms, shutdown=4.20ms. Last idle heap=5,487,424,
after GC=3,302,432 (before=5,256,352 после других package tests).
Windows equivalent PASS 2.12s: max New=9.89ms, shutdown=2.89ms.
Это test-process measurements, не isolated daemon memory minimum.

Windows TempDir failures не объявлены data race:
baseline queue cleanup ERROR_DIR_NOT_EMPTY исправлен existing bounded helper;
final logs использовал старую local variant — заменена общей.
50 normal /20 race log restart repeats PASS; real locked handle 20 normal/20 race
по-прежнему возвращает ошибку без удаления bytes. Во втором полном normal run отдельный sharing violation cleanup managed temp file
сохранён. Count200 PASS8.612s; ignored Win32 handle-probe overlay после500×5
lifecycles/15,000atomic saves PASS20.150s: own fixture handles=0 безGC,
FILE_TYPE_DISK handles1→1; positive held-file control добавляет1, Close убирает1.
Владелец кратковременной блокировки не установлен; scanner не обвиняется.
Общий helper намеренно не retry sharing/access errors; production handle leak
не подтверждён. Изолированный полный normal repeat PASS17.374s.
Runtime storage close/retry не ослаблен, бесконечных cleanup retries нет.

## 19. Stress results

Опт-in command:
`INFRAPULSE_STRESS_SECONDS=65 go test -count=1 -run '^TestPhase8MixedProtocolStress$' -timeout=6m -v ./internal/agent`.
Linux эквивалент: CGO0 crosscompile `go test -c -o .cache/phase8-stress/linux-v3.test ./internal/agent`,
затем WSL `sh .cache/phase8-stress/run-v3.sh` с `-test.cpuprofile=.../cpu-v3.pprof`.

10/50/100 targets, equal five-protocol mix: actual SNMP UDP/RouterOS HTTPS/
SIP UDP/Redfish HTTPS/LANplus adapters с isolated fixtures. Workers=4,
Syslog≈50 writes/s, queue128/2MiB/inflight16. Backend offline первые 1/3,
затем verified mTLS Mock; rapid disable/enable/remove/add revisions2–5;
SIP503→200 fixture. Fast=1s, hardware=30s, inventory/topology=5m.
Это test settings и reference workloads, а не production capacity claims.

Linux v3 PASS 195.44s (65.17/65.16/65.11s):

| Metric | 10 targets | 50 targets | 100 targets |
|---|---:|---:|---:|
| Prepared-target heap bytes | 2,610,672 | 4,825,088 | 6,259,384 |
| Peak HeapAlloc / HeapInuse bytes | 15,112,024 / 18,104,320 | 22,667,272 / 26,910,720 | 36,650,264 / 41,664,512 |
| Peak RSS bytes | 50,417,664 | 61,681,664 | 83,251,200 |
| Peak goroutines / FDs / sockets | 63 / 28 / 20 | 109 / 62 / 54 | 170 / 100 / 91 |
| After goroutines / FDs | 10 / 12 | 10 / 12 | 10 / 12 |
| Queue peak messages / bytes | 128 / 335,780 | 128 / 576,926 | 128 / 544,614 |
| Poll runs / reader calls | 375 / 331 | 1,949 / 1,559 | 3,888 / 3,135 |
| Reader errors / max poll | 1 / 56.14ms | 1 / 1,000.74ms | 0 / 18.03ms |
| Received wire messages / second | 2,507 / 38.52 | 3,353 / 51.51 | 4,214 / 64.75 |
| Remote admitted / rejected | 302 / 98 | 1,393 / 470 | 2,705 / 1,077 |
| Queue drops telemetry/logs/other | 76 / 1,057 / 1,152 | 341 / 603 / 1,066 | 670 / 408 / 1,499 |
| Syslog writes / frames / admitted | 3,250 / 3,250 / 3,249 | 3,181 / 2,581 / 2,575 | 3,143 / 2,200 / 2,191 |
| Syslog admission failures / reconnects | 0 / 0 | 6 / 6 | 9 / 9 |
| Redfish login/logout | 8 / 8 | 30 / 30 | 63 / 63 |
| Listener startup / initial apply | 20.25 / 16.77ms | 10.47 / 13.67ms | 10.09 / 7.68ms |
| Four rapid changes total | 385.58ms | 26.47ms | 26.39ms |
| Shutdown | 22.11ms | 18.02ms | 29.99ms |
| TotalAlloc bytes / allocations | 7,201,631,064 / 20,050,263 | 7,874,178,056 / 23,469,276 | 14,349,436,280 / 38,784,164 |

Counters включают same-process fixture servers, queue scans, Mock и framework.
Received включает replay duplicates; это не unique persisted Backend throughput.
Remote rejection, queue eviction и sender writes не равны receiver admission.
Before heap уже включает applied targets, не empty-daemon idle.
Residual 13/14 messages в 50/100 run сохранены в durable outbox при shutdown;
немедленное полное ACK drain не является shutdown requirement.
Reader error rates≈0.30%/0.064%/0%; final 50-target job retained one unavailable
status, polling continued. Все five protocol markers observed; RF writes=0,
BMC ActiveSessions=0 после stop. After counters включают живые fixtures.

CPU profile `go tool pprof -top .cache/phase8-stress/linux-v3.test .cache/phase8-stress/cpu-v3.pprof`:
102.90 CPU seconds /195.44 wall seconds (52.65% одного core).
JSON encode/decode, GC и syscalls заметны; профиль агрегирован по уровням,
параллельно выполнялись regression tasks. Per-target CPU SLA не выводится.

Low-resource isolated child: `ulimit -n 128; GOMEMLIMIT=32MiB GOMAXPROCS=2 INFRAPULSE_STRESS_SECONDS=65 .../linux-v3.test -test.run=TestPhase8MixedProtocolStress/50 -test.count=1 -test.timeout=3m -test.v=true`.
PASS65.15s; peak heap18,106,360/inuse20,447,232/RSS54,128,640 bytes;
FDpeak61/sockets53/goroutines109; queue128/691,741; shutdown19.90ms;
2 reader errors/1,585 calls, finalqueue0, 30/30 RFsessions, BMC0.
Soft GOMEMLIMIT не является RSS ceiling. Это не реальный OpenWrt RAM test.

Windows race opt-in: тот же command с `-race` и `-run 'TestPhase8MixedProtocolStress/10$'`,
CGO1/portable CC, PASS67.413s (workload65.77s), race reports отсутствуют;
shutdown516.43ms, queue128, peak heap7,690,512 bytes. Throughput под detector
существенно ниже; TCP sender buffer не означает lossless admission.
FD/socket/RSS на Windows в этих counters NOT AVAILABLE.

Промежуточные runs сохранены: Windows195.31s и Linux195.43s были bounded, но
SNMP fixture community mismatch и inventory marker gap не позволяли считать
five-protocol scenario успешным. V2 Linux195.33s получил все protocols, но sender
останавливался при первом admission timeout (50 targets402frames,100targets86).
V3 добавил single bounded sender reconnect (250ms pause, 1s dial context).
WSL CLI failures из-за quoting были исправлены LF shell script, не скрыты.

## 20. Performance comparison

Matched managed benchmarks (Windows fixture):
validation 8.844→9.134µs, 4516→4528B,46allocs/op;
apply 988.112→1043.241µs,17326→17225B,145allocs/op;
prepare32 40.731→36.910µs; live32 57.284→61.671µs.
Single-host noise не доказывает значимый regression/improvement.
High cumulative JSON allocations под ingestion зафиксированы, но retained memory
bounded. Измерения не оправдывают library replacement, новый build matrix или
переписывание queue. Более длительный isolated performance campaign остаётся P2.

## 21. Binary size

Linux amd64 stripped matched baseline 26,366,112 →26,374,304 bytes:
+8,192 bytes (+0.031%). Тот же CGO0/trimpath/buildvcs=false/-s -w command.
Dependencies/go.mod/go.sum неизменны. Release builds других architectures:
arm64 24,576,160; arm7 24,838,304; mips/mipsle 27,525,311 bytes.
Binary size не является оценкой runtime RAM/flash endurance.

## 22. Deployment/systemd

Unit сохраняет nonroot User/Group, private StateDirectory=0700, UMask0077,
NoNewPrivileges/ProtectSystem/ProtectHome/PrivateTmp; добавлены WorkingDirectory,
kernel/control-group/SUID restrictions и empty CapabilityBoundingSet.
Default Syslog6514 не требует privileged port. Separate administrative drop-in
для CAP_NET_BIND_SERVICE описан только при намеренной необходимости.
Runtime waits components≈5s, service TimeoutStopSec10s; filesystem Close не имеет
доказанного hard deadline на зависшем storage.
systemd-analyze/runtime NOT AVAILABLE в WSL; host services не изменялись.
[Deployment guide](production-deployment.md).

## 23. Deployment/OpenWrt

Подготовлен nonroot procd init: fixed command/config path, respawn3600/5/5,
term_timeout10, stdout/stderr, config watch. `sh -n` actual Linux PASS;
PowerShell scripts AST PASS. Primary procd source reviewed.
Live firmware/procd/ubus ACL/reboot/RAM/flash NOT AVAILABLE.
Guide требует persistent mount (обычный /var может быть volatile), private owner
и конкретный ABI; создание аккаунта и install выполняет администратор.
[Manual validation](real-hardware-validation.md), [OpenWrt](openwrt.md).

## 24. Release artifacts

`./scripts/release.ps1 -Version phase8-review -AllowDirty -OutputDir .cache/phase8-release-fixed`
создаёт пять CGO0 archives, version/commit/Go/target/Git HEAD timestamp,
source SHA256 manifest, safe example configs/service/init/all Markdown guides,
inner и outer SHA256SUMS с LF и slash paths.
Без AllowDirty dirty tree отвергается (expected rejection PASS).
Commit marker включает -dirty; это локальный review candidate, не опубликованный release.

Actual first Linux BusyBox `sha256sum -c SHA256SUMS` FAIL5/5 из-за CRLF filename:
`.cache/phase8-release-verify.log`. Writer исправлен с WriteAllLines на explicit LF
WriteAllText, independent AST/CR=0/BOM=false review PASS.
Repacked Linux checksum verification PASS:5outer archives и52inner files ×5;
actual extraction/path/private-file allowlist checks PASS; amd64 version
phase8-review /727c6e5e90984259efb331c6ae409e5b67b826c1-dirty/
2026-10-10T21:16:26Z,12packaged config examples PASS. Verify10.119s,
package10.471s. Final package с итоговыми docs: .cache/phase8-release-final; SHA256SUMS
рядом с archives является authoritative archive hash list. Archive bit-identical reproducibility
не обещана. Два повторных builds дали одинаковые binary SHA256 для всех пяти
architectures (JSON .cache/phase8-release-binary-reproducibility.json):
amd64 109df75dbaefb14bb3cb8d44ef67892862eab796943583894a0703a1a3f6ff53.
Windows tar modes требуют explicit install/chmod0755, guides это учитывают.
Никаких tag/publication/install или CI/CD service не добавлено.

## 25. Collector/Backend protocol compatibility

Protocol v1, field numbers, descriptors, digest vector и persisted format не
изменены. Hello/Heartbeat/capabilities, inventory/telemetry/hardware/log/audit/
topology/source/time, application ACK и managed_config_v1 проверены реальным
loopback gRPC/mTLS обменом с Mock. Production Backend source не изменён.
Process Session ID сохраняется при reconnect, меняется при Collector restart.
[Contract/E2E handoff](collector-backend-e2e.md).

## 26. Backend integration gaps

Production Collector→Backend→PostgreSQL→REST и Backend desired B9→Collector
status NOT AVAILABLE. Требуются durable commit/dedup до ACK, revision reconciliation,
UUID/session/tenant authorization, B3/B7 source-aware identity/topology resolution,
реальные REST/schema mappings и operational PKI revocation.
Backend без новых fields/B9 при корректном wire — integration gap.
В отдельном guide заданы setup, evidence и критерии PASS без выдуманных endpoints.

## 27. Full regression и integration matrix

Команды исполняются Windows Go1.27.1; race portable CC; Linux crosscompiled suites
исполнены в WSL. Exact script invocations:
`./scripts/validate-phase8.ps1 -Mode Mandatory -GoPath <SDK> -OutputDir .cache/phase8-final-repeat`,
`-Mode Extras -OutputDir .cache/phase8-final`, `-Mode Linux -OutputDir .cache/phase8-final`.
Final normal repeat выполнен отдельно после cleanup investigation; PASS17.374s.

| Exact command / check | Result | Duration / limitation |
|---|---|---|
| `gofmt -l <LF-normalized copies, batches30>` | PASS | 1.180s, sources не переписываются для проверки EOL |
| `go build ./...` | PASS | 2.017s |
| `go vet ./...` | PASS | 0.670s |
| `go test -count=1 ./...` | PASS17.374s | промежуточные FAIL22.375s logs cleanup и FAIL18.889s managed sharing cleanup сохранены |
| `go test -race -count=1 ./...` | PASS | 56.924s; earlier full PASS67.626s |
| `go test -count=1 -p=1 -tags=integration ./...` | PASS | final94.210s; first114.647s |
| `go mod verify` | PASS | 0.448s |
| `./scripts/build.ps1` / `-Cross` | PASS | 6.517 /21.578s |
| pinned protoc33.5/go1.36.12/grpc1.6.0 scratch regeneration | PASS | 1.363s, exact LF equality, checkout CRLF raw hashes recorded |
| `infrapulse-agent validate-config --config <12 examples>` | PASS | 0.788s |
| 24 Linux suites compile/run | PASS | platform/linux, logs, queue, commands, protocol/snmp, driver/snmp, driver/routeros, driver/redfish, driver/ipmi, pki, platform/openwrt, agent, simulator, telephony, transport/grpc, config, platform, syslogreceiver, managedconfig, scheduler, remote, driver/telephony, protocol/routeros, model |
| final Linux admission/provenance binary repeat | PASS | new bounded success assertions; initial direct CLI FAIL quoting, LF script PASS |
| documentation links / `git diff --check` | PASS | 35Markdown files,351local targets,5anchors; no missing links; diff exit0 |

Live endpoint tests без environment SKIPPED; Windows symlink privilege probe
SKIPPED, Linux real FIFO/symlink tests PASS. Normal run skips opt-in stress;
он выполнен отдельно. Integration-tag tests — fixtures/Linux runtime, не hardware E2E.
Test output exact commands/exit codes/durations сохранены в results JSON/logs.

| Scenario | Setup | Expected | Actual / status | Limitation |
|---|---|---|---|---|
| A Stable | actual adapters + loopback fixtures + mTLS Mock | polling/data/ACK | all five observed, stress PASS | fixture workload |
| B Backend offline | first third stress; existing delivery E2E | bounded retention | queue128 + visible drops/rejections, PASS | no infinite capacity promise |
| C Reconnect | Mock restart, delayed/half-open peer | bounded retry/cancellation | transport/Agent loops PASS | Hello→EOF resets backoff |
| D Replay/ACK | blackhole/lost/duplicate/out-of-order ACK, restart | IDs retained until durable ACK | queue/real gRPC E2E PASS | Backend dedup external |
| E Managed update | mTLS five-adapter E2E; rapid10–13 | serialized honest apply | final13/single adapter, PASS | stress directApply separate |
| F Invalid managed | policy/UUID/session/digest/stale conflict | reject without changing active | negative/lifecycle tests PASS | real B9 unavailable |
| G LKG | restart/current changed policy/storage faults | restore or truthful FAILED | managed/store/Agent fixtures PASS | physical flash powerloss absent |
| H Syslog pressure | 4TLS senders+2slow, protectedqueue8 | controlled admission/replay | 4expected failures/4recovered IDs, PASS | sender-hop not lossless |
| I Redfish unavailable | fixture timeout/auth/partial/reset | bounded partial/error/session close | driver/Agent tests PASS | real BMC unavailable |
| J IPMI unavailable | BMC fixture cancel/retry/guard faults | no downgrade, bounded cleanup | driver/Agent tests PASS | vendorfirmware absent |
| K SNMP timeout | controlled UDP fixture/nonresponse | cancellation/no scheduler halt | SNMP/slow-target tests PASS | realpacket-loss rates absent |
| L SIP degradation | OPTIONS503→200/slowTLS/TLSfaults | health transitions/bounded latency | telephony/stress PASS | actual PBX absent |
| M Partial topology | LLDP/RouterOS partial/failed snapshots | no false removal, raw facts | model/registry/Phase6 E2E PASS | no real cable graph |
| N Two identities | two certs/Collector IDs/outboxes/revisions | no cross-config apply | authenticated managed E2E PASS | no Backend globalmerge claim |
| O Abrupt restart | 60 subprocess crash boundaries | durable retained IDs/replay | count20 integration PASS | os.Exit, not powercut |
| P Corrupt state | bad records/index/checksum/LKG/journal | controlled errors/degraded | queue/managed/commands fixtures PASS | no destructive hostdisk tests |
| Q Certificate failure | wrongCA/name/expiry/UUID/FIFO | reject, no insecure fallback | TLS/PKI/adapter tests PASS | revocation external |
| R Resource exceeded | MaxSize commit faults/queue caps/oversize/fd child | bounds/errors/no panic | deterministic tests/lowresource PASS | ENOSPC injected, not full hostdisk |

Fault injection/unit tests в matrix не выданы за network E2E.
Exact targeted commands и fuzz details — в приложениях.

## 28. Cross-build

Collector linux/amd64, arm64, arm GOARM=7, mips/mipsle hardfloat PASS CGO0.
Mock Controller, Simulator и Mock Enrollment amd64/arm64 PASS.
Native development tools также собраны. Cross-build не подтверждает работу
реального OpenWrt firmware, ABI, RAM/flash или procd.

## 29. Phase 8A acceptance

**COMPLETE.** Все автоматические критерии выполнены. Independent final code и
documentation review PASS; actual package verification PASS. Physical hardware не
является обязательным условием Phase8A и остаётся отдельно в Phase8B.

| Acceptance group | Status / evidence |
|---|---|
| Phase0–7 preserved; committed baseline verified | PASS,727c6e5/main→separate branch |
| Baseline measured/saved | PASS, before production edits/source hashes unchanged |
| Confirmed P0/P1; critical security blockers | PASS, confirmed P1 fixed, no known unresolved P0/P1; finite audit limitations stated |
| Queue/replay/application ACK/outage/reconnect | PASS, actual gRPC fixtures + deterministic storage transactions |
| Disk failure/corruption/crash recovery | PASS, injected faults and60subprocess boundaries; physicalpowerloss gap |
| Bounded admission/backpressure | PASS, protected records conserved, visible queue drops/rejections |
| Scheduler concurrency/starvation/isolation | PASS,10/50/100startup/50fairness/pendingremove/hungBMC/race |
| Managed lifecycle/rapid changes/LKG/policy/storage failure | PASS, final13/oneadapter, truthful restore/apply statuses |
| PKI/mTLS/Syslog/Redfish/IPMI security | PASS, TLSnegative cases/FIFO/crypto/session/guard regression |
| Identity/topology/time/provenance | PASS, scoped facts/partial semantics/ninepayload wire and durable IDs |
| Graceful shutdown/restart/resource stabilization | PASS, repeated lifecycle/FD/socket/session tests |
| Stress/resource/performance evidence | PASS,65s×10/50/100, lowresource50, race10, heaps/RSS/CPU/drops/timings |
| Fivearchitecture CGO0 cross-build/development tools | PASS |
| gofmt/build/vet/test/race/mod verify | PASS, finalcommands/durations §27 |
| Integration/24Linux suites | PASS, exactpackages/timings AppendixD |
| Protobuf reproducibility/12examples/docs links/diff | PASS, normalizedEOL explicitly distinguished |
| Deployment/release procedure | PASS, guides/procdsyntax; actual5archive checksum/version/config and binary repeat |
| Backend contract/gaps/manualhardware instructions | PASS, Mock wire evidence separate from real infrastructure |
| Independent final code review | PASS, sibling reviews of scheduler/trust/outbox/stress/admission/release |

## 30. Phase 8B acceptance

**PARTIAL**: actual Linux amd64 proc/sys/files/network runtime PASS в WSL.
Systemd installation/restart/permissions as deployed service NOT EXECUTED;
systemd runtime здесь NOT AVAILABLE. OpenWrt/procd/reboot, actual SNMP v2c/v3,
RouterOS, SIP PBX, Redfish/IPMI BMC, rsyslog/auditd forwarding, corporate NTP,
production Backend/PostgreSQL/REST и B9 NOT AVAILABLE / NOT EXECUTED.
[Manual checklist](real-hardware-validation.md) включает setup, expected result,
evidence fields и statuses PASS/FAIL/NOT EXECUTED/NOT AVAILABLE.
Прежние local/fixture проверки не переименованы в production acceptance.

## 31. Changed files

Runtime: scheduler pending/fairness; bounded local trust readers (PKI/gRPC/
Syslog/SIP). Test-only Windows cleanup; queue/crash/ACK/concurrency/network,
managed faults/rapid revisions, adapter fuzz, provenance/topology, lifecycle/stress.
Deployment: existing systemd unit и новый procd init.
Tooling: existing-matrix validation/release scripts.
Docs: five Phase8 guides/report и ссылки в README/security/topology/OpenWrt/
deployment/support/validation. Полный список actual changed files добавлен ниже.
Generated protobuf, api contract, go.mod/go.sum не изменены.

## 32. Remaining P0/P1/P2

Подтверждённых P0 не найдено; два P1 (scheduler starvation, blocking PEM readers)
исправлены и имеют failing-before/passing-after evidence. Новых unresolved P0/P1
independent code review не выявил; это конечный audit, а не proof of absence.

P2: JSON queue allocation/CPU overhead под длительным ingestion; sizing dense
topology без automatic batch splitting; HelloAck→immediateEOF reset backoff
(1s sample15attempts, gaps53–92ms at initial80ms/max640ms).
Последний случай strictly bounded initial equal jitter, не unbounded retry storm;
sustained-session exponential escalation для него не обещана.
Windows strict test cleanup может кратковременно встретить unowned sharing violation;
исходный FAIL, passing repeats и file-handle audit сохранены. Владелец не установлен,
production file leak не подтверждён, sharing retry не расширен.
Physical storage, live hardware/Backend/PKIrevocation остаются validation gaps.

## 33. Known limitations

Delivery гарантируется для retained durable records при исправном synced FS,
capacity/retention/priority условиях и корректном Backend ACK. Обычные
logs/telemetry/topology/events могут быть rejected/evicted; sender→Syslog без ACK.
Soft GOMEMLIMIT не ограничивает whole RSS; resource samples включают fixtures.
Sanitizer знает ограниченный набор patterns, не универсальный secret detector.
Administrator защищает local state/trust dirs; Stat/Open race не решён.
Actual firmware/flash endurance/powerloss/production PKI/database integration
не проверены. Не добавлены новые protocols/services/Backend/globalAssets/NTP/TSDB.

## 34. Final readiness assessment

Phase8A COMPLETE: Collector пригоден для controlled
integration/demo validation по guides. Production readiness не заявляется
до real infrastructure и Backend evidence Phase8B. Изменения остаются reviewable
в отдельной ветке без commit/push; публикация/развёртывание не выполнялись.

## Приложение A — durable и transport targeted tests

| Exact Go command (following environment above) | Actual result / evidence | Limits |
|---|---|---|
| `go test -count=1 -v -run '^TestPhase8Durable' ./internal/queue` | PASS1.288s. True bbolt commit page-allocation error; failed ACK and Push preserve protected record/lease; restart preserves message ID and subsequent ACK drains. Out-of-order/delayed/duplicate/unsent ACK safety. 8producers×128mixed telemetry/log/event/topology offline envelopes705.7ms,64retained/15876bytes/file262144;968drops including256logs;8protected results survive. | Simulated source fixture; retained at-least-once, documented evictions/rejections, no disk power-loss claim. |
| `go test -race -count=3 -v -run '^TestPhase8Durable' ./internal/queue` | PASS43.840s, no race.3×1024ingestions13.0–13.8s under detector;64retained/15880bytes/file262144,968drops. `.cache/phase8-queue-race.log` | Race overhead is not deployment throughput. Close waiting fixture subsequently gained an explicit5ms waiting barrier and separate race repeat. |
| `go test -race -count=3 -v -run '^TestPhase8DurableCloseUnblocksWaitingPop$' ./internal/queue` | PASS6.199s, both empty/full-inflight cases wait before Close and unblock with retained state safe. `.cache/phase8-queue-close-race.log` | Latest fixture barrier validated separately after main count3. |
| `go test -count=20 -tags=integration -v -run '^TestDurableSubprocessCrashRecovery$' ./internal/queue` | PASS32.697s,60subprocess enqueue/send/ACK os.Exit boundaries. `.cache/phase8-queue-crash20.log` | Process exit, not hardware power failure. |
| `go test -count=20 -v -run '^TestPendingDirectoryCleanupDoesNotMaskLockedFiles$' ./internal/agent` | PASS0.995s; real Windows handle without FILE_SHARE_DELETE remains inaccessible to cleanup; narrow helper does not mask handle leak. `.cache/phase8-locked-handle20.log` | First invocation failed compile because root stress file was WIP (`Stats.Running` nonexistent); corrected root fixture and this exact repeat passed. |
| `go test -count=1 -v -run '^TestPhase8' ./internal/transport/grpc ./internal/agent` | PASS transport1.637s/Agent1.778s.20half-open HTTP2 handshake cancellations40ms in809.5ms/20socket opens20closes;50real TCPstreams opened/closed50, goroutines2→3with shared server still running. Stalled reader Close concurrently unblocks Send/Receive0.504ms after one1MiB Send. `.cache/phase8-transport-targeted.log` | Mixed stress is opt-in and SKIP in this command; root executes it separately. |
| `go test -race -count=3 -v -run '^TestPhase8(HalfOpen\|CloseUnblocks\|RepeatedStream\|Handshake)' ./internal/transport/grpc ./internal/agent` | PASS transport5.115s/Agent5.047s;3×20half-open and3×50stream lifecycles; final goroutines2→3–5with shared server; Close0–1.57ms. `.cache/phase8-transport-race.log` | Stabilization bounds, not absolute proof of no future leak. |
| `go test -run '^$' -fuzz '^FuzzPhase8AgentEnvelopeDecode$' -fuzztime=10s -parallel=2 ./internal/transport/grpc` | PASS12.216s,60687executions/4new interesting corpus cases. Decode/encode accepted data preserve message/Agent/session IDs and telemetry source; malformed/unknown data safely rejected. `.cache/phase8-protobuf-fuzz.log` | Input64KiB campaign bound; finite campaign does not prove vulnerability absence. |


## Приложение B — scheduler/managed targeted tests

## Команды и результаты

1. `go test -count=1 -v ./internal/scheduler` — PASS, package0.543s. After-fix startup10/50/100 PASS, saturated50 targets each twice/workers<=2 PASS; existing dynamic Change/cancellation/rollback PASS. Evidence `.cache/phase8-scheduler-pass.txt`.

2. `go test -count=1 -v ./internal/scheduler ./internal/managedconfig ./internal/remote ./internal/credentials` — PASS, scheduler2.120s, managedconfig1.955s, remote2.119s, credentials1.397s (parallel packages). Windows symlink privilege test SKIPPED; Linux suite must cover it. Evidence `.cache/phase8-managed-targeted.txt`.
   - Startup10/50/100 all serviced; saturated50 fairness; pending50 removal no callbacks; one hung BMC leaves50 healthy startup callbacks.
   - Rapid concurrent revisions10–13 statuses сериализованы, intermediate applied metadata truthful; finalactive13, one adapter.
   - ENOSPC injection before publication, after real atomic publication, permanent failure: runtime/LKG original1 preserved, replacement closed, persistent recovery failure sets degraded and refuses later revision.
   - Cancellation triggered by original poll context during transition drain: original remains active1 and notclosed; failed status truthful.
   -100 sequential add/poll/remove fixture cycles:171.0295ms,100 opened/100 closed/100 polls,goroutines2->2 afterclose+GC. This verifies fixture lifetime; physical sockets checked by root actual adapter stress separately.
   - Existing20x32 cycles:161.862ms,640opened/640closed,580polls,goroutines2->7peak7->2,totalalloc7,820,800bytes,retainedqueue32messages/18,489bytes,droppedtelemetry580. Queue is memory fixture; drop documented.
   - Existing50 persisted revisions:122.0288ms,totalalloc1,052,864bytes,heap631008->503752,goroutines2->2; restore525.2us,state1319bytes.

3. `go test -race -count=1 ./internal/scheduler ./internal/managedconfig ./internal/remote ./internal/credentials` — PASS, scheduler6.677s,managedconfig6.006s,remote5.809s,credentials5.098s. CGO_ENABLED=1,GOOS=windows,GOARCH=amd64,CC=.tools/llvm-mingw-20260922-ucrt-x86_64/bin/x86_64-w64-mingw32-gcc.exe. Evidence `.cache/phase8-managed-race.txt`.

4. `go test ./internal/managedconfig -run '^$' -fuzz '^FuzzManagedCanonicalDigest$' -fuzztime=15s -parallel=2` — PASS;90,205executions,24newinteresting inputs,29total corpus,package20.953s. Properties: normalize idempotence,digest stability after target reorder/deliverymetadata changes; malformed JSON/options rejected. Fuzzing is finite evidence, not security proof. Evidence `.cache/phase8-managed-fuzz.txt`.

5. `go test ./internal/managedconfig ./internal/remote -run '^$' -bench 'BenchmarkManaged' -benchmem -benchtime=300ms -count=1` — PASS. Validation9134ns/op,4528B/op,46allocs/op; Apply1043241ns/op,17225B/op,145allocs/op; prepare32targets36910ns/op,131665B/op,526allocs/op; liveadd/remove32targets61671ns/op,142667B/op,608allocs/op. This is Windows fixture benchmark, not production capacity. Evidence `.cache/phase8-managed-benchmarks.txt`.

6. `gofmt -w internal/scheduler/scheduler.go internal/scheduler/hardening_test.go internal/remote/managed_resources_test.go internal/managedconfig/hardening_test.go internal/managedconfig/fuzz_test.go` выполнен.


## Приложение C — adapter security, fuzz и independent review

### Подтверждённый P1: blocking reads локальных TLS/PKI файлов

FIFO вместо CA/cert/key блокировал startup/reconnect до достижения сетевых timeout/cancellation. Изменены только readers: `internal/syslogreceiver/receiver.go`, `internal/telephony/probe.go`, `internal/pki/manager.go`, `internal/pki/identity.go`, `internal/transport/grpc/client.go`. До Open проверяются тип и размер конечного файла; открытый handle повторно проверяется; LimitReader сохраняет bound при изменении размера. Syslog/SIP/manual gRPC material ограничен 1 MiB на файл, PKI bundle/manual identity reader — 128 KiB. Verified TLS, CA/SAN, client authentication и UUID URI binding не ослаблены. Administrator symlink к regular файлу поддерживается. Ошибки не раскрывают настроенные пути/secrets.

Это не atomic защита от hostile actor, который может заменить файл между Stat/Open. Нужны защищённые administrator-owned directories. Возможные hangs удалённой filesystem не объявлены устранёнными.

| Исходный сценарий | До исправления | После исправления | Evidence |
|---|---|---|---|
| Syslog LoadTLS, SIP Probe(timeout=100ms), PKI New(timeout=100ms), actual FIFO | FAIL: три subprocess завершены внешним timeout2s, BusyBox SIGTERM exit143; всего6.378s | PASS: те же вызовы возвращают fixed error category, все exit0 | fifo-baseline.log / fifo-final.log |
| Backend gRPC TLSConfig CA/cert/key, FIFO и symlink к FIFO | FAIL: все6 subprocess требуют watchdog2s; test12.02s | PASS: все6 отвергнуты; test0.08s; client/server regular symlinks работают | transport-grpc-manual-original-fail.log / transport-grpc-manual-final-pass.log |
| PKI VerifyCertificateFile, FIFO cert и FIFO key | FAIL: cert FIFO убит watchdog2s; исходный test остановился на первой ошибке | PASS: оба режима отвергнуты; test0.01s | pki-manual-original-fail.log / pki-manual-final-pass.log |
| Correct enrolled certificate через regular cert/key symlinks; UUID mismatch; oversized linked cert | До изменения отдельный positive fixture не запускался | PASS0.04s: valid UUID принят, другой UUID и >128KiB отвергнуты | pki-manual-final-pass.log |

Exact reproduction commands:

```powershell
# Linux environment: CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -o .cache/phase8-adapters/fifo-probe ./.cache/phase8-adapters/fifo-probe.go
wsl.exe -d docker-desktop -- sh /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/phase8-adapters/fifo-probe.sh

# Для package pki и transport/grpc; output name pki.test либо transport-grpc.test:
go test -c -o .cache/phase8-adapters/<name>.test ./internal/<package>
wsl.exe -d docker-desktop -- /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/phase8-adapters/<name>.test -test.run=TestManual -test.count=1 -test.v=true -test.timeout=30s

# Для syslogreceiver, telephony, pki: реальные FIFO + negative/positive symlinks:
go test -c -o .cache/phase8-adapters/<package>.test ./internal/<package>
wsl.exe -d docker-desktop -- /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/phase8-adapters/<package>.test -test.run=FIFO -test.count=1 -test.v=true -test.timeout=30s
```

`<package>/<name>` — явные перечисленные варианты команды, не одна реально выполненная literal команда с placeholder. FIFO shell создаёт только собственный mktemp directory; subprocess watchdog предотвращает блокирование test runner. Linux original/manual regressions выполнены до production fix и после него. Final grouped manual compile/run3.862s; последняя PKI positive/identity/bounds regression вместе с compile2.774s. Original FIFO tests для первых3 readers после fix PASS примерно0.01s каждый.

### Targeted regression

Exact final commands:

```sh
go test -count=1 ./internal/pki ./internal/transport/grpc ./internal/syslogreceiver ./internal/telephony ./internal/driver/redfish ./internal/driver/ipmi ./internal/model
go test -race -count=1 ./internal/pki ./internal/transport/grpc ./internal/syslogreceiver ./internal/telephony ./internal/driver/redfish ./internal/driver/ipmi ./internal/model
```

| Command / environment | Result | Command wall | Package times в порядке команды |
|---|---|---:|---|
| Normal Windows CGO0 | PASS | 5.831s | 2.036/2.001/2.953/2.161/1.863/1.335/0.827s |
| Race Windows CGO1 portable CC | PASS, race reports отсутствуют | 11.348s | 2.247/3.122/3.524/3.164/5.537/2.586/2.118s |

Logs: `targeted-manual-final-windows.log`, `targeted-manual-final-windows-race.log`. Это targeted проверки; полные repository regression/race отражаются отдельно root-agent.

Промежуточные failures не скрыты: первый targeted compile нового fuzz test использовал несуществующий `Observation`, исправлен на фактический `ObservationMetadata`, повтор PASS. Первый shell FIFO probe имел CRLF syntax failure; LF normalization исправила только ignored fixture. Два ранних Linux invocations имели argv quoting ошибки (`|` был обработан shell; unquoted PowerShell flags разбивались); финальная quoted argument array PASS. WSL warning про localhost proxy в NAT не повлиял на loopback fixtures.

### Fuzz/property campaigns

Каждая кампания: Windows CGO0, один worker, requested5s, общий timeout30s. Новые targets: same-origin bounded Redfish references; IPMI FRU/SDR decoding и authenticated guard; identity canonicalization и scoped topology identifiers/deep ownership. Existing Syslog/log parser targets переиспользованы. Dependencies не изменены. Нет I/O в новых decoder/reference fuzz targets; IPMI input ограничен4096/8192bytes. Fuzz worker shutdown может увеличить reported elapsed до6s; package/build overhead отдельно от requested workload.

| Exact command | Result | Executions | Package duration | Full command wall |
|---|---|---:|---:|---:|
| `go test ./internal/syslogreceiver -run=^$ -fuzz=^FuzzReadFrame$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 7604 | 6.881s | 10.672s |
| `go test ./internal/logs -run=^$ -fuzz=^FuzzSyslogParse$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 5973 | 7.736s | 9.321s |
| `go test ./internal/logs -run=^$ -fuzz=^FuzzStructuredDataBoundary$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 117779 | 6.808s | 8.752s |
| `go test ./internal/driver/redfish -run=^$ -fuzz=^FuzzRedfishResourceReference$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 33706 | 5.549s | 27.226s |
| `go test ./internal/driver/ipmi -run=^$ -fuzz=^FuzzIPMIStandardDecoders$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 38664 | 6.384s | 16.532s |
| `go test ./internal/driver/ipmi -run=^$ -fuzz=^FuzzIPMILANplusPacketGuard$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 33447 | 6.248s | 7.812s |
| `go test ./internal/model -run=^$ -fuzz=^FuzzIdentityCanonicalization$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 21108 | 7.519s | 9.265s |
| `go test ./internal/model -run=^$ -fuzz=^FuzzTopologyStableScopeAndOwnership$ -fuzztime=5s -parallel=1 -count=1 -timeout=30s` | PASS | 44805 | 7.194s | 8.761s |

Individual logs `<target>.log` сохраняют coverage/input counts. Cached corpus existing targets использован; new interesting corpus в ignored Go cache, failure corpus не появился. Short fuzz PASS не доказывает отсутствия vulnerabilities.

### Syslog multiple senders / pressure / lifecycle

`TestPhase8MultipleTLSSendersPressureRecoveryReplay`: четыре concurrent verified TLS senders плюс raw no-handshake и partial counted frame clients. MaxConnections8; actual peak6. Backend-offline queue fixture заполнен8 защищёнными CommandResult. Четыре ожидаемых syslog admission failures bounded; все8 protected records сохранены. Fixture lease/ACK освобождает capacity, четыре sender reconnect приняты durable. Receiver stop + outbox close/reopen сохраняют envelope/log IDs, certificate source attribution, real SourceRef и последующий ACK. Это actual TLS Receiver + durable queue fixture, без gRPC Backend; sender→Receiver не lossless. Проверка production real provenance здесь не является hardware evidence; synthetic stress отдельно использует simulated marker.

`TestPhase8TLSReceiverFiftyStartStopCycles`:50 verified TLS ingestion +50 slow no-handshake клиентов,50 start/stop; внутренние connection/source/peer states очищаются. Измерения после полного workload и GC, client/server делят один process. Нет утверждения об isolated Collector RSS/SLA.

```sh
go test -count=3 -run '^TestPhase8' -v ./internal/syslogreceiver
go test -race -count=3 -run '^TestPhase8' -v ./internal/syslogreceiver
# Linux CGO0 compile, затем:
go test -c -o .cache/phase8-adapters/syslogreceiver.test ./internal/syslogreceiver
wsl.exe -d docker-desktop -- /mnt/host/c/Users/kiril/GolandProjects/InfraPulseAgent/.cache/phase8-adapters/syslogreceiver.test -test.run=TestPhase8 -test.count=3 -test.v=true -test.timeout=30s
```

| Environment | Result | Command / package | Три50cycle elapsed | Max shutdown трёх samples | Stabilization |
|---|---|---|---|---|---|
| Windows normal | PASS | 4.768s /3.393s | 238.046/234.062/239.898ms | 528.5/549.6/544.4us | goroutines2→6→2; FD NOT AVAILABLE |
| Windows race | PASS | 5.851s /3.686s | 419.202/422.037/452.693ms | 0.518/1.006/1.008ms | goroutines2→6→2; no race |
| Linux | PASS | 2.585s grouped compile/run | 182.771/177.606/174.448ms | 301.665/169.840/127.847us | goroutines2→6→2; FD6→6 каждый sample |

Linux retained heap delta -39312/-101816/-26344bytes; TotalAlloc9808960/9811968/9822344bytes. Windows normal TotalAlloc9914864/9914192/9847896bytes; race10950240/10908328/10914688bytes. Девять pressure instances PASS, ожидаемые failures не скрыты как успешный приём. Logs `syslog-stress-windows.log`, `syslog-stress-windows-race.log`, `syslog-stress-linux.log`.

### Security/provenance границы

Existing tests сохраняют IPMI17 default/3 explicit/no0/no v1.5 downgrade, RAKP/HMAC/AES/session/sequence/read-only allowlist/socket cleanup. Точный pinned go-ipmi `v0.9.2-0.20261009051248-b3670df57943` primary source inspected locally; library/packet guard не заменялись. Redfish verified HTTPS/customCA/origin/resource/response bounds/session cleanup/secret redaction regression PASS. Syslog authenticated identity не берётся из HOSTNAME; RFC5425 и6587 primary documents сверены. Identity/Topology scoped facts, partial/failed semantics и deep ownership сохранены; local Asset merge не добавлен. PKI CRL/OCSP/revocation и serverCA rollover остаются documented unsupported boundaries. NTP работает через existing OS services; настройки/часы host не менялись.

### Independent final review (read-only)

- Scheduler pending — bounded coalesced state/job, fair nextJob cursor, existing bounded changed channel; Pause/Commit/Rollback/remove/cancellation не создают duplicate workers. Collector/onError должны соблюдать context. Новых P0/P1 не найдено.
- Durable queue fault tests проверяют actual bbolt allocation/commit error, failed ACK record+lease retention, capacity и8×128 concurrent mixed producers; crash cleanup использует existing helper, queue protocol не изменён. Новых P0/P1 не найдено.
- Stress использует actual adapter network fixtures, bounded workers/queue, hardware30s, inventory/topology5m; explicit simulated wrapping. Managed apply в stress прямой fixture, real B9stream отдельно existing E2E. Измерения включают fixtures в том же process.
- `phase8_admission_test.go`: девять payloads проходят actual Agent.push→durablePop→protobuf marshal/unmarshal→Mock decode→Contains→ACK; все SourceRef simulated/collector/target и envelope IDs проверены. Это mapping fixture без actual TCP claim. Topology48/256/512 budgets измерены, silent truncation не допускается. На момент peer review найден P2 test-strength gap: success branch не проверял actual retained size. После review добавлены assertions retained JSON/protobuf≤limit; final normal/race/Linux regressions PASS. Production bug не подтверждён.
- `release.ps1` и `release-checklist.md` повторно read-only review: пять CGO0 Linux architectures, explicit template configs без recursive private config/key copy, all docs, deployment files; perfile/archive SHA256; fixed HEAD timestamp и dirty source manifest; nonoverwrite staging directories; environment restoration; без Git mutation/tag/publish/install. На момент subagent review packaging ещё не выполнялся; actual root verification PASS описан в разделе24. Исторические links к api/internal source требуют repository; архив не содержит весь исходный код. Tar headers/permissions/timestamps host-specific, bit-identical archives не обещаны; deterministic binary сравнивается отдельно.
- Systemd/procd minimal account/permissions/restart/10s timeout/security directives соответствуют manual deployment guides; live daemon installation/procd не проверялись. NTP/firewall/services не менялись.
- Peer independent review production FIFO diffs PASS: bounded regular-file checks и administrator symlinks корректны в заявленной local trust boundary; verified TLS fields/UUID binding сохранены, новых P0/P1 не найдено.

Этот fragment не подменяет полный root report, repository full regression или Phase8B acceptance. Adapter scope tracked edits frozen; основная документация/исходники во время fragment preparation не менялись.
## Приложение D — Linux suite timings

Все24 suites PASS; exact package/crosscompile/run command записан в results-Linux.json.
WSL cwd каждого suite — internal/<package>; CGO0, integration tag, -test.count=1/-test.timeout=120s.

| Package | Compile seconds | Run seconds | Status |
|---|---:|---:|---|
| platform-linux | 0.901 | 4.051 | PASS |
| logs | 1.706 | 2.172 | PASS |
| queue | 1.842 | 12.206 | PASS |
| commands | 0.740 | 0.890 | PASS |
| protocol-snmp | 0.741 | 1.921 | PASS |
| driver-snmp | 0.722 | 0.295 | PASS |
| driver-routeros | 0.985 | 1.676 | PASS |
| driver-redfish | 2.075 | 1.376 | PASS |
| driver-ipmi | 2.071 | 0.642 | PASS |
| pki | 0.987 | 0.747 | PASS |
| platform-openwrt | 1.322 | 1.240 | PASS |
| agent | 2.993 | 22.797 | PASS |
| simulator | 1.140 | 0.867 | PASS |
| telephony | 0.810 | 1.638 | PASS |
| transport-grpc | 1.578 | 1.266 | PASS |
| config | 0.912 | 0.253 | PASS |
| platform | 0.590 | 0.194 | PASS |
| syslogreceiver | 0.831 | 1.523 | PASS |
| managedconfig | 0.936 | 0.784 | PASS |
| scheduler | 0.559 | 0.314 | PASS |
| remote | 0.944 | 0.717 | PASS |
| driver-telephony | 0.714 | 0.237 | PASS |
| protocol-routeros | 0.788 | 0.444 | PASS |
| model | 0.890 | 0.295 | PASS |

## Приложение E — дополнительные команды и evidence

| Exact command / platform | Result | Workload / duration / limitation |
|---|---|---|
| Windows `go test -count=50 -v -run '^TestOversizedLineBoundedDiscardRestart$' ./internal/logs` | PASS | 4.773s, после замены test-only helper |
| Windows portable CC `go test -race -count=20 -v -run '^TestOversizedLineBoundedDiscardRestart$' ./internal/logs` | PASS | 6.368s, no race report |
| Windows `go test -count=20 -v -run '^TestPendingDirectoryCleanupDoesNotMaskLockedFiles$' ./internal/agent`; тот же с `-race` | PASS | 1.029/1.742s, actual handle без FILE_SHARE_DELETE остаётся видимым failure |
| Windows `go test -count=200 -run '^TestFullSnapshotLifecycleAndRecovery$' ./internal/managedconfig` | PASS | 8.612s, примерно6000 atomic saves; strict cleanup не изменён |
| Windows `go test -overlay .cache/phase8-managed-handles-overlay.json -count=1 -v ./internal/managedconfig -run '^TestPhase8ManagedFileHandles$'` | PASS | 20.150s,500×5lifecycle/15000saves, no GC, disk handles1→1; positive held-handle control detected; ignored diagnostic fixture, не versioned regression |
| Windows `go test -count=1 -v ./internal/agent -run '^TestPhase8(SimulatedPayloadsSurviveDurableWireRoundTrip\|TypicalTopologyAdmissionBudget)$'` (regex с обычным alternation `\|`, без backslash в shell) | PASS | final2.222s,9payloads и48/256/512budgets |
| Тот же actual regex с `-race`/portable CC | PASS | final7.865s, bounded retained JSON/protobuf assertions |
| Windows `go test -count=1 -p=1 -tags=integration -run '^TestPhase8IdleDurableStartupShutdownResources$' -v ./internal/agent` | PASS | package2.824s,20startup/stopcycles; FD/RSS counters NOT AVAILABLE |
| Linux compiled integration Agent suite `-test.count=1 -test.v=true -test.timeout=120s` | PASS | idle20cycles2.16s внутри22.797s suite; Linux FD/proc counters доступны |
| Linux `sh -n deployments/openwrt/infrapulse-agent.init` | PASS | syntax only, не live procd |
| PowerShell `Language.Parser.ParseFile` обеих новых scripts | PASS | AST only; actual validation/packaging выполнены отдельно |
| PowerShell local Markdown link/anchor scanner | PASS |35files/351targets/5anchors, cache JSON; external URLs не проверены этим scanner |
| `git diff --check` | PASS | exit0; LF→CRLF advisories не whitespace defects |

Исходные temporary fixture compile/argv failures отражены в соответствующих
appendices. Linux scripts имеют LF. Общая среда/caches/CC описана в разделе3;
процессные/fault-injection и network E2E результаты различаются.

## Приложение F — actual changed files

- `deployments/openwrt/infrapulse-agent.init`
- `deployments/systemd/infrapulse-agent.service`
- `docs/collector-backend-e2e.md`
- `docs/deployment.md`
- `docs/openwrt.md`
- `docs/phase-8-report.md`
- `docs/production-deployment.md`
- `docs/real-hardware-validation.md`
- `docs/release-checklist.md`
- `docs/security.md`
- `docs/topology.md`
- `docs/vendor-support.md`
- `docs/validation.md`
- `internal/agent/phase8_admission_test.go`
- `internal/agent/phase8_lifecycle_integration_test.go`
- `internal/agent/phase8_stress_test.go`
- `internal/agent/reconnect_hardening_test.go`
- `internal/driver/ipmi/decoder_fuzz_test.go`
- `internal/driver/redfish/reference_fuzz_test.go`
- `internal/logs/tempdir_windows_test.go`
- `internal/managedconfig/fuzz_test.go`
- `internal/managedconfig/hardening_test.go`
- `internal/model/identifier_fuzz_test.go`
- `internal/pki/identity.go`
- `internal/pki/manager.go`
- `internal/pki/trust_file_linux_test.go`
- `internal/queue/crash_integration_test.go`
- `internal/queue/hardening_test.go`
- `internal/remote/managed_resources_test.go`
- `internal/scheduler/hardening_test.go`
- `internal/scheduler/scheduler.go`
- `internal/syslogreceiver/hardening_test.go`
- `internal/syslogreceiver/receiver.go`
- `internal/syslogreceiver/tls_file_linux_test.go`
- `internal/telephony/probe.go`
- `internal/telephony/tls_file_linux_test.go`
- `internal/transport/grpc/client.go`
- `internal/transport/grpc/hardening_test.go`
- `internal/transport/grpc/tls_file_linux_test.go`
- `internal/transport/grpc/tls_material_test.go`
- `README.md`
- `scripts/release.ps1`
- `scripts/validate-phase8.ps1`

## Independent final report review

Read-only review всех34sections/appendices и deployment/release/Backend/hardware guides PASS.
Исправлена неточность §17: новый nine-payload test не делает restart; restart/replay
проверяются отдельными existing network E2E. Команды, measurement scope, delivery
условия, local ownership, Git restrictions и8A/8B/Backend status separation подтверждены.
Финальный Go sourcefreeze:313files, hashchanges0 после регрессии.
