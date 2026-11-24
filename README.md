# asterisk-linphone-push

**English** · [Русская документация](README.ru.md) · [Changelog](CHANGELOG.md) · [Asterisk 24 OPTIONS patch](patches/asterisk24-linpush-qualify.patch)

A self-hosted Go service that wakes a custom-built **Linphone for Android** with Firebase Cloud Messaging (FCM HTTP v1). It integrates with **Asterisk PJSIP** through an inexpensive CLI and a local Unix socket. Registered FCM tokens are discovered through Asterisk Manager Interface (AMI) and stored in SQLite.

The project provides **three selectable wake modes**; none requires changing your existing dialplan until you explicitly choose the mode.

## Three wake modes

| Mode | Command | Action | Wait ends when | Requirements |
| --- | --- | --- | --- | --- |
| **1. Fixed delay (legacy)** | `--wait 5` | Always sends FCM if enabled and a token exists | The configured post-send delay expires | FCM; no AMI required to send saved tokens |
| **2. REGISTER-aware** | `--wait-register 15` | Always sends FCM, then watches for a fresh registration | A fresh FCM REGISTER is observed, or timeout | AMI synchronization, strongly recommended |
| **3. OPTIONS + FCM (patched Asterisk)** | `--wait-ready 15 --push-grace-ms 300` | For an existing FCM Contact, sends a fresh OPTIONS; if no confirmed SIP 2xx within the grace period, sends FCM. Without a Contact, sends FCM immediately | A correlated OPTIONS 2xx from an FCM Contact, a new REGISTER, or the common deadline | AMI plus the optional Asterisk 24 patch for reliable OPTIONS results |

**Default:** omitting all three flags uses the daemon's `default_wait_seconds` policy (mode 1). The CLI rejects combinations of mode flags, including `--wait 0` together with another mode. `--push-grace-ms` is only valid with `--wait-ready`.

```bash
# Mode 1 — send FCM, then wait exactly 5 seconds (if FCM was accepted)
linpush wake --number 5 --wait 5

# Mode 2 — send FCM; finish on fresh REGISTER or at 15 seconds
linpush wake --number 5 --wait-register 15

# Mode 3 — try fresh, correlated OPTIONS first; fallback to FCM after 300 ms
linpush wake --number 5 --wait-ready 15 --push-grace-ms 300

# Any mode supports several extensions with ONE shared deadline
linpush wake --numbers 5:10:11 --wait-ready 15 --push-grace-ms 300
```

Only numeric extension names (1–16 digits) are accepted. A batch accepts at most 32 targets, deduplicates repeated numbers and FCM tokens, and limits concurrent FCM sends to eight across requests. Push-disabled extensions and extensions without usable tokens do not incur an artificial delay. The group waits for its relevant destinations concurrently, not once per number.

### What mode 3 can and cannot prove

Asterisk's cached `ContactStatus: Reachable` is **not** proof that an Android app is awake now. Standard AMI `PJSIPQualify` acknowledges starting an OPTIONS check, but ordinarily does **not** produce an event for repeated `Reachable → Reachable` replies. The optional patch emits `LinpushQualifyResult` after an on-demand SIP transaction, including its `LinpushProbeID`, `ContactID`, `AOR`, `SIP2xx` and round-trip timing. The Go daemon correlates the reply with its own probe and known FCM ContactID; an unrelated contact, stale state or non-2xx reply is not success.

Mode 3 may still send FCM to an awake phone if an OPTIONS reply takes longer than the grace period; this is intentional. If no correlated event can be obtained, it falls back to FCM plus REGISTER/timeout, never to a stale `Reachable` state. `--push-grace-ms 0` starts FCM immediately, concurrently with OPTIONS. The timeout supplied to `--wait-ready` covers **the entire operation**, including the grace period and FCM delivery attempt.

A successful FCM HTTP response proves Google accepted a message, **not** that Android received it or the user answered. A successful REGISTER proves the client registered; an OPTIONS 2xx proves the SIP Contact responded, but neither guarantees delivery of the subsequent INVITE. Evaluate `PJSIP_DIAL_CONTACTS(...)` **after** the wait.

## Requirements

- Arch Linux, Go with CGO, GCC, pkgconf, SQLite development headers; at runtime `libsqlite3`.
- A custom Android Linphone build using your Firebase project's `google-services.json` with push enabled.
- A **server-side Firebase Service Account JSON** for FCM HTTP v1 — this is not `google-services.json`.
- Asterisk PJSIP and a restricted local AMI user for automatic token discovery and modes 2/3.
- For *correlated* mode 3: Asterisk 24 built with `patches/asterisk24-linpush-qualify.patch`.

