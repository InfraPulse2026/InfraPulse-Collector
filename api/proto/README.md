# InfraPulse wire contract

The authoritative contract is `infrapulse/v1/agent.proto`. Generated Go sources
in `api/gen` are produced by official protoc and plugins and are checked in, so
ordinary builds do not require protoc.

Pinned generators:

- protoc 33.5 from the official protocolbuffers/protobuf release;
- protoc-gen-go v1.36.12;
- protoc-gen-go-grpc v1.6.0.

Windows bootstrap and regeneration from the project root:

```powershell
./scripts/generate-proto.ps1 -Bootstrap
./scripts/generate-proto.ps1
```

The Windows bootstrap verifies the downloaded release archive's pinned SHA-256.
Pass `-Go <path-to-go.exe>` if the selected Go SDK is not in PATH. On Linux, use
`sh scripts/generate-proto.sh` with official protoc 33.5 installed, including its
standard protobuf include files.

Timestamps represent UTC instants. Missing CPU usage on the first sample is
distinct from measured zero; CPU usage, memory pressure and interface speed are
optional scalar fields, while rates have message presence. Agent envelopes and
controller commands have typed oneof payloads. Unknown command enums or absent
actions are rejected; no shell command action exists.

P0 sends one normalized telemetry sample in each `TelemetryBatch`. The wire
batch supports multiple samples for a future Backend. The P0 `DecodeEnvelope`
adapter intentionally rejects multi-sample batches rather than dropping samples.
Inventory/events can already contain multiple entries. Enrollment, topology,
logs and other future messages will be added explicitly as their collectors are
implemented; P0 advertises no unsupported collector as working.
