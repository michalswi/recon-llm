---
name: adsb-aircraft-report
description: Use when the user asks what aircraft a local ADS-B receiver (dump1090/readsb) sees and wants a summary report.
---
# Prepare an ADS-B aircraft report

1. Confirm the source: the user's receiver host and port, or a saved aircraft.json/SBS file path. Do not guess a host. Do not use third-party aggregators unless the user names one.
2. Read the supplied file or sample the specified receiver once. For HTTP use `curl --fail --silent --show-error --max-time 15 http://HOST:8080/data/aircraft.json` with the supplied URL (dump1090-fa commonly uses `/skyaware/data/aircraft.json`). Report fetch failures, malformed data, or incomplete input as such, not as zero aircraft.
3. For a JSON snapshot, use jq on the complete document. Require an `aircraft` array: an empty array means zero entries in this snapshot. Count entries and entries with both latitude and longitude before limiting the displayed rows. Report available ICAO hex, callsign, altitude, ground speed, track, squawk, RSSI and seconds since last seen; missing fields are unknown, not zero. Use the snapshot's `now` for capture time. If absent, say "capture time unknown"; retrieval time is not capture time.
4. For SBS/BaseStation text, read at most 200 lines (e.g. `nc -w 10 HOST 30003 | head -n 200` for a live stream). Parse valid comma-separated MSG records, not JSON. Count distinct ICAO addresses in this sample (field 5), not rows; count an address as positioned only when a record contains both coordinates. Messages carry partial updates. Report the sampled window from record timestamps if available, and distinguish no usable messages from a receiver reporting no aircraft.
5. Flag squawk 7500, 7600 or 7700 factually, and note that codes can be set in error. Do not speculate about intent or cause.
6. State the limits: one snapshot is not a traffic count, coverage depends on antenna and line of sight, and not every aircraft broadcasts ADS-B.
7. Do not track, name, or single out private individuals or owners. Report aggregate or operationally neutral observations.
8. Never transmit or replay signals. Do not run additional queries unless the user asks.
