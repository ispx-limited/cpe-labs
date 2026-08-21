"""Render the repository's CHANGELOG.md as the docs site's Changelog page.

The release automation writes CHANGELOG.md with a link on every version
heading (compare view) and after every entry (pull request, commit). For a
reader of the docs site those links are noise, and when the repository is
private they are dead, so the page gets the file with the links removed. The
repository copy is untouched.
"""

import re
from pathlib import Path

PAGE = "changelog.md"

TITLE = re.compile(r"\A\s*# [^\n]*\n")
LINKED_HEADING = re.compile(r"^(#{1,6}) \[([^\]]+)\]\([^)]*\)", re.M)
TRAILING_REFS = re.compile(r"(?:\s+\(\[[^\]]+\]\([^)]*\)\))+\s*$", re.M)
CLOSES_REFS = re.compile(r",? closes \[#\d+\]\([^)]*\)")
WARNING_SIGN = re.compile(r"^(#{1,6}) ⚠️? ", re.M)


def on_page_markdown(markdown, page, config, files):
    if page.file.src_uri != PAGE:
        return markdown
    changelog = Path(config.config_file_path).parent / "CHANGELOG.md"
    # The page already has a title from the nav; the file's H1 would repeat it.
    body = TITLE.sub("", changelog.read_text(encoding="utf-8"))
    body = LINKED_HEADING.sub(r"\1 \2", body)
    body = TRAILING_REFS.sub("", body)
    body = CLOSES_REFS.sub("", body)
    body = WARNING_SIGN.sub(r"\1 ", body)
    return markdown.rstrip() + "\n\n" + body
