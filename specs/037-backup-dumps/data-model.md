# Data model: the dumps directory

Host `/var/lib/can/dumps` is the `can-dumps` volume, which can-core sees as `/var/lib/can-dumps`
(`CAN_DUMPS_DIR`). Owner 65532:65532, mode 0700.

| Path (relative to the directory) | Content |
|---|---|
| `can-core/state.db` | `VACUUM INTO` copy of can-core's database, integrity-checked; replaced even by a run that later fails |
| `company.yaml` | the applied company file; likewise replaced in place |
| `modules/<id>/<name>` | the uploads of the last successful run, 0600 |
| `DUMPED` | `<id> <database>` per module dumped in that run |
| `LAST-RUN` | JSON: run id, time, ok, error, skipped modules |
| `COMPLETE` | `run_id=` and `finished_at=` lines (specs/008 format), present only after a fully successful run |
| `.run-<id>/` | staging of the run in progress (`tmp/`, `modules/`); never backed up |

Run states: idle -> running (marker and `DUMPED` removed; one module's hook window open at a time; uploads staged)
-> published (staged `modules/` swapped in, `DUMPED`, `LAST-RUN`, fsync, marker) or failed (staging discarded,
previous `modules/` kept without a marker, `LAST-RUN` records why). A skipped module's published files are hard
linked into the new set (carried forward, no age limit). Modules without a record (purged) are removed
from the directory at the next run; a purge through can-core removes them at once.
