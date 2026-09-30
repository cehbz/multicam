# TODO

Design and findings: KB node `projects/multicam.md`.

## Verifications

Pixel 3 XL flash: stock Android 12, Magisk, Alynx v4 crosshatch build,
`Wireless_firmware.zip` module.

After flash:
1. `zcat /proc/config.gz`: record the shipped driver set (settles
   MT7601U).
2. Camera STA (2.4 GHz) plus 5 GHz hotspot still run together on the
   Alynx kernel.
3. Routing: hub-AP client reaches a camera subnet via iptables.
4. Dongle on the phone: interface up, sustained STA association to a
   camera AP, throughput at or above the liveview rate.
5. Hub charge+host test per hub, including a charger plug-cycle.

Pixel 9 leg:
6. Wireless-ADB re-attach script (`adb mdns services`); scrcpy from the
   Mac; ws-scrcpy fork against Android 16 only if dashboard embedding is
   wanted.

## Build
- SSDP discovery sends on the camera's interface.
- Go core: Sony API client, liveview parser, record-all orchestration with
  clip-end auto-restart, ADB supervision, browser UI.
- Exposure and focus controls: probe both bodies in Manual mode first to
  see whether `setShutterSpeed`, `setFNumber` and `setFocusMode` become
  available.
