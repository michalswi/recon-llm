---
name: drone-remote-id-report
description: Use when the user provides a log or capture of drone Remote ID broadcasts and wants a privacy-aware observation report.
---
# Prepare a drone Remote ID observation report

1. Confirm the source file path and that the user is authorized to analyze it (own capture, operator, or safety/regulatory purpose). Do not capture anything yourself and never transmit.
2. Inspect the file with ls and head. Use jq for JSON, grep and awk for text logs. Do not guess the format. If it is a raw pcap, say which fields can actually be read with the available tools.
3. Report only fields present in the data: ID type, UA (aircraft) type, a short ID, timestamp, position, altitude, speed, heading, status. Missing fields are "unknown".
4. Count distinct broadcasters and the observation window. State how long and how many messages the sample contains.
5. Treat operator location and persistent IDs as sensitive. Include them only when the user states an authorized safety or regulatory need. Otherwise summarize them as present or absent.
6. Remote ID is Wi-Fi or Bluetooth, not an SDR band, and many drones do not broadcast it. Absence of messages does not prove absence of drones.
7. Do not identify a drone model or operator unless a broadcast field says so. Do not help locate, follow, or interfere with a drone or its operator.
8. Separate Observations (from the data) from Interpretation (labeled). Do not run additional commands unless the user asks.
