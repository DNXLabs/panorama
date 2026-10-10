#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

#
# Write the template a release ships: panorama.yaml with the Release mapping's
# image tag set to that release's version. An empty ImageTag, the default,
# runs that tag, so the template and the image are versioned together:
# deploying a release's template runs that release, and deploying the next
# one's moves the image with it.
#
#   release-template.sh v1.4.0 > panorama.yaml
#
# The repository's copy keeps `stable`, because it belongs to no release.
#
# Exactly one line changes, and it must be found where it is expected: the
# first `Tag: stable` inside the Release mapping. Anything else - the mapping
# renamed, its value changed, the block reshaped - fails here rather than
# shipping a template that still follows a channel. CI runs this with a
# placeholder version so that failure arrives with the change that caused it,
# not at release time.

set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 TAG [TEMPLATE]" >&2
  exit 2
fi

tag=$1
template=${2:-$(dirname "$0")/panorama.yaml}

if ! printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "release-template: '$tag' is not vMAJOR.MINOR.PATCH[-prerelease]" >&2
  exit 1
fi

awk -v tag="$tag" '
  /^  Release:$/                   { block = 1; print; next }
  block && /^      Tag: stable$/    { print "      Tag: " tag; replaced++; block = 0; next }
  block && /^  [^ ]/                { block = 0 }
                                    { print }
  END {
    if (replaced != 1) {
      print "release-template: expected one \"Tag: stable\" under the Release mapping, found " replaced + 0 > "/dev/stderr"
      exit 1
    }
  }
' "$template"
