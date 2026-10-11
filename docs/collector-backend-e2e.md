# Collector и Backend: contract и E2E acceptance

Production Backend находится в другом репозитории; его source code здесь не
меняется. Real Backend/PostgreSQL/REST endpoints в Phase8 environment отсутствуют.
Статус real Backend integration — NOT AVAILABLE. Development Mock E2E использует
настоящее TCP/gRPC/mTLS, но его receipt ACK не является PostgreSQL commit.

## Совместимость и handoff

ProtocolVersion1, frozen field numbers и generated protobuf сохранены. Backend
читает AgentHello/HelloAck, heartbeat/version/capabilities, inventory/telemetry/
hardware, logs/audit, topology/evidence и source/time metadata. Phase6 legacy
wire compatibility fixtures и Phase7 descriptor/digest contract остаются tests.
Схема: [protocol](protocol.md), [Identity/Topology handoff](backend-identity-topology-contract.md),
[B9 typed managed configuration](backend-managed-config-contract.md).

Capabilities означают реализованные функции: managed_config_v1 negotiates typed
full snapshots по аутентифицированному production mTLS stream; локальный opt-in
и policy обязательны. PKI/NTP/security/receiver/state/credential values не входят
в remote model. Backend не присылает arbitrary command JSON вместо DesiredConfig.
Wrong Collector UUID или process Session ID, stale revision, digest conflict,
forbidden destination/reference/protocol отвергаются. Не отправляйте новые
messages старому peer без negotiation.

## Воспроизводимые automated fixtures

```sh
go test -count=1 -v ./internal/agent -run 'Test(Phase6AdaptersIdentityTopologyDurableReplayACK|ManagedConfiguration|Durable|AgentSyslog|AgentRedfish|AgentIPMI)'
go test -count=1 -v ./internal/transport/grpc ./internal/mockcontroller
go test -count=1 -p=1 -tags=integration ./...
```

Точные выполненные commands/platform/duration и names находятся в
[Phase8 report](phase-8-report.md). Mock сценарии: offline→reconnect, retained
original IDs→restart→replay/ACK, authenticated typed add/update/disable/remove
пяти adapters, invalid snapshot с продолжающейся active configuration, duplicate/
stale/LKG restore, wrong Collector/session binding и две certificates/identities.
Phase 6, новые stress и provenance fixtures явно маркируют synthetic данные
как simulated. Некоторые существующие wire/policy fixtures проверяют default
real marker настоящего adapter в изолированной среде. Ни один обмен с loopback
fixture не является evidence проверки реального оборудования.

## Real integrated scenario

На отдельном test environment Backend developer предоставляет verified CA/SAN,
Collector UUID enrollment/client certificate workflow, gRPC endpoint, sandbox
tenant, PostgreSQL access/commit evidence и REST URLs/auth. Secrets передаются
закрытым каналом, не вставляются в этот guide. Не придумывайте REST endpoints:
используйте фактическую schema именно deployed Backend revision.

1. Создайте A/B с отдельными state/cert/UUID и read-only targets. Backend должен
   привязать каждый stream к certificate UUID, tenant, Hello и current Session ID.
2. Подтвердите negotiated application_ack и managed_config_v1; запишите версии.
3. Сопоставьте `(collector_id,message_id)` для каждого payload с durable rows и
   REST representation. SourceRef/subject/origin, real/simulated, subtype/raw ID,
   complete/partial/failed должны сохраняться. received_at выставляет Backend;
   source_timestamp/observed_at/collected_at не заменяются им.
4. ACK отправляется лишь после durable transaction commit и dedup. Прервите
   test connection между commit/ACK: retained replay должен иметь прежний ID,
   новое gRPC соединение и одну committed semantic record согласно Backend
   dedup. Process Session ID сохраняется при reconnect и меняется только после
   Collector restart; duplicates на wire допустимы, exactly-once delivery не обещан.
5. DesiredConfig revision10→11→12→13, valid/invalid/stale/conflict/duplicate,
   Collector restart/LKG policy change. Сравните received/validating/terminal
   status с фактически active revision и desired state; FAILED restore не APPLIED.
6. Wrong UUID/session snapshot A→B отвергается, B outbox/revision не меняются.
   Один physical device у A/B остаётся двумя source observations до Backend merge.
7. Backend outage плюс Syslog и monitoring: bounded queue/drop diagnostics,
   reconnect/replay/ACK; receiver sender-hop limitations фиксируются отдельно.
8. STOP/restart, expired/wrong CA/name certificate, absent hardware и partial
   topology должны давать honest status, не silent TLS downgrade/graph deletion.

## Точные внешние gaps

| Gap / owner | Что требуется до integrated PASS |
|---|---|
| Backend application ACK | PostgreSQL commit/dedup прежде ACK; retry lost ACK; bind ACK к authorized stream/Collector |
| Backend B9 | normalization/digest-v1 vector, persistent monotonic revision history, full snapshots, negotiate capability, bind UUID/session, reconcile terminal status/LKG |
| Backend B3/B7 | scoped IdentityEvidence/TopologyObservation, partial-failure semantics, source-aware global entity resolution вне Collector |
| Backend REST/schema | actual routes/auth/payload mapping и real persisted evidence; отсутствующие newer fields оформить как Backend gap |
| PKI operations | production CA/token tenancy/revocation/CRL/OCSP/rotation policy; Collector revocation механизм здесь не реализован |
| Infrastructure | real devices/BMC/NTP/rsyslog, durable filesystem/power failure, OpenWrt firmware resource budget |

Если Backend не поддерживает B9/new fields при сохранённом wire contract,
это integration gap, не автоматически дефект Collector. Evidence содержит
deployed revisions, exact requests/commands (sanitized), expected/actual и
status PASS/FAIL/NOT AVAILABLE. Mock Controller не закрывает эти gaps.
