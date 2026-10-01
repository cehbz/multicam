# TODO

Design and findings: KB node `projects/multicam.md`.

## Stories, in order
1. Record start/stop for the camera on the console.
2. Second Sony camera, which needs the flash and a dongle:
   - Flash: stock Android 12, Magisk, Alynx v4 crosshatch build,
     `Wireless_firmware.zip` module.
   - Dongle: interface up, sustained STA association to a camera AP,
     throughput at or above the liveview rate.
3. Clip-end auto-restart from `getEvent` recording status.
4. Pixel 9 over wireless ADB on the home Wi-Fi, with the Pixel 3 XL on home
   Wi-Fi as a second STA on its internal radio: view and record.

Regroup after these. Candidates: exposure, focus and zoom controls (probe
both bodies in Manual mode first for `setShutterSpeed`, `setFNumber`,
`setFocusMode`); multi-hour power (hub charge+host test per hub, including
a charger plug-cycle).

## Nice to have
- Mac console over the home LAN.
