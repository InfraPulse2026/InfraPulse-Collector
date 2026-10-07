# Корпоративный NTP: системное время Collector

Backend host, все Collector hosts и network/server equipment, где это возможно,
следует синхронизировать с одной корпоративной NTP hierarchy. NTP servers
предоставляет организация; публичные servers или fallback без решения
администратора не добавляются.

Стандартная служба ОС синхронизирует системные часы. Collector продолжает
использовать обычное системное время для `observed_at` и `collected_at`.
Source timestamp, сообщённый устройством/log source, сохраняется отдельно.
NTP protocol/server, virtual clock, manual offsets, clock history и
Collector ↔ Backend clock exchange отсутствуют. Backend NTP — deployment
requirement, а настройка NTP удалённых устройств не входит в эту фазу.

## Configuration contract

```yaml
time_sync:
  enabled: true
  servers:
    - ntp1.company.local
    - ntp2.company.local
```

Секция принадлежит существующей strict YAML configuration:

| Поле | Contract |
|---|---|
| `enabled` | Boolean; default `false` |
| `servers` | Список 1–16 servers при enabled; пустой список допустим при disabled |
| Server | IPv4, IPv6 literal без brackets/zone или DNS hostname; URL, port, whitespace/control characters и command options недопустимы |

Supplied servers проверяются и при `enabled: false`: disabled не разрешает
invalid values или список длиннее 16 entries. Unspecified/multicast IP
недопустимы; IPv4-mapped IPv6 нормализуется к IPv4.

IP приводится к канонической форме, DNS — к lower case без завершающей точки;
нормализация удаляет повторения с сохранением первого порядка. Lookup NTP/DNS
не требуется для проверки config. Неуказанные servers не заменяются public
fallback. Invalid configuration отклоняется обычным `validate-config`/startup
до любых system changes. Готовый development example —
[config.time-sync.example.yaml](../configs/config.time-sync.example.yaml).

## Deployment apply и runtime status

На Linux сначала установите подходящий NTP client стандартными средствами
администратора и запустите единственную выбранную службу. Затем выполните:

Перед первым запуском нового OS client администратор проверяет его исходные
sources и заменяет distro/public defaults корпоративными. Helper не считает
package defaults решением организации о выборе NTP hierarchy.

```sh
infrapulse-agent validate-config --config /etc/infrapulse/config.yaml
sudo infrapulse-agent apply-time-sync --config /etc/infrapulse/config.yaml
infrapulse-agent time-sync-status --config /etc/infrapulse/config.yaml
```

CLI использует тот же полный config, включая существующую security validation.
`apply-time-sync` требует прав записи system config и restart выбранной службы.
Он не устанавливает пакеты, не включает службы при boot и не переключает
конкурирующие clients. Inactive service требует administrator start.
Обычный systemd service остаётся от `infrapulse`, с прежним sandbox.
Deployment apply ограничен общим timeout 15 секунд; после failed restart,
включая истечение apply context, отдельный recovery budget до 2 секунд
предназначен для восстановления owned file и той же службы.

`run` делает один read-only status check с общим timeout до 5 секунд; запись
config и restart на startup отсутствуют. Статус попадает в существующий runtime
diagnostic snapshot. Он описывает момент проверки; для свежего состояния
используйте `time-sync-status`. Collector не ждёт synchronization: standard
OS client продолжает работу, monitoring запускается и при warning.
Поведение одинаково при Local Mode и `local.enabled: false`.

`enabled: false` отключает opt-in integration и не удаляет ранее созданные
managed files. Отключение интеграции и rollback OS configuration — разные
administrative действия.

JSON status различает `available`, `active`, `configured` и optional
`synchronized`: `null` означает, что ОС не подтвердила synchronized state.
`configured_servers` — наблюдённые servers, `desired_servers` — trusted YAML.
`managed_file`, `changed`, `diagnostic` и `checked_at` описывают operation;
это не NTP history и не observation clock-quality score.

## Linux provider selection

Выбор опирается на фактические systemd units и их состояние. Приоритет:
active chrony → active systemd-timesyncd → available chrony → available
systemd-timesyncd. Так installed inactive chrony не вытесняет уже работающий
timesyncd. Поддерживаются
service names `chrony.service`/`chronyd.service` и
`systemd-timesyncd.service`. Helper не создаёт собственный daemon и не запускает
inactive NTP client. При активных конфликтующих clients apply отказывается от
изменений; администратор сам выбирает единственную службу.

