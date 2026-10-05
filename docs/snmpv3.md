# SNMPv3 USM

Generic SNMP driver использует один существующий adapter для `v2c` и `v3`.
SYSTEM/ENTITY/IF-MIB/IF-X-MIB, rates и LLDP normalization не меняются от выбора
security mode. SNMPv3 не объявляет отдельную business capability и не добавляет
write operations.

```yaml
credentials:
  switch-v3:
    username_env: INFRAPULSE_SNMP_USER
    auth_password_env: INFRAPULSE_SNMP_AUTH
    privacy_password_env: INFRAPULSE_SNMP_PRIVACY
devices:
  - id: switch-01
    address: 192.0.2.10
    driver: snmp
    credential_ref: switch-v3
    snmp:
      version: v3
      security_level: authPriv
      auth_protocol: SHA256
      privacy_protocol: AES
      port: 161
      retries: 1
      # context_name: management
    polling:
      timeout: 5s
```

`version` по умолчанию `v2c`, чтобы существующие конфигурации продолжали работать.
При явном `v3` defaults: `authPriv`, `SHA256`, `AES` (AES128).
`authNoPriv` включается явно, требует username/auth password и запрещает privacy
algorithm/password reference. Это аутентификация без шифрования; использовать
такой режим следует только с учётом confidentiality management network.
`noAuthNoPriv` не поддерживается. Автоматического downgrade к v2c нет.

Алгоритмы соответствуют constants pinned
[GoSNMP v1.45.0](https://github.com/gosnmp/gosnmp/blob/v1.45.0/v3_usm.go):

| Назначение | Допустимые имена | Default для v3 |
|---|---|---|
| Authentication | MD5, SHA, SHA224, SHA256, SHA384, SHA512 | SHA256 |
| Privacy | DES, AES, AES192, AES256, AES192C, AES256C | AES |

`SHA` здесь означает библиотечный SHA1. MD5/SHA/DES доступны только при явной
настройке для compatibility и не становятся defaults. AES192/AES256 используют
Blumenthal key extension, AES192C/AES256C — Reeder; они не взаимозаменяемы.
Фактическая поддержка на устройстве зависит от firmware и настроек USM.
Выбор имени из этой таблицы не означает hardware validation.

Username и оба passwords разрешаются через `CredentialProvider` при каждом
новом exchange. Password длиной от 8 до 1024 bytes, username от 1 до 32 bytes.
Plaintext passwords в YAML запрещены strict schema. Credentials не включаются
в normalized inventory, protobuf, diagnostics или ошибки. GoSNMP packet logger
отключён; ошибки выходят как безопасные категории. Configured auth/privacy
password values также участвуют в sanitizer logs/envelopes.

Engine discovery и USM key localization выполняет GoSNMP. Exchange получает
новую bounded session и context deadline; cancellation/Close закрывают socket.
Adapter дополнительно отклоняет response с несовместимым version/USM/security
level, другим username, context или неверным OID. Discovery Report не становится
telemetry. Credentials остаются в process memory; zeroization/hardware-backed
key protection не реализованы.

GoSNMP global password cache отключён при initialization adapter: upstream map
не имеет retention/size bound и использует passphrase в cache key. Это исключает
рост такого cache при rotation credentials. Tradeoff — password-to-key/USM
localization требует CPU для каждого нового exchange. На constrained OpenWrt
нужно подбирать polling intervals и remote concurrency по фактическому runtime;
приемлемая CPU/RAM стоимость на конкретной firmware не подтверждена.

Offline tests обмениваются настоящими UDP BER/USM messages с локальным fixture:
authPriv/authNoPriv, SHA256/AES, SHA512/AES256C и явно заданные SHA/DES, engine
discovery, reconnect, неправильные username/auth/privacy secrets, timeout,
cancellation, malformed OID/context, downgrade и redaction. Существующие
SNMPv2c/driver/LLDP/rates regression tests сохранены. Это protocol fixtures,
не подтверждение конкретного switch или SNMP daemon. Реальные команды проверок
публикуются в [validation](validation.md).
