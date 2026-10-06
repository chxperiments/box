// box site behaviour. Every piece checks for its own markup, so one
// script serves every page.
(function () {
  "use strict";
  var calm = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function onView(el, fn) {
    if (!("IntersectionObserver" in window)) { fn(); return; }
    var io = new IntersectionObserver(function (es) {
      if (es.some(function (e) { return e.isIntersecting; })) { io.disconnect(); fn(); }
    }, { threshold: 0.3 });
    io.observe(el);
  }

  // SMIL motion in the figures stops under reduced motion.
  if (calm) document.querySelectorAll("svg.art").forEach(function (svg) {
    if (svg.pauseAnimations) { svg.pauseAnimations(); svg.setCurrentTime(0); }
  });

  // Reveal sections as they enter, in reading order; inverted sections wipe in.
  document.querySelectorAll(".reveal, .wipe").forEach(function (el) {
    if (calm) { el.classList.add("in"); return; }
    onView(el, function () { el.classList.add("in"); });
  });

  // Marker strokes and piece-by-piece builds start when they come into view.
  document.querySelectorAll(".on-view, .build").forEach(function (el) {
    if (calm) { el.classList.add("go"); return; }
    onView(el, function () { el.classList.add("go"); });
  });

  // Latency bars grow once visible, so the comparison is read as it draws.
  document.querySelectorAll(".bars").forEach(function (el) {
    if (calm) return;
    el.classList.add("pending");
    onView(el, function () { requestAnimationFrame(function () { el.classList.remove("pending"); }); });
  });

  // Copy buttons: data-copy holds the text, or data-copy-from names an element.
  document.querySelectorAll("[data-copy], [data-copy-from]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var text = btn.getAttribute("data-copy");
      var from = btn.getAttribute("data-copy-from");
      if (text === null && from) { var el = document.querySelector(from); text = el ? el.textContent : ""; }
      if (text === null) {
        var src = btn.closest(".codebox");
        var panel = src && src.querySelector("pre:not([hidden])");
        text = panel ? panel.textContent : "";
      }
      var label0 = btn.textContent;
      var done = function (label) { btn.textContent = label; setTimeout(function () { btn.textContent = label0; }, 1600); };
      if (navigator.clipboard) navigator.clipboard.writeText(text.trim()).then(function () { done("Copied"); }, function () { done("Select it"); });
      else done("Select it");
    });
  });

  // Long messages start folded; the button opens and closes them.
  document.querySelectorAll("[data-expand]").forEach(function (box) {
    var btn = box.querySelector(".msg-more");
    if (!btn) return;
    btn.addEventListener("click", function () {
      var open = box.classList.toggle("open");
      btn.setAttribute("aria-expanded", open ? "true" : "false");
      btn.textContent = open ? "Fold it back" : "Show the whole prompt";
    });
  });

  // Language tabs: one choice switches every code sample on the page and is
  // remembered, as reference docs usually do.
  var langKey = "box-lang";
  function setLang(lang) {
    document.querySelectorAll("[data-lang-panel]").forEach(function (p) {
      var group = p.closest("[data-lang-group]");
      var has = group && group.querySelector('[data-lang-panel="' + lang + '"]');
      p.hidden = has ? p.getAttribute("data-lang-panel") !== lang : p.hidden;
    });
    document.querySelectorAll("[data-lang]").forEach(function (t) {
      var group = t.closest("[data-lang-group]");
      var has = group && group.querySelector('[data-lang-panel="' + lang + '"]');
      if (has) {
        var on = t.getAttribute("data-lang") === lang;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
      }
    });
    try { localStorage.setItem(langKey, lang); } catch (e) {}
  }
  var tabs = document.querySelectorAll("[data-lang]");
  tabs.forEach(function (t) {
    t.addEventListener("click", function () { setLang(t.getAttribute("data-lang")); });
    t.addEventListener("keydown", function (e) {
      var d = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
      if (!d) return;
      var sib = Array.prototype.slice.call(t.parentNode.querySelectorAll("[data-lang]"));
      var next = sib[(sib.indexOf(t) + d + sib.length) % sib.length];
      setLang(next.getAttribute("data-lang")); next.focus(); e.preventDefault();
    });
  });
  if (tabs.length) {
    var saved = null;
    try { saved = localStorage.getItem(langKey); } catch (e) {}
    setLang(saved || tabs[0].getAttribute("data-lang"));
  }

  // Boxfile explorer: point at a line to read what it declares.
  var lines = Array.prototype.slice.call(document.querySelectorAll("#bf .ln[data-note]"));
  if (lines.length) {
    var key = document.getElementById("bf-key"), what = document.getElementById("bf-what"), becomes = document.getElementById("bf-becomes");
    var show = function (ln) {
      lines.forEach(function (l) { l.classList.toggle("on", l.dataset.k === ln.dataset.k); });
      key.textContent = ln.dataset.k; what.textContent = ln.dataset.note; becomes.textContent = ln.dataset.becomes;
    };
    lines.forEach(function (ln) {
      ["mouseenter", "focus", "click"].forEach(function (ev) { ln.addEventListener(ev, function () { show(ln); }); });
    });
    show(lines.filter(function (l) { return l.dataset.k === "backend"; })[0] || lines[0]);
  }

  // Op-art: concentric squares, each turned a little more than the one
  // outside it, so the stack reads as a box falling into itself. The twist
  // follows the pointer; it only animates while on screen, and holds one
  // still frame under reduced motion.
  document.querySelectorAll(".opart canvas").forEach(function (cv) {
    var ctx = cv.getContext("2d"); if (!ctx) return;
    var ink = getComputedStyle(cv).color || "#fff";
    var w = 0, h = 0, dpr = 1, t = 0, target = 0.06, twist = 0.06, running = false, raf = 0;
    function size() {
      dpr = Math.min(window.devicePixelRatio || 1, 2);
      w = cv.clientWidth; h = cv.clientHeight;
      cv.width = Math.round(w * dpr); cv.height = Math.round(h * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }
    function paint() {
      ctx.clearRect(0, 0, w, h);
      ctx.strokeStyle = ink; ctx.lineWidth = 1;
      var cx = w * 0.62, cy = h * 0.46, max = Math.hypot(w, h) * 0.75, n = 46;
      for (var i = 0; i < n; i++) {
        var f = 1 - i / n, s = max * f * f;
        ctx.save();
        ctx.translate(cx, cy);
        ctx.rotate(i * twist + t * (0.08 + i * 0.004));
        ctx.globalAlpha = 0.25 + 0.75 * (i / n);
        ctx.strokeRect(-s / 2, -s / 2, s, s);
        ctx.restore();
      }
    }
    function loop() {
      twist += (target - twist) * 0.05; t += 0.004;
      paint();
      if (running) raf = requestAnimationFrame(loop);
    }
    size(); paint();
    window.addEventListener("resize", function () { size(); paint(); });
    if (calm) return;
    cv.parentNode.addEventListener("pointermove", function (e) {
      var r = cv.getBoundingClientRect();
      target = 0.02 + ((e.clientX - r.left) / r.width) * 0.12;
    });
    if ("IntersectionObserver" in window) {
      new IntersectionObserver(function (es) {
        var vis = es.some(function (x) { return x.isIntersecting; });
        if (vis && !running) { running = true; loop(); }
        if (!vis) { running = false; cancelAnimationFrame(raf); }
      }).observe(cv);
    } else { running = true; loop(); }
  });

  // The box: a shaded ASCII cube, drawn with a z-buffer and flat lighting.
  // Drag turns it; it rests still under reduced motion.
  var out = document.getElementById("cube"), stage = document.getElementById("cube-stage");
  if (!out || !stage) return;
  var W = 46, H = 23, SCALE = 0.98, STEPS = 64, CAM = 4.2, RAMP = ".,-~:;=!*#$@";
  var LIGHT = [-0.42, -0.6, -0.68], PITCH = 1.4;
  var FACES = [
    [[1, 0, 0], [0, 1, 0], [0, 0, 1]], [[-1, 0, 0], [0, 0, 1], [0, 1, 0]],
    [[0, 1, 0], [0, 0, 1], [1, 0, 0]], [[0, -1, 0], [1, 0, 0], [0, 0, 1]],
    [[0, 0, 1], [1, 0, 0], [0, 1, 0]], [[0, 0, -1], [0, 1, 0], [1, 0, 0]]
  ];
  var zbuf = new Float32Array(W * H), cbuf = new Uint8Array(W * H);
  function frame(ax, ay) {
    var ca = Math.cos(ax), sa = Math.sin(ax), cb = Math.cos(ay), sb = Math.sin(ay);
    function rot(x, y, z) { var x1 = x * cb + z * sb, z1 = -x * sb + z * cb; return [x1, y * ca - z1 * sa, y * sa + z1 * ca]; }
    zbuf.fill(0); cbuf.fill(0);
    FACES.forEach(function (f) {
      var c = f[0], u = f[1], v = f[2], n = rot(c[0], c[1], c[2]);
      if (1 + n[2] * CAM >= 0) return;
      var lum = 0.18 + 0.82 * Math.max(0, n[0] * LIGHT[0] + n[1] * LIGHT[1] + n[2] * LIGHT[2]);
      var gi = Math.min(RAMP.length - 1, Math.max(0, Math.floor(lum * RAMP.length))) + 1;
      for (var i = 0; i <= STEPS; i++) {
        var s = -1 + (2 * i) / STEPS;
        for (var j = 0; j <= STEPS; j++) {
          var t = -1 + (2 * j) / STEPS;
          var p = rot(c[0] + u[0] * s + v[0] * t, c[1] + u[1] * s + v[1] * t, c[2] + u[2] * s + v[2] * t);
          var k = 1 / (p[2] + CAM), px = (W / 2 + p[0] * k * W * SCALE) | 0, py = (H / 2 + p[1] * k * H * SCALE) | 0;
          if (px < 0 || px >= W || py < 0 || py >= H) continue;
          var o = py * W + px;
          if (k > zbuf[o] + 1e-4) { zbuf[o] = k; cbuf[o] = gi; }
        }
      }
    });
    var str = "";
    for (var r = 0; r < H; r++) {
      var row = "";
      for (var x = 0; x < W; x++) { var g = cbuf[r * W + x]; row += g ? RAMP[g - 1] : " "; }
      str += row + "\n";
    }
    return str;
  }
  var ax = 0.52, ay = 0.68, vx = 0, vy = 0, dragging = false, lx = 0, ly = 0;
  var clamp = function (v, a, b) { return v < a ? a : v > b ? b : v; };
  function draw() { out.textContent = frame(ax, ay); }
  stage.addEventListener("pointerdown", function (e) { dragging = true; lx = e.clientX; ly = e.clientY; vx = vy = 0; stage.setPointerCapture(e.pointerId); });
  stage.addEventListener("pointermove", function (e) {
    if (!dragging) return;
    var dx = e.clientX - lx, dy = e.clientY - ly; lx = e.clientX; ly = e.clientY;
    ay += dx * 0.011; ax = clamp(ax + dy * 0.009, -PITCH, PITCH); vy = dx * 0.011; vx = dy * 0.009;
    if (calm) draw();
  });
  var release = function () { dragging = false; };
  stage.addEventListener("pointerup", release);
  stage.addEventListener("pointercancel", release);
  draw();
  if (calm) return;
  (function tick() {
    if (!dragging) { ay += 0.008 + vy; ax = clamp(ax + vx, -PITCH, PITCH); vy *= 0.94; vx *= 0.94; }
    draw();
    requestAnimationFrame(tick);
  })();
})();
