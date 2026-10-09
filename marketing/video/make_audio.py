"""Voice-over and an original score for the box launch film.

    venv/bin/python make_audio.py <kokoro-v1.0.onnx> <voices-v1.0.bin> out.wav

The voice is Kokoro-82M (Apache 2.0), run locally. The music is synthesised
here from oscillators and noise, so it is original and free to use. Lines are
placed at their scene's start; a line that would overrun its scene is spoken
a little faster until it fits.
"""
import os
import sys

import numpy as np
import soundfile as sf

SR = 48000
DUR = 39.0
N = int(SR * DUR)
rng = np.random.default_rng(7)

# (start, latest end, line): times match the scenes in make_video.py
LINES = [
    (0.35, 3.2, "Two a.m. Your agent is still working."),
    (4.7, 7.5, "Relax. It was in a box."),
    (9.9, 12.0, "Meet box. A box for your AI."),
    (12.8, 19.0, "Every command runs in a fresh micro VM, with its own kernel. Nothing outside changes."),
    (19.7, 23.6, "Fast enough to forget it's there."),
    (24.4, 29.2, "Let it try three things, and keep one. Your agent can fork. Only you can apply."),
    (29.7, 32.5, "It plugs into the agent you already use."),
    (32.8, 35.3, "Zero escapes. Run the tests yourself."),
    (35.85, 38.7, "Box. A box for your AI."),  # on the final downbeat
]


def voice(model, voices):
    import espeakng_loader
    from kokoro_onnx import Kokoro, EspeakConfig
    pkg = os.path.dirname(espeakng_loader.__file__)
    k = Kokoro(model, voices, espeak_config=EspeakConfig(lib_path=espeakng_loader.get_library_path(), data_path=pkg))
    out = np.zeros(N)
    for start, end, line in LINES:
        speed = 1.0
        while True:
            s, sr = k.create(line, voice="af_heart", speed=speed, lang="en-us")
            if len(s) / sr <= end - start or speed >= 1.3:
                break
            speed += 0.05
        s = np.interp(np.arange(0, len(s), sr / SR), np.arange(len(s)), s)  # 24 kHz -> 48 kHz
        i = int(start * SR)
        seg = s[: N - i]
        out[i:i + len(seg)] += seg
        print(f"{start:5.1f}s  {len(s) / SR:4.2f}s  x{speed:.2f}  {line}")
    return out / (np.abs(out).max() + 1e-9) * 0.9


# --- synthesis ---------------------------------------------------------------

t = np.arange(N) / SR


def note(f): return 440.0 * 2 ** ((f - 69) / 12)


def place(buf, start, sig, gain=1.0):
    i = int(start * SR)
    if i >= len(buf):
        return
    seg = sig[: len(buf) - i]
    buf[i:i + len(seg)] += gain * seg


def env_ad(n, attack, decay):
    a = int(attack * SR)
    e = np.exp(-np.arange(n) / (decay * SR))
    if a:
        e[:a] *= np.linspace(0, 1, a)
    return e


def kick(level=1.0):
    n = int(0.6 * SR)
    tt = np.arange(n) / SR
    f = 45 + 95 * np.exp(-tt * 28)
    return level * np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-tt * 7)


def hat(level=0.2, decay=0.03):
    n = int(0.12 * SR)
    x = rng.standard_normal(n)
    x = x - np.convolve(x, np.ones(6) / 6, mode="same")  # crude high-pass
    return level * x * np.exp(-np.arange(n) / (decay * SR))


def pluck(midi, level=0.25, decay=0.35):
    n = int(1.2 * SR)
    tt = np.arange(n) / SR
    f = note(midi)
    x = np.sin(2 * np.pi * f * tt) + 0.35 * np.sin(4 * np.pi * f * tt) + 0.12 * np.sin(6 * np.pi * f * tt)
    return level * x * env_ad(n, 0.004, decay)


