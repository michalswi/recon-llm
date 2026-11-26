---
name: ping-report
description: Use when the user asks to run ping and report on connectivity.
---
# Prepare a ping report

1. Run ping with the host and packet count requested by the user.
   On macOS/Linux, five pings to google.com use: ping -c 5 google.com
2. Read the actual command output. Never invent results.
3. Report packets sent, packets received, and packet loss percentage.
4. Include minimum, average, and maximum round-trip time when available.
5. Explain what these results show in plain language. Five pings are
   only a brief sample, not proof of overall connection reliability.
6. If ping fails, report the error. Missing replies alone do not prove
   the host is offline; ICMP traffic may be blocked.
7. Do not run additional network checks unless the user asks.