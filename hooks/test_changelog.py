"""Run with: python -m unittest discover -s hooks"""

import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

import changelog

GENERATED = """# Changelog

## [0.2.0](https://example.test/compare/v0.1.0...v0.2.0) (2026-08-21)


### ⚠ BREAKING CHANGES

* **api:** the v1 session routes are gone ([#12](https://example.test/issues/12)) ([abc1234](https://example.test/commit/abc1234))


### Added

* **auth:** sign-in is throttled per address ([#10](https://example.test/issues/10)) ([def5678](https://example.test/commit/def5678))

## 0.1.0 (2026-08-20)

### Fixed

* **firmware:** a delayed completion report no longer re-flashes a device, closes [#3](https://example.test/issues/3)
"""


class Clean(unittest.TestCase):
    def test_strips_title_links_and_warning_sign(self):
        body = changelog.clean(GENERATED)
        self.assertNotIn("# Changelog", body)
        self.assertNotIn("example.test", body)
        self.assertIn("## 0.2.0 (2026-08-21)", body)
        self.assertIn("### BREAKING CHANGES", body)
        self.assertIn("* **auth:** sign-in is throttled per address\n", body)
        self.assertIn("no longer re-flashes a device\n", body)


class WithNotes(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.notes = Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)

    def test_without_notes_the_entries_are_unchanged(self):
        body = changelog.clean(GENERATED)
        self.assertEqual(changelog.with_notes(body, self.notes), body)

    def test_notes_replace_the_entries_of_their_version_only(self):
        (self.notes / "0.2.0.md").write_text("- Sign-in is throttled per address\n", encoding="utf-8")
        page = changelog.with_notes(changelog.clean(GENERATED), self.notes)
        self.assertIn(
            "## 0.2.0 (2026-08-21)\n\n- Sign-in is throttled per address\n\n## 0.1.0 (2026-08-20)\n\n### Fixed\n",
            page,
        )
        self.assertNotIn("**api:**", page)
        self.assertNotIn("**auth:**", page)
        self.assertNotIn("BREAKING", page)
        self.assertIn("* **firmware:** a delayed completion report", page)

    def test_notes_for_an_uncut_release_are_not_rendered(self):
        (self.notes / "0.3.0.md").write_text("- Not released\n", encoding="utf-8")
        page = changelog.with_notes(changelog.clean(GENERATED), self.notes)
        self.assertNotIn("Not released", page)


class Page(unittest.TestCase):
    def render(self, generated, notes=None):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        (root / "CHANGELOG.md").write_text(generated, encoding="utf-8")
        for version, text in (notes or {}).items():
            (root / changelog.NOTES_DIR).mkdir(exist_ok=True)
            (root / changelog.NOTES_DIR / f"{version}.md").write_text(text, encoding="utf-8")
        page = SimpleNamespace(file=SimpleNamespace(src_uri=changelog.PAGE))
        config = SimpleNamespace(config_file_path=str(root / "mkdocs.yml"))
        return changelog.on_page_markdown("<!-- filled -->\n", page, config, None)

    def test_empty_changelog_reads_no_releases(self):
        self.assertEqual(self.render("# Changelog\n"), "<!-- filled -->\n\nNo releases.\n")

    def test_page_carries_notes_where_they_exist_and_entries_elsewhere(self):
        page = self.render(GENERATED, {"0.1.0": "- The first release\n"})
        self.assertIn("## 0.1.0 (2026-08-20)\n\n- The first release\n", page)
        self.assertNotIn("**firmware:**", page)
        self.assertIn("## 0.2.0 (2026-08-21)\n\n\n### BREAKING CHANGES", page)

    def test_other_pages_are_untouched(self):
        page = SimpleNamespace(file=SimpleNamespace(src_uri="index.md"))
        self.assertEqual(changelog.on_page_markdown("hello", page, None, None), "hello")


if __name__ == "__main__":
    unittest.main()
