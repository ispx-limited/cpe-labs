"""Render the repository's CHANGELOG.md as the docs site's Changelog page.

The release automation writes CHANGELOG.md with a link on every version
heading (compare view) and after every entry (pull request, commit). For a
reader of the docs site those links are noise, and when the repository is
private they are dead, so the page gets the file with the links removed. The
repository copy is untouched.

A release can carry notes written for the user in release-notes/<version>.md,
beside CHANGELOG.md and outside the docs directory so mkdocs never builds the
file as a page of its own. When a version has notes the page shows them under
the version heading and folds the generated entries beneath, so the page reads
as release notes and the full list stays one click away. A version without
notes renders its entries as before. Notes for a version CHANGELOG.md does not
list are not rendered, so the page never describes a release that has not been
cut.
"""

import re
from pathlib import Path

PAGE = "changelog.md"
NOTES_DIR = "release-notes"
FOLD_TITLE = "All changes"

TITLE = re.compile(r"\A\s*# [^\n]*\n")
LINKED_HEADING = re.compile(r"^(#{1,6}) \[([^\]]+)\]\([^)]*\)", re.M)
TRAILING_REFS = re.compile(r"(?:\s+\(\[[^\]]+\]\([^)]*\)\))+\s*$", re.M)
CLOSES_REFS = re.compile(r",? closes \[#\d+\]\([^)]*\)")
WARNING_SIGN = re.compile(r"^(#{1,6}) ⚠️? ", re.M)
VERSION_HEADING = re.compile(r"^## (\d+\.\d+\.\d+)\b[^\n]*$", re.M)
SECTION_HEADING = re.compile(r"^### (.+)$", re.M)


def clean(changelog):
    """The changelog without its title and without the generator's links."""
    # The page already has a title from the nav; the file's H1 would repeat it.
    body = TITLE.sub("", changelog)
    body = LINKED_HEADING.sub(r"\1 \2", body)
    body = TRAILING_REFS.sub("", body)
    body = CLOSES_REFS.sub("", body)
    return WARNING_SIGN.sub(r"\1 ", body)


def with_notes(body, notes_dir):
    """Each version's notes under its heading, the generated entries folded beneath."""
    headings = list(VERSION_HEADING.finditer(body))
    if not headings:
        return body
    out = [body[: headings[0].start()]]
    for i, heading in enumerate(headings):
        end = headings[i + 1].start() if i + 1 < len(headings) else len(body)
        entries = body[heading.end() : end]
        notes = notes_dir / f"{heading.group(1)}.md"
        if not notes.is_file():
            out.append(heading.group(0) + entries)
            continue
        # A heading inside the fold would still reach the page's table of
        # contents, and a link there would open on a collapsed block.
        folded = SECTION_HEADING.sub(r"**\1**", entries.strip())
        out.append(
            f"{heading.group(0)}\n\n"
            f"{notes.read_text(encoding='utf-8').strip()}\n\n"
            f"/// details | {FOLD_TITLE}\n{folded}\n///\n\n"
        )
    return "".join(out)


def on_page_markdown(markdown, page, config, files):
    if page.file.src_uri != PAGE:
        return markdown
    root = Path(config.config_file_path).parent
    changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
    body = with_notes(clean(changelog), root / NOTES_DIR)
    if not body.strip():
        body = "No releases.\n"
    return markdown.rstrip() + "\n\n" + body
