# TODO

Design and findings: KB node `projects/multicam.md`.

## Stories, in order
0. Cold-start run of the rig: the Pixel 3 XL rebooted and unlocked once,
   the Sony bodies off, the Pixel 9 locked with Blackmagic Camera closed;
   then open the app, turn everything on and Connect. Check that the
   supervisor started at boot and the server restored its saved state.
   End with Stop all, then Disconnect.
1. Clip-end restart: a Sony body whose status goes from `MovieRecording`
   to `IDLE` without a stop from the console is started again, after its
   gap. Sony stops a clip at about 29 minutes.
2. Disconnect while recording, and Disconnect during a Connect.
3. The camera's overheating flag on its tile: `continuousError` carrying
   `Overheating Warning` with `isContinued: true` in the Sony event
   stream; Blackmagic's equivalent unknown. When the RX100M6 shows its
   thermometer, read its state with `/data/local/tmp/readyprobe -iface cam2
   -nojoin`, which doesn't touch the link.
4. Run the `scripts/phone-run.sh --foreground` deploy.
5. Whether the RX10M4 keeps `startLiveview` listed while live (measured on
   the RX100M6 only).
6. A brief app switch on the Pixel 9 closes Blackmagic's server; the
   camera's one try comes at once and is refused, so the user has to
   Connect again. Design point to raise.
7. `TestSupervisorResetsTheCountAfterALongRun` failed once under load
   (14 starts, want 10; 0 of ~42 reruns). `date +%s` truncation lets a
   stalled quick run measure 2 s against the 2 s threshold. Print the
   supervisor log in the test's failure message to catch the evidence.
8. Delete the throwaway files on the phone, once agreed (`readyprobe`
   after item 3):
   `/data/local/tmp/{readyprobe,coldcap.sh,linkprobe,lp.sh,wpactl}`,
   `/data/local/tmp/mc/coldcap.*.pcap`, and in `/data/local/tmp/mc`:
   `t.sh`, `rep.sh`, `links-reloc.sh`, `sony-only.toml` and the Termux-era
   `rig.sh`, `links.sh`, `notify.sh`, `links.conf`, `*.pid` and logs.

Regroup after these. Candidates:
- A parked format and a recording format per camera, switched at Start and
  back at Stop, so the RX100M6 idles in 1080p and can still take short 4K
  clips.
- Exposure, focus and zoom controls (probe both bodies in Manual mode first
  for `setShutterSpeed`, `setFNumber`, `setFocusMode`).
- Multi-hour power (dummy batteries in the cameras, the phone on a charger).

## Nice to have
- Both cameras on one 2.4 GHz channel, so their liveview isn't time-sliced:
  find out whether the Sony bodies can be made to use a set Wi-Fi channel.
  Measured with both connected: 24.8 and 24.7 fps when both were on
  channel 1, 6.4 and 10.5 fps on channels 1 and 11.
- Stream quality for the Pixel 9's picture: the SRT platform's bitrate and
  size are set by hand on the phone; a test target of 720p30 H.264 at
  2.5 Mbit/s was agreed and not run.
- The Pixel 9 as a camera while Blackmagic Camera is not in front: nothing
  is handled.
