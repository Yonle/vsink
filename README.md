# vsink

`vsink` is a PulseAudio virtual sink that routes system audio through a virtual sink while forwarding playback to the physical output.

It can also:

* Bypass selected applications from capture.
* Route selected recording applications to the virtual sink monitor.
* Exclusively capture selected applications into the virtual sink.
* Optionally set the virtual sink as the system default sink.
* Track physical output changes.
* Restore audio routing on shutdown.

This program is still in the experimental phase, so expect dirty code.

## Usage

```sh
go build -o vsink .
./vsink
```

### Bypass applications

```sh
./vsink -bypass stoat-desktop,Discord
```

Bypassed applications are routed directly to the physical sink instead of the capture sink.

### Monitor applications

```sh
./vsink -monitor obs,ffmpeg,stoat-desktop,Discord
```

Selected applications recording the physical output monitor are redirected to `SystemCaptureSink.monitor`.

### Only capture selected applications

```sh
./vsink -onlyCapture firefox,mpv
```

Only the specified applications are routed into the capture sink. Other applications are left on the normal physical output.

### Default sink mode

```sh
./vsink -defaultSinkMode
```

Sets the virtual sink as the system default sink.

This may help applications that do not correctly follow explicit stream routing, but can interfere with volume control depending on the desktop environment or application.

### Combined

```sh
./vsink \
  -bypass Discord,Spotify,stoat-desktop \
  -monitor obs,ffmpeg \
  -onlyCapture Firefox,mpv
```

## Options

| Option             | Description                                                                                                  |
| ------------------ | ------------------------------------------------------------------------------------------------------------ |
| `-bypass`          | Comma-separated application names to bypass capture                                                          |
| `-defaultSinkMode` | Set the virtual sink as the default sink. May help with some apps, but can cause issues with volume control. |
| `-monitor`         | Comma-separated application names whose recordings should use the capture sink monitor                       |
| `-onlyCapture`     | Comma-separated application names to exclusively capture into the capture sink                               |
| `-vSinkName`       | The name of the virtual sink                                                                                 |

## Requirements

* Linux
* PulseAudio or PipeWire with `pipewire-pulse`
* `pactl`
* Go
* `github.com/the-jonsey/pulseaudio`

## License

Copyright 2026 Yonle [yonle@proton.me](mailto:yonle@proton.me)

Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice, this list of conditions and the following disclaimer in the documentation and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its contributors may be used to endorse or promote products derived from this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
