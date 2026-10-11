# Release candidate: воспроизводимая подготовка

Этот этап создаёт локальные artifacts. Публикация, Git tag/commit/push не
выполняются автоматически. Final release требует чистого сохранённого дерева;
Phase8 review candidate может использовать явный -AllowDirty с manifest.

## Проверки перед package

- [ ] Чистая утверждённая база и сохранённые Phase0–7; изучены diff/staged files.
- [ ] Пройден `scripts/validate-phase8.ps1 -Mode All -GoPath <go>` либо те же exact
  команды из [отчёта](phase-8-report.md); failures/skip не скрыты.
- [ ] build/vet/test-count1/race-count1/integration-count1-p1/mod verify PASS.
- [ ] LF-normalized gofmt/protobuf pinned reproducibility, YAML examples,
  documentation links и `git diff --check` PASS.
- [ ] Collector пяти architectures CGO0; Mock/Simulator/Enrollment amd64/arm64.
- [ ] Queue/replay/ACK/storage fault, managed LKG/lifecycle, TLS/adapters,
  provenance, resource/stress evidence сохранены.
- [ ] Нет открытых подтверждённых P0/P1; Backend/hardware limitations названы.
- [ ] Production PKI/config/credentials/state не упакованы вместе с examples.

## Команда и содержимое

PowerShell, Go1.27.1, native tar. Existing build matrix используется без нового
CI/CD или runtime dependency. Local caches можно задать согласно AGENTS.md.

```powershell
./scripts/release.ps1 -Version 0.8.0-rc1 -GoPath 'C:\Users\kiril\sdk\go1.27.1\bin\go.exe' -OutputDir .cache/release-0.8.0-rc1
```

До commit разрешён только review package с `-AllowDirty`; version/manifest
показывают HEAD плюс `-dirty`, а manifest содержит SHA256 всех source inputs.
Финальный package не должен иметь dirty=true. Output должен быть новым каталогом;
существующие staging directories не перезаписываются и не удаляются script.

Каждый linux/amd64, arm64, arm(GOARM7), mips/mipsle(hardfloat) archive содержит
CGO0 stripped `infrapulse-agent`, example configs, deployment files, README,
операционные guides, build-manifest.json и SHA256SUMS; общий SHA256SUMS фиксирует
hash archives. `-trimpath -buildvcs=false`, version/commit/build_time через existing
ldflags. Build time фиксирован timestamp Git HEAD, не текущим wall-clock.
Go version/target/CGO/dirty/source hashes записаны. Source manifest привязан к
working-tree байтам; CRLF/LF учитываются явно. Повтор на одинаковом source/toolchain
проверяется сравниванием binary SHA256. Tar/gzip headers/permissions/timestamps
зависят от packaging host; bit-identical archive не обещан.

Generated protobuf вручную не меняется. Pinned protoc33.5, protoc-gen-go1.36.12,
protoc-gen-go-grpc1.6.0; Windows checkout EOL сравниваются после LF normalization.
Wire fields/numbers и persistence formats не менялись в Phase8.

## Проверка artifacts и handoff

```sh
sha256sum -c SHA256SUMS
tar -tzf infrapulse-collector-0.8.0-rc1-linux-amd64.tar.gz
tar -xzf infrapulse-collector-0.8.0-rc1-linux-amd64.tar.gz
cd infrapulse-collector-0.8.0-rc1-linux-amd64
sha256sum -c SHA256SUMS
chmod 0755 ./infrapulse-agent
./infrapulse-agent version
./infrapulse-agent validate-config --config configs/config.collector.example.yaml
```

Проверять binary target в соответствующей среде; не запускать MIPS binary на x86.
Permissions файла после Windows tar необходимо нормализовать `install -m0755`
при Linux installation. После распаковки проверьте внутренний SHA256SUMS из
корня package. Package examples — template, не deployment config с настоящими
секретами. Они требуют approved endpoint/PKI/state paths.

Markdown guides включены целиком; ссылки на исходный Go/protobuf код требуют
checkout репозитория, который не входит в runtime archive. Build manifest
фиксирует source hashes; это не копия исходного кода.

Release notes связывают исправления с failing/passing regression и прикладывают
[8A/8B разделённый отчёт](phase-8-report.md). Перед rollout выполните manual Linux/
OpenWrt/hardware/Backend acceptance на выделенном environment. Upgrade/backup/
rollback — [deployment](production-deployment.md). Не заявлять production-ready
по одному package/cross-build/fixture PASS.
