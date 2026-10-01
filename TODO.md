# TODO

Design and findings: KB node `projects/multicam.md`.

## Stories, in order
1. Record all cameras with one control.
2. Keep the Pixel 3 XL awake while it is on power and the console is in
   use: when its screen sleeps, the page's pictures freeze until it is
   reloaded. Open: whether "in use" means the server running or the page
   being viewed.

Regroup after these. Candidates: clip-end auto-restart from `getEvent`
recording status; exposure, focus and zoom controls (probe both bodies in
Manual mode first for `setShutterSpeed`, `setFNumber`, `setFocusMode`);
multi-hour power (dummy batteries in the cameras, the phone on a charger).

## Nice to have
- Mac console over the home LAN.
- Two viewers of one camera at once, such as the phone and the Mac: today a
  Sony body's second viewer is refused and stops the first viewer's picture.
- Both cameras on one 2.4 GHz channel, so their liveview isn't time-sliced:
  find out whether the Sony bodies can be made to use a set Wi-Fi channel.
  Measured with both connected: 24.8 and 24.7 fps when both were on
  channel 1, 6.4 and 10.5 fps on channels 1 and 11.
- Rejoin a camera's link when the camera wakes; today
  `scripts/phone-links.sh` is rerun by hand.
- The Pixel 9's picture shows only the viewfinder; today it is the whole
  screen, camera buttons included.
- Find the Pixel 9's debugging port without editing the config; it changes
  when the phone's wireless debugging restarts.
- The Pixel 9 as a camera while its screen is off or locked: its picture is
  black and Start is refused.
- Smooth video from the Pixel 9 in place of screenshots.