The daemon requires outbound HTTPS to `oauth2.googleapis.com` and `fcm.googleapis.com`.

## Build and install (Arch Linux)

```bash
sudo pacman -S --needed go gcc pkgconf sqlite
make test
make build

sudo install -Dm755 linpush /usr/local/bin/linpush
```

The Go service uses system SQLite with CGO and has no external Go module dependencies. The repository also provides `.gitlab-ci.yml` and GitHub Actions workflows.

For a new installation, create the service account and protected configuration:

```bash
sudo groupadd -r linpush
sudo useradd -r -g linpush -d /var/lib/linpush -s /usr/bin/nologin linpush
sudo install -d -o root -g linpush -m 0750 /etc/linpush
sudo install -m 0640 -o root -g linpush examples/config.json /etc/linpush/config.json
sudo install -m 0640 -o root -g linpush /path/to/service-account.json /etc/linpush/firebase-service-account.json

openssl rand -hex 32 | sudo tee /etc/linpush/api-token >/dev/null
openssl rand -hex 32 | sudo tee /etc/linpush/metrics-token >/dev/null
sudo chown root:linpush /etc/linpush/{api-token,metrics-token}
sudo chmod 0640 /etc/linpush/{api-token,metrics-token}

# Create /etc/linpush/ami-password containing the existing AMI user's password
sudo chown root:linpush /etc/linpush/ami-password
sudo chmod 0640 /etc/linpush/ami-password

sudo install -m 0644 systemd/linpush.service /etc/systemd/system/linpush.service
sudo systemctl daemon-reload
sudo systemctl enable --now linpush
```

For an **existing installation**, keep your current `/etc/linpush/config.json`, Firebase keys and SQLite database. Just replace the binary and restart `linpush`; the three mode flags are independent of the stored schema.

```bash
sudo systemctl restart linpush
curl -fsS http://127.0.0.1:9099/healthz
sudo systemctl status linpush --no-pager
```

Edit `/etc/linpush/config.json`: `firebase_project_id` is the string Firebase project ID (or leave it empty to derive it from the service account); `firebase_project_number` is the numeric Firebase project number found in SIP `pn-param`. The service listens on HTTP loopback and exposes a group-restricted Unix socket at `/run/linpush/linpush.sock` (mode `0660`, normally group `asterisk`). The `max_wait_seconds` config controls the upper bound of timeouts; the sample default is 30.

## Asterisk AMI and SIP configuration

Create or update a **local-only** `manager.conf` user. Replace the secret with a strong value and put it separately into `/etc/linpush/ami-password`:

```ini
[linpush]
secret = REPLACE_WITH_A_STRONG_SECRET
read = system,command
write = system
deny = 0.0.0.0/0.0.0.0
permit = 127.0.0.1/255.255.255.255
```

Example `/etc/linpush/config.json` fields (merge with your existing JSON):

```json
{
  "ami_enabled": true,
  "ami": {
    "address": "127.0.0.1:5038",
    "username": "linpush",
    "password_file": "/etc/linpush/ami-password",
    "sync_interval_seconds": 60,
    "single_contact_aors": ["5"]
  }
}
```

Set `single_contact_aors` only for AoRs with `max_contacts=1`; otherwise token rotation may deactivate a second device's valid token. Other settings are shown in [examples/config.json](examples/config.json).

In the existing global section of `pjsip.conf`, enable:

```ini
[global]
type=global
send_contact_status_on_update_registration=yes
```

Do **not** create duplicate `[global]` or AoR sections. For each relevant AoR (illustrative settings, merge into yours):

```ini
[5]
type=aor
max_contacts=1
qualify_frequency=30
qualify_timeout=2.0
qualify_2xx_only=yes
```

`qualify_frequency` marks an unresponsive Contact as `Unreachable`; it does **not** instantly unregister it or remove its saved FCM token. `qualify_2xx_only` is recommended; mode 3 additionally checks the real `SIP2xx` field from the patch.

```bash
asterisk -rx 'manager reload'
asterisk -rx 'pjsip reload'
```

### Automatic token discovery

