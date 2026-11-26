---
name: drone-remote-id-report
description: Use when the user provides a log or capture of drone Remote ID broadcasts and wants a privacy-aware observation report.
---
# Prepare a drone Remote ID observation report

1. Confirm the source file path and that the user is authorized to analyze it (own capture, operator, or safety/regulatory purpose). Do not capture anything yourself and never transmit.
2. For decoded JSON use jq; for text logs use head, grep or awk. Do not read binary captures with head. Recon's PCAP attachment summary describes packets, not decoded Remote ID records. Without decoded records, report "Remote ID presence undetermined" and request a decoded export if needed; do not report zero drones.
3. Report only fields present in decoded records: ID type, UA (aircraft) type, timestamp, altitude, speed, heading and status. Missing fields are "unknown". If records need to be distinguished, assign report-local labels such as UA-1; do not publish fragments of a persistent ID.
4. Count decoded messages and distinct observed IDs, not physical drones. Messages can repeat and identifiers can change. Report the sampled observation window from usable message timestamps only.
5. Omit persistent IDs and operator coordinates from ordinary reports; state only whether these fields were present. Include sensitive detail only to the minimum needed for an authorized safety or regulatory purpose.
6. Remote ID is Wi-Fi or Bluetooth, not an SDR band, and many drones do not broadcast it. Absence of messages does not prove absence of drones.
7. Do not identify a drone model or operator unless a broadcast field says so. Do not help locate, follow, or interfere with a drone or its operator.
8. Separate Observations (from the data) from Interpretation (labeled). Do not run additional commands unless the user asks.
