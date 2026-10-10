#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

#
# Upload one built version of the docs to the landing page's bucket, under the
# prefix that version is served at, and rewrite the version list.
#
# The bucket and the distribution belong to the website stack, which owns the
# distribution these docs are a cache behaviour on. This script therefore reads
# both from that stack's outputs rather than holding a copy: a bucket name
# pasted into two repositories is a bucket name that is wrong in one of them
# the first time the stack is recreated.
#
# The docs have a bucket of their own, and hold the site at its root. The
# /docs prefix is stripped by the distribution's viewer-request function, so
# what is uploaded here is what a reader sees under /docs.
#
#   scripts/deploy.sh next        -> s3://<docs-bucket>/next/
#   scripts/deploy.sh v1.2        -> s3://<docs-bucket>/v1.2/
#   scripts/deploy.sh latest      -> s3://<docs-bucket>/       (what /docs serves)
#
# Environment:
#   WEBSITE_STACK   CloudFormation stack that owns the bucket and distribution
#   AWS_REGION      its region
#   DOCS_LATEST     the series /docs currently serves, for versions.json
#
set -euo pipefail

target=${1:?usage: deploy.sh <next|vMAJOR.MINOR|latest>}
stack=${WEBSITE_STACK:-panorama-landing}
region=${AWS_REGION:-ap-southeast-2}
dist_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/dist"

CACHE_LONG="public, max-age=31536000, immutable"
CACHE_HTML="public, max-age=0, must-revalidate"
CACHE_SHORT="public, max-age=60, must-revalidate"

stack_output() {
  aws cloudformation describe-stacks --stack-name "$stack" --region "$region" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}

bucket=$(stack_output DocsBucketName)
distribution=$(stack_output DistributionId)

for pair in "DocsBucketName:$bucket" "DistributionId:$distribution"; do
  output_name=${pair%%:*}
  output_value=${pair#*:}
  if [ -z "$output_value" ] || [ "$output_value" = "None" ]; then
    echo "error: stack $stack has no $output_name output." >&2
    echo "       the website stack must be deployed before the docs can be." >&2
    exit 1
  fi
done

if [ ! -d "$dist_dir" ]; then
  echo "error: no build at $dist_dir. Run: npm run build" >&2
  exit 1
fi

case "$target" in
  latest)
    key=""          # the bucket root
    url="/docs/"
    ;;
  next | v[0-9]*.[0-9]*)
    key="$target/"
    url="/docs/$target/"
    ;;
  *)
    echo "error: '$target' is not 'latest', 'next', or a vMAJOR.MINOR series." >&2
    exit 1
    ;;
esac

echo "==> uploading $dist_dir/ to s3://$bucket/$key"

# `--delete` removes pages that no longer exist, which is the point of a sync.
# At the bucket root it would also delete every versioned prefix, because those
# live inside it, so they are excluded by name. Getting this wrong does not
# fail: it quietly removes every archived version, which is the one thing here
# that cannot be rebuilt from this commit.
delete_guards=()
if [ -z "$key" ]; then
  delete_guards+=(--exclude "next/*" --exclude "v[0-9]*/*" --exclude "versions.json")
fi

# Hashed assets first, with a year's cache. Astro fingerprints everything under
# _astro/, so those URLs never change meaning.
aws s3 sync "$dist_dir/_astro" "s3://$bucket/${key}_astro" \
  --region "$region" --delete --cache-control "$CACHE_LONG"

aws s3 sync "$dist_dir" "s3://$bucket/$key" \
  --region "$region" --exclude "*" --include "*.woff2" \
  --content-type "font/woff2" --cache-control "$CACHE_LONG"

aws s3 sync "$dist_dir" "s3://$bucket/$key" \
  --region "$region" --delete \
  --cache-control "$CACHE_SHORT" \
  --exclude "_astro/*" --exclude "*.woff2" --exclude "*.html" \
  ${delete_guards[@]+"${delete_guards[@]}"}

# HTML last and uncached, so a reader never holds a page that points at assets
# the sync has already replaced.
aws s3 sync "$dist_dir" "s3://$bucket/$key" \
  --region "$region" \
  --exclude "*" --include "*.html" \
  --content-type "text/html; charset=utf-8" --cache-control "$CACHE_HTML" \
  ${delete_guards[@]+"${delete_guards[@]}"}

# The version list, rebuilt from what the bucket now holds.
echo "==> rewriting s3://$bucket/versions.json"
versions=$(mktemp)
trap 'rm -f "$versions"' EXIT

aws s3 ls "s3://$bucket/" --region "$region" \
  | node "$(dirname "${BASH_SOURCE[0]}")/versions.mjs" --latest "${DOCS_LATEST:-}" \
  > "$versions"
cat "$versions"

aws s3 cp "$versions" "s3://$bucket/versions.json" \
  --region "$region" \
  --content-type "application/json" --cache-control "$CACHE_SHORT"

echo "==> invalidating ${url}*"
invalidation=$(aws cloudfront create-invalidation \
  --distribution-id "$distribution" \
  --paths "${url}*" "/docs/versions.json" \
  --query 'Invalidation.Id' --output text)
echo "    $invalidation"

echo
echo "https://panorama.dnx.ai${url}"
