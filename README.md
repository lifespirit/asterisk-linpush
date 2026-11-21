# asterisk-linphone-push

**Версия 0.3.0:** параллельный push на группу номеров через одну команду, единое ожидание и экспорт метрик Prometheus. См. раздел [v0.3.0](#whats-new-in-v030-parallel-multi-extension-wakeup-and-prometheus).

Небольшой self-hosted Go-сервис для пробуждения собранного своими ключами **Linphone Android** через **Firebase Cloud Messaging HTTP v1**. Вызывается из dialplan Asterisk через `${SHELL(...)}` и имеет API для управления push по номерам. Хранилище — **SQLite** (один локальный файл), поддерживает несколько устройств на один номер.

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

## Ограничения версии 0.2.0

- Только **Android + FCM**; APNs не реализован.
- AMI синхронизация реализована; настоящий hrouter ещё не тестировался новым Go-клиентом (проверка AMI раньше выполнялась Python-скриптом). Подтвердите фактический `ContactList.Uri` и `ObjectName` в статусе синхронизации.
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

## What's new in v0.3.0: parallel multi-extension wakeup and Prometheus

This release retains the **v0.2.0 SQLite schema, AMI synchronization, single-target CLI, and REST endpoints**. You can replace the binary and restart the unit without resetting the database. No schema migration is required.

### One SHELL call for multiple extensions

Instead of running separate `SHELL()` commands for each endpoint and waiting multiple times, make **one** call:

```bash
linpush wake --numbers 5:10:11 --wait 5
# Commas are also accepted: --numbers 5,10,11 (quote when used in dialplan)
```

The daemon sends Firebase messages for the **distinct target extensions concurrently**, with a shared limit of eight in-flight FCM calls across all requests. Multiple occurrences of one number are ignored. If two numbers share the same Firebase token, it is sent only once per batch. Maximum batch size: **32 input targets**. Strictly numeric extension names, 1–16 digits each, are supported.

**Wait semantics:** after all FCM HTTP requests in the batch have completed, the daemon waits only **once** for the specified number of seconds, and only if at least one FCM message was accepted. A target with push disabled or no active tokens does not delay the other targets. `sent` means FCM accepted the send request, **not** that a client has registered or answered.

Asterisk `extensions.conf` group example:

```ini
[linphone-ring-group]
exten => 500,1,NoOp(Ring group 5 10 11)
 same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --numbers 5:10:11 --wait 5)})
 same => n,NoOp(Push result ${PUSH_RESULT})
 same => n,Dial(${PJSIP_DIAL_CONTACTS(5)}&${PJSIP_DIAL_CONTACTS(10)}&${PJSIP_DIAL_CONTACTS(11)},30)
 same => n,Hangup()
```

`PJSIP_DIAL_CONTACTS` is resolved **after** the shared wait. Use actual endpoint names from your dialplan; a group with no callable contacts needs a suitable fallback route. For dynamic lists, use a trusted list or `FILTER(0-9:,${PUSH_TARGETS})` before shell interpolation and NEVER pass unsanitized SIP headers / caller ID into `SHELL()`.

Existing single-number invocation **still works**:

```bash
linpush wake --number 5 --wait 5
```

The Unix-socket endpoint `POST /wake` now also accepts:

```json
{"numbers":["5","10","11"],"wait_seconds":5}
```

The Bearer-protected REST API adds `POST /v1/wake` with the same body:

```bash
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS -X POST http://127.0.0.1:9099/v1/wake \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"numbers":["5","10","11"],"wait_seconds":0}'
```

Batch results include `status`, `numbers`, `results` (per-extension statuses), `sent`, `failed`, `invalidated`, and `wait_seconds`. The existing `/v1/extensions/{number}/wake` and `linpush wake --number` interfaces retain their original single-result format. The batch request fails on invalid data before any pushes are sent.

### Prometheus metrics

v0.3.0 adds a text exposition endpoint:

- URL: `http://127.0.0.1:9099/metrics`
- Method: `GET`
- Format: Prometheus text `version=0.0.4`
- Authorization: **Bearer token required** (same loopback binding as the main API).

For least privilege, set **`metrics_token_file`** in `/etc/linpush/config.json`, alongside the existing `api_token_file`:

```json
"metrics_token_file": "/etc/linpush/metrics-token"
```

Generate a *different* random token. This token only permits `/metrics`, **not** control actions (`/v1/extensions/...`). If `metrics_token_file` is omitted for backward compatibility, the admin API token is accepted for `/metrics` (not recommended). When configured, the admin token is not accepted on `/metrics`.

```bash
openssl rand -hex 32 | sudo tee /etc/linpush/metrics-token >/dev/null
sudo chown root:linpush /etc/linpush/metrics-token
sudo chmod 0640 /etc/linpush/metrics-token
sudo systemctl restart linpush

# Test without exposing credentials to process arguments beyond the local curl process:
METRICS_TOKEN="$(sudo cat /etc/linpush/metrics-token)"
curl -fsS -H "Authorization: Bearer $METRICS_TOKEN" \
  http://127.0.0.1:9099/metrics
```

Prometheus `scrape_configs` example: `examples/prometheus.yml`. Prometheus must be able to read the credentials file. For a local Prometheus daemon:

```bash
sudo install -Dm640 -o root -g prometheus /etc/linpush/metrics-token /etc/prometheus/linpush-metrics-token
```

Do not expose this endpoint unauthenticated or open `127.0.0.1:9099` directly to the internet. For Prometheus on another server, route scrapes through a VPN/reverse proxy with TLS and authorization.

**Metric families** (all have bounded label cardinality; no extension numbers, IPs, FCM tokens, or device identifiers):

| Metric | Type | Meaning |
|---|---|---|
| `linpush_up` | Gauge | Process available |
| `linpush_wake_inflight` | Gauge | Requests in progress |
| `linpush_wake_requests_total{mode,status}` | Counter | Single/batch requests by result |
| `linpush_wake_duration_seconds` | Histogram | Total request duration including FCM sends and wait |
| `linpush_wake_wait_seconds` | Histogram | Configured successful post-FCM wait |
| `linpush_wake_targets` | Histogram | Count of distinct target numbers per request |
| `linpush_fcm_messages_total{outcome}` | Counter | Accepted, failed, or UNREGISTERED sends |
| `linpush_ami_connected` | Gauge | AMI connection state |
| `linpush_ami_last_success_timestamp_seconds` | Gauge | Last successful complete AMI snapshot |
| `linpush_ami_contacts` | Gauge | Last complete AMI contact count |
| `linpush_ami_fcm_contacts` | Gauge | FCM contacts found in last complete AMI snapshot |
| `linpush_ami_sync_total{result}` | Counter | Completed snapshots and disconnect/errors |
| `linpush_devices{source,state}` | Gauge | Active/inactive manual/asterisk device records |
| `linpush_extensions{push_enabled}` | Gauge | Extensions with push enabled/disabled |

Counters and histograms reset when the daemon restarts; use `rate()` / `increase()` in Prometheus. Inventory gauges reflect the current SQLite state. Some useful queries:

```promql
rate(linpush_fcm_messages_total{outcome="failed"}[5m])
rate(linpush_fcm_messages_total{outcome="unregistered"}[1h])
linpush_ami_connected
(time() - linpush_ami_last_success_timestamp_seconds) > 180
histogram_quantile(0.95, sum by (le) (rate(linpush_wake_duration_seconds_bucket[5m])))
```

Metrics cover the **Go daemon** only; they do not assert that an Android handset received FCM, performed SIP REGISTER, or answered the call. For end-to-end observability, add Asterisk registration/call metrics separately.
