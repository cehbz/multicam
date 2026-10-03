# TODO

Design and findings: KB node `projects/multicam.md`.

## Stories, in order
1. Keep the Pixel 3 XL awake while it is on power and the console page is
   being viewed: when its screen sleeps, the page's pictures freeze until it
   is reloaded.
2. Clip-end restart: a Sony body whose status goes from `MovieRecording`
   to `IDLE` without a stop from the console is started again, after its
   gap. Sony stops a clip at about 29 minutes.
3. The camera's overheating flag on its tile: `continuousError` carrying
   `Overheating Warning` with `isContinued: true` in the Sony event
   stream; Blackmagic's equivalent unknown.

Regroup after these. Candidates:
- A parked format and a recording format per camera, switched at Start and
  back at Stop, so the RX100M6 idles in 1080p and can still take short 4K
  clips.
- Exposure, focus and zoom controls (probe both bodies in Manual mode first
  for `setShutterSpeed`, `setFNumber`, `setFocusMode`).
- Multi-hour power (dummy batteries in the cameras, the phone on a charger).

## Nice to have
- Mac console over the home LAN: the server binds localhost; the Mac reaches
  it over `adb forward`.
- Two viewers of one camera at once, such as the phone and the Mac: today a
  Sony body's second viewer is refused and stops the first viewer's picture.
- Both cameras on one 2.4 GHz channel, so their liveview isn't time-sliced:
  find out whether the Sony bodies can be made to use a set Wi-Fi channel.
  Measured with both connected: 24.8 and 24.7 fps when both were on
  channel 1, 6.4 and 10.5 fps on channels 1 and 11.
- Rejoin a camera's link when the camera wakes; today `rig.sh start` or
  `scripts/phone-links.sh` is rerun by hand.
- Stream quality for the Pixel 9's picture: the SRT platform's bitrate and
  size are set by hand on the phone; a test target of 720p30 H.264 at
  2.5 Mbit/s was agreed and not run.
- The Pixel 9 as a camera while Blackmagic Camera is not in front: nothing
  is handled.
