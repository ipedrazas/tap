"""forecast: --lat <num> --lon <num>; prints current conditions and 3 days as JSON."""
import argparse
import json
import sys
import urllib.parse
import urllib.request

WMO = {
    0: "clear sky", 1: "mainly clear", 2: "partly cloudy", 3: "overcast",
    45: "fog", 48: "rime fog", 51: "light drizzle", 53: "drizzle", 55: "dense drizzle",
    61: "light rain", 63: "rain", 65: "heavy rain", 71: "light snow", 73: "snow", 75: "heavy snow",
    80: "rain showers", 81: "heavy rain showers", 82: "violent rain showers",
    95: "thunderstorm", 96: "thunderstorm with hail", 99: "severe thunderstorm with hail",
}

def describe(code):
    return WMO.get(code, f"code {code}")

parser = argparse.ArgumentParser()
parser.add_argument("--lat", type=float, required=True)
parser.add_argument("--lon", type=float, required=True)
args = parser.parse_args()

query = urllib.parse.urlencode({
    "latitude": args.lat,
    "longitude": args.lon,
    "current": "temperature_2m,wind_speed_10m,weather_code",
    "daily": "temperature_2m_max,temperature_2m_min,weather_code",
    "timezone": "auto",
    "forecast_days": 3,
})
with urllib.request.urlopen("https://api.open-meteo.com/v1/forecast?" + query, timeout=10) as resp:
    data = json.load(resp)

cur, daily = data["current"], data["daily"]
json.dump({
    "timezone": data.get("timezone"),
    "current": {
        "time": cur["time"],
        "temperature_c": cur["temperature_2m"],
        "wind_kmh": cur["wind_speed_10m"],
        "weather": describe(cur["weather_code"]),
    },
    "daily": [
        {"date": d, "min_c": lo, "max_c": hi, "weather": describe(c)}
        for d, lo, hi, c in zip(daily["time"], daily["temperature_2m_min"], daily["temperature_2m_max"], daily["weather_code"])
    ],
}, sys.stdout)
