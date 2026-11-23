# asterisk-linphone-push

**Русский** · [English README](README.md) · [История изменений](CHANGELOG.md) · [Патч Asterisk 24](patches/asterisk24-linpush-qualify.patch)

Сервис поддерживает **три независимых режима ожидания**: фиксированную задержку после FCM, ожидание нового SIP REGISTER и ожидание свежего SIP OPTIONS/REGISTER с патчем Asterisk 24. Старый режим полностью сохранён.

Небольшой self-hosted Go-сервис для пробуждения собранного своими ключами **Linphone Android** через **Firebase Cloud Messaging HTTP v1**. Вызывается из dialplan Asterisk через `${SHELL(...)}` и имеет API для управления push по номерам. Хранилище — **SQLite** (один локальный файл), поддерживает несколько устройств на один номер.

## Три режима пробуждения

| Режим | Команда | Действие | Условие завершения |
| --- | --- | --- | --- |
| **1. Всегда FCM с задержкой** | `--wait 5` | Отправляет push, даже если телефон уже зарегистрирован | 5 секунд после принятой отправки |
| **2. FCM + проверка регистрации** | `--wait-register 15` | Всегда отправляет push | Свежий SIP REGISTER или тайм-аут |
| **3. OPTIONS + FCM** | `--wait-ready 15 --push-grace-ms 300` | Для существующего FCM-контакта сначала инициирует OPTIONS. Если нет подтверждённого ответа за 300 мс — отправляет FCM. При отсутствии контакта FCM немедленно | Новый подтверждённый OPTIONS 2xx от нужного FCM-контакта, новый REGISTER или общий тайм-аут |

```bash
# Режим 1 (работает и без доступного AMI, если токены уже записаны):
linpush wake --number 5 --wait 5

# Режим 2 (нужен AMI для определения нового REGISTER):
linpush wake --number 5 --wait-register 15

# Режим 3 (патч Asterisk 24 + AMI):
linpush wake --number 5 --wait-ready 15 --push-grace-ms 300

# Тот же режим для группы; один общий лимит времени:
linpush wake --numbers 5:10:11 --wait-ready 15 --push-grace-ms 300
```

Без указания режима используется `default_wait_seconds` из конфигурации — это **режим 1**. Указывать два режима одновременно нельзя, включая сочетание `--wait 0` с другим режимом. `--push-grace-ms` допустим только с `--wait-ready`. Максимальное время определяется `max_wait_seconds`.

При `--wait-ready` старый статус `Reachable` **не** считается подтверждением доступности: нужен именно новый ответ OPTIONS. Для этого наш патч Asterisk выдаёт AMI-событие `LinpushQualifyResult` с `ActionID`, `AOR`, `ContactID`, `SIP2xx`. Сервис проверяет соответствие номера и FCM-контакта, поэтому ответ другого устройства не завершит ожидание. Отрицательный ответ, отсутствие события или неподтверждённый OPTIONS приводят к FCM и ожиданию REGISTER/тайм-аута. `--push-grace-ms 0` запускает OPTIONS и FCM практически одновременно. Время `--wait-ready` включает задержку перед FCM.

**Важно:** `qualify_frequency=30` в Asterisk не удаляет SIP-регистрацию при засыпании Linphone, а только помечает контакт как `Unreachable`. Поэтому сохранённый `Reachable` также не гарантирует, что Android сейчас способен принять INVITE. Режим 3 предназначен именно для новой проверки.

### Пример маршрута Asterisk (выбери ОДНУ строку SHELL)

```asterisk
; Режим 1:
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait 5)})
; Режим 2:
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait-register 15)})
; Режим 3:
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait-ready 15 --push-grace-ms 300)})
; После выбранной строки:
same => n,NoOp(Linpush: ${PUSH_RESULT})
same => n,Dial(${PJSIP_DIAL_CONTACTS(5)},30)
```

В реальном dialplan эти три строки **не идут подряд**. Вызов `PJSIP_DIAL_CONTACTS()` должен выполняться **после** ожидания. Динамические номера очищай через `FILTER()`, не передавай в `SHELL()` сырые SIP-заголовки.

