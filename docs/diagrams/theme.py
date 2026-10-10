# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

"""The two themes every diagram is rendered in, and the helpers that apply them.

The site's default theme is dark and its alternative is paper, so a diagram is
rendered once for each and the page shows the one that matches (see
src/components/ThemeImage.astro). The colours are the console's own, copied from
ui/design-tokens.css: Graphviz cannot read a CSS custom property, so they are
repeated here, and this is the one place they are repeated.

SIZE IS A READABILITY RULE, NOT A TASTE. A figure is held to 58rem
(--measure-figure in docs.css) however wide the page, so a diagram is shown at
about 930px whatever it was rendered at. At 192dpi that
means a graph much wider than 12 inches has its 14pt labels scaled under 12px
on the page. Keep a
diagram to five or six ranks across and check the printed size in preview.py.

The background is transparent so the page's own ground shows through. That is
also why monochrome icons are inverted for the dark theme: a black GitHub mark
on a transparent ground is invisible on #1a1b1e.
"""

from __future__ import annotations

import hashlib
import tempfile
from dataclasses import dataclass
from pathlib import Path

from diagrams import Cluster, Diagram, Edge, Node

OUT = Path(__file__).resolve().parent.parent / "src" / "assets" / "diagrams"
FONT = "Helvetica"


@dataclass(frozen=True)
class Theme:
    name: str
    ink: str  # text read as language
    ink_muted: str  # edge labels, secondary text
    rule: str  # cluster borders, edges
    panel: str  # a cluster's fill
    inset: str  # a cluster inside a cluster
    control: str  # a plain box
    accent: str  # the thing the diagram is about
    accent_ink: str  # text set on the accent
    invert_mono: bool  # whether black-on-transparent icons need inverting


DARK = Theme(
    name="dark",
    ink="#f2f3f5",
    ink_muted="#a4a7ad",
    rule="#4a4d54",
    panel="#212327",
    inset="#26282c",
    control="#2d2f34",
    accent="#64b5fe",
    accent_ink="#10202e",
    invert_mono=True,
)

LIGHT = Theme(
    name="light",
    ink="#1a1b1e",
    ink_muted="#5e6064",
    rule="#c9cbd1",
    panel="#ffffff",
    inset="#fafafb",
    control="#ebecef",
    accent="#0057d7",
    accent_ink="#ffffff",
    invert_mono=False,
)

THEMES = (DARK, LIGHT)


def diagram(filename: str, theme: Theme, direction: str = "LR", **graph) -> Diagram:
    """A Diagram wired to a theme, written to src/assets/diagrams/<filename>-<theme>.png."""
    OUT.mkdir(parents=True, exist_ok=True)
    graph_attr = {
        "bgcolor": "transparent",
        "dpi": "192",
        "pad": "0.3",
        "nodesep": "0.55",
        "ranksep": "0.9",
        "fontname": FONT,
        "fontcolor": theme.ink,
        "splines": "spline",
    }
    graph_attr.update(graph)
    return Diagram(
        "",
        filename=str(OUT / f"{filename}-{theme.name}"),
        outformat="png",
        show=False,
        direction=direction,
        graph_attr=graph_attr,
        node_attr={"fontname": FONT, "fontsize": "14", "fontcolor": theme.ink},
        edge_attr={
            "fontname": FONT,
            "fontsize": "12.5",
            "fontcolor": theme.ink_muted,
            "color": theme.rule,
            "penwidth": "1.4",
            "arrowsize": "0.7",
        },
    )


def cluster(label: str, theme: Theme, inset: bool = False, accent: bool = False, **graph) -> Cluster:
    """A rounded panel. `accent` marks the one cluster a diagram is about."""
    graph_attr = {
        "bgcolor": theme.inset if inset else theme.panel,
        "pencolor": theme.accent if accent else theme.rule,
        "penwidth": "1.6" if accent else "1",
        "fontname": FONT,
        "fontsize": "14",
        "fontcolor": theme.accent if accent else theme.ink_muted,
        "labeljust": "l",
        "style": "rounded",
        "margin": "18",
    }
    graph_attr.update(graph)
    return Cluster(label, graph_attr=graph_attr)


