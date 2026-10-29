"""Post one release's notes to a Discord channel.

The notes the docs site renders for a version (release-notes/<version>.md,
see changelog.py) are also the text a reader of the ispx Discord gets when
the release lands: one message per release, the version as the title linked
to the version's section of the Changelog page, the notes as the body. The
webhook URL arrives in DISCORD_WEBHOOK_URL and is never printed. A version
whose notes are longer than a Discord embed allows (4096 characters) has its
body cut at the last whole line that fits; the notes standard keeps them far
shorter, so the cut is a guard, not a path.

Run with: python hooks/announce.py <version>
"""

import json
import os
import re
import sys
import urllib.request
from pathlib import Path

CHANGELOG_URL = "https://docs.cpe-labs.ispx.co/changelog/"
NOTES_DIR = "release-notes"
EMBED_DESCRIPTION_LIMIT = 4096


def anchor(changelog, version):
    """The Changelog page's fragment for a version, derived the way mkdocs does.

    The page heading is the generated "## [1.2.0](compare) (2026-10-29)" with
    its link removed, so its slug is the version and the date with every
    character outside letters, digits, spaces and hyphens dropped. A version
    CHANGELOG.md does not list yet has no section to point at, so the link
    goes to the page itself.
    """
    heading = re.search(
        rf"^## \[?{re.escape(version)}\]?(?:\([^)]*\))? \((\d{{4}}-\d{{2}}-\d{{2}})\)",
        changelog,
        re.M,
    )
    if not heading:
        return ""
    slug = re.sub(r"[^\w\s-]", "", f"{version} ({heading.group(1)})").strip().lower()
    return "#" + re.sub(r"\s+", "-", slug)


def body(notes, limit=EMBED_DESCRIPTION_LIMIT):
    text = notes.strip()
    if len(text) <= limit:
        return text
    cut = text.rfind("\n", 0, limit)
    return text[: cut if cut > 0 else limit].rstrip()


def payload(version, notes, changelog):
    return {
        "username": "cpe-labs releases",
        "allowed_mentions": {"parse": []},
        "embeds": [
            {
                "title": f"cpe-labs {version}",
                "url": CHANGELOG_URL + anchor(changelog, version),
                "description": body(notes),
            }
        ],
    }


def post(url, message):
    request = urllib.request.Request(
        url,
        data=json.dumps(message).encode("utf-8"),
        headers={"Content-Type": "application/json", "User-Agent": "cpe-labs-release-notes"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.status


def main(argv):
    if len(argv) != 2:
        print("usage: announce.py <version>", file=sys.stderr)
        return 2
    version = argv[1]
    url = os.environ.get("DISCORD_WEBHOOK_URL", "")
    if not url:
        print("DISCORD_WEBHOOK_URL is not set, nothing posted", file=sys.stderr)
        return 2
    root = Path(__file__).resolve().parent.parent
    notes = root / NOTES_DIR / f"{version}.md"
    if not notes.is_file():
        print(f"no notes at {NOTES_DIR}/{version}.md", file=sys.stderr)
        return 1
    changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
    message = payload(version, notes.read_text(encoding="utf-8"), changelog)
    status = post(url, message)
    print(f"posted {version} ({status})")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