## Архитектура

- `linpush serve`: постоянно работающий демон. HTTP API — только `127.0.0.1:9099`, авторизация Bearer-токеном. Внутренний Unix-сокет `/run/linpush/linpush.sock` доступен только владельцу и группе `asterisk`.
- `linpush wake --number 101 --wait 5`: короткая CLI-команда Asterisk. Посылает запрос в демон, ждёт результат отправки FCM **и затем** ещё 5 секунд, прежде чем выйти.
- SQLite: `extensions` (номер, разрешение отправки), `devices` (номер, стабильный device_id, токен FCM, `pn_param`, active, timestamp, last_error). FCM-токены автоматически обновляются через AMI; ручной `PUT` остаётся аварийным механизмом. Невалидные FCM-токены отключаются при `UNREGISTERED`.
- AMI Synchronizer: при запуске, по `ContactStatus` и каждые 60 секунд делает `PJSIPShowContacts`, сохраняет **только** FCM контакты, не удаляет токен при исчезновении SIP-регистрации.
- FCM: **data-only**, `android.priority=HIGH`, `ttl=45s`, поля данных в стиле Linphone/Flexisip. Когда указан `call_id`, он передаётся в `data["call-id"]`.

**ВАЖНО про SIP Call-ID.** При типовом сценарии «`SHELL()` → пауза → `Dial()`» будущий исходящий INVITE ещё не сформирован, поэтому его фактический Call-ID обычно неизвестен. В этом случае отправляется push **без** `call-id` — Linphone SDK принимает его как пустую строку и пытается пробудить приложение. Этот режим *best-effort*: он работет, Linphone после пробуждения действительно делает новый REGISTER и Asterisk вызывает новый Contact. Но это хак, для строго корректного RFC 8599 поведения (согласование Call-ID и отложенный INVITE) нужен более тесный SIP proxy/ARI/пред-dial контур, а не только этот CLI.

## Требования

- Arch Linux: `go`, `gcc`, `pkgconf`, `sqlite` (у `sqlite` установлен заголовок `sqlite3.h`).
- Скомпилированный Android Linphone с **вашим** `google-services.json` и включёнными push-настройками учётной записи.
- Отдельный **Firebase Service Account JSON** (Firebase Console → Project settings → Service accounts → Generate new private key), с разрешением отправлять FCM через HTTP v1. Это **НЕ** `google-services.json`. В проекте Firebase должен быть включён Cloud Messaging API.
- Asterisk `res_pjsip`, пользователь/группа `asterisk` (если другая — скорректируйте systemd и `socket_group`).

Сервису нужен исходящий HTTPS к `oauth2.googleapis.com` и `fcm.googleapis.com`.

## Сборка

```bash
sudo pacman -S --needed go gcc pkgconf sqlite
make test
make build
sudo install -Dm755 linpush /usr/local/bin/linpush
```

Нет сторонних Go-модулей: используем системную SQLite через cgo. Поэтому при сборке обязательно `CGO_ENABLED=1`. Запуск готового двоичного файла не требует установленного Go; нужен `libsqlite3`.

Для GitLab включён `.gitlab-ci.yml`, тесты и бинарный artifact (Linux AMD64).

## Установка

```bash
sudo groupadd -r linpush
sudo useradd -r -g linpush -d /var/lib/linpush -s /usr/bin/nologin linpush
sudo install -d -o root -g linpush -m 0750 /etc/linpush
sudo install -m 0640 -o root -g linpush examples/config.json /etc/linpush/config.json

# Скопировать собственный server-side Firebase Service Account JSON:
sudo install -m 0640 -o root -g linpush /path/to/service-account.json /etc/linpush/firebase-service-account.json

# Создать секрет для REST API (32 байта случайного значения => 64 hex-символа):
openssl rand -hex 32 | sudo tee /etc/linpush/api-token >/dev/null
sudo chown root:linpush /etc/linpush/api-token
sudo chmod 0640 /etc/linpush/api-token

# Отдельный read-only токен для Prometheus (/metrics)
openssl rand -hex 32 | sudo tee /etc/linpush/metrics-token >/dev/null
sudo chown root:linpush /etc/linpush/metrics-token
sudo chmod 0640 /etc/linpush/metrics-token

# Только для AMI: локальный секрет к существующему пользователю linpush
sudo install -m 0640 -o root -g linpush /path/to/ami-password /etc/linpush/ami-password

sudo install -m 0644 systemd/linpush.service /etc/systemd/system/linpush.service
sudo systemctl daemon-reload
sudo systemctl enable --now linpush
sudo systemctl status linpush
```

