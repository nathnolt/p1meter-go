// Tiny dependency-free SVG charts: time-series line charts (with optional
// min/max band) and bar charts (optionally mirrored for import/export).
// Features: hover crosshair + tooltip, legend toggles, drag-to-zoom (double
// click resets), and a table view for accessibility.
(function () {
  const NS = "http://www.w3.org/2000/svg";
  const pad2 = (n) => String(n).padStart(2, "0");
  const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  const fmtHM = (t) => { const d = new Date(t * 1000); return pad2(d.getHours()) + ":" + pad2(d.getMinutes()); };
  const fmtHMS = (t) => { const d = new Date(t * 1000); return fmtHM(t) + ":" + pad2(d.getSeconds()); };
  const fmtDay = (t) => { const d = new Date(t * 1000); return DAYS[d.getDay()] + " " + d.getDate(); };
  const fmtDate = (t) => { const d = new Date(t * 1000); return DAYS[d.getDay()] + " " + d.getDate() + " " + MONTHS[d.getMonth()]; };
  const fmtFull = (t) => fmtDate(t) + " " + fmtHMS(t);

  function svg(tag, attrs, parent) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs || {}) {
      if (k === "text") e.textContent = attrs[k];
      else if (k === "style") Object.assign(e.style, attrs[k]);
      else e.setAttribute(k, attrs[k]);
    }
    if (parent) parent.appendChild(e);
    return e;
  }
  function html(tag, cls, parent, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    if (parent) parent.appendChild(e);
    return e;
  }

  function niceTicks(min, max, count) {
    if (!(max > min)) { max = min + 1; }
    const raw = (max - min) / Math.max(1, count);
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const norm = raw / mag;
    const step = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
    const lo = Math.floor(min / step) * step, hi = Math.ceil(max / step) * step;
    const ticks = [];
    for (let v = lo; v <= hi + step * 1e-6; v += step) ticks.push(+v.toFixed(10));
    return { ticks, step, lo, hi };
  }

  function timeTicks(t0, t1, maxN) {
    const span = t1 - t0;
    const steps = [1, 2, 5, 10, 15, 30, 60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 604800];
    const step = steps.find((s) => span / s <= maxN) || 604800;
    const out = [];
    if (step >= 86400) {
      const d = new Date(t0 * 1000);
      d.setHours(0, 0, 0, 0);
      if (d.getTime() / 1000 < t0) d.setDate(d.getDate() + 1);
      for (; d.getTime() / 1000 <= t1; d.setDate(d.getDate() + step / 86400)) {
        out.push({ t: d.getTime() / 1000, label: fmtDay(d.getTime() / 1000), major: true });
      }
    } else {
      const off = -new Date(t0 * 1000).getTimezoneOffset() * 60;
      for (let t = Math.ceil((t0 + off) / step) * step - off; t <= t1; t += step) {
        const d = new Date(t * 1000);
        const midnight = d.getHours() === 0 && d.getMinutes() === 0;
        out.push({ t, label: midnight && span > 36 * 3600 ? fmtDay(t) : step < 60 ? fmtHMS(t) : fmtHM(t), major: midnight && span > 36 * 3600 });
      }
    }
    return out;
  }

  function barPath(x, y0, w, h, r, up) {
    // rounded data end (away from the baseline), square at the baseline
    r = Math.max(0, Math.min(r, w / 2, Math.abs(h)));
    if (Math.abs(h) < 0.5) return "";
    const yt = up ? y0 - h : y0 + h; // far end
    if (up) {
      return `M${x},${y0}V${yt + r}Q${x},${yt} ${x + r},${yt}H${x + w - r}Q${x + w},${yt} ${x + w},${yt + r}V${y0}Z`;
    }
    return `M${x},${y0}V${yt - r}Q${x},${yt} ${x + r},${yt}H${x + w - r}Q${x + w},${yt} ${x + w},${yt - r}V${y0}Z`;
  }

  class TimeChart {
    constructor(host, kind) {
      this.host = host;
      this.kind = kind;
      this.hidden = new Set();
      this.seen = new Set();
      this.view = null;
      this.table = false;
      host.classList.add("chart");
      host.innerHTML = "";
      this.legendEl = html("div", "chart-legend", host);
      this.toolsEl = html("div", "chart-tools", host);
      this.plot = html("div", "chart-plot", host);
      this.tip = html("div", "chart-tip", host);
      this.tip.style.display = "none";
      this.tableEl = html("div", "chart-table", host);
      this.tableEl.style.display = "none";
      this.ro = new ResizeObserver(() => { if (this.o && host.clientWidth !== this.lastW) this.render(); });
      this.ro.observe(host);
    }

    set(o) {
      this.o = o;
      o.bands = (o.bands || (o.band ? [o.band] : [])).filter(Boolean);
      if (o.persist && !this.pLoaded) {
        this.pLoaded = true;
        try {
          const sv = JSON.parse(localStorage.getItem("p1h:" + o.persist) || "null");
          if (Array.isArray(sv)) { this.hidden = new Set(sv); for (const s of o.series) this.seen.add(s.key); }
        } catch {}
      }
      for (const s of o.series) {
        if (!this.seen.has(s.key)) { this.seen.add(s.key); if (s.hidden) this.hidden.add(s.key); }
      }
      this.render();
    }

    xRange() {
      const o = this.o;
      if (this.view) return this.view;
      if (o.xDomain) return o.xDomain;
      const n = o.x.length;
      if (!n) return [0, 1];
      return [o.x[0], o.x[n - 1] + (this.kind === "bar" ? o.bucket || 0 : 0)];
    }

    render() {
      const o = this.o;
      if (!o) return;
      this.renderLegend();
      this.renderTools();
      if (this.table) {
        this.plot.style.display = "none";
        this.tableEl.style.display = "";
        this.renderTable();
        return;
      }
      this.plot.style.display = "";
      this.tableEl.style.display = "none";

      const W = (this.lastW = this.host.clientWidth || 600), H = o.height || 240;
      const m = { l: o.marginLeft || 46, r: 10, t: o.unit ? 20 : 8, b: 22 };
      const iw = W - m.l - m.r, ih = H - m.t - m.b;
      const [x0, x1] = this.xRange();
      const X = (t) => m.l + ((t - x0) / (x1 - x0 || 1)) * iw;
      this.plot.innerHTML = "";
      const root = svg("svg", { width: W, height: H, viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": o.title || "chart" }, this.plot);
      this.geom = { m, iw, ih, x0, x1, W, H };

      const vis = o.series.filter((s) => !this.hidden.has(s.key));
      const n = o.x.length;
      // visible index range
      let i0 = 0, i1 = n - 1;
      if (this.view) {
        const bw = this.kind === "bar" ? o.bucket : 0;
        while (i0 < n && o.x[i0] + bw < x0) i0++;
        while (i1 >= 0 && o.x[i1] > x1) i1--;
      }

      // ---- y domain ----
      let ymin = Infinity, ymax = -Infinity;
      const take = (v) => { if (v != null && isFinite(v)) { if (v < ymin) ymin = v; if (v > ymax) ymax = v; } };
      if (this.kind === "bar") {
        for (let i = i0; i <= i1; i++) {
          let pos = 0, neg = 0;
          for (const s of vis) {
            const v = s.data[i];
            if (v == null) continue;
            const sv = s.negate ? -v : v;
            if (o.stacked) { if (sv >= 0) pos += sv; else neg += sv; } else take(sv);
          }
          if (o.stacked) { take(pos); take(neg); }
        }
        if (o.zeroBase !== false) take(0);
      } else {
        for (const s of vis) for (let i = i0; i <= i1; i++) take(s.data[i]);
        if (o.bands.length && !this.hidden.has("__band")) for (const bd of o.bands) for (let i = i0; i <= i1; i++) { take(bd.lo[i]); take(bd.hi[i]); }
        if (o.zeroLine !== false) take(0);
      }
      if (!isFinite(ymin)) { ymin = 0; ymax = 1; }
      if (o.yMin != null) ymin = Math.min(ymin, o.yMin);
      if (o.yMax != null) ymax = Math.max(ymax, o.yMax);
      const nt = niceTicks(ymin, ymax, Math.max(2, Math.floor(ih / 42)));
      const yLo = nt.lo, yHi = nt.hi;
      const Y = (v) => m.t + ih - ((v - yLo) / (yHi - yLo || 1)) * ih;
      this.geom.Y = Y; this.geom.X = X; this.geom.yLo = yLo; this.geom.yHi = yHi;

      // ---- grid & axes ----
      const dec = nt.step < 1 ? Math.min(5, Math.ceil(-Math.log10(nt.step))) : 0;
      for (const v of nt.ticks) {
        svg("line", { x1: m.l, x2: W - m.r, y1: Y(v), y2: Y(v), class: v === 0 && yLo < 0 ? "zero" : "grid" }, root);
        svg("text", { x: m.l - 6, y: Y(v) + 4, class: "tick", "text-anchor": "end", text: v.toLocaleString(undefined, { maximumFractionDigits: dec }) }, root);
      }
      if (o.unit) svg("text", { x: 2, y: 11, class: "unit", text: o.unit }, root);

      if (this.kind === "bar") {
        const bw = o.bucket || 3600;
        const slotW = (iw * bw) / (x1 - x0 || 1);
        const every = Math.max(1, Math.ceil((bw >= 86400 ? 46 : bw < 3600 ? 64 : 46) / slotW));
        for (let i = i0; i <= i1; i++) {
          if ((i - i0) % every) continue;
          const t = o.x[i];
          const label = bw >= 86400 ? fmtDay(t) : bw >= 3600 ? pad2(new Date(t * 1000).getHours()) + ":00" : bw >= 60 ? fmtHM(t) : fmtHMS(t);
          const cx = X(t) + slotW / 2;
          svg("text", { x: cx, y: H - 6, class: "tick", "text-anchor": "middle", text: label }, root);
        }
      } else {
        const tt = timeTicks(x0, x1, Math.max(2, Math.floor(iw / 70)));
        for (const tk of tt) {
          const x = X(tk.t);
          svg("line", { x1: x, x2: x, y1: m.t + ih, y2: m.t + ih + 4, class: "axis" }, root);
          svg("text", { x, y: H - 6, class: "tick" + (tk.major ? " major" : ""), "text-anchor": "middle", text: tk.label }, root);
        }
        svg("line", { x1: m.l, x2: W - m.r, y1: m.t + ih, y2: m.t + ih, class: "axis" }, root);
      }

      // ---- marks ----
      const clip = "clip" + Math.random().toString(36).slice(2);
      const cp = svg("clipPath", { id: clip }, root);
      svg("rect", { x: m.l, y: m.t - 2, width: iw, height: ih + 4 }, cp);
      const g = svg("g", { "clip-path": `url(#${clip})` }, root);
      // hatched areas = no data was logged there
      const hatch = [];
      if (this.kind === "bar" && o.cover) {
        const bw0 = o.bucket || 3600;
        for (let i = i0; i <= i1; i++) if (o.cover[i] != null && o.cover[i] < 0.9) hatch.push([o.x[i], o.x[i] + bw0]);
      } else if (o.gaps) for (const gp of o.gaps) hatch.push(gp);
      if (hatch.length) {
        const pid = "hp" + Math.random().toString(36).slice(2);
        const pat = svg("pattern", { id: pid, width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" }, root);
        svg("line", { x1: 0, y1: 0, x2: 0, y2: 6, style: { stroke: "var(--axis)", "stroke-width": 1.5 } }, pat);
        for (const [a, b] of hatch) {
          const xa = Math.max(m.l, X(a)), xb = Math.min(m.l + iw, X(b));
          if (xb > xa) svg("rect", { x: xa, y: m.t, width: xb - xa, height: ih, style: { fill: `url(#${pid})`, opacity: 0.55 } }, g);
        }
      }

      if (this.kind === "bar") {
        const bw = o.bucket || 3600;
        const slotW = (iw * bw) / (x1 - x0 || 1);
        const w = Math.max(1, slotW - Math.min(3, slotW * 0.2) - (slotW > 12 ? 1 : 0));
        const gap = (slotW - w) / 2;
        const grp = o.grouped && !o.stacked && vis.length > 1 ? vis.length : 1; // side-by-side bars
        const B = Math.min(Math.max(0, yLo), yHi); // baseline: zero, or the axis edge when the axis does not start at zero
        for (let i = i0; i <= i1; i++) {
          const x = X(o.x[i]) + gap;
          let pos = 0, neg = 0;
          for (let k = 0; k < vis.length; k++) {
            const s = vis[k];
            const v = s.data[i];
            if (v == null || !isFinite(v)) continue;
            const sv = s.negate ? -v : v;
            let base, y;
            if (o.stacked) { base = sv >= 0 ? pos : neg; if (sv >= 0) pos += sv; else neg += sv; } else base = B;
            const yA = Y(base), yB = o.stacked ? Y(base + sv) : Y(sv);
            const up = o.stacked ? sv >= 0 : sv >= B;
            const h = Math.abs(yA - yB);
            const sub = w / grp;
            const d = grp > 1 ? barPath(x + k * sub, yA, Math.max(1, sub - (sub > 4 ? 1 : 0)), h, Math.min(3, sub / 2), up) : barPath(x, yA, w, h, 3, up);
            if (d) svg("path", { d, style: { fill: s.color, opacity: o.cover && o.cover[i] != null && o.cover[i] < 0.9 ? 0.5 : 1 } }, g);
          }
        }
      } else {
        if (o.bands.length && !this.hidden.has("__band")) for (const bd of o.bands) this.drawBand(g, bd, X, Y, i0, i1);
        for (const s of vis) this.drawLine(g, s, X, Y, i0, i1, o);
      }
      if (o.marker != null && o.marker >= x0 && o.marker <= x1) {
        svg("line", { x1: X(o.marker), x2: X(o.marker), y1: m.t, y2: m.t + ih, class: "marker" }, g);
      }

      // ---- hover layer ----
      const cross = svg("line", { y1: m.t, y2: m.t + ih, class: "cross", style: { display: "none" } }, root);
      const hl = svg("rect", { y: m.t, height: ih, class: "hl", style: { display: "none" } }, root);
      const dots = vis.map((s) => svg("circle", { r: 4, style: { fill: s.color, display: "none" }, class: "dot" }, root));
      const ov = svg("rect", { x: m.l, y: m.t, width: iw, height: ih, class: "overlay" }, root);
      const sel = svg("rect", { y: m.t, height: ih, class: "sel", style: { display: "none" } }, root);

      const idxAt = (px) => {
        const t = x0 + ((px - m.l) / iw) * (x1 - x0);
        if (this.kind === "bar") return Math.floor((t - o.x[0]) / (o.bucket || 3600));
        let lo = 0, hi = n - 1;
        while (lo < hi) { const mid = (lo + hi) >> 1; if (o.x[mid] < t) lo = mid + 1; else hi = mid; }
        if (lo > 0 && Math.abs(o.x[lo - 1] - t) < Math.abs(o.x[lo] - t)) lo--;
        return lo;
      };
      const local = (ev) => { const r = ov.getBoundingClientRect(); return ev.clientX - r.left + m.l; };
      const hide = () => {
        this.tip.style.display = "none";
        cross.style.display = hl.style.display = "none";
        dots.forEach((d) => (d.style.display = "none"));
      };
      const show = (ev) => {
        if (drag) return;
        const px = local(ev);
        const i = idxAt(px);
        if (i < 0 || i >= n || o.x[i] < x0 - (o.bucket || 0) || o.x[i] > x1) return hide();
        const t = o.x[i];
        let anyVal = false;
        let rows = "";
        vis.forEach((s, k) => {
          const v = s.data[i];
          const has = v != null && isFinite(v);
          anyVal = anyVal || has;
          rows += `<div class="tip-row"><i style="background:${s.color}"></i><span>${s.name}</span><b>${has ? this.fv(s, v, i) : "–"}</b></div>`;
          const d = dots[k];
          if (this.kind === "line" && has) {
            d.setAttribute("cx", X(t)); d.setAttribute("cy", Y(v)); d.style.display = "";
          } else d.style.display = "none";
        });
        if (this.kind === "line" && !this.hidden.has("__band")) {
          for (const bd of o.bands) {
            if (bd.lo[i] == null) continue;
            const f = bd.fmt || o.fmt || ((v) => v);
            rows += `<div class="tip-row"><i class="band-sw" style="background:${bd.color}"></i><span>${bd.name || "range"}</span><b>${f(bd.lo[i])} … ${f(bd.hi[i])}</b></div>`;
          }
        }
        if (this.kind === "bar" && o.cover && o.cover[i] != null && o.cover[i] < 0.99) {
          rows += `<div class="tip-row"><i></i><span>Data coverage</span><b>${Math.round(o.cover[i] * 100)}%</b></div><div class="tip-note">Missing time is spread evenly</div>`;
        }
        if (this.kind === "line" && o.gaps && o.gaps.some((gp) => t >= gp[0] && t <= gp[1])) {
          rows += `<div class="tip-row"><i></i><span>No data logged</span><b></b></div>`;
        }
        if (!anyVal && !rows) return hide();
        const bw = o.bucket || 3600;
        if (this.kind === "bar") {
          const sx = X(t), sw = (iw * bw) / (x1 - x0 || 1);
          hl.setAttribute("x", sx); hl.setAttribute("width", sw); hl.style.display = "";
        } else {
          cross.setAttribute("x1", X(t)); cross.setAttribute("x2", X(t)); cross.style.display = "";
        }
        const title = o.tipTitle ? o.tipTitle(t) : fmtFull(t);
        this.tip.innerHTML = `<div class="tip-title">${title}</div>${rows}`;
        this.tip.style.display = "";
        const cx = this.kind === "bar" ? X(t) + (iw * bw) / (x1 - x0 || 1) / 2 : X(t);
        const tw = this.tip.offsetWidth;
        let left = cx + 14;
        if (left + tw > W - 4) left = cx - tw - 14;
        this.tip.style.left = Math.max(4, left) + "px";
        this.tip.style.top = this.plot.offsetTop + m.t + 6 + "px";
      };

      let drag = null;
      ov.addEventListener("pointermove", (ev) => {
        if (drag) {
          drag.x = local(ev);
          const a = Math.min(drag.x0, drag.x), b = Math.max(drag.x0, drag.x);
          sel.setAttribute("x", a); sel.setAttribute("width", b - a); sel.style.display = "";
          return;
        }
        show(ev);
      });
      ov.addEventListener("pointerleave", () => { if (!drag) hide(); });
      if (o.zoomable !== false && (this.kind === "line" || o.onZoom)) {
        ov.addEventListener("pointerdown", (ev) => {
          if (ev.button !== 0) return;
          drag = { x0: local(ev), x: local(ev) };
          ov.setPointerCapture(ev.pointerId);
          hide();
        });
        const end = (ev) => {
          if (!drag) return;
          const a = Math.min(drag.x0, drag.x), b = Math.max(drag.x0, drag.x);
          drag = null;
          sel.style.display = "none";
          if (b - a > 10) {
            const ta = x0 + ((a - m.l) / iw) * (x1 - x0), tb = x0 + ((b - m.l) / iw) * (x1 - x0);
            if (o.onZoom) { o.onZoom(ta, tb); return; } // the page re-queries at finer detail
            this.view = [ta, tb];
            this.render();
          }
        };
        ov.addEventListener("pointerup", end);
        ov.addEventListener("pointercancel", () => { drag = null; sel.style.display = "none"; });
        ov.addEventListener("dblclick", () => { if (this.view) { this.view = null; this.render(); } });
      }
    }

    // formatted value of series s at index i (s.raw = real values when s.data is normalized)
    fv(s, v, i) {
      const o = this.o;
      if (s.fmt) return s.fmt(s.raw ? s.raw[i] : v);
      return o.fmt ? o.fmt(v, s) : v;
    }

    drawLine(g, s, X, Y, i0, i1, o) {
      const gap = o.gap || Infinity;
      const runs = [];
      let cur = null, last = null;
      const a = Math.max(0, i0 - 1), b = Math.min(o.x.length - 1, i1 + 1);
      for (let i = a; i <= b; i++) {
        const v = s.data[i];
        const ok = v != null && isFinite(v);
        if (!ok) { cur = null; last = null; continue; }
        if (cur && last != null && o.x[i] - last > gap) cur = null;
        const x = X(o.x[i]), y = Y(v);
        if (!cur) { cur = { cmd: `M${x.toFixed(1)},${y.toFixed(1)}`, rest: "", x0: x, y0: y, xn: x }; runs.push(cur); }
        else {
          const seg = o.step ? `H${x.toFixed(1)}V${y.toFixed(1)}` : `L${x.toFixed(1)},${y.toFixed(1)}`;
          cur.cmd += seg; cur.rest += seg;
        }
        cur.xn = x;
        last = o.x[i];
      }
      if (!runs.length) return;
      if (s.fill) {
        const { yLo, yHi } = this.geom;
        const yb = Y(Math.min(Math.max(0, yLo), yHi));
        const area = runs.map((r) => `M${r.x0.toFixed(1)},${yb.toFixed(1)}L${r.x0.toFixed(1)},${r.y0.toFixed(1)}${r.rest}L${r.xn.toFixed(1)},${yb.toFixed(1)}Z`).join("");
        svg("path", { d: area, class: "area", style: { fill: s.color } }, g);
      }
      svg("path", { d: runs.map((r) => r.cmd).join(""), class: "line", style: { stroke: s.color, "stroke-width": s.width || 2 } }, g);
    }

    drawBand(g, band, X, Y, i0, i1) {
      const o = this.o;
      let seg = [];
      const flush = () => {
        if (seg.length > 1) {
          const top = seg.map((i) => `${X(o.x[i]).toFixed(1)},${Y(band.hi[i]).toFixed(1)}`).join("L");
          const bot = seg.slice().reverse().map((i) => `${X(o.x[i]).toFixed(1)},${Y(band.lo[i]).toFixed(1)}`).join("L");
          svg("path", { d: `M${top}L${bot}Z`, class: "band", style: { fill: band.color } }, g);
        }
        seg = [];
      };
      for (let i = Math.max(0, i0 - 1); i <= Math.min(o.x.length - 1, i1 + 1); i++) {
        if (band.lo[i] == null || band.hi[i] == null) flush(); else seg.push(i);
      }
      flush();
    }

    renderLegend() {
      const o = this.o, el = this.legendEl;
      el.innerHTML = "";
      const items = o.series.map((s) => ({ key: s.key, name: s.name, color: s.color }));
      if (o.bands.length) items.push({ key: "__band", name: o.bandName || "Lowest…highest", color: o.bands[0].color, band: true });
      if (items.length < 2) { el.style.display = "none"; return; }
      el.style.display = "";
      html("span", "lg-hint", el, "Show:");
      for (const it of items) {
        const b = html("button", "lg" + (this.hidden.has(it.key) ? " off" : ""), el);
        b.type = "button";
        const sw = html("i", it.band ? "band-sw" : "", b);
        sw.style.background = it.color;
        html("span", "", b, it.name);
        b.title = "Show / hide";
        b.onclick = () => {
          this.hidden.has(it.key) ? this.hidden.delete(it.key) : this.hidden.add(it.key);
          if (o.persist) { try { localStorage.setItem("p1h:" + o.persist, JSON.stringify([...this.hidden])); } catch {} }
          this.render();
        };
      }
    }

    renderTools() {
      const el = this.toolsEl;
      el.innerHTML = "";
      if (this.view) {
        const b = html("button", "tool", el, "Reset zoom");
        b.type = "button";
        b.onclick = () => { this.view = null; this.render(); };
      }
      if (this.o.table !== false) {
        const b = html("button", "tool", el, this.table ? "Chart" : "Table");
        b.type = "button";
        b.onclick = () => { this.table = !this.table; this.render(); };
      }
    }

    renderTable() {
      const o = this.o, el = this.tableEl;
      const n = o.x.length;
      const stride = Math.max(1, Math.ceil(n / 300));
      let h = `<table><thead><tr><th>Time</th>${o.series.map((s) => `<th>${s.name}</th>`).join("")}</tr></thead><tbody>`;
      for (let i = 0; i < n; i += stride) {
        const t = o.x[i];
        const label = this.kind === "bar" && (o.bucket || 0) >= 86400 ? fmtDate(t) : fmtDate(t) + " " + ((o.bucket || 0) < 60 ? fmtHMS(t) : fmtHM(t));
        h += `<tr><td>${label}</td>${o.series.map((s) => `<td>${s.data[i] == null ? "–" : this.fv(s, s.data[i], i)}</td>`).join("")}</tr>`;
      }
      el.innerHTML = h + "</tbody></table>" + (stride > 1 ? `<p class="muted">Showing every ${stride}th row.</p>` : "");
    }
  }


  // ---------------------------------------------------------------- pie / donut
  class PieChart {
    constructor(host) {
      this.host = host;
      this.hidden = new Set();
      this.seen = new Set();
      host.classList.add("chart");
      host.innerHTML = "";
      this.wrap = html("div", "pie-wrap", host);
      this.tip = html("div", "chart-tip", host);
      this.tip.style.display = "none";
      this.ro = new ResizeObserver(() => { if (this.o && host.clientWidth !== this.lastW) this.render(); });
      this.ro.observe(host);
    }
    set(o) {
      this.o = o;
      if (o.persist && !this.pLoaded) {
        this.pLoaded = true;
        try {
          const sv = JSON.parse(localStorage.getItem("p1h:" + o.persist) || "null");
          if (Array.isArray(sv)) this.hidden = new Set(sv);
        } catch {}
      }
      this.render();
    }
    render() {
      const o = this.o, W = (this.lastW = this.host.clientWidth || 500);
      const slices = o.slices;
      const vis = slices.filter((s) => !this.hidden.has(s.key) && s.value > 0);
      const total = vis.reduce((a, s) => a + s.value, 0);
      const S = Math.max(150, Math.min(o.height || 260, W * (W < 520 ? 0.9 : 0.5)));
      const R = S / 2 - 4, r = o.donut ? R * 0.6 : 0, c = S / 2;
      this.wrap.innerHTML = "";
      const box = html("div", "pie-svg", this.wrap);
      const root = svg("svg", { width: S, height: S, viewBox: `0 0 ${S} ${S}`, role: "img", "aria-label": o.title || "pie chart" }, box);
      const pt = (rad, a) => [c + rad * Math.sin(a), c - rad * Math.cos(a)];
      let a0 = 0;
      const paths = [];
      for (const s of vis) {
        const frac = s.value / total, a1 = a0 + frac * 2 * Math.PI;
        let d;
        if (frac > 0.9999) {
          const top = pt(R, 0), bot = pt(R, Math.PI);
          d = `M${top}A${R},${R} 0 1 1 ${bot}A${R},${R} 0 1 1 ${top}Z`;
          if (r) { const ti = pt(r, 0), bi = pt(r, Math.PI); d += `M${ti}A${r},${r} 0 1 0 ${bi}A${r},${r} 0 1 0 ${ti}Z`; }
        } else {
          const large = a1 - a0 > Math.PI ? 1 : 0;
          const p0 = pt(R, a0), p1 = pt(R, a1);
          if (r) { const q1 = pt(r, a1), q0 = pt(r, a0); d = `M${p0}A${R},${R} 0 ${large} 1 ${p1}L${q1}A${r},${r} 0 ${large} 0 ${q0}Z`; }
          else d = `M${c},${c}L${p0}A${R},${R} 0 ${large} 1 ${p1}Z`;
        }
        const el = svg("path", { d, "fill-rule": "evenodd", class: "slice", style: { fill: s.color } }, root);
        paths.push([s, el, frac]);
        el.addEventListener("pointermove", (ev) => this.showTip(ev, s, frac));
        el.addEventListener("pointerleave", () => (this.tip.style.display = "none"));
        a0 = a1;
      }
      if (!vis.length) svg("text", { x: c, y: c, "text-anchor": "middle", class: "tick", text: "nothing to show" }, root);
      if (o.donut && vis.length) {
        svg("text", { x: c, y: c - 2, "text-anchor": "middle", class: "pie-total", text: o.totalText ? o.totalText(total) : String(Math.round(total)) }, root);
        svg("text", { x: c, y: c + 16, "text-anchor": "middle", class: "tick", text: o.totalLabel || "total" }, root);
      }
      // legend with value and share; click hides a slice
      const lg = html("div", "pie-legend", this.wrap);
      for (const s of slices) {
        const off = this.hidden.has(s.key);
        const b = html("button", "pl" + (off ? " off" : ""), lg);
        b.type = "button";
        const sw = html("i", "", b); sw.style.background = s.color;
        html("span", "pl-name", b, s.name);
        html("b", "pl-val", b, s.fmt(s.value));
        html("em", "pl-pct", b, !off && total > 0 && s.value > 0 ? (s.value / total * 100).toFixed(s.value / total < 0.1 ? 1 : 0) + " %" : "–");
        b.title = "Show / hide";
        b.onclick = () => {
          off ? this.hidden.delete(s.key) : this.hidden.add(s.key);
          if (o.persist) { try { localStorage.setItem("p1h:" + o.persist, JSON.stringify([...this.hidden])); } catch {} }
          this.render();
        };
      }
    }
    showTip(ev, s, frac) {
      const r = this.host.getBoundingClientRect();
      this.tip.innerHTML = `<div class="tip-title">${s.name}</div><div class="tip-row"><i style="background:${s.color}"></i><span>${s.fmt(s.value)}</span><b>${(frac * 100).toFixed(1)} %</b></div>`;
      this.tip.style.display = "";
      let left = ev.clientX - r.left + 14;
      if (left + this.tip.offsetWidth > r.width) left = ev.clientX - r.left - this.tip.offsetWidth - 14;
      this.tip.style.left = Math.max(4, left) + "px";
      this.tip.style.top = ev.clientY - r.top + 12 + "px";
    }
  }

  // Reuse the chart on a host only if it is the same kind; otherwise tear it down and build a new one
  // (switching a panel between line, bars and pie must redraw with the new type).
  function chartFor(host, Cls, kind) {
    const c = host.__chart;
    if (c && c instanceof Cls && (kind === undefined || c.kind === kind)) return c;
    if (c && c.ro) c.ro.disconnect();
    host.innerHTML = "";
    return (host.__chart = kind === undefined ? new Cls(host) : new Cls(host, kind));
  }

  window.Charts = {
    pie: (host, o) => { const c = chartFor(host, PieChart); c.set(o); return c; },
    line: (host, o) => { const c = chartFor(host, TimeChart, "line"); c.set(o); return c; },
    bar: (host, o) => { const c = chartFor(host, TimeChart, "bar"); c.set(o); return c; },
    fmtHM, fmtHMS, fmtDay, fmtDate, fmtFull,
  };
})();
