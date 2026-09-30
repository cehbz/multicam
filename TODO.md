# TODO

Design and findings: KB node `projects/multicam.md`.

## Verifications

No flash needed:
1. Sony API probe, per body: SSDP discover, `getAvailableApiList` in movie
   mode, confirm `startMovieRec`/`actZoom`/exposure set, sample liveview
   quality. Gates the Sony plan.
2. gphoto2 PC Remote probe (`--list-config` per body): movie control
   present? Only revives the USB-PTP fallback.
3. Stock-Android concurrency preview on the un-flashed Pixel 3 XL: join a
   camera AP and enable the hotspot. Framework-level signal only.
4. RX100 mark (menu version screen or body label).

Pixel 3 XL flash: stock Android 12, Magisk, Alynx v4 crosshatch build,
`Wireless_firmware.zip` module.

After flash:
5. `zcat /proc/config.gz`: record the shipped driver set (settles
   MT7601U).
6. qcacld STA+AP with an internet-less STA (manual hostapd or settings AP).
7. Routing: hub-AP client reaches a camera subnet via iptables.
8. Dongle on the phone: interface up, sustained STA association to a
   camera AP, throughput at or above the liveview rate.
9. Hub charge+host test per hub, including a charger plug-cycle.

Pixel 9 leg:
10. Wireless-ADB re-attach script (`adb mdns services`); scrcpy from the
    Mac; ws-scrcpy fork against Android 16 only if dashboard embedding is
    wanted.

## Build
- Go core: Sony API client, liveview parser, record-all orchestration with
  clip-end auto-restart, ADB supervision, browser UI.