**Обязательно:** до запуска измените `firebase_project_id` и `firebase_project_number` в `/etc/linpush/config.json`. `firebase_project_id` — строковый ID проекта, `firebase_project_number` — числовой **Project Number** из Firebase / `pn-param` из SIP REGISTER. Можно оставить `firebase_project_id` пустым: сервис возьмёт его из Service Account JSON. Если `firebase_project_number` пустой, проверка совпадения `pn_param` будет отключена.

Убедитесь, что группа `asterisk` существует и Asterisk работает от её имени. Systemd создаст `/var/lib/linpush/` и `/run/linpush/`. База создаётся автоматически с правами 0600, Unix-сокет — 0660, группа `asterisk`.

Проверить:

```bash
curl -s http://127.0.0.1:9099/healthz
sudo -u asterisk /usr/local/bin/linpush wake --number 101 --wait 0
```

Для входа в HTTP API нужен Bearer-токен:

```bash
TOKEN="$(sudo cat /etc/linpush/api-token)"
API=http://127.0.0.1:9099
```

### 1. Включение и отключение push для номера

```bash
# Включить
curl -fsS -X PUT "$API/v1/extensions/101" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"push_enabled":true}'

# Выключить, например из бота
curl -fsS -X PUT "$API/v1/extensions/101" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"push_enabled":false}'

# Посмотреть номера
curl -fsS "$API/v1/extensions" -H "Authorization: Bearer $TOKEN"
```

Этот API можно вызывать из Stoat-бота или Home Assistant. Для удалённого доступа используйте защищённый TLS reverse proxy / VPN, не открывайте сам HTTP-порт в интернет.

## Автоматическая синхронизация из Asterisk AMI (v0.2.0)

Мы подтвердили на Asterisk hrouter, что `PJSIPShowContacts` возвращает **7 контактов**, а номер **5** содержит все параметры `pn-prid`, `pn-provider=fcm`, `pn-param=750067000448`. В этой версии они читаются автоматически.

В `manager.conf` должен быть уже настроен пользователь `linpush`:

```ini
[linpush]
secret = YOUR_AMI_PASSWORD
read = system,command
write = system
deny = 0.0.0.0/0.0.0.0
permit = 127.0.0.1/255.255.255.255
```

AMI слушает только локальный `127.0.0.1:5038`. В Asterisk желательно включить событие обновления регистрации:

```ini
[global]
type=global
send_contact_status_on_update_registration=yes
```

Не создавайте второй `[global]`, если раздел уже есть. После изменения `pjsip.conf` используйте `asterisk -rx 'pjsip reload'`; для `manager.conf` — `asterisk -rx 'manager reload'`.

В `/etc/linpush/config.json`:

```json
"ami_enabled": true,
"ami": {
  "address": "127.0.0.1:5038",
  "username": "linpush",
  "password_file": "/etc/linpush/ami-password",
  "sync_interval_seconds": 60,
  "single_contact_aors": ["5"]
}
```

`single_contact_aors` следует указывать **только** там, где реально настроено `max_contacts=1`. Для номера 5 это подтверждено твоим выводом Asterisk. Если несколько SIP-устройств на одном AOR, не включайте для него этот режим: иначе новое автоматическое устройство отключит токены других устройств.

При синхронизации:

