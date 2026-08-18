# combo

The **combo** branch is the go2rtc reference used by the realtime app **OneDoor**.

It is a collection of several unmerged PRs and custom features on top of go2rtc:

- **Realtime tuning** — various fine-tuning for low-latency realtime streaming.
- **go2sip** — a self-answering SIP consumer that serves as a portal to "generic" cameras
  (video/audio in **and** two-way/backchannel audio back).
- **Explicit codec matching for sendrecv** — the SIP answer is always a single
  `sendrecv` `m=audio` line, and the backchannel codec is matched to the camera's
  `recvonly` codec by name+rate (PCMA/8000), keeping the two directions cleanly
  separated for the pool.
- **Stream preload + GOP cache** — persistent, warm streams / GOP caching.

## Recent stable additions (2026-08-18)

Applied onto this branch (3 stable commits):

1. `42c1c3ad` — streams: prefer camera recvonly codec for backchannel when present (go2rtc-core matcher)
2. `5426367d` — rtp: register backchannel receiver so `Stop()` closes it
3. `91db776e` — sip: always answer one sendrecv `m=audio` line

## Branch layout (tracking)

- **combo** — stable experimental reference (this branch). Holds the 3 stable commits above.
- **go2sip** — clean PR branch: rtp receiver-registration + the sip single-`sendrecv` answer.
- **explicit** — working/experimental history (deeper backchannel experiments and diagnostics).

## TODO

- dahua private protocol
