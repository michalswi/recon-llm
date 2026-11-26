---
name: satellite-tle-report
description: Use when the user asks about a satellite's orbit or receive-only reception, using public TLE data.
---
# Prepare a satellite orbit report from public TLE data

1. Use the satellite or group identified by the request or supplied elements. Ask only if the target is ambiguous.
2. Fetch the public elements once with curl, for example:
   curl --fail --silent --show-error --max-time 15 "https://celestrak.org/NORAD/elements/gp.php?CATNR=25544&FORMAT=tle"
   For a weather group, replace `CATNR=25544` with `GROUP=weather`.
   If the user gave a TLE file, read that instead. Fetch each source only once.
3. Distinguish a failed fetch, an explicit no-data response, and malformed or incomplete elements. None supports an orbit calculation. For each requested object require adjacent lines starting with `1 ` and `2 ` whose catalog IDs (columns 3-7) match each other and the selected object. Do not join lines from different records.
4. Read the fixed-width fields with awk or cut: NORAD ID, epoch (year and fractional day of year, UTC), inclination, eccentricity (implied decimal), and mean motion (line 2, columns 53-63, revolutions per day). Require numeric, positive mean motion before dividing.
5. Derive the approximate orbital period as 1440 divided by mean motion, in minutes. Say it is approximate.
6. Check the epoch age against the current date. Warn when elements are more than a few days old, since pass accuracy degrades.
7. Do not claim exact pass times, elevations, or Doppler values. They need a pass-prediction tool or the user's own output. If asked, say this and offer to interpret the user's prediction output.
8. A TLE is not evidence of an active transmitter or a downlink frequency. For a reception question, cite a dated operator bulletin, status source, or supplied observation for each such claim; otherwise label the status or frequency unverified. Do not fetch radio status for an orbit-only report. Never suggest transmitting or uplinking.
9. Do not run additional queries unless the user asks.