1. Полный снимок `PJSIPShowContacts` должен завершиться `ContactListComplete`, а число `ListItems` совпасть с числом полученных контактов. Иначе **никаких изменений** в SQLite не вносим.
2. Из `ContactList.Uri` парсим только `pn-provider=fcm`, `pn-prid` и `pn-param`. Если `firebase_project_number` задан, параметр должен совпасть. Секреты никогда не выводим в log.
3. Если доступен `+sip.instance`, он используется как основа устойчивого ID; иначе ID — хеш токена. Если при ротации токена нет стабильного идентификатора, старые автоматические записи остаются, кроме явно настроенного одноконтактного AOR.
4. Отключенная настройка `push_enabled` **не включается автоматически**. Новые номера создаются выключенными, пока пользователь/бот не разрешит push.
5. Исчезновение SIP-регистрации **не удаляет** токен: он нужен именно для пробуждения спящего приложения. Полностью недействительные токены отключаются при ответе Firebase `UNREGISTERED`. При следующем REGISTER с тем же отклонённым токеном они автоматически не оживают.
6. Когда Asterisk недоступен, работает последняя сохранённая база. AMI переподключается автоматически. События `ContactStatus` ускоряют синхронизацию, а регулярный опрос компенсирует пропущенные события.

Проверка статуса (без выдачи полного токена):

```bash
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS http://127.0.0.1:9099/v1/sync -H "Authorization: Bearer $TOKEN"
# Ожидается seen_contacts: 7, fcm_tokens: 1 (при наличии той же регистрации)

curl -fsS http://127.0.0.1:9099/v1/extensions/5/devices -H "Authorization: Bearer $TOKEN"
# source: "asterisk"; полный FCM-токен скрыт
```

Если хочешь реально отправлять push номеру 5, включи его отдельно:

```bash
curl -fsS -X PUT http://127.0.0.1:9099/v1/extensions/5 \
 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
 -d '{"push_enabled":true}'
```

**Приоритет:** сначала пробуем активные токены `source=asterisk`; при отсутствии или полном провале отправки используем `source=manual` как аварийный резерв. Если FCM подтвердил отправку хотя бы одному автоматическому устройству, ручной резерв не отправляется.

### 2. Резервное ручное добавление FCM-токена

В обычной работе это **не требуется**: синхронизатор забирает токены из Asterisk. Если AMI временно недоступен, для аварийного ввода из SIP `REGISTER` Linphone ищите `pn-provider=fcm`, `pn-prid=<FCM TOKEN>`, `pn-param=<FIREBASE PROJECT NUMBER>` и `+sip.instance` (UUID устройства). Используйте стабильный `device_id` для данного устройства, например UUID из `+sip.instance` **без** `urn:uuid:`. При новом токене повторяйте тот же `PUT`, и старая запись будет заменена.

```bash
curl -fsS -X PUT "$API/v1/extensions/101/devices/android-main" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"token":"PASTE_REAL_FCM_TOKEN_HERE","pn_param":"123456789012"}'
```

Измените `pn_param` на номер своего Firebase-проекта. Если номер ещё не был добавлен, запись номера создаётся автоматически **с выключенными push**. Отдельно вызовите `PUT /v1/extensions/101` для включения.

```bash
# Устройства, без раскрытия самих токенов
curl -fsS "$API/v1/extensions/101/devices" -H "Authorization: Bearer $TOKEN"

# Удаление устройства
curl -fsS -X DELETE "$API/v1/extensions/101/devices/android-main" \
  -H "Authorization: Bearer $TOKEN"
```

### 3. Проверка отправки

```bash
# Через CLI (так Asterisk будет вызывать сервис)
linpush wake --number 101 --wait 5

# Через HTTP (для тестов/бота)
curl -fsS -X POST "$API/v1/extensions/101/wake" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"wait_seconds":0}'
```

Пример ответа:

```json
{"status":"sent","number":"101","sent":2,"failed":0,"invalidated":0,"wait_seconds":5}
```

Возможные `status`: `sent`, `disabled`, `no_devices`, `send_failed`. При выключенном номере или отсутствии устройств CLI завершается **сразу**, не ожидая таймера. `sent` означает **принятие сообщения FCM**, но не гарантированную доставку или новый REGISTER.

По умолчанию задержка 5 секунд; максимальная по конфигурации — 30. `wait_seconds:0` отключает ожидание. При ошибке `UNREGISTERED` FCM отключает запись конкретного устройства (`active=0`), её можно восстановить повторным `PUT` с новым токеном.

