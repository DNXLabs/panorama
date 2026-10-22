---
title: Upgrading
description: >-
  How to move a deployment to a new release on AWS and on a server, what happens
  to the database, how to roll back, and how releases and channels are named.
sidebar:
  order: 6
---

An upgrade is a new image and, sometimes, database migrations. Migrations only
ever move forward, so **back up the database first** and you can always get
back to where you were.

Read the [release notes](https://github.com/dnxlabs/panorama/releases)
for the versions you are crossing before you start.

## On AWS (CloudFormation)

The template leaves `AutoMigrate` off, so that migrations are an explicit step
you take **before** the service rolls. That ordering is what makes a rolling
update safe with two tasks running.

An upgrade is the new release's template. It runs that release, and brings
whatever the release needs from the stack (a new permission, a new setting)
with it:

```sh
REGION=us-east-1   # the region the stack is in

# 1. The new release's template, deployed with no tasks running
curl -fsSLO https://github.com/dnxlabs/panorama/releases/download/v1.4.0/panorama.yaml
aws cloudformation deploy --region "$REGION" --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=0

# 2. Run the MigrateCommand stack output, and wait for exit code 0

# 3. Roll the service
aws cloudformation deploy --region "$REGION" --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=2
```

Every parameter you leave out keeps its value. In the AWS console it is the
same three steps: **Update**, **Replace existing template** with the new
`panorama.yaml`, and `DesiredCount` set to `0`, then to `2`.

**If you set `ImageTag`**, the stack keeps running that image whatever template
you deploy. Pass the new version with `ImageTag=v1.4.0`, or `ImageTag=""` to go
back to following the template.

**To avoid dropping to zero**, deploy the new template with your current
`DesiredCount`, and run the migration task against the new task definition
while the service is still rolling. That is safe because every migration is
backward-compatible with the previous release, so the old tasks keep working
against the new schema.

Aurora takes its own automated backups. Take a manual snapshot before an
upgrade that crosses a major version.

A request streaming on a task that is being replaced gets up to 300 seconds to
finish. See
[the four settings](/docs/deploy/aws/#four-settings-you-should-not-tune-casually).

## On a server (Compose)

The Compose files set `PANORAMA_DATABASE_AUTO_MIGRATE=true`, so migrations run
when the new container starts.

1. **Back up.** On SQLite, stop the container first, because copying a live
   SQLite file can capture it mid-write:

   ```sh
   docker compose -f deploy/compose/docker-compose.yml stop
   docker run --rm -v panorama_panorama-data:/data -v "$PWD":/backup busybox \
     cp -a /data /backup/panorama-data-$(date +%F)
   ```

   On Postgres, use `pg_dump` the way you already do.

2. **Get the new version and start it.**

   ```sh
   git fetch --tags
   git checkout v1.4.0
   docker compose -f deploy/compose/docker-compose.yml up -d --build
   ```

3. **Check it.**

   ```sh
   curl https://panorama.your-company.internal/readyz
   docker compose -f deploy/compose/docker-compose.yml exec panorama panorama migrate status
   docker compose -f deploy/compose/docker-compose.yml exec panorama panorama version
   ```

   `/readyz` returns 200 only when the schema is current, so it is the one
   check that covers both the container and the migration.

In-flight requests are cut when the single container restarts. Coding tools
retry the turn. Pick a quiet moment anyway.

## Rolling back

**Roll the image back, and fix the schema forward.** Deploy the previous
release's template on AWS, or check out the previous tag on Compose. The previous release
runs against the newer schema, because migrations are written to be compatible
one release back. Migrations themselves are never reverted.

That guarantee covers **one** release. To go back further, restore the backup.

## Versions and channels

Releases are `vMAJOR.MINOR.PATCH`. Two channel tags point at them: `stable`
for the latest release and `beta` for the latest prerelease.

A channel only ever moves forward. A backported `v1.3.5` published after
`v1.4.0` gets its own version tag and leaves `stable` where it is, so following
a channel can never downgrade you.

**In production, deploy a version tag or a digest, not a channel.** See
[Choose which image to run](/docs/deploy/aws/#choose-which-image-to-run).

## Documentation for your version

These pages describe the latest release. The version selector at the top of the
sidebar shows the documentation as it was when an earlier series shipped, and
`next` shows what is on the main branch.
