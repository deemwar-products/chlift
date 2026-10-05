# Upgrade from v0.1.0-preview: tested on 2026-10-05

**Question:** after replacing the v0.1.0-preview binary with the new one, do mirrors created by the preview keep
working, from status through checks to cleanup?

**Steps** (dev environment: 3 SSH nodes, 2 ClickHouse replicas + 3 Keeper members, PeerDB, Postgres 16):
1. **Download** the public v0.1.0-preview release binary and verify it against the release's `SHA256SUMS`
   (`binaries.log`).
2. **Preview binary:** `chlift install` (`install-preview.log`), then `migrate plan` + `migrate start
   --apply-postgres` for one table, `public.notes_demo` (`start-preview.log`).
3. **New binary:**
   - `migrate status` showed the mirror RUNNING (output not saved).
   - Rows were inserted, updated and deleted in Postgres, carried by CDC.
   - `migrate check --checksums`: 18,804 rows on Postgres and on both replicas, 0 of 64 checksum buckets differ
     (`check-main.log`).
4. **New binary: `migrate abort`.** Peers, the ClickHouse database and the publication were dropped, and 0
   replication slots were left (`abort-main.log`).

**Result:** upgrading means replacing the binary. Mirrors created by v0.1.0-preview keep working with the new one.
