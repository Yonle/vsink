# vsink

`vsink` is a PulseAudio virtual sink that routes system audio through a virtual sink while forwarding playback to the physical output.

It can also:

* Bypass selected applications from capture.
* Route selected recording applications to the virtual sink monitor.
* Track physical output changes.
* Restore audio routing on shutdown.

This program is still in experimental phase, so expect dirty codes.

## Usage

```sh
go build -o vsink .
./vsink
```

### Bypass applications

```sh
./vsink -bypass discord,spotify,sto
```

Bypassed applications are routed directly to the physical sink.

### Monitor applications

```sh
./vsink -monitor obs,ffmpeg
```

Selected applications recording the physical output monitor are redirected to `SystemCaptureSink.monitor`.

### Combined

```sh
./vsink -bypass discord,spotify -monitor obs
```

## Options

| Option     | Description                                                         |
| ---------- | ------------------------------------------------------------------- |
| `-bypass`  | Comma-separated applications excluded from capture                  |
| `-monitor` | Comma-separated applications redirected to the virtual sink monitor |

## Requirements

* Linux
* PulseAudio
* `pactl`
* Go
* `github.com/the-jonsey/pulseaudio`

## License

Copyright 2026 Yonle <yonle@proton.me>

Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice, this list of conditions and the following disclaimer in the documentation and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its contributors may be used to endorse or promote products derived from this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
