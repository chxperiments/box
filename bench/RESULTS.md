# Benchmark results

`python3 bench/bench.py -n 20`, run 2026-09-26 on 6.18.33.2-microsoft-standard-WSL2, crun 1.28,
libkrun 1.19.0, pydantic-monty 1.0.0. Wall-clock latency from the host,
median of 20 runs after one warm-up. Warm runs are spaced 2.5s
apart, so each finds a pooled VM while the pool is refilling in the background.
With the refill finished, a warm no-op run measures ~20-30ms. The "python" workload is
`sum(i * i for i in range(100_000))`.

```
tool                         isolation                 noop median  python median  python p95
host process                 none                          0.78 ms       19.42 ms    27.44 ms
podman + runc                container                   447.61 ms      490.04 ms   589.06 ms
podman + krun                microVM                    1487.67 ms     1834.47 ms  1936.43 ms
box run (cold)           microVM                    1357.34 ms     1477.01 ms  1637.08 ms
box run (warm: 2)        microVM, fresh per run       44.42 ms      151.08 ms   188.34 ms
box exec (up)            microVM, persistent          14.84 ms       56.42 ms    68.26 ms
monty, new session           interpreter subprocess        0.49 ms       12.19 ms    13.57 ms
monty, same session          interpreter subprocess        0.21 ms       12.07 ms    13.64 ms
```

boxd was not measured (it is a hosted service). Its published figures are a
fresh boot in under 10ms, a fork in under 200ms and a resume in under 1ms,
with no stated hardware or method.
