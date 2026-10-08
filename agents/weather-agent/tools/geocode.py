"""geocode: --name <place>; prints the best match as JSON."""
import argparse
import json
import sys
import urllib.parse
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--name", required=True)
args = parser.parse_args()

url = "https://geocoding-api.open-meteo.com/v1/search?" + urllib.parse.urlencode({"name": args.name, "count": 1, "language": "en", "format": "json"})
with urllib.request.urlopen(url, timeout=10) as resp:
    data = json.load(resp)

results = data.get("results") or []
if not results:
    json.dump({"found": False, "name": args.name}, sys.stdout)
else:
    r = results[0]
    json.dump({
        "found": True,
        "name": r.get("name"),
        "country": r.get("country"),
        "latitude": r.get("latitude"),
        "longitude": r.get("longitude"),
        "timezone": r.get("timezone"),
    }, sys.stdout)
