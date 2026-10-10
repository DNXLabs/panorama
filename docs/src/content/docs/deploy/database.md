---
title: Choosing a database
description: >-
  SQLite is the default and runs the whole product on one container. Postgres
  is for more than one. The one rule about SQLite that breaks silently.
sidebar:
  order: 5
---

| | SQLite | Postgres |
| --- | --- | --- |
| **Setup** | None. It is built into the binary. | A database you run, or the one in the Postgres compose file. |
| **Runs** | Everything: gateway, console, budgets, attribution and every background job. | The same. |
| **Replicas** | One container. SQLite allows a single writer. | As many as you like. |
| **Choose it when** | One container serves your team. | You want more than one replica, or you want your existing backup, failover and monitoring to cover it. |

SQLite is a real answer for a team, not a trial mode. Both run the same schema
through the same migrations, so moving to Postgres later is a deployment
decision and not a rewrite.

## The one rule about SQLite

:::danger[The SQLite file must sit on a real Docker volume]
Never a bind mount from the host, an NFS share, or anything else that emulates
file locking. `docker-compose.yml` already uses a named volume. Leave it that
way.

Both ways of breaking this are silent. On a bind mount, `policy apply` can
report success while the gateway keeps serving the old policy, and committed
rows can vanish across a restart.
:::

## Using Postgres

```sh
docker compose -f deploy/compose/docker-compose.postgres.yml up -d --build
```

That file runs Postgres beside Panorama and points the gateway at it. Its
credentials default to `panorama` / `panorama-dev-password`, so override
`POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` in `.env`.

To use a Postgres of your own, set `PANORAMA_DATABASE_DSN` with
`sslmode=require`. The bundled file uses `sslmode=disable`, which is only
acceptable because that traffic never leaves the container network.

On AWS, the CloudFormation template runs Aurora Serverless v2 for you. See
[Deploying on AWS](/docs/deploy/aws/).

## Backups

See [Upgrading](/docs/deploy/upgrading/#on-a-server-compose) for how to copy
the SQLite volume safely. The database holds metadata only: no prompt or
response is ever written to it.
