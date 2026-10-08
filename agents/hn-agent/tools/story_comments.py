"""story_comments: --id <item id> --limit <n>; prints a story and its top comments as JSON.

Uses the official HN Firebase API (/item/<id>.json). A missing item comes back
as JSON null with status 200, which we report as {"error": "not_found"}.
Expected failures are JSON with an "error" field and exit 0.
"""
import argparse
import html
import json
import re
import sys
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

API = "https://hacker-news.firebaseio.com/v0"
HEADERS = {"Accept": "application/json", "User-Agent": "tap-hn-agent/0.1"}
MAX_TEXT = 1500


def get(path):
    req = urllib.request.Request(API + path, headers=HEADERS)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


def plain(text):
    """HN comment HTML -> plain text: paragraphs to blank lines, links to their href."""
    if not text:
        return ""
    text = re.sub(r"<p>", "\n\n", text)
    text = re.sub(r'<a [^>]*href="([^"]*)"[^>]*>.*?</a>', r"\1", text, flags=re.S)
    text = re.sub(r"<[^>]+>", "", text)
    text = html.unescape(text).strip()
    if len(text) > MAX_TEXT:
        text = text[:MAX_TEXT].rstrip() + "..."
    return text


parser = argparse.ArgumentParser()
parser.add_argument("--id", type=int, required=True)
parser.add_argument("--limit", type=int, required=True)
args = parser.parse_args()
limit = max(1, min(args.limit, 10))

try:
    story = get(f"/item/{args.id}.json")
    if not isinstance(story, dict) or story.get("deleted"):
        json.dump({"error": "not_found", "id": args.id}, sys.stdout)
        sys.exit(0)
    if story.get("type") not in ("story", "poll", "job"):
        json.dump({"error": "not_a_story", "id": args.id, "type": story.get("type")}, sys.stdout)
        sys.exit(0)
    kid_ids = [k for k in story.get("kids", []) if isinstance(k, int)]
    # Fetch a few extra in case some top comments are deleted or dead.
    candidates = kid_ids[: limit + 5]
    with ThreadPoolExecutor(max_workers=8) as pool:
        kids = list(pool.map(lambda i: get(f"/item/{i}.json"), candidates))
except urllib.error.HTTPError as e:
    json.dump({"error": "upstream", "status": e.code}, sys.stdout)
    sys.exit(0)
except (urllib.error.URLError, TimeoutError) as e:
    json.dump({"error": "unreachable", "detail": str(getattr(e, "reason", e))}, sys.stdout)
    sys.exit(0)

comments = []
for c in kids:
    if not isinstance(c, dict) or c.get("deleted") or c.get("dead"):
        continue
    comments.append({
        "id": c.get("id"),
        "by": c.get("by"),
        "text": plain(c.get("text")),
        "replies": len(c.get("kids", [])),
        "time": c.get("time"),
    })
    if len(comments) == limit:
        break

hn_url = f"https://news.ycombinator.com/item?id={args.id}"
json.dump({
    "story": {
        "id": args.id,
        "title": story.get("title", ""),
        "url": story.get("url") or hn_url,
        "hn_url": hn_url,
        "points": story.get("score", 0),
        "comments": story.get("descendants", 0),
        "by": story.get("by"),
        "text": plain(story.get("text")),
    },
    "top_comments": comments,
}, sys.stdout)
