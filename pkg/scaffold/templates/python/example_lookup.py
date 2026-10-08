"""example_lookup: --id <value>; prints one JSON value on stdout.

Expected failures (not found, bad upstream reply) are JSON with an "error"
field and exit 0, so the model sees a useful message. Exit non-zero only on bugs.
"""
import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--id", required=True)
args = parser.parse_args()

url = "https://api.example.com/items/" + urllib.parse.quote(args.id)
try:
    with urllib.request.urlopen(url, timeout=10) as resp:
        item = json.load(resp)
    json.dump({"id": args.id, "name": item.get("name")}, sys.stdout)
except urllib.error.HTTPError as e:
    json.dump({"error": "not_found" if e.code == 404 else "upstream", "status": e.code}, sys.stdout)
