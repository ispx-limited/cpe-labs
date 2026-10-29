"""Run with: python -m unittest discover -s hooks"""

import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

import announce

CHANGELOG = """# Changelog

## [0.76.0](https://example.test/compare/v0.75.2...v0.76.0) (2026-10-29)

### Added

* **scripts:** the System page shows what each script costs

## 0.1.0 (2026-08-20)
"""

NOTES = """- The System page ranks scripts by run time, host time and failure rate
- Action: previewing a topology rule now requires the config:write scope
"""


class Anchor(unittest.TestCase):
    def test_matches_the_docs_page_heading_id(self):
        self.assertEqual(announce.anchor(CHANGELOG, "0.76.0"), "#0760-2026-10-29")

    def test_first_release_heading_has_no_link(self):
        self.assertEqual(announce.anchor(CHANGELOG, "0.1.0"), "#010-2026-08-20")

    def test_unlisted_version_links_to_the_page(self):
        self.assertEqual(announce.anchor(CHANGELOG, "0.77.0"), "")
        self.assertEqual(
            announce.payload("0.77.0", NOTES, CHANGELOG)["embeds"][0]["url"],
            announce.CHANGELOG_URL,
        )


class Payload(unittest.TestCase):
    def test_one_embed_titled_by_version_linking_to_its_section(self):
        message = announce.payload("0.76.0", NOTES, CHANGELOG)
        self.assertEqual(message["allowed_mentions"], {"parse": []})
        (embed,) = message["embeds"]
        self.assertEqual(embed["title"], "cpe-labs 0.76.0")
        self.assertEqual(embed["url"], announce.CHANGELOG_URL + "#0760-2026-10-29")
        self.assertEqual(embed["description"], NOTES.strip())

    def test_overlong_notes_are_cut_at_a_whole_line(self):
        lines = "\n".join(f"- line {i} " + "x" * 100 for i in range(60))
        text = announce.body(lines)
        self.assertLessEqual(len(text), announce.EMBED_DESCRIPTION_LIMIT)
        self.assertTrue(text.endswith("x"))
        self.assertEqual(text, lines[: len(text)])
        self.assertEqual(lines[len(text)], "\n")


class Post(unittest.TestCase):
    def test_posts_json_to_the_webhook(self):
        received = {}

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers["Content-Length"])
                received["body"] = json.loads(self.rfile.read(length))
                received["type"] = self.headers["Content-Type"]
                self.send_response(204)
                self.end_headers()

            def log_message(self, *_):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}/webhook"
            status = announce.post(url, announce.payload("0.76.0", NOTES, CHANGELOG))
        finally:
            server.shutdown()
        self.assertEqual(status, 204)
        self.assertEqual(received["type"], "application/json")
        self.assertEqual(received["body"]["embeds"][0]["title"], "cpe-labs 0.76.0")


if __name__ == "__main__":
    unittest.main()