## Интеграция Asterisk

Скопируйте пример `examples/extensions.conf` в нужный dialplan-контекст. Минимально:

```ini
[linphone-users]
exten => _XXX,1,NoOp(Wake Linphone ${EXTEN})
 same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number ${FILTER(0-9,${EXTEN})} --wait 5)})
 same => n,NoOp(Linphone push result: ${PUSH_RESULT})
 same => n,Dial(${PJSIP_DIAL_CONTACTS(${EXTEN})},30)
 same => n,Hangup()
```

`${PJSIP_DIAL_CONTACTS(...)}` вычисляется **после** ожидания, что важно для получения обновлённых регистраций. Если новых контактов не появится, Dial не сможет дозвониться. Для модели `SHELL() → Dial()` пользователь-звонивший ждёт 5 секунд **до начала SIP вызова**; при необходимости отправляйте ringing/progress отдельно.

Не передавайте в `SHELL()` непроверенные `CALLERID(...)` и заголовки SIP: это может привести к инъекции команд оболочки. Для номера применяется `FILTER(0-9,...)`, а сам Go-сервис принимает только цифровые номера.

## API

| Метод | Путь | Назначение |
|---|---|---|
| `GET` | `/healthz` | Проверка процесса, без авторизации |
| `GET` | `/v1/sync` | Статус AMI, дата последней успешной сверки, количество FCM |
| `GET` | `/v1/extensions` | Список номеров и числа активных устройств |
| `GET` | `/v1/extensions/101` | Настройки номера |
| `PUT` | `/v1/extensions/101` | `{"push_enabled":true/false}` |
| `GET` | `/v1/extensions/101/devices` | Метаданные устройств, без полного токена |
| `PUT` | `/v1/extensions/101/devices/android-main` | `{"token":"...","pn_param":"..."}` |
| `DELETE` | `/v1/extensions/101/devices/android-main` | Удалить устройство |
| `POST` | `/v1/extensions/101/wake` | Разовый push, опциональное `wait_seconds` |

Все эндпоинты кроме `/healthz` требуют `Authorization: Bearer <API_TOKEN>`. HTTP API слушает только loopback. Путь `/wake` внутреннего Unix-сокета не доступен через публичный HTTP.

## Схема SQLite

```sql
CREATE TABLE extensions (
    number TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
CREATE TABLE devices (
    number TEXT NOT NULL REFERENCES extensions(number) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    token TEXT NOT NULL,
    pn_param TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual',
    last_seen TEXT NOT NULL DEFAULT '',
    contact_hash TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (number, device_id)
);
```

Для резервного копирования при работающем сервисе используйте SQLite `.backup` (или остановите сервис и копируйте БД). Файл `.sqlite3` нельзя безопасно копировать без WAL во время записи. Сервис не сохраняет полный FCM-токен в журнал и не выдаёт его через REST API.

## Обновление с v0.1.0

Сначала сделайте резервную копию SQLite при остановленном сервисе:

```bash
sudo systemctl stop linpush
sudo cp -a /var/lib/linpush/linpush.sqlite3 /var/lib/linpush/linpush.sqlite3.before-v0.2
```

Замените бинарник и unit-файлы, добавьте `ami_enabled` и `ami` в **существующий** `config.json` (не перезаписывайте свои Firebase настройки), создайте `/etc/linpush/ami-password` и запустите сервис. При открытии БД автоматически добавятся три поля (`source`, `last_seen`, `contact_hash`) с сохранением всех старых данных. У всех старых записей `source=manual`, то есть они станут резервными. Права на файл копии БД должны оставаться ограниченными — там находятся FCM-токены.

## Общие ограничения

