# Сбор Linux logs и auditd

Agent читает явно настроенные обычные файлы. Наличие rsyslog/auditd и их paths
не предполагается: отсутствующий источник имеет состояние `unavailable`,
остальные collectors продолжают работу. При восстановлении файла чтение
возобновляется. Agent не запускает rsyslog/auditd и не меняет audit rules.
Journald API, сетевой syslog listener, OpenWrt `logread` и распаковка архивов
не входят в этот file-based pipeline.

Пример конфигурации:

```yaml
logs:
  interval: 1s
  timeout: 2s
  max_line_bytes: 8192
  max_batch_records: 64
  max_batch_bytes: 131072
  max_read_bytes: 262144
  sources:
    - id: system
      path: /var/log/syslog
      format: syslog
    - id: audit
      path: /var/log/audit/audit.log
      format: auditd
```

`format` принимает `syslog`, `auditd` и `raw`. Paths разрешаются относительно
YAML-файла. Настраивайте только существующие на вашем хосте источники; Linux
может использовать `/var/log/messages`, `/var/log/syslog`, собственный template
или вовсе не иметь file logs. Один stable `id` соответствует одному path и
формату. Для смены path/формата используйте новый source ID либо выполните
явную процедуру восстановления cursor state.

Отдельный scheduler logs имеет одного worker. Один poll обрабатывает не более
`max_read_bytes` байтов строк, включая discard oversized lines, и отправляет
не более одного `LogBatch`, ограниченного числом records и сериализованным
размером. Read-ahead и дополнительные anchor reads не входят в этот byte budget;
их работа ограничена числом records/files/chunks и context deadline.
Очередь Controller не заставляет reader
ждать отправку по сети. Размер batch должен помещаться в `queue.max_message_bytes`
вместе с envelope metadata. Нет отдельной неограниченной очереди строк.

Каждая запись сохраняет stable source ID, local device ID, фактический source
(`linux-file` либо `auditd`), observation time, identity файла, byte offset,
sanitized raw record и message. ID записи вычисляется из source ID, file identity,
поколения файла и offset; повтор после сбоя сохранения cursor имеет тот же ID.
Host/process/PID, severity/facility и исходный timestamp добавляются только
при подтверждённом parsing.

Поддерживаются RFC5424, обычный BSD/RFC3164 header и rsyslog file format с
RFC3339 timestamp. BSD timestamp не содержит год и timezone, поэтому Agent
не выдумывает абсолютное время: сохраняются raw record и observation time.
Custom/malformed формат передаётся как sanitized text без guessed fields.
Некорректный UTF-8 заменяется безопасным Unicode replacement character.

Строка длиннее `max_line_bytes` передаётся один раз как redaction marker
(обычно `[OVERSIZED LOG LINE REDACTED]`) с `truncated=true`; её содержимое
не отправляется.
Это исключает публикацию неполного secret value, который уже не совпадает
с полным configured credential. Остаток такой строки пропускается bounded
chunks, а discard state сохраняется между polls и restart. Короткая строка
без завершающего newline в текущем файле ожидает продолжения; последний
fragment файла, который уже заменён новым, может передаваться при его drain.
Если одна запись превышает byte limit batch из-за JSON escaping, вместо её
содержимого передаётся marker `[LOG RECORD EXCEEDS BATCH LIMIT; REDACTED]` с
`truncated=true`. Такая запись не блокирует последующие logs. Normalized fields,
выходящие за wire bounds, остаются unavailable; sanitized raw record сохраняется
в пределах допустимого размера.
Если sanitizer увеличил строку сверх `max_line_bytes`, содержимое заменяется
целым redaction marker и `truncated=true`, без обрезки marker или secret value.

## Persistent cursors и rotation

Cursor хранится в `state_dir/log-cursors/<SHA256(source ID)>.json` с permissions
0600; directory использует 0700 там, где ОС поддерживает POSIX permissions.
Windows permissions не заменяют ACL. В cursor нет raw log text и credentials:
сохраняются source/path/format, file identities, offsets, поколение, bounded
hash anchor, discard state и флаг подавления private key block.

Linux identity — device/inode и immutable birth time из `statx`, если kernel
и filesystem предоставляют `STATX_BTIME`; Windows fixture identity —
volume/file index и creation time. Birth time различает delete/recreate даже
при немедленном повторном использовании inode. При недоступности `STATX_BTIME`
identity явно содержит `no-btime`; равенство повторно использованного inode
и одинакового содержимого тогда нельзя доказательно отличить от прежнего файла.
Offset дополнен SHA256 последних до 64 bytes перед ним.
Уменьшение размера или изменение anchor распознаёт truncate/copytruncate
и overwrite после regrowth; offset сбрасывается, generation увеличивается,
появляется `log.truncate`. Если файл переписан совершенно одинаковыми bytes
и успел вырасти между polls, это нельзя достоверно отличить от append только
по snapshot обычного файла. Никакая гарантированная обнаруживаемость всех
copytruncate операций не заявляется.

