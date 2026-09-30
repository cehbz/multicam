# TODO

Design and findings: KB node `projects/multicam.md`.

## Verifications

No flash needed:
1. gphoto2 PC Remote probe (`--list-config` per body): movie control
   present? Only revives the USB-PTP fallback.
2. Probe both bodies in Manual exposure mode: confirm `setShutterSpeed`,
   `setFNumber` and `setFocusMode` become available.

Pixel 3 XL flash: stock Android 12, Magisk, Alynx v4 crosshatch build,
`Wireless_firmware.zip` module.

After flash:
3. `zcat /proc/config.gz`: record the shipped driver set (settles
   MT7601U).
4. Camera STA (2.4 GHz) plus 5 GHz hotspot still run together on the
   Alynx kernel.
5. Routing: hub-AP client reaches a camera subnet via iptables.
6. Dongle on the phone: interface up, sustained STA association to a
   camera AP, throughput at or above the liveview rate.
7. Hub charge+host test per hub, including a charger plug-cycle.

Pixel 9 leg:
8. Wireless-ADB re-attach script (`adb mdns services`); scrcpy from the
   Mac; ws-scrcpy fork against Android 16 only if dashboard embedding is
   wanted.

## Build
- SSDP discovery sends on the camera's interface.
- Go core: Sony API client, liveview parser, record-all orchestration with
  clip-end auto-restart, ADB supervision, browser UI.
