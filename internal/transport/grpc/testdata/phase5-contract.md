`phase5-agent.desc` is the protobuf FileDescriptorSet of
`api/proto/infrapulse/v1/agent.proto` at Phase 5 base commit `14abc4e`, including
the official duration/timestamp imports. It was generated using pinned protoc
33.5 with `--include_imports --descriptor_set_out`.

Compatibility tests compare every existing field and RPC with this baseline and
decode Phase 6 topology through a dynamic Phase 5 peer. This fixture is historical
and must not be regenerated from the current contract to make a test pass.