При rename/recreate старый descriptor сохраняется и его tail читается перед
новыми файлами. После restart старый файл ищется по identity среди несжатых
siblings `basename.*` в том же directory. Уже существовавшие rotations при
первом подключении источника не читаются как исторический архив. Новые
unseen rotations после начала наблюдения могут быть прочитаны, что помогает
при нескольких rename между polls. При copytruncate отдельная копия имеет
новую identity и может повторить ранее прочитанные records; равенство текста
не используется как эвристическое доказательство уже доставленных данных.

В памяти удерживается максимум восемь descriptors на источник. Scanner читает
directory порциями и ограничивает scan 1024 entries и 64 несжатыми rotated
files. При превышении tracking limit pipeline сообщает ошибку и по возможности
drains уже открытые файлы, освобождая capacity для следующих polls. Полностью
исчезнувший unread файл после restart сопровождается `log.source.gap` и
diagnostic error; его данные восстановить невозможно. Удаление/сжатие rotations
до чтения и неограниченное число файлов не маскируются гарантией доставки.

State записывается через temporary file, `Sync`, закрытие и rename в том же
directory; Linux дополнительно synchronizes directory. Зависимость от
crash guarantees конкретного filesystem сохраняется. Schema/version/path,
identity membership, offsets, anchor и bounds проверяются при загрузке.
Повреждённый state останавливает создание pipeline с явной ошибкой и никогда
не заменяется пустым cursor автоматически.

Для восстановления остановите Agent, сохраните повреждённый cursor отдельно,
исправьте state по достоверным данным либо вручную переместите его из
`log-cursors` и запустите Agent заново. Отсутствующий cursor начинает чтение
текущего файла с начала и допускает replay. Осознанное удаление state может
потерять возможность найти ранее tracked rotations; перед этим сохраните
нужные log files. Не удаляйте cursor во время работающего reader.

## Доставка

Discovery identities с нулевыми offsets сохраняются до первой попытки batch,
чтобы отказ RAM outbox не позволил забыть исходный файл после rotation.
Read offset продвигается только после успешного принятия соответствующего
`LogBatch` в текущий bounded RAM outbox. Отказ admission сохраняет прежние
offsets и приводит к повторному чтению. После admission cursor синхронно
сохраняется; если persistence не удалась, новые reads прекращаются до успешной
повторной записи. Crash между admission и cursor persistence допускает replay
с теми же record IDs.

Persistent cursor не является durable outbox. Crash после успешного сохранения
cursor, последующее queue eviction либо Send error после Pop могут потерять
уже прочитанные logs. Успешный gRPC Send не подтверждает сохранение Backend.
P2 использует best-effort delivery с возможными пропусками и повторами;
exactly-once и durable delivery отсутствуют. Persistent outbox с application
ACK относится к P3.

## Audit и чувствительные данные

Auditd передаёт отдельные sanitized raw records. Agent сохраняет точный
timestamp/serial audit event ID, record type, хорошо определяемые uid/auid/pid,
quoted executable, числовой syscall, success и exit result. Значение
`auid=4294967295` сохраняется буквально; числовой syscall не переименовывается
без architecture mapping. Конфликтующие duplicate fields остаются unknown.

Корреляция ограничена одним batch: записи группируются по точному event ID,
source ID, device ID и node. Records не объединяются эвристически; group может
быть partial, продолжаться в другом batch либо после restart. Agent не делает
SIEM/AI inference и не создаёт security event из произвольного текста.

Перед отправкой очищаются известные password/token/community/authorization
patterns, Bearer/Basic headers и configured secret values. PEM private key
blocks подавляются построчно, флаг сохраняется через restart и rotation.
Literal replacement выполняется за один проход; повторная очистка не увеличивает
markers. Если configured value пересекается с обычным marker, используется
безопасный альтернативный символ. Если ни один marker не подходит, текст
подавляется целиком. Содержимое marker не является стабильным wire field.
Literal replacements выполняются за один проход; вставленные markers не
раздуваются повторными replacements. Если configured credential совпадает
с обычным marker либо его частью, выбирается безопасный альтернативный
символ. Повторная sanitation сохраняет уже очищенный bounded payload.
В diagnostics находятся только bounded source states и безопасные error
categories. Sanitizer не гарантирует обнаружение неизвестных секретов,
универсальную анонимизацию или декодирование arbitrary encoded payloads.
Защищайте доступ service user к logs, audit records и state directory.

Parser основан на [RFC5424](https://www.rfc-editor.org/rfc/rfc5424.html),
[RFC3164](https://www.rfc-editor.org/rfc/rfc3164.html),
[rsyslog templates](https://docs.rsyslog.com/doc/reference/templates/templates-type-string.html)
и [upstream audit-userspace ausearch semantics](https://github.com/linux-audit/audit-userspace/blob/master/docs/ausearch.8).
