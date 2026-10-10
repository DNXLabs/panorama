# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

"""What happens to one request: the diagram on "What Panorama is".

The point it has to make in one look: a coding tool talks to Panorama instead
of to Bedrock, four things happen to the request on the way through, and the
only thing kept afterwards is metadata.

Top to bottom rather than left to right, because it is read in a column of
prose: four steps side by side is as wide as the column gets, and a fifth rank
to the right of them is a strip nobody can read.
"""

from diagrams.aws.ml import Bedrock
from diagrams.onprem.client import Users
from diagrams.onprem.database import PostgreSQL
from diagrams.onprem.vcs import Github

from theme import THEMES, box, cluster, diagram, edge, mono, same_rank, steps

for t in THEMES:
    with diagram("request-flow", t, direction="TB", ranksep="0.6", nodesep="0.35"):
        with cluster("An engineer's laptop", t):
            tools = [
                box("Claude Code", t, width="1.6"),
                box("opencode", t, width="1.6"),
                box("Codex CLI", t, width="1.6"),
            ]

        with cluster("Panorama  ·  one container, in your infrastructure", t, accent=True):
            with cluster("On the request path", t, inset=True):
                path = steps(
                    [
                        "Authenticate\nthe token",
                        "Resolve route\nto a model",
                        "Check\nbudgets",
                        "Read branch,\nticket and PR",
                    ],
                    t,
                )
            with cluster("Off the request path", t, inset=True) as off:
                console = box("Console\nwho spent what, on what", t, width="2.6")
                jobs = box("Background jobs\nconfirm work, refresh prices", t, width="2.6")
                same_rank(off, [console, jobs])

        db = PostgreSQL("SQLite or Postgres\nmetadata, never a prompt")
        admins = mono(Users, t)("Admins and managers\nover SSO")
        github = mono(Github, t)("GitHub")
        bedrock = Bedrock("Amazon Bedrock\nin your AWS account")

        # Every tool enters at step 1; the stream and the metadata leave from step 4.
        for tool in tools:
            tool >> edge(t, accent=True, headport="s1:n") >> path

        # Holds the second row under the first without drawing anything.
        path >> edge(t, invisible=True) >> console
        path >> edge(t, invisible=True) >> jobs

        path >> edge(t, "streamed,\nnever buffered", accent=True, tailport="s4:s") >> bedrock
        path >> edge(t, "tokens, cost,\nwork item", dashed=True, tailport="s1:s") >> db
        console >> edge(t, dashed=True) >> db
        console << edge(t, dashed=True) << admins
        jobs >> edge(t, "asks only about\nwhat it has seen", dashed=True) >> github
        # Bedrock belongs on the bottom row with everything else outside the
        # container; without this it floats beside the cluster.
        jobs >> edge(t, invisible=True) >> bedrock