def box(label: str, theme: Theme, accent: bool = False, width: str = "1.9") -> Node:
    """A step with no icon: a rounded box, for things that are not a service."""
    return Node(
        label,
        shape="box",
        style="rounded,filled",
        fillcolor=theme.accent if accent else theme.control,
        color=theme.accent if accent else theme.rule,
        fontcolor=theme.accent_ink if accent else theme.ink,
        fontname=FONT,
        fontsize="14",
        labelloc="c",
        fixedsize="false",
        width=width,
        height="0.55",
        margin="0.18,0.1",
    )


def edge(
    theme: Theme,
    label: str = "",
    accent: bool = False,
    dashed: bool = False,
    invisible: bool = False,
    **attrs,
) -> Edge:
    """An edge in the theme. `invisible` ranks two nodes without drawing anything."""
    if invisible:
        return Edge(style="invis", **attrs)
    # The font is named here and not left to the diagram's edge_attr, because
    # mingrammer's Edge sets "Sans-Serif" on itself and an edge's own attribute
    # wins. On a slim image that name resolves to whatever fontconfig finds
    # first, which can be a monospace, and labels come out in two faces.
    return Edge(
        label=label,
        color=theme.accent if accent else theme.rule,
        fontcolor=theme.ink_muted,
        fontname=FONT,
        fontsize="12.5",
        style="dashed" if dashed else "solid",
        **attrs,
    )


def steps(labels: list[str], theme: Theme) -> Node:
    """Numbered steps as one node: an HTML table whose cell order cannot move.

    Not four boxes on one rank, because Graphviz treats left-to-right order
    among same-rank nodes in a cluster as a preference, and renders the steps
    as 1, 4, 2, 3 once the edges leaving step 4 pull it sideways. A table has no order to minimise. Each cell is a port, `s1` to
    `sN`, so an edge can still enter at the first step and leave from the last.
    """
    cells = []
    for i, label in enumerate(labels, start=1):
        text = label.replace("\n", "<BR/>")
        cells.append(
            f'<TD PORT="s{i}" BGCOLOR="{theme.control}" BORDER="1" COLOR="{theme.rule}" '
            f'STYLE="rounded" CELLPADDING="10"><FONT COLOR="{theme.accent}"><B>{i}</B></FONT>'
            f'<FONT COLOR="{theme.ink}">  {text}</FONT></TD>'
        )
        if i < len(labels):
            cells.append(f'<TD BORDER="0"><FONT COLOR="{theme.accent}" POINT-SIZE="18">&#8594;</FONT></TD>')
    table = (
        '<<TABLE BORDER="0" CELLBORDER="0" CELLSPACING="6"><TR>'
        + "".join(cells)
        + "</TR></TABLE>>"
    )
    # mingrammer fixes every node at 1.4in square, which is an icon's size and
    # a quarter of this table's: left alone, the table overflows its own node
    # and the cluster is drawn around the small box rather than the steps.
    return Node(
        table,
        shape="plain",
        fixedsize="false",
        width="0",
        height="0",
        margin="0",
        labelloc="c",
        fontname=FONT,
        fontsize="14",
    )


def same_rank(container: Cluster, nodes: list[Node]) -> None:
    """Hold nodes on one rank, in the order given.

    mingrammer has no word for this, so it is written into the cluster's DOT
    body. Edges between the nodes then run along the rank in their direction,
    which is what makes numbered steps read 1 to 4 rather than in whatever
    order crossing-minimisation preferred.
    """
    ids = "; ".join(f'"{n._id}"' for n in nodes)
    container.dot.body.append(f"\t{{ rank=same; {ids} }}")


def mono(cls: type[Node], theme: Theme) -> type[Node]:
    """A node class whose black-on-transparent icon is inverted on the dark theme.

    AWS icons are coloured tiles and read on either ground. The generic ones
    (a person, a laptop, the GitHub mark) are black line art and do not.
    """
    if not theme.invert_mono:
        return cls

    from PIL import Image, ImageOps

    class Inverted(cls):  # type: ignore[misc, valid-type]
        def _load_icon(self):  # noqa: D401 - mingrammer's hook
            src = Path(super()._load_icon())
            key = hashlib.sha1(str(src).encode()).hexdigest()[:10]
            dst = Path(tempfile.gettempdir()) / f"panorama-diagram-{key}.png"
            if not dst.exists():
                img = Image.open(src).convert("RGBA")
                r, g, b, a = img.split()
                inverted = ImageOps.invert(Image.merge("RGB", (r, g, b)))
                Image.merge("RGBA", (*inverted.split(), a)).save(dst)
            return str(dst)

    Inverted.__name__ = cls.__name__
    return Inverted