The AMI synchronizer initially and periodically executes `PJSIPShowContacts`; it also reacts to `ContactStatus`. Only SIP Contacts with valid `pn-provider=fcm`, `pn-prid` and (if configured) matching `pn-param` enter SQLite. An incomplete `ContactListComplete` snapshot is rejected. Expired registrations do **not** delete FCM tokens: the tokens are necessary to wake the app again. Firebase `UNREGISTERED` responses disable invalid tokens. Newly discovered extensions default to *push disabled* until enabled explicitly.

```bash
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:9099/v1/sync
curl -fsS -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:9099/v1/extensions/5/devices
```

## Asterisk dialplan examples

These are **alternatives**, not consecutive commands. Pick exactly one `SHELL` line for each route:

```asterisk
; Mode 1
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait 5)})

; Mode 2
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait-register 15)})

; Mode 3 (patched Asterisk 24)
same => n,Set(PUSH_RESULT=${SHELL(/usr/local/bin/linpush wake --number 5 --wait-ready 15 --push-grace-ms 300)})

; Follow with:
same => n,NoOp(Linpush: ${PUSH_RESULT})
same => n,Dial(${PJSIP_DIAL_CONTACTS(5)},30)
same => n,Hangup()
```

For a group, replace `--number 5` with `--numbers 5:10:11` and, after the wait, build a `Dial()` string from the now-current contacts. See [examples/extensions.conf](examples/extensions.conf). If your Asterisk expression may yield no contacts, handle the empty dial string before `Dial()`.

**Security:** sanitize dynamic identifiers with `FILTER(0-9,...)` or `FILTER(0-9:,...)` as appropriate; never concatenate untrusted caller IDs, SIP headers, or account names into `SHELL()`.

### Optional Asterisk 24 patch

The [patch](patches/asterisk24-linpush-qualify.patch) modifies `res/res_pjsip/pjsip_options.c` and must be applied to Asterisk **source before compilation**. It is a source patch, not a dialplan or a Linux module. The AUR `prepare()` can apply it together with other Asterisk-level `.patch` files.

```bash
# From the root of your unpacked Asterisk 24 source:
patch -p1 --dry-run < /path/to/asterisk24-linpush-qualify.patch
```

Verify compatibility with the exact Asterisk release before building. Use a separate patch for PJProject compatibility when needed; that patch belongs in `third-party/pjproject/patches/`, not in place of the Linpush patch. You must reinstall the rebuilt Asterisk package and restart its process before the new AMI event can exist.

The normal AMI `PJSIPQualify` response alone cannot indicate a SIP reply. Our patched event carries `ActionID`, `AOR`, `ContactID`, `Status`, `SIP2xx` and `RoundtripUsec`. Linpush supplies a private `LinpushProbeID` header, correlated with `ActionID` in the emitted result.

## First live test on patched Asterisk

Use an actual Linphone extension with push already enabled and at least one FCM token. **Running `linpush wake` by itself is a readiness test; to confirm ringing, also place an actual call via your dialplan.**

```bash
# Replace the Go binary with your rebuilt version first; then:
sudo systemctl restart linpush

# Confirm AMI synchronization:
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9099/v1/sync

# Confirm push permission:
sudo -u asterisk /usr/local/bin/linpush control status 5

# First validate each mode without changing the dialplan:
sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait 2
sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait-register 10
sudo -u asterisk /usr/local/bin/linpush wake --number 5 --wait-ready 15 --push-grace-ms 300

# Examine daemon/AMI errors in another terminal:
sudo journalctl -u linpush -f
```

Expected mode-3 output for an awake phone is `ready sent=0 ... registration=options`; a sleeping phone can instead produce `sent sent=1 ... registration=registered` after the FCM wake, or `registration=timeout` when nothing responds. If the extension has no enabled token, the result will be `disabled` or `no_devices` — that is **not** a successful probe. A stock or unpatched Asterisk will not generate `LinpushQualifyResult`.

When using the actual call route, evaluate `PJSIP_DIAL_CONTACTS` after the wait and observe `pjsip set logger on` on a test system if you need to inspect OPTIONS/INVITE exchanges. Turn verbose SIP logging off afterwards.

## Push control and IVR

For an authenticated *local* administrator or trusted Asterisk dialplan, change an extension's push setting through the Unix socket:

```bash
sudo -u asterisk /usr/local/bin/linpush control status 5
sudo -u asterisk /usr/local/bin/linpush control enable 5
sudo -u asterisk /usr/local/bin/linpush control disable 5
```

