---
name: adsb-aircraft-report
description: Use when the user asks what aircraft a local ADS-B receiver (dump1090/readsb) sees and wants a summary report.
---
# Prepare an ADS-B aircraft report

1. Confirm the source: the user's receiver host and port, or a saved aircraft.json/SBS file path. Do not guess a host. Do not use third-party aggregators unless the user names one.
2. Read the data once, with bounded output.
   - JSON over HTTP: curl -s http://HOST:8080/data/aircraft.json
     (dump1090-fa uses /skyaware/data/aircraft.json)
   - SBS stream: nc -w 10 HOST 30003 | head -n 200
   - File: use head/jq on the path the user gave.
3. Summarize with jq. For each aircraft report, when present: ICAO hex, callsign (flight), altitude, ground speed, track, squawk, RSSI, seconds since last seen. Missing fields are "unknown", never zero.
4. Report totals: aircraft seen, aircraft with position, and the capture time (the file's "now" field or the system date).
5. Flag squawk 7500, 7600 or 7700 factually, and note that codes can be set in error. Do not speculate about intent or cause.
6. State the limits: one snapshot is not a traffic count, coverage depends on antenna and line of sight, and not every aircraft broadcasts ADS-B.
7. Do not track, name, or single out private individuals or owners. Report aggregate or operationally neutral observations.
8. Never transmit or replay signals. Do not run additional queries unless the user asks.
