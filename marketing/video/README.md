# The box launch film

Every frame and every sound of the film on the site is generated here.

- `make_video.py` draws the picture (1920×1080, 30 fps) from scene functions of time.
- `make_audio.py` speaks the narration with [Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M)
  (Apache 2.0, run locally) and synthesises the score, an original track, so both are free to use.

## Render

```sh
python3 -m venv venv
venv/bin/pip install skia-python imageio-ffmpeg numpy kokoro-onnx soundfile

# the voice model, about 340 MB, not kept in the repo
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin

venv/bin/python make_video.py ../box-launch-silent.mp4
venv/bin/python make_audio.py kokoro-v1.0.onnx voices-v1.0.bin audio.wav

FF=$(venv/bin/python -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())")
$FF -i ../box-launch-silent.mp4 -i audio.wav -map 0:v -map 1:a -c:v copy \
    -af "loudnorm=I=-14:TP=-1.5:LRA=11,aresample=48000" -c:a aac -b:a 192k \
    -shortest -movflags +faststart ../box-launch.mp4
cp ../box-launch.mp4 ../../docs/assets/box-launch.mp4
```

`venv/bin/python make_video.py --still 14.5 frame.png` renders a single frame,
which is the quick way to check a change.

If Kokoro stops with `Error processing file '…/phontab'`, its bundled espeak
looks for its data one folder up. Link it there:

```sh
P=$(venv/bin/python -c "import espeakng_loader, os; print(os.path.dirname(espeakng_loader.__file__))")
for f in "$P"/espeak-ng-data/*; do ln -sf "espeak-ng-data/$(basename "$f")" "$P/"; done
```

## Timing

The music runs at 120 bpm in 2-second bars from 7.8 s, and the ending lands on
the bar line at 35.8 s. Voice lines start at their scene's start (`LINES` in
`make_audio.py`); scene times are in each `scene_*` function of
`make_video.py`. Move one, move the other.

Fonts: Geist (Vercel) and IBM Plex Mono, both SIL Open Font License 1.1.
