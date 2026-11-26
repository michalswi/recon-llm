---
name: satellite-tle-report
description: Use when the user asks about a satellite's orbit or receive-only reception, using public TLE data.
---
# Prepare a satellite orbit report from public TLE data

1. Identify the satellite by name or NORAD catalog number. If unclear, ask the user.
2. Fetch the public elements once with curl, for example:
   curl -s "https://celestrak.org/NORAD/elements/gp.php?CATNR=25544&FORMAT=tle"
   For a weather group: curl -s "https://celestrak.org/NORAD/elements/gp.php?GROUP=weather&FORMAT=tle"
   If the user gave a TLE file, read that instead. Fetch each source only once.
3. Read the actual TLE lines. Never invent elements. If the response is empty or an error, report that and stop.
4. Extract with awk or cut, from the TLE text only: NORAD ID, epoch (year and day of year), inclination, eccentricity, and mean motion (revolutions per day).
5. Derive the approximate orbital period as 1440 divided by mean motion, in minutes. Say it is approximate.
6. Check the epoch age against the current date. Warn when elements are more than a few days old, since pass accuracy degrades.
7. Do not claim exact pass times, elevations, or Doppler values. They need a pass-prediction tool or the user's own output. If asked, say this and offer to interpret the user's prediction output.
8. For reception, name only commonly known receive-only downlinks (e.g. NOAA/METEOR weather imagery on VHF ~137 MHz, public amateur beacons), and tell the user to verify that the satellite is active and the frequency is current. Never suggest transmitting or uplinking.
9. Do not run additional queries unless the user asks.