Отсутствие supported provider, systemd status interface или безопасной
configuration integration возвращает понятный unavailable/unsupported
diagnostic. Наличие бинарника не считается доказательством active service или
достигнутой synchronization.

### Chrony

Main administrator config не заменяется. Рассматриваются реальные варианты
`/etc/chrony.conf` и `/etc/chrony/chrony.conf`, если выбор однозначен; explicit
`-f` path определяется из `/proc/<MainPID>/cmdline` работающего chronyd.
Запись выполняется только в отдельный InfraPulse-managed fragment,
подключённый существующим `confdir`, `sourcedir` или подходящим `include`.
Имя — `90-infrapulse.conf`, для `sourcedir` — `90-infrapulse.sources`.
Допустим только persistent location под `/etc`; dynamic/DHCP sourcedir,
неподдерживаемые/неоднозначные include layouts требуют administrator setup.
`.conf`/`.sources` semantics
описаны в [chrony 4.7 manual](https://chrony-project.org/doc/4.7/chrony.conf.html#confdir).

Если безопасный persistent fragment нельзя определить, helper возвращает
unsupported и не редактирует main file. Администратор может подключить
отдельный каталог своей deployment configuration, например:

```text
confdir /etc/chrony/infrapulse.d
```

Этот путь — пример administrator setup, не обязательный distro path.
Настройки server в управляемом fragment имеют вид:

```text
# InfraPulse-managed time synchronization; edit local Collector config, not this file.
server ntp1.company.local iburst
server ntp2.company.local iburst
```

Existing source вне desired corporate list, `refclock` или `local` mode
приводят к `setup_required` до записи: helper не дописывает corporate servers
к конфликтующим sources и не удаляет настройки администратора. DHCP fragments
и нестандартный layout требуют отдельного administrator review.
Состояние synchronization читается через standard chronyc interface;
CSV/report semantics — [chronyc 4.7](https://chrony-project.org/doc/4.7/chronyc.html).
Collector не использует отчёт для собственной timestamp correction.

### Systemd-timesyncd

Managed drop-in:
`/etc/systemd/timesyncd.conf.d/90-infrapulse.conf`.
Main `/etc/systemd/timesyncd.conf` сохраняется. Drop-in содержит:

```ini
# InfraPulse-managed time synchronization; edit local Collector config, not this file.
[Time]
NTP=
NTP=ntp1.company.local ntp2.company.local
FallbackNTP=
FallbackNTP=ntp1.company.local ntp2.company.local
```

Пустые assignments сбрасывают предыдущие global NTP/fallback lists, затем
обеим lists назначаются те же corporate servers. Только пустое `FallbackNTP=`
не гарантирует отсутствие compiled-in defaults: в implementations systemd
v252/v257 требуется явный corporate fallback list. Такое поведение проверено
по [systemd v257 configuration parser](https://github.com/systemd/systemd/blob/v257/src/timesync/timesyncd-conf.c).
Файл не содержит public fallback. Runtime verification требует desired
SystemNTPServers и отсутствие noncorporate LinkNTPServers/FallbackNTPServers.
При noncorporate `LinkNTPServers` helper
возвращает `setup_required` до записи; networkd/DHCP policy администратор
настраивает отдельно. Later drop-ins могут изменить effective servers;
post-apply verification сообщает несоответствие.
См. [systemd timesyncd.conf](https://github.com/systemd/systemd/blob/v257/man/timesyncd.conf.xml).
Status использует machine-readable systemd/timedatectl properties, а не
formatted `timedatectl status` output; интерфейс документирован в
[timedatectl](https://github.com/systemd/systemd/blob/v257/man/timedatectl.xml).

## Idempotency, ownership и rollback

Managed file явно обозначен как InfraPulse-managed. Helper сравнивает desired
content с existing content; совпадение не создаёт файлы и не перезапускает
service. После изменения restart выполняется только для выбранной active
службы. Existing file без ownership marker не перезаписывается. Чужие main
files, fragments и settings не удаляются.

Для rollback администратор сохраняет предыдущий managed file перед изменением
deployment, затем восстанавливает его либо удаляет только созданный
InfraPulse-managed file и перезапускает ту же службу. Main administrator
configuration не требует восстановления. Удаление fragment может вернуть
прежние OS/vendor sources, поэтому их соответствие corporate policy нужно
проверить. При failed restart helper восстанавливает прежний owned file либо
удаляет созданный файл и пытается восстановить службу повторным restart.
Обнаруженные concurrent changes owned file, rollback failure и failed recovery
явно сообщаются. Deployment apply и administrator edits должны выполняться
последовательно: первая публикация использует no-clobber hard link, обновление
существующего файла — ownership/content check и atomic rename, а не CAS.
Это не atomic OS transaction. Если restart успешен, но effective runtime servers
не совпадают с desired, новый file сохраняется и возвращается error для
administrator review. Неизменный managed file при runtime mismatch также
не вызывает repeated restart: recovery делает администратор.

## OpenWrt administrator setup

OpenWrt integration читает `system.ntp` и procd `sysntpd`; automatic UCI apply
отсутствует. Standard sysntpd использует одну administrator-owned секцию
`system.ntp`. Безопасного отдельного managed fragment для этой секции нет.
Active service не доказывает synchronization, поэтому synchronized state
остаётся unknown. [Sysntpd v24.10.0](https://github.com/openwrt/openwrt/blob/v24.10.0/package/utils/busybox/files/sysntpd).

Перед изменением проверьте firmware, `uci show system.ntp`, другие NTP clients
и сохраните `/etc/config/system` для rollback. Следующий administrator script
предполагает уже существующую named section `system.ntp=timeserver`, не допускает
pending чужих changes и коммитит/restarts только при отличии desired state:

```sh
set -eu
[ "$(uci -q get system.ntp)" = timeserver ] || exit 1
[ -z "$(uci changes system)" ] || exit 1
changed=0
if [ "$(uci -q get system.ntp.server)" != "ntp1.company.local ntp2.company.local" ]; then
    if uci -q get system.ntp.server >/dev/null; then
        uci delete system.ntp.server
    fi
    uci add_list system.ntp.server='ntp1.company.local'
    uci add_list system.ntp.server='ntp2.company.local'
    changed=1
fi
if [ "$(uci -q get system.ntp.use_dhcp)" != 0 ]; then
    uci set system.ntp.use_dhcp='0'
    changed=1
fi
if [ "$(uci -q get system.ntp.enabled)" != 1 ]; then
    uci set system.ntp.enabled='1'
    changed=1
fi
if [ "$changed" = 1 ]; then
    uci commit system && /etc/init.d/sysntpd restart
fi
```

`use_dhcp=0` исключает дополнение списка DHCP NTP servers в этом sysntpd setup.
Другие settings секции не меняются. При command failure не запускайте script
дальше; проверьте `uci changes system` перед commit. Для rollback администратор
восстанавливает свои прежние UCI NTP values и выполняет commit/restart.
Схема read-only JSON подтверждена [rpcd UCI](https://github.com/openwrt/rpcd/blob/master/uci.c).
Firmware/hardware runtime и real NTP synchronization этим примером не проверены.

## Failure behaviour и безопасность

| Ситуация | Результат |
|---|---|
| Invalid server / enabled без servers | Обычная configuration error до system apply |
| Нет supported service / safe chrony fragment | Clear unsupported/unavailable; system configuration не заменяется |
| Service inactive / конфликт clients | Administrator action требуется; helper не запускает/отключает clients |
| Permission denied | Apply возвращает error; `run` продолжает monitoring с diagnostic при недоступном status |
| NTP server unreachable / synchronization ещё нет | Warning/status unsynchronized или unknown; ожидания на startup нет |
| Status timeout / malformed OS response | Bounded unavailable diagnostic; raw process output не передаётся |

NTP servers приходят только из trusted local config. SNMP/LLDP/Syslog и remote
device timestamps не могут изменить host time settings. Backend-managed NTP
configuration, remote-device NTP management и clock-quality scoring отсутствуют.
Unit/provider tests используют isolated files и command fixtures: реальные часы
test machine и connection к NTP servers не нужны. Выполненные regression и
runtime checks записываются отдельно в [validation](validation.md).
