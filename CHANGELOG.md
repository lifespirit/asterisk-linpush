# Changelog

## 0.2.0 — 2026-11-21

- Automatic Asterisk AMI `PJSIPShowContacts` synchronization at startup, every 60 seconds and after `ContactStatus` changes.
- Strict complete-snapshot validation using `ContactListComplete` / `ListItems` and SQLite transaction.
- Preserve tokens when SIP registrations expire; preserve existing manual tokens and extension permissions.
- SQLite migration adds `source`, `last_seen`, `contact_hash` to devices; manual tokens remain emergency fallback.
- Safe optional `single_contact_aors` mode for AORs with Asterisk `max_contacts=1`.
- Automatic FCM invalid-token handling without reviving rejected tokens on unchanged re-registration.
- `GET /v1/sync` for non-secret AMI synchronization status.
- Tests for seven-contact AMI snapshot, incomplete response rejection, parser, migration and manual fallback.

Note: This version was tested against a simulated AMI server, not the real hrouter. Asterisk's ContactList URI presence was independently confirmed by the user's Python AMI diagnostic.

## v0.3.0 — 2026-11-21

- Add one-shot multi-target wake via `--numbers 5:10:11` / JSON `numbers` array, parallel FCM requests, and a single shared post-push wait.
- Deduplicate target numbers and shared FCM tokens within each batch; limit concurrent sends across all calls to eight.
- Add REST `POST /v1/wake` and retain the original single-target API, CLI and Unix-socket routes.
- Expose authenticated Prometheus 0.0.4 metrics on `/metrics` (bounded labels, AMI state, FCM outcomes, wait histograms, SQLite inventory).
- Add optional independent read-only `metrics_token_file`, sample Prometheus scrape, tests and ring-group dialplan example.
- No SQLite schema changes.
