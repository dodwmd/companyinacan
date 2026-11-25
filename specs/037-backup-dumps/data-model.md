# Data model: the dumps directory

Host `/var/lib/can/dumps` is the `can-dumps` volume, which can-core sees as `/var/lib/can-dumps`
(`CAN_DUMPS_DIR`). Owner 65532:65532, mode 0700.

| Path (relative to the directory) | Content |
|---|---|
| `can-core/state.db` | `VACUUM INTO` copy of can-core's database, integrity-checked |
| `company.yaml` | the applied company file |
| `modules/<id>/<name>` | uploads of the module's backup hook, 0600 |
| `COMPLETE` | `run_id=` and `finished_at=` lines (specs/008 format), present only after a fully successful run |

Run states: idle (marker present or not) -> running (marker removed, uploads accepted) -> idle with marker
(success) or without (any failure). Dumps of a disabled module stay until removed by hand.
