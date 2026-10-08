"""front_page: --limit <n>; prints the current Hacker News front page as JSON.

Uses the official HN Firebase API: /topstories.json for the ranked IDs, then
/item/<id>.json for each story. Expected failures are JSON with an "error"
field and exit 0.
"""
import argparse
import json
import sys
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

API = "https://hacker-news.firebaseio.com/v0"
HEADERS = {"Accept": "application/json", "User-Agent": "tap-hn-agent/0.1"}


def get(path):
    req = urllib.request.Request(API + path, headers=HEADERS)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


parser = argparse.ArgumentParser()
parser.add_argument("--limit", type=int, required=True)
args = parser.parse_args()
limit = max(1, min(args.limit, 30))

try:
    ids = get("/topstories.json")
    if not isinstance(ids, list):
        json.dump({"error": "upstream", "detail": "unexpected topstories reply"}, sys.stdout)
        sys.exit(0)
    ids = [i for i in ids if isinstance(i, int)][:limit]
    with ThreadPoolExecutor(max_workers=8) as pool:
        items = list(pool.map(lambda i: get(f"/item/{i}.json"), ids))
except urllib.error.HTTPError as e:
    json.dump({"error": "upstream", "status": e.code}, sys.stdout)
    sys.exit(0)
except (urllib.error.URLError, TimeoutError) as e:
    json.dump({"error": "unreachable", "detail": str(getattr(e, "reason", e))}, sys.stdout)
    sys.exit(0)

stories = []
for rank, (story_id, item) in enumerate(zip(ids, items), start=1):
    if not isinstance(item, dict) or item.get("deleted") or item.get("dead"):
        continue
    hn_url = f"https://news.ycombinator.com/item?id={story_id}"
    stories.append({
        "rank": rank,
        "id": story_id,
        "title": item.get("title", ""),
        "url": item.get("url") or hn_url,
        "hn_url": hn_url,
        "points": item.get("score", 0),
        "comments": item.get("descendants", 0),
        "by": item.get("by"),
        "type": item.get("type"),
        "time": item.get("time"),
    })

json.dump({"count": len(stories), "stories": stories}, sys.stdout)