- Только **Android + FCM**; APNs не реализован.
- AMI-синхронизацию и три режима нужно проверить на действующем Asterisk и телефоне; автоматические Go-тесты не заменяют реальную SIP/FCM-проверку.
- При ротации FCM-токена без `+sip.instance` сервис не может достоверно отличить устройство от нового. Для `max_contacts=1` используйте `single_contact_aors`; для многоконтактных AOR старые токены остаются до `UNREGISTERED` либо ручной очистки.
- Удаление приложения не обязательно вызывает SIP unregister; токен может оставаться до ответа Firebase `UNREGISTERED`.
- API применяется к **целому номеру**; отдельная настройка push по устройствам пока только `active=0` на ошибках FCM.
- FCM data push требует разрешения на обработку background сообщений со стороны клиента. Убедитесь, что в Linphone реально разрешены push и разрешены фоновые вызовы Android.
- FCM `200 OK` значит лишь, что сервер Google принял уведомление. Это не гарантия, что Android проснулся или Asterisk нашёл новый контакт.

## Тесты

```bash
go test -race ./...
go vet ./...
```

Тесты изолированы от настоящего Firebase и Asterisk: проверяют API, SQLite, включение/выключение номера, ожидание, FCM HTTP v1 с тестовым OAuth2, обработку `UNREGISTERED`, парсинг AMI URI, фиктивный список из 7 PJSIP-контактов и отклонение неполной синхронизации.

---

## Многопользовательский вызов и Prometheus (v0.3.0)

Одна команда `linpush wake --numbers 5:10:11 --wait 5` отправляет FCM всем подходящим номерам параллельно. Допустимы разделители двоеточие или запятая; повторяющиеся номера и токены удаляются из пакета. Максимум 32 входных номера, до восьми одновременных FCM-отправок во всём сервисе. Для `--wait` ожидание начинается после завершения всех FCM-запросов, но выполняется **один раз**, а не по числу абонентов.

```asterisk
[linphone-ring-group]
exten => 500,1,NoOp(Ring group 5, 10, 11)
 same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --numbers 5:10:11 --wait 5)})
 same => n,NoOp(${PUSH_RESULT})
 same => n,Dial(${PJSIP_DIAL_CONTACTS(5)}&${PJSIP_DIAL_CONTACTS(10)}&${PJSIP_DIAL_CONTACTS(11)},30)
 same => n,Hangup()
```

HTTP API `POST /v1/wake` принимает `{"numbers":["5","10","11"],"wait_seconds":5}`. Для второго режима используй `"wait_register_seconds":15`, для третьего — `"wait_ready_seconds":15,"push_grace_ms":300`. Эти поля взаимоисключающие. Результат содержит общий статус, число отправок, список результатов по номерам и причину окончания ожидания.

Метрики Prometheus доступны по `GET /metrics` с отдельным Bearer-токеном из `metrics_token_file`. Пример сбора — [examples/prometheus.yml](examples/prometheus.yml). Метрики включают FCM-отправки, длительность запросов, состояние AMI, количество FCM-контактов, результаты ожидания REGISTER, изменение push через IVR. Номера телефонов и FCM-токены в метки метрик не попадают.

```bash
METRICS_TOKEN="$(sudo cat /etc/linpush/metrics-token)"
curl -fsS -H "Authorization: Bearer $METRICS_TOKEN" http://127.0.0.1:9099/metrics
```

## AMI и обработка REGISTER (v0.4.0)

Для второго и третьего режимов настрой `ami_enabled=true`, действительные AMI-учётные данные и в уже существующем глобальном разделе `pjsip.conf`:

```ini
[global]
type=global
send_contact_status_on_update_registration=yes
```

Asterisk отправляет `ContactStatus: Updated` при обновлении SIP REGISTER; линейный синхронизатор также сравнивает полные снимки `PJSIPShowContacts` и запрашивает их во время ожидания. Исходный снимок фиксируется **до** FCM. Обычный `Reachable` от периодического OPTIONS не завершает ожидание регистрации. Если AMI недоступен, режим 2 ждёт до тайм-аута, а не считает старую регистрацию свежей.

Результаты CLI: `registration=registered`, `registration=options` или `registration=timeout`. `sent` означает, что FCM принял запрос, а не что телефон ответил. Если push отключён или нет активных токенов, команда завершает работу без искусственного ожидания.

## Патч Asterisk 24 для третьего режима (v0.4.1)