def pad(chord, start, length, level=0.12, attack=0.6):
    n = int(length * SR)
    tt = np.arange(n) / SR
    left = np.zeros(n)
    right = np.zeros(n)
    for m in chord:
        f = note(m)
        left += np.sin(2 * np.pi * f * 0.998 * tt) + 0.3 * np.sin(2 * np.pi * 2 * f * tt)
        right += np.sin(2 * np.pi * f * 1.002 * tt) + 0.3 * np.sin(2 * np.pi * 2 * f * 1.001 * tt)
    e = np.minimum(1, tt / attack) * np.minimum(1, (length - tt) / 0.8).clip(0)
    return level * left * e / len(chord), level * right * e / len(chord)


def reverb(x, seconds=2.2, mix=0.28):
    n = int(seconds * SR)
    ir = rng.standard_normal(n) * np.exp(-np.arange(n) / (seconds / 6.5 * SR))
    ir[0] = 0
    size = 1 << int(np.ceil(np.log2(len(x) + n)))
    wet = np.fft.irfft(np.fft.rfft(x, size) * np.fft.rfft(ir, size), size)[: len(x)]
    wet *= np.abs(x).max() / (np.abs(wet).max() + 1e-9)
    return (1 - mix) * x + mix * wet


def score():
    L = np.zeros(N)
    R = np.zeros(N)

    def both(start, sig, gain=1.0, pan=0.0):
        place(L, start, sig, gain * (1 - max(0, pan)))
        place(R, start, sig, gain * (1 + min(0, pan)))

    # hook: drone on A, ticking hats, rising tension
    dr = (np.sin(2 * np.pi * 55 * t) + 0.5 * np.sin(2 * np.pi * 110.4 * t)) * np.clip(t / 0.8, 0, 1) * (t < 4.2)
    dr *= 0.16 * (1 + 0.6 * np.clip((t - 2.2) / 2, 0, 1))
    L += dr
    R += dr
    for k in range(int(4.0 / 0.125)):
        both(k * 0.125, hat(0.05 + 0.002 * k, 0.015), pan=0.2 if k % 2 else -0.2)
    # glitch: crushed noise bursts as the screen breaks
    for k in range(9):
        s = 3.2 + k * 0.06 + rng.uniform(0, 0.03)
        n = int(rng.uniform(0.02, 0.07) * SR)
        burst = np.round(rng.standard_normal(n) * 3) / 3 * 0.25
        both(s, burst, pan=rng.uniform(-0.6, 0.6))

    # the screen switching off: a falling whine as the picture folds to a dot
    n = int(0.6 * SR)
    tt = np.arange(n) / SR
    f = 80 + 1100 * np.exp(-tt * 7)
    whine = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-tt * 4.5) * np.minimum(1, tt / 0.01)
    both(3.55, whine, 0.16)

    # the turn: a deep hit, then a warm pad
    both(4.2, kick(1.2))
    both(4.2, np.sin(2 * np.pi * 41.2 * np.arange(int(2.5 * SR)) / SR) * env_ad(int(2.5 * SR), 0.01, 0.9) * 0.5)
    l, r = pad([57, 60, 64, 71], 4.5, 3.6, 0.16)  # A minor add 9
    place(L, 4.5, l)
    place(R, 4.5, r)

    # groove from the logo on: 120 bpm, Am - F - C - G, one chord per bar. Bars
    # start every 2 s from 7.8, so the ending lands on the bar line at 35.8;
    # the bar before it turns to E major and builds into the downbeat.
    beat = 0.5
    END = 35.8
    prog = [[57, 60, 64], [53, 57, 60], [48, 52, 55, 60], [55, 59, 62]]
    arp = [[69, 72, 76, 72], [65, 69, 72, 69], [64, 67, 72, 67], [62, 67, 71, 67]]
    start = 7.8
    bar = 0
    while start + bar * 4 * beat < END - 1e-6:
        b0 = start + bar * 4 * beat
        ch = bar % 4
        last = b0 + 4 * beat >= END - 1e-6
        if last:
            prog[ch], arp[ch] = [52, 56, 59, 64], [68, 71, 76, 71]  # E major, the way home
        l, r = pad([m + 0 for m in prog[ch]], b0, 4 * beat + (0.05 if last else 0.4), 0.11)
        place(L, b0, l)
        place(R, b0, r)
        both(b0, np.sin(2 * np.pi * note(prog[ch][0] - 24) * np.arange(int(1.9 * SR)) / SR)
             * env_ad(int(1.9 * SR), 0.02, 0.7) * 0.22)  # bass
        for q in range(4):
            tq = b0 + q * beat
            if bar > 0 or q >= 2:
                both(tq, kick(0.55 if q % 2 == 0 else 0.35))
            both(tq + beat / 2, hat(0.07), pan=0.3)
            for e8 in range(2):
                m = arp[ch][(q * 2 + e8) % 4]
                both(tq + e8 * beat / 2, pluck(m, 0.09, 0.28), pan=-0.35 if e8 else 0.35)
        if last:
            # the build: sixteenth hats rising, a short roll, a riser cut on the line
            for k in range(8):
                both(b0 + 2 * beat + k * beat / 4, hat(0.05 + 0.012 * k, 0.02), pan=0.25 if k % 2 else -0.25)
            for k in range(4):
                both(b0 + 3 * beat + k * beat / 4, hat(0.10 + 0.04 * k, 0.06))
            n = int(4 * beat * SR)
            riser = rng.standard_normal(n)
            riser = riser - np.convolve(riser, np.ones(24) / 24, mode="same")
            both(b0, riser * np.linspace(0, 1, n) ** 2.2 * 0.09)
        bar += 1

    # the downbeat at 35.8: kick, sub, cymbal and the home chord, ringing out
    both(END, kick(1.1))
    both(END, np.sin(2 * np.pi * 41.2 * np.arange(int(2.6 * SR)) / SR) * env_ad(int(2.6 * SR), 0.005, 0.9) * 0.45)
    n = int(2.8 * SR)
    crash = rng.standard_normal(n)
    crash = crash - np.convolve(crash, np.ones(8) / 8, mode="same")
    both(END, crash * env_ad(n, 0.002, 0.7) * 0.11)
    l, r = pad([45, 57, 60, 64, 71], END, DUR - END, 0.2, attack=0.04)
    place(L, END, l)
    place(R, END, r)
    for k, m in enumerate([69, 72, 76, 81]):
        both(END + k * beat / 2, pluck(m, 0.08, 0.6), pan=-0.3 if k % 2 else 0.3)

    L = reverb(L)
    R = reverb(R)
    fade = np.clip((DUR - t) / 1.2, 0, 1)
    return L * fade, R * fade


def main():
    model, voices, out = sys.argv[1], sys.argv[2], sys.argv[3]
    v = voice(model, voices)
    L, R = score()
    # duck the music under the voice
    envv = np.convolve(np.abs(v), np.ones(int(0.12 * SR)) / int(0.12 * SR), mode="same")
    envv = np.convolve(envv, np.ones(int(0.25 * SR)) / int(0.25 * SR), mode="same")
    duck = 1 - 0.55 * np.clip(envv / (envv.max() * 0.25 + 1e-9), 0, 1)
    m = max(np.abs(L).max(), np.abs(R).max()) + 1e-9
    music_gain = 0.42 / m
    L = L * music_gain * duck + v * 0.95
    R = R * music_gain * duck + v * 0.95
    peak = max(np.abs(L).max(), np.abs(R).max())
    st = np.stack([L, R], axis=1) / peak * 0.89  # about -1 dBFS
    sf.write(out, st.astype(np.float32), SR)
    print("wrote", out)


if __name__ == "__main__":
    main()
