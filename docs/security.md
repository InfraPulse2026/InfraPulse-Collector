# Безопасность InfraPulse Agent

Документ описывает фактическую реализацию P0. Agent собирает локальные данные и сам открывает исходящий gRPC bidirectional stream. Входящий management listener на Agent отсутствует. Mock Controller предназначен для разработки и не является production Backend.

## Транспорт

`security.mode: production` требует CA, certificate и private key. Agent проверяет цепочку и имя сертификата Controller; серверный transport Mock Controller требует и проверяет client certificate. Минимальная версия TLS — 1.2. `InsecureSkipVerify` не используется. Сертификаты и доверенные CA пока устанавливаются оператором вручную.

`security.mode: development` явно включает plaintext только для адреса `localhost` или loopback IP. В этом режиме transport не аутентифицирует Controller или Agent; доступный локальный процесс может подключиться к Mock Controller. Development listener нельзя публиковать на внешнем адресе. Это режим демонстрации, не production security.

mTLS удостоверяет владение сертификатом, подписанным доверенным CA. Привязка SAN/subject client certificate к заявленному `AgentID`, tenancy, operator authorization и отзыв доступа относятся к будущему Backend и в Mock Controller не реализованы. Persistent UUID сам по себе не является credential.

## Команды

В protobuf есть только типизированные interface и service actions. Agent не принимает shell command, CLI fragment, HTTP endpoint или произвольную программу. Любой initiator (`human`, `ai_suggested`, `automation`) проходит одинаковые проверки. Поле initiator не является доказательством human approval.

В P0 отсутствуют mutation executors. Команды проходят validation, проверку target и policy; запрещённые команды возвращают `rejected`, разрешённые типизированные команды — `unsupported`. `changed` остаётся false. `commands.enabled` по умолчанию false и не превращает P0 в исполнитель действий. Configuration apply/rollback, service writes, interface writes и arbitrary remote shell не реализованы.

Validation ограничивает timeout, размер идентификаторов, допустимые операции и возраст requested timestamp. Policy поддерживает `protected_devices`, `protected_interfaces`, `protected_services`. В P0 protection проверяется по переданным идентификаторам; перед появлением executor драйвер должен разрешить canonical interface ID и name aliases, проверить protection повторно непосредственно перед Apply и выполнить read-after-write verification. Защита bridge/bond/VLAN parent relationships и автоматическое определение management path пока отсутствуют. Management интерфейсы оператор должен явно пометить protected.

`idempotency_key` предусмотрен в модели. Persistent execution journal, bounded duplicate-result cache и гарантии exactly-once не реализованы. Добавление write actions требует admission control с резервом для результата, проверки duplicate identity и handling uncertain state после timeout.

## Identity и секреты

Agent ID — UUID, сохраняемый в `state_dir/agent-id`; каждый запуск получает отдельный session ID. Повреждённый identity file вызывает ошибку, а не заменяется новым ID. Файл создаётся с permissions 0600, directory — 0700 там, где ОС поддерживает эти mode bits. На Windows mode bits не заменяют ACL. State directory и private keys должны принадлежать пользователю сервиса и не быть доступны для изменения посторонним пользователям.

YAML читается с ограничением 1 MiB и строгой проверкой известных полей. Размер outbox, messages и число workers ограничены config. Пути CA/certificate/private key и state directory можно задать через документированные `INFRAPULSE_*` environment variables. P0 не реализует remote device credential provider; vendor drivers и SNMP community/password ещё не используются. Не помещайте credentials в management URL.

`DataSanitizer` очищает известные password/token/API key/community/authorization patterns в free-text event messages, error reports и command result errors перед отправкой. Это ограниченная redaction, не полная анонимизация или система предотвращения утечек. Sanitizer не исследует arbitrary binary data, все поля inventory, hostname или комментарии location. Диагностические выводы и config logging должны выбирать только безопасные поля, а не печатать весь config.

Agent не содержит LLM client и не отправляет данные cloud AI напрямую. Будущий Backend решает, допустим ли cloud AI и какие данные разрешены; передача события Agent не означает разрешения передавать его стороннему LLM.

## Очередь и доставка

P0 outbox хранится в RAM. Ограничения охватывают число сообщений, retained JSON envelope bytes и размер отдельного envelope. Telemetry может терять старые samples при переполнении; drop counters доступны в queue state/heartbeat. Command results имеют высший приоритет и не вытесняют уже принятые results. Если свободную ёмкость нельзя получить вытеснением менее важных сообщений, `Push` возвращает `ErrFull`; результат этой команды не принят в queue. Core обязан учитывать эту ошибку. In-memory priority не является гарантией доставки.

Перезапуск Agent теряет очередь. Успешный gRPC `Send` означает передачу сообщения transport, не durable commit на Backend. P0 не использует application delivery ACK, persistent outbox или гарантированное replay. Разрыв соединения может потерять сообщение или привести к повторному наблюдению. `message_id`, session ID и sequence позволяют будущему Backend обнаруживать повторы, но сами по себе не создают delivery guarantee. Логи и persistent cursors пока не реализованы.

Scheduler использует ограниченное число workers и запрещает overlapping runs одного job. Timeout передаётся collector через context; Go не может принудительно остановить collector, который игнорирует cancellation. Production adapters должны использовать context-aware I/O и ограничивать response sizes.

## Enrollment и дальнейшие шаги

One-time enrollment token, выпуск сертификатов, rotation, revocation и certificate-to-AgentID authorization — следующий этап integration с Backend. В P0 enrollment endpoint и CA workflow отсутствуют; никакой token не подменяет готовый client certificate.

Перед write-enabled deployment нужны реальные drivers, canonical protected-resource checks, bounded command admission/deduplication, verification, аудит и документированные delivery semantics. SNMPv2c будет read-only и не обеспечит шифрования community; production SNMPv3 requires отдельная проверенная implementation. Возможности, которых нет в текущем коде, остаются unsupported.
