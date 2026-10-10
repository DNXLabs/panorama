# Moving an existing database onto the baseline

Panorama's migrations were squashed into one baseline per dialect,
`internal/store/migrations/{sqlite,postgres}/00001_initial.sql`. A new
deployment needs nothing from this directory: it creates the baseline on its
first start.

A database created before the squash was migrated by the old chain, versions 1
to 27, and goose remembers that. It needs `rebaseline.sql` run once, so that its
schema matches the baseline and goose's record says version 1. Without it the
new release starts, but the next migration added after the baseline would be
refused as out of order.

The script is Postgres only, because every deployment that predates the squash
runs on Postgres. A SQLite evaluation database from before the squash is
recreated instead (`docker compose down -v`).

## What it changes

In one transaction, refusing to start unless the database is at version 27:

1. Requests recorded with the decision `routed`, which only an older release
   wrote, become `allowed`: they were served.
2. The `requests.decision` check constraint is replaced without `routed`.
3. The `requests.fallback_from` column is dropped. Nothing has written it since
   a route became one model.
4. `goose_db_version` is rewritten to versions 0 and 1.

Nothing else in the schema changes; the baseline is the old chain's schema with
those two leftovers removed, checked by dumping both and comparing them.

## Running it

1. Stop the old release (scale the ECS service to zero). Nothing may write to
   `requests` while the constraint is replaced.
2. Take a snapshot of the Aurora cluster.
3. Run the script against the database, as the deployment's own user:

   ```sh
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/rebaseline/rebaseline.sql
   ```

   It prints `COMMIT` when it has done everything. If it prints the
   "only moves a database at 27" error, nothing was changed: the database is at
   another version, and the release it is on has to be brought to the last one
   before the squash first.
4. Deploy the release that carries the baseline, and start it.

`panorama migrate status` on the new release then lists `00001_initial.sql` as
applied and nothing pending.

## The same release changes the encryption key derivation

Integration credentials (the GitHub App key or token on Settings,
Integrations) are sealed with the deployment's `encryption_key`. When that key
is a passphrase, rather than the 32 bytes `panorama admin generate-key` mints,
the sealing key is derived from it, and the derivation's label changed in this
release (it carried the product's old name). A credential stored under a
passphrase before this release cannot be opened afterwards, and the fix is to
enter it again on Settings, Integrations. A key minted by `generate-key` is
used as given and is not affected. Nothing else is encrypted with that key.