В Asterisk 24 штатный AMI `PJSIPQualify` подтверждает **запуск** проверки, а не результат SIP-ответа. При повторном успешном OPTIONS сохранённый статус `Reachable` может не измениться, поэтому события `ContactStatus` недостаточно. Наш [asterisk24-linpush-qualify.patch](patches/asterisk24-linpush-qualify.patch) добавляет события `LinpushQualifyResult` с уникальным ID, реальным SIP 2xx и конкретным идентификатором контакта.

Патч применяется к исходникам Asterisk до сборки, например в `prepare()` PKGBUILD. После изменения нужно установить новый пакет и перезапустить процесс Asterisk. Это отдельный патч от исправлений встроенного PJProject.

Для соответствующих AoR рекомендуется (добавить в **существующий** раздел, а не создавать дубль):

```ini
[5]
type=aor
max_contacts=1
qualify_frequency=30
qualify_timeout=2.0
qualify_2xx_only=yes
```

В `manager.conf` пользователь `linpush` должен иметь право на AMI `PJSIPQualify` и чтение событий `system`. Пример находится выше в разделе AMI.

## Локальное IVR *98 и уведомления

`linpush control` даёт возможность получить статус или включить/отключить push без передачи API-токена в dialplan:

```bash
sudo -u asterisk /usr/local/bin/linpush control status 5
sudo -u asterisk /usr/local/bin/linpush control enable 5
sudo -u asterisk /usr/local/bin/linpush control disable 5
```

Файл [examples/extensions.conf](examples/extensions.conf) содержит `[linpush-ivr]` для `*98`: **1** — статус, **2** — включить, **3** — выключить, **0** — выйти. Доступ разрешён только проверенному цифровому `CHANNEL(endpoint)`, совпадающему с собственным SIP-номером. Контекст подключается **только** доверенным внутренним абонентам, не транкам.

```bash
sudo pacman -S --needed espeak ffmpeg
bash scripts/generate-ivr-audio.sh ./ivr-ru
sudo install -d -m755 /var/lib/asterisk/sounds/ru/custom
sudo install -m644 ivr-ru/linpush-*.wav /var/lib/asterisk/sounds/ru/custom/
asterisk -rx 'dialplan reload'
```

Путь к звуковому каталогу — пример; проверь путь твоего Asterisk. Файлы — WAV PCM mono 8 kHz, голос синтезатора eSpeak простой и роботизированный.

## Проверка трёх режимов на собранном Asterisk

После установки новой Go-версии и перезапуска демона проверь синхронизацию AMI, затем по очереди три режима. Пробное выполнение `linpush wake` не создаёт телефонный звонок: для проверки входящего вызова нужен `Dial()` в dialplan.

```bash
sudo systemctl restart linpush
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9099/v1/sync
sudo -u asterisk /usr/local/bin/linpush control status 5

sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait 2
sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait-register 10
sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait-ready 15 --push-grace-ms 300
```

На активном Linphone в режиме 3 ожидается `ready sent=0 ... registration=options` — если подтвердился новый OPTIONS до FCM. На спящем клиенте ожидается `sent sent=1 ... registration=registered` или `registration=timeout`, если телефон не проснулся. `disabled` и `no_devices` указывают на настройки токена и разрешения, а не успешную проверку доступности.

Журнал демона:

```bash
sudo journalctl -u linpush -f
```

Во время тестирования можно включить `asterisk -rx 'pjsip set logger on'` (после теста выключи), чтобы видеть SIP OPTIONS и REGISTER. Не публикуй лог целиком без удаления секретов SIP-аутентификации и FCM.

## Обновление и безопасность

Схема SQLite для трёх режимов не меняется; при обновлении **не** перезаписывай свой `config.json` и приватные Firebase-ключи. HTTP работает только на loopback, Unix-сокет доступен владельцу и доверенной группе. FCM `200 OK` подтверждает принятие сообщения Google, но не доставку на Android. REGISTER и OPTIONS не гарантируют успешный последующий INVITE; реальный SIP-вызов проверяется отдельно.

**Английская документация:** [README.md](README.md).