[examples/extensions.conf](examples/extensions.conf) includes a trusted `[linpush-ivr]` context with dial code `*98`: press **1** for status, **2** to enable, **3** to disable, **0** to exit. It derives the number from authenticated `CHANNEL(endpoint)` and only permits self-service from matching numeric PJSIP endpoint names. Never include it in a public trunk context. Unix socket access itself is privileged: do not grant it to untrusted local users.

Generate basic **Russian**, mono 8 kHz PCM WAV IVR prompts:

```bash
sudo pacman -S --needed espeak ffmpeg
bash scripts/generate-ivr-audio.sh ./ivr-ru
sudo install -d -m755 /var/lib/asterisk/sounds/ru/custom
sudo install -m644 ivr-ru/linpush-*.wav /var/lib/asterisk/sounds/ru/custom/
asterisk -rx 'dialplan reload'
```

The sound directory is an example; check your Asterisk sound path. eSpeak voice quality is intentionally basic.

## REST API and SQLite

HTTP binds to loopback (`127.0.0.1:9099`); all endpoints except `GET /healthz` require `Authorization: Bearer <API_TOKEN>`. The internal Unix-socket `POST /wake` is not exposed on the public HTTP listener.

| Method | Path | Function |
| --- | --- | --- |
| `GET` | `/healthz` | Unauthenticated liveness check |
| `GET` | `/v1/sync` | AMI status |
| `GET` | `/v1/extensions` | Extensions and enabled devices |
| `GET`, `PUT` | `/v1/extensions/5` | Read or set `{"push_enabled":true}` |
| `GET` | `/v1/extensions/5/devices` | Device metadata, no raw FCM tokens |
| `PUT`, `DELETE` | `/v1/extensions/5/devices/android-main` | Emergency manual token management |
| `POST` | `/v1/extensions/5/wake` | One extension; accepts `wait_seconds` etc. |
| `POST` | `/v1/wake` | Multiple extensions; accepts `numbers` and wake mode fields |
| `GET` | `/metrics` | Authenticated Prometheus exposition |

```bash
TOKEN="$(sudo cat /etc/linpush/api-token)"
curl -fsS -X PUT "http://127.0.0.1:9099/v1/extensions/5" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"push_enabled":true}'

curl -fsS -X POST "http://127.0.0.1:9099/v1/wake" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"numbers":["5","10"],"wait_ready_seconds":15,"push_grace_ms":300}'
```

The SQLite schema stores enabled extensions and devices with source `asterisk` or `manual`, token, stable device ID, last seen timestamp and contact hash. For a consistent online backup use SQLite's `.backup` command, not an uncoordinated copy of the live WAL database. The service does not log full FCM tokens or reveal them in API responses.

## Prometheus and operational checks

`GET /metrics` requires an API token, or the separate least-privilege `metrics_token_file`. See [examples/prometheus.yml](examples/prometheus.yml). Counters and histograms include FCM outcomes, single/batch request status and duration, AMI sync results, registration waits and IVR control attempts. High-cardinality identifiers (phone numbers, FCM tokens, IPs) are not metric labels.

```bash
METRICS_TOKEN="$(sudo cat /etc/linpush/metrics-token)"
curl -fsS -H "Authorization: Bearer $METRICS_TOKEN" http://127.0.0.1:9099/metrics
go test -race ./...
go vet ./...
```

## Security notes and limitations

- The daemon's HTTP API and AMI connection are loopback-only; expose remote access through a secured TLS reverse proxy or VPN, never by opening the plain port.
- The Unix socket is accessible to its owner and configured group; treat access as privileged, especially for extension control.
- FCM HTTP 200 does not guarantee delivery. Neither REGISTER nor OPTIONS guarantees a completed call.
- The `SHELL() → Dial()` flow generally does not know the eventual outbound INVITE Call-ID when it sends FCM. It sends a best-effort data-only push without this field unless supplied. Full RFC 8599 call correlation requires deeper SIP integration.
- Only Android/FCM is supported; APNs is not implemented.
- Asterisk contact qualification does not remove SIP registrations; a Contact can remain registered but be marked `Unreachable`.
- The correlated OPTIONS patch has compile/application tests, but should be validated with a real phone and your specific Asterisk build.

For additional details and prior migration notes, see [README.ru.md](README.ru.md) and [CHANGELOG.md](CHANGELOG.md).

## Forced wake

Use `linpush wake --numbers 5:10 --wait 10 --force` to bypass the stored `push_enabled` setting for this request only. The setting is not changed. Only active device tokens are used. With `--wait-ready`, force also skips the pre-push OPTIONS grace period. Authenticated wake API requests accept `"force": true`.
