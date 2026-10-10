# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

"""Composite each rendered diagram on its page colour, to look at before committing.

A transparent PNG opens on white in most viewers, which makes the dark theme's
light text look broken when it is not. Writes to the system temp directory and
prints the paths; nothing here is published.
"""

import sys
import tempfile
from pathlib import Path

from PIL import Image

from theme import OUT

GROUND = {"dark": "#1a1b1e", "light": "#f4f4f6"}

for png in sorted(OUT.glob("*.png")):
    if sys.argv[1:] and not any(a in png.name for a in sys.argv[1:]):
        continue
    theme = png.stem.rsplit("-", 1)[-1]
    img = Image.open(png).convert("RGBA")
    ground = Image.new("RGBA", img.size, GROUND[theme])
    ground.alpha_composite(img)
    out = Path(tempfile.gettempdir()) / f"preview-{png.name}"
    ground.convert("RGB").save(out)
    inches = img.size[0] / 192
    on_page = 14 * 192 / 72 * min(1.0, 930 / img.size[0])
    print(f"{out}  {img.size[0]}x{img.size[1]}  {inches:.1f}in wide  14pt label shows at {on_page:.1f}px")
