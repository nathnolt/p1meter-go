(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const nf = (v, d = 0) => (v == null || isNaN(v) ? "–" : v.toLocaleString(undefined, { minimumFractionDigits: d, maximumFractionDigits: d }));
  const kwh = (v) => nf(v, v != null && Math.abs(v) < 1 ? 3 : 2);
  const pad2 = (n) => String(n).padStart(2, "0");
  const C = Charts;
  const store = {
    get: (k, d) => { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
    set: (k, v) => { try { localStorage.setItem(k, v); } catch {} },
  };

  async function api(method, path, body) {
    let r, j;
    try {
      r = await fetch(path, { method, headers: body ? { "content-type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
      j = await r.json();
    } catch (e) {
      return { ok: false, error: r ? "HTTP " + r.status : "Could not reach the app: " + e.message };
    }
    if (r.status >= 400 && j.ok === undefined) j.ok = false;
    return j;
  }
  function toast(msg, bad) {
    const d = document.createElement("div");
    d.textContent = msg;
    if (bad) d.className = "bad";
    $("toast").appendChild(d);
    setTimeout(() => d.remove(), bad ? 6000 : 3500);
  }
  const fmtPower = (w) => (Math.abs(w) >= 1000 ? [nf(w / 1000, 2), "kW"] : [nf(w, 0), "W"]);
  const fmtBytes = (b) => (b < 1024 ? b + " B" : b < 1048576 ? nf(b / 1024, 0) + " kB" : nf(b / 1048576, 1) + " MB");
  const fmtDur = (s) => {
    s = Math.max(0, Math.round(s));
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    return d ? `${d} d ${h} h` : h ? `${h} h ${m} min` : m ? `${m} min ${s % 60} s` : `${s} s`;
  };
  const fmtClock = (s) => {
    s = Math.max(0, Math.round(s));
    const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
    return (h ? h + ":" + pad2(m) : m) + ":" + pad2(s % 60);
  };
  const ymd = (d) => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
  const fmtBucket = (b) => (!b ? "every sample" : b < 60 ? b + " s" : b < 3600 ? b / 60 + " min" : b < 86400 ? b / 3600 + " h" : "day");
  const hm = (t, b) => (b < 60 ? C.fmtHMS(t) : C.fmtHM(t));
  const bucketTitle = (t, b) => (b >= 86400 ? C.fmtDate(t) : `${C.fmtDate(t)} ${hm(t, b)}–${hm(t + b, b)}`);
  const any = (a, min = 0.0005) => a && a.some((v) => v != null && v > min);
  const present = (a) => a && a.some((v) => v != null);

  const FIELDS = {
    power_w: ["Power (total)", "W"], power_l1_w: ["Power L1", "W"], power_l2_w: ["Power L2", "W"], power_l3_w: ["Power L3", "W"],
    voltage_l1_v: ["Voltage L1", "V"], voltage_l2_v: ["Voltage L2", "V"], voltage_l3_v: ["Voltage L3", "V"],
    current_l1_a: ["Current L1", "A"], current_l2_a: ["Current L2", "A"], current_l3_a: ["Current L3", "A"],
    frequency_hz: ["Frequency", "Hz"],
    energy_import_kwh: ["Import meter (total)", "kWh"], energy_import_t1_kwh: ["Import meter, tariff 1", "kWh"], energy_import_t2_kwh: ["Import meter, tariff 2", "kWh"],
    energy_export_kwh: ["Export meter (total)", "kWh"], energy_export_t1_kwh: ["Export meter, tariff 1", "kWh"], energy_export_t2_kwh: ["Export meter, tariff 2", "kWh"],
    gas_m3: ["Gas meter", "m³"], water_m3: ["Water meter", "m³"], tariff: ["Active tariff", ""],
  };

  let info = { prices: { currency: "€", import: 0, export: 0, gas: 0 } };
  let LABELS = {};
  const cost = (imp, exp, gas) => {
    const p = info.prices;
    if (imp == null && gas == null) return null;
    return (imp || 0) * p.import - (exp || 0) * p.export + (gas || 0) * p.gas;
  };
  const money = (v) => (v == null ? "–" : (v < 0 ? "−" : "") + info.prices.currency + nf(Math.abs(v), 2));

  // ---------------------------------------------------------------- tabs
  const tabs = ["live", "history", "logs", "device"];
  let tab = null, timers = [];
  function clearTimers() { timers.forEach(clearInterval); timers = []; }
  function every(ms, fn) { timers.push(setInterval(fn, ms)); }

  function showTab(name) {
    if (!tabs.includes(name)) name = "live";
    if (tab === name) return;
    tab = name;
    clearTimers();
    stopStream();
    document.querySelectorAll(".tab").forEach((s) => s.classList.toggle("on", s.id === "tab-" + name));
    document.querySelectorAll("#nav button").forEach((b) => b.classList.toggle("on", b.dataset.tab === name));
    if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
    if (name === "live") enterLive();
    if (name === "history") enterHistory();
    if (name === "logs") enterLogs();
    if (name === "device") enterDevice();
  }
  document.querySelectorAll("#nav button").forEach((b) => (b.onclick = () => showTab(b.dataset.tab)));
  window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));
  document.addEventListener("visibilitychange", () => {
    if (tab === "live") document.hidden ? stopStream() : liveSt.active && startStream();
  });

  const themes = ["auto", "light", "dark"];
  function applyTheme(t) {
    if (t === "auto") delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t;
    $("theme").textContent = t === "auto" ? "Auto" : t === "light" ? "Light" : "Dark";
    $("theme").title = "Theme: " + t;
  }
  let theme = store.get("theme", "auto");
  applyTheme(theme);
  $("theme").onclick = () => {
    theme = themes[(themes.indexOf(theme) + 1) % 3];
    store.set("theme", theme);
    applyTheme(theme);
  };

  // ---------------------------------------------------------------- shared info / status
  function setPill(kind, text, title) {
    const p = $("status");
    p.className = "pill " + (kind || "");
    p.lastElementChild.textContent = text;
    p.title = title || "";
  }
  let streamOK = false;
  async function refreshInfo() {
    const j = await api("GET", "/api/info");
    if (!j.ok) return setPill("err", "App unreachable");
    info = j; LABELS = j.labels || {};
    $("setup-banner").hidden = !!(j.host && j.paired);
    if (streamOK) return j;
    if (!j.host || !j.paired) setPill("err", "Not connected", "Set the meter address and pair it on the Device tab");
    else if (j.meterError) setPill("err", "Meter unreachable", j.meterError);
    else setPill("ok", "Meter OK");
    return j;
  }

  // ---------------------------------------------------------------- stat tiles
  function delta(cur, prev, label) {
    if (cur == null || prev == null || prev < 0.01) return "";
    const pct = ((cur - prev) / prev) * 100;
    if (Math.abs(pct) < 0.5) return `<span class="muted">same as ${label}</span>`;
    return `<span class="delta ${pct > 0 ? "up" : "down"}">${pct > 0 ? "▲ +" : "▼ −"}${nf(Math.abs(pct), 0)}%</span> <span class="muted">vs ${label}</span>`;
  }
  const tile = (label, value, unit, sub, swatch) =>
    `<div class="stat"><div class="label">${swatch ? `<span class="sw" style="background:${swatch}"></span>` : ""}${label}</div>` +
    `<div class="v">${value}${unit ? `<small>${unit}</small>` : ""}</div><div class="s">${sub || ""}</div></div>`;

  function summaryTiles(sm, prev, prevLabel) {
    const out = [];
    if (sm.import_kwh != null) {
      const split = sm.import_t1_kwh != null && sm.import_t2_kwh != null && sm.import_t1_kwh + sm.import_t2_kwh > 0
        ? `${tName(1)} ${nf(sm.import_t1_kwh, 2)} · ${tName(2)} ${nf(sm.import_t2_kwh, 2)}` : "";
      out.push(tile("Electricity import", kwh(sm.import_kwh), "kWh", (prev ? delta(sm.import_kwh, prev.import_kwh, prevLabel) + "<br>" : "") + split, "var(--s2)"));
    }
    if (sm.export_kwh != null && (sm.export_kwh > 0.0005 || (prev && prev.export_kwh > 0.005)))
      out.push(tile("Fed back to grid", kwh(sm.export_kwh), "kWh", prev ? delta(sm.export_kwh, prev.export_kwh, prevLabel) : "", "var(--s3)"));
    if (sm.gas_m3 != null)
      out.push(tile("Gas", nf(sm.gas_m3, sm.gas_m3 < 1 ? 3 : 2), "m³", prev ? delta(sm.gas_m3, prev.gas_m3, prevLabel) : "", "var(--s4)"));
    if (sm.water_m3 != null && sm.water_m3 > 0) out.push(tile("Water", nf(sm.water_m3 * 1000, 0), "L", prev ? delta(sm.water_m3, prev.water_m3, prevLabel) : ""));
    if (sm.import_kwh != null || sm.gas_m3 != null) {
      const c = cost(sm.import_kwh, sm.export_kwh, sm.gas_m3);
      const ce = cost(sm.import_kwh, sm.export_kwh, 0), cg = (sm.gas_m3 || 0) * info.prices.gas;
      out.push(tile("Estimated cost", money(c), "", `electricity ${money(ce)} · gas ${money(cg)}`));
    }
    if (sm.peak_w != null) out.push(tile("Peak draw", nf(sm.peak_w, 0), "W", sm.peak_t ? "at " + C.fmtHMS(sm.peak_t) : ""));
    if (sm.min_w != null && sm.min_w < -1) out.push(tile("Peak feed-in", nf(-sm.min_w, 0), "W", "at " + C.fmtHMS(sm.min_t)));
    if (sm.avg_w != null) out.push(tile("Average power", nf(sm.avg_w, 0), "W", sm.base_w != null ? `lowest typical draw ${nf(sm.base_w, 0)} W` : ""));
    return out.join("");
  }

  // ---------------------------------------------------------------- chart helpers (Live page "today" charts)
  function elecBars(host, u, h, dom, o = {}) {
    const exp = any(u.export);
    C.bar(host, {
      x: u.t, bucket: u.bucket, height: h, unit: "kWh", title: "Electricity", xDomain: dom, cover: u.cover,
      persist: o.persist, onZoom: o.onZoom,
      series: [
        { key: "imp", name: "Import", color: "var(--s2)", data: u.import },
        ...(exp ? [{ key: "exp", name: "Fed back", color: "var(--s3)", data: u.export, negate: true }] : []),
      ],
      fmt: (v) => nf(v, 4) + " kWh", tipTitle: (t) => bucketTitle(t, u.bucket),
    });
  }
  function gasBars(host, u, h, dom, o = {}) {
    if (!present(u.gas)) {
      empty(host, "No gas data in this period.");
      return;
    }
    C.bar(host, {
      x: u.t, bucket: u.bucket, height: h, unit: "m³", title: "Gas", xDomain: dom, cover: u.cover,
      persist: o.persist, onZoom: o.onZoom,
      series: [{ key: "gas", name: "Gas", color: "var(--s4)", data: u.gas }],
      fmt: (v) => nf(v, 3) + " m³", tipTitle: (t) => bucketTitle(t, u.bucket),
    });
  }
  const empty = (host, msg) => { if (host.__chart && host.__chart.ro) host.__chart.ro.disconnect(); host.__chart = null; host.innerHTML = `<div class="empty">${msg}</div>`; };

  // ================================================================ PANELS
  // A panel = name + chart type + any mix of fields. Definitions live on the server (data/panels.json).
  // base = default name; tn = tariff number (1/2) so tariff names set under Device → Names are used automatically.
  const mk = (src) => (id, base, unit, dec, key, color, tn = 0) => ({ id, base, unit, dec, src, key, color, tn });
  const g = mk("g"), us = mk("use"), rd = mk("rd");
  const FGROUPS = [
    ["Measurements", [
      g("power_w", "Power (total)", "W", 0, "power", "s1"), g("power_l1_w", "Power L1", "W", 0, "l1", "s2"), g("power_l2_w", "Power L2", "W", 0, "l2", "s3"), g("power_l3_w", "Power L3", "W", 0, "l3", "s4"),
      g("voltage_l1_v", "Voltage L1", "V", 1, "v1", "s2"), g("voltage_l2_v", "Voltage L2", "V", 1, "v2", "s3"), g("voltage_l3_v", "Voltage L3", "V", 1, "v3", "s4"),
      g("current_l1_a", "Current L1", "A", 2, "a1", "s2"), g("current_l2_a", "Current L2", "A", 2, "a2", "s3"), g("current_l3_a", "Current L3", "A", 2, "a3", "s4"),
      g("frequency_hz", "Frequency", "Hz", 3, "hz", "s1"),
    ]],
    ["Usage per period (kWh / m³ used in each bucket)", [
      us("use:import", "Electricity imported", "kWh", 3, "import", "s2"), us("use:export", "Electricity fed back", "kWh", 3, "export", "s3"),
      us("use:import_t1", "Electricity imported", "kWh", 3, "import_t1", "s5", 1), us("use:import_t2", "Electricity imported", "kWh", 3, "import_t2", "s7", 2),
      us("use:gas", "Gas used", "m³", 3, "gas", "s4"), us("use:water", "Water used", "m³", 3, "water", "s1"),
    ]],
    ["Meter readings (the counters themselves)", [
      rd("rd:energy_import_kwh", "Import meter", "kWh", 3, "energy_import_kwh", "s2"), rd("rd:energy_export_kwh", "Export meter", "kWh", 3, "energy_export_kwh", "s3"),
      rd("rd:energy_import_t1_kwh", "Import meter", "kWh", 3, "energy_import_t1_kwh", "s5", 1), rd("rd:energy_import_t2_kwh", "Import meter", "kWh", 3, "energy_import_t2_kwh", "s7", 2),
      rd("rd:energy_export_t1_kwh", "Export meter", "kWh", 3, "energy_export_t1_kwh", "s6", 1), rd("rd:energy_export_t2_kwh", "Export meter", "kWh", 3, "energy_export_t2_kwh", "s8", 2),
      rd("rd:gas_m3", "Gas meter", "m³", 3, "gas_m3", "s4"), rd("rd:water_m3", "Water meter", "m³", 3, "water_m3", "s1"), rd("rd:tariff", "Active tariff", "", 0, "tariff", "s8"),
    ]],
  ];
  const FDEFS = {};
  FGROUPS.forEach((gr) => gr[1].forEach((f) => (FDEFS[f.id] = f)));

  // ---- calculated fields: calc:delta:<reading> (change between points), calc:rate:<reading> (per hour), calc:diff:<A>|<B> (A − B)
  const calcCache = {};
  function calcDef(id) {
    const m = id.match(/^calc:(delta|rate|diff):(.+)$/);
    if (!m) return null;
    const kind = m[1];
    if (kind === "diff") {
      const [a, b] = m[2].split("|"), A = FDEFS[a], B = FDEFS[b];
      if (!A || !B) return null;
      return { id, src: "calc", kind, axis: A.src, a: A, b: B, unit: A.unit, dec: Math.max(A.dec, B.dec), color: "s6", tn: 0, base: A.base + " − " + B.base };
    }
    const S = FDEFS["rd:" + m[2]];
    if (!S) return null;
    const rate = kind === "rate";
    return { id, src: "calc", kind, axis: rate ? "rate" : "use", from: S, unit: rate ? (S.unit === "kWh" ? "kW" : S.unit + "/h") : S.unit,
      dec: rate ? 3 : S.dec, color: S.color, tn: S.tn, base: S.base + (rate ? " per hour" : " change") };
  }
  const defOf = (id) => FDEFS[id] || (id.startsWith("calc:") ? (calcCache[id] ||= calcDef(id)) : null);
  const isUsageOnly = (f) => f.src === "use" || (f.kind === "diff" && f.a.src === "use" && f.b.src === "use");
  const needsRd = (id) => id.startsWith("rd:") || id.startsWith("calc:delta:") || id.startsWith("calc:rate:") || (id.startsWith("calc:diff:") && id.includes("rd:"));
  const CALC_PRESETS = ["calc:rate:gas_m3", "calc:delta:gas_m3", "calc:rate:energy_import_kwh", "calc:rate:energy_export_kwh", "calc:rate:water_m3", "calc:delta:water_m3"];
  const CALC_NAMES = {
    "calc:rate:gas_m3": "Gas flow", "calc:delta:gas_m3": "Gas used (change)", "calc:rate:energy_import_kwh": "Import power (from meter)",
    "calc:rate:energy_export_kwh": "Export power (from meter)", "calc:rate:water_m3": "Water flow", "calc:delta:water_m3": "Water used (change)",
  };

  const axisKey = (f) => f.unit + "|" + (f.axis || f.src);
  // names: panel override > global name (Device → Names) > default (+ tariff name)
  const tName = (n, long) => LABELS["tariff:" + n] || (long ? "tariff " + n : "T" + n);
  function fieldLabel(f, p) {
    const own = (p && p.labels && p.labels[f.id]) || LABELS[f.id];
    if (own) return own;
    if (f.src === "calc") {
      if (CALC_NAMES[f.id]) return CALC_NAMES[f.id];
      if (f.kind === "diff") return fieldLabel(f.a, p) + " − " + fieldLabel(f.b, p);
      return fieldLabel(f.from, p) + (f.kind === "rate" ? " per hour" : " change");
    }
    return f.tn ? f.base + ", " + tName(f.tn, true) : f.base;
  }
  const isBandPanel = (p) => p.type === "band" || p.band;
  const isPie = (p) => p.type === "pie" || p.type === "donut";
  const TYPES = [
    ["line", "Line"], ["area", "Area"], ["step", "Step line"], ["band", "Line + variability band"],
    ["bar", "Bars"], ["stacked", "Stacked bars"], ["pie", "Pie"], ["donut", "Donut"], ["stat", "Values only"],
  ];
  const AGGS = { last: "last value", avg: "average", min: "lowest", max: "highest", sum: "total" };
  const HEIGHTS = { small: 200, medium: 260, large: 340 };

  let PANELS = [];
  const forced = { rd: false, ranges: new Set() }; // data the editor needs before a panel is saved
  const gaugeNeedsRange = (id) => { const f = defOf(id); return f && f.src === "g" && id !== "power_w"; };
  const rangeList = () => [...new Set([...forced.ranges, ...PANELS.filter(isBandPanel).flatMap((p) => p.fields.filter(gaugeNeedsRange))])];
  const needReadings = () => forced.rd || PANELS.some((p) => p.fields.some(needsRd));
  const extraQuery = () => (needReadings() ? "&readings=1" : "") + (rangeList().length ? "&ranges=" + rangeList().join(",") : "");
  const panelsReady = (async () => { const r = await api("GET", "/api/panels"); if (r.panels) PANELS = r.panels; })();

  // a field keeps its own colour; clashes inside one panel move to the next free palette slot
  function colorsFor(defs) {
    const used = new Set(), out = [];
    const order = ["s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"];
    for (const f of defs) {
      let c = f.color;
      if (used.has(c)) c = order.find((x) => !used.has(x)) || c;
      used.add(c); out.push(`var(--${c})`);
    }
    return out;
  }
  function normalize(a) {
    let lo = Infinity, hi = -Infinity;
    for (const v of a) if (v != null) { if (v < lo) lo = v; if (v > hi) hi = v; }
    const span = hi - lo;
    return a.map((v) => (v == null ? null : span > 0 ? ((v - lo) / span) * 100 : 50));
  }
  // value of the usage bucket that contains each x (usage fields drawn on a measurement time axis)
  function stepMap(u, vals, xs) {
    const out = new Array(xs.length).fill(null);
    let j = 0;
    for (let i = 0; i < xs.length; i++) {
      while (j + 1 < u.t.length && u.t[j + 1] <= xs[i]) j++;
      if (xs[i] >= u.t[j] && xs[i] < u.t[j] + u.bucket) out[i] = vals[j];
    }
    return out;
  }
  // the numbers of a field, aligned with x (the series time axis, or the usage buckets when needSeries is false)
  function valuesFor(f, d, x, needSeries) {
    const s = d.series, u = d.usage;
    if (f.src === "g") return s[f.key];
    if (f.src === "rd") return s.rd ? s.rd[f.key] : null;
    if (f.src === "use") { const a = u[f.key]; return a && needSeries ? stepMap(u, a, x) : a; }
    if (f.kind === "diff") {
      const a = valuesFor(f.a, d, x, needSeries), b = valuesFor(f.b, d, x, needSeries);
      return a && b ? a.map((v, i) => (v == null || b[i] == null ? null : v - b[i])) : null;
    }
    // delta / rate: change of a meter reading between consecutive points (never negative)
    const r = s.rd ? s.rd[f.from.key] : null;
    if (!r) return null;
    return r.map((v, i) => {
      if (i === 0 || v == null || r[i - 1] == null) return null;
      const dv = Math.max(0, v - r[i - 1]);
      return f.kind === "rate" ? dv / ((s.t[i] - s.t[i - 1]) / 3600) : dv;
    });
  }
  const panelMode = (p) => {
    const defs = p.fields.map(defOf).filter(Boolean);
    const same = new Set(defs.map(axisKey)).size <= 1;
    return { defs, same, normalized: p.scale === "normalized" || !same };
  };
  const fmtF = (f) => (v) => nf(v, f.dec) + (f.unit ? " " + f.unit : "");

  // ---- big values (the "Values only" type, and the optional row above a chart)
  function statTiles(p, d) {
    const defs = p.fields.map(defOf).filter(Boolean);
    const colors = colorsFor(defs);
    if (!d || !d.series || !d.summary.samples) return '<div class="muted fine">No data logged in this period.</div>';
    return defs.map((f, i) => {
      const needSeries = !isUsageOnly(f);
      const x = needSeries ? d.series.t : d.usage.t;
      const arr = valuesFor(f, d, x, needSeries);
      const idx = [];
      (arr || []).forEach((v, k) => { if (v != null) idx.push(k); });
      const label = esc(fieldLabel(f, p));
      if (!idx.length) return tile(label, "–", "", needsRd(f.id) && !d.series.rd ? "loading…" : "no data in this period", colors[i]);
      const vals = idx.map((k) => arr[k]);
      let v, sub = AGGS[p.agg] || AGGS.last;
      if (p.agg === "avg") v = vals.reduce((a, b) => a + b, 0) / vals.length;
      else if (p.agg === "min") v = Math.min(...vals);
      else if (p.agg === "max") v = Math.max(...vals);
      else if (p.agg === "sum") v = vals.reduce((a, b) => a + b, 0);
      else { v = vals[vals.length - 1]; sub += " · " + hm(x[idx[idx.length - 1]], 0); }
      return tile(label, nf(v, f.dec), f.unit, sub, colors[i]);
    }).join("");
  }
  function drawStat(host, p, d) {
    clearHost(host);
    host.innerHTML = `<div class="stats pstats">${statTiles(p, d)}</div>`;
  }
  function clearHost(host) {
    if (host.__chart && host.__chart.ro) host.__chart.ro.disconnect();
    host.__chart = null;
  }

  // pie / donut: every field becomes a slice (average for measurements, total used for usage, used-in-period for meter readings)
  function drawPie(host, p, d, ctx) {
    const defs = p.fields.map(defOf).filter((f) => f && f.id !== "rd:tariff");
    if (!defs.length) return empty(host, "Pick at least one field.");
    if (!d || !d.series || !d.summary.samples) return empty(host, "No data logged in this period.");
    if (new Set(defs.map((f) => f.unit)).size > 1) return empty(host, "Pie and donut charts need fields with the same unit (for example kWh with kWh).");
    const colors = colorsFor(defs);
    const slices = defs.map((f, i) => {
      const needSeries = !isUsageOnly(f);
      const arr = (valuesFor(f, d, needSeries ? d.series.t : d.usage.t, needSeries) || []).filter((v) => v != null);
      const mode = f.src === "g" ? "avg" : f.src === "use" ? "sum" : f.src === "rd" ? "span" : f.kind === "delta" ? "sum" : "avg";
      let v = 0;
      if (arr.length) v = mode === "sum" ? arr.reduce((a, b) => a + b, 0) : mode === "avg" ? arr.reduce((a, b) => a + b, 0) / arr.length : arr[arr.length - 1] - arr[0];
      return { key: f.id, name: fieldLabel(f, p), color: colors[i], value: Math.max(0, v), fmt: fmtF(f) };
    });
    if (!slices.some((x) => x.value > 0)) return empty(host, "No usage for these fields in this period.");
    const f0 = defs[0];
    C.pie(host, { slices, donut: p.type === "donut", height: HEIGHTS[p.height] || 260, title: p.name,
      persist: ctx.persist ? ctx.persist + "-" + p.id : undefined,
      totalText: (t) => nf(t, f0.dec) + (f0.unit ? " " + f0.unit : ""), totalLabel: defs.every((f) => f.src === "g") ? "average" : "total" });
  }

  function drawPanel(host, p, d, ctx) {
    if (ctx.pvals) ctx.pvals.innerHTML = p.values && !isPie(p) && p.type !== "stat" ? statTiles(p, d) : "";
    if (p.type === "stat") return drawStat(host, p, d);
    if (isPie(p)) return drawPie(host, p, d, ctx);
    const { defs, normalized } = panelMode(p);
    if (!defs.length) return empty(host, "Pick at least one field.");
    if (!d || !d.series || !d.summary.samples) return empty(host, "No data logged in this period.");
    const bar = p.type === "bar" || p.type === "stacked";
    const s = d.series, u = d.usage;
    const needSeries = defs.some((f) => !isUsageOnly(f));
    const x = needSeries ? s.t : u.t;
    const bucket = needSeries ? d.bucket || d.dt : u.bucket;
    const colors = colorsFor(defs);
    let waitRd = false;
    const series = [];
    defs.forEach((f, i) => {
      const data = valuesFor(f, d, x, needSeries);
      if (!data) { if (needsRd(f.id) && !s.rd) waitRd = true; return; }
      if (!present(data)) return;
      series.push({ key: f.id, name: fieldLabel(f, p), color: colors[i], width: 1.8, fill: p.type === "area", negate: bar && f.id === "use:export",
        data: normalized ? normalize(data) : data, raw: normalized ? data : null, fmt: fmtF(f), def: f });
    });
    if (!series.length) return empty(host, waitRd ? "Loading meter readings…" : "No data for these fields in this period. Is the field being tracked (Logs → Global tracking)?");
    const common = {
      x, height: HEIGHTS[p.height] || 260, unit: normalized ? "%" : defs[0].unit, title: p.name, series, xDomain: [d.from, d.to],
      persist: ctx.persist ? ctx.persist + "-" + p.id : undefined, onZoom: ctx.onZoom,
      tipTitle: (t) => (bucket > 0 && (needSeries ? d.bucket > 0 : true) ? bucketTitle(t, bucket) : C.fmtDate(t) + " " + C.fmtHMS(t)),
      yMin: normalized ? 0 : undefined, yMax: normalized ? 100 : undefined,
    };
    if (bar) {
      C.bar(host, { ...common, bucket, stacked: p.type === "stacked", grouped: p.type === "bar", zeroBase: p.zero !== "no", cover: needSeries ? null : u.cover });
      return;
    }
    // variability band(s): lowest…highest per bucket, one shaded area per measurement in the panel
    const bands = [];
    if (isBandPanel(p) && !normalized && needSeries && d.bucket > 0) {
      for (const sr of series) {
        if (sr.def.src !== "g") continue;
        let lo = s.mn && s.mn[sr.def.id], hi = s.mx && s.mx[sr.def.id];
        if (!lo && sr.def.id === "power_w") { lo = s.pmin; hi = s.pmax; }
        if (lo && present(lo)) bands.push({ lo, hi, color: sr.color, name: sr.name + " range", fmt: sr.fmt });
      }
    }
    const autoNonZero = !normalized && defs.every((f) => f.unit === "V" || f.unit === "Hz" || f.src === "rd");
    // a line may only break where points are really missing: allow at least 3.5 buckets between points
    C.line(host, { ...common, bucket: needSeries ? d.bucket : u.bucket, step: p.type === "step" || !needSeries,
      zeroLine: p.zero === "yes" ? true : p.zero === "no" ? false : !autoNonZero,
      gap: needSeries ? Math.max(180, d.dt * 4, (d.bucket || 0) * 3.5) : u.bucket * 3.5, gaps: d.gaps,
      marker: d.now > d.from && d.now < d.to ? d.now : null, bands });
  }

  const typeName = (p) => (p.band && p.type === "line" ? "line + variability band" : (TYPES.find((t) => t[0] === p.type) || ["", p.type])[1].toLowerCase());
  function panelSub(p, d) {
    if (p.type === "stat") return "values · " + (AGGS[p.agg] || AGGS.last);
    if (isPie(p)) return typeName(p) + " · " + (p.fields.every((id) => id.startsWith("rd:") || id.startsWith("use:")) ? "totals for the period" : "averages for the period");
    const { defs, normalized } = panelMode(p);
    const usage = defs.length && defs.every(isUsageOnly);
    return typeName(p) + (usage && d ? " · per " + fmtBucket(d.usage.bucket) : "") + (normalized ? " · each scaled 0–100 %" : "");
  }

  function renderPanels(box, d, ctx) {
    box.__ctx = ctx;
    const sig = JSON.stringify(PANELS) + JSON.stringify(LABELS);
    if (box.dataset.sig !== sig) {
      box.dataset.sig = sig;
      box.innerHTML = "";
      PANELS.forEach((p, i) => {
        const card = document.createElement("div");
        card.className = "card panel " + (p.width === "full" ? "p-full" : "p-half");
        card.dataset.id = p.id;
        card.innerHTML = `<div class="card-head"><h2>${esc(p.name)} <small></small></h2><div class="ptools">` +
          `<button class="tool" data-pa="up" title="Move up"${i === 0 ? " disabled" : ""}>↑</button><button class="tool" data-pa="down" title="Move down"${i === PANELS.length - 1 ? " disabled" : ""}>↓</button>` +
          `<button class="tool" data-pa="edit">Edit</button></div></div><div class="stats pvals"></div><div class="phost"></div>`;
        box.appendChild(card);
      });
    }
    if (!box.__bound) {
      box.__bound = true;
      box.addEventListener("click", (e) => {
        const b = e.target.closest("button[data-pa]");
        if (!b) return;
        const i = PANELS.findIndex((p) => p.id === b.closest(".panel").dataset.id);
        if (i < 0) return;
        if (b.dataset.pa === "edit") return openEditor(PANELS[i], box.__ctx);
        const j = b.dataset.pa === "up" ? i - 1 : i + 1;
        const list = PANELS.slice();
        [list[i], list[j]] = [list[j], list[i]];
        savePanels(list, box.__ctx);
      });
    }
    PANELS.forEach((p) => {
      const card = box.querySelector(`[data-id="${p.id}"]`);
      if (!card) return;
      card.querySelector("small").textContent = panelSub(p, d);
      drawPanel(card.querySelector(".phost"), p, d, { ...ctx, pvals: card.querySelector(".pvals") });
    });
  }

  async function savePanels(list, ctx) {
    const r = await api("PUT", "/api/panels", { panels: list });
    if (!r.ok) { toast(r.error || "Could not save panels", true); return false; }
    PANELS = r.panels;
    if (ctx) await ctx.reload();
    return true;
  }

  // ---- little pictures of every chart type for the editor
  function thumb(type) {
    const L = "stroke:var(--s1);stroke-width:2;fill:none;stroke-linejoin:round", A = "fill:var(--s1);opacity:.2";
    const pts = [[6, 40], [20, 28], [34, 35], [48, 16], [62, 26], [76, 10], [90, 22]];
    const poly = pts.map((p) => p.join(",")).join(" ");
    const arc = (r0, r1, a0, a1, col) => {
      const P = (r, a) => `${(48 + r * Math.sin(a)).toFixed(1)},${(28 - r * Math.cos(a)).toFixed(1)}`, big = a1 - a0 > Math.PI ? 1 : 0;
      const d = r0 ? `M${P(r1, a0)}A${r1},${r1} 0 ${big} 1 ${P(r1, a1)}L${P(r0, a1)}A${r0},${r0} 0 ${big} 0 ${P(r0, a0)}Z` : `M48,28L${P(r1, a0)}A${r1},${r1} 0 ${big} 1 ${P(r1, a1)}Z`;
      return `<path d="${d}" style="fill:${col};stroke:var(--surface);stroke-width:1.5"/>`;
    };
    const slices = (r0) => [[0, 0.45, "var(--s1)"], [0.45, 0.75, "var(--s2)"], [0.75, 1, "var(--s3)"]].map((s) => arc(r0, 25, s[0] * 2 * Math.PI, s[1] * 2 * Math.PI, s[2])).join("");
    const box = (x, n, c) => `<rect x="${x}" y="12" width="26" height="32" rx="4" style="fill:var(--surface);stroke:var(--axis)"/><rect x="${x + 4}" y="17" width="6" height="6" rx="1.5" style="fill:${c}"/><text x="${x + 13}" y="38" text-anchor="middle" style="font-size:12px;font-weight:700;fill:var(--text)">${n}</text>`;
    const body = {
      line: `<polyline points="${poly}" style="${L}"/>`,
      area: `<polygon points="6,50 ${poly} 90,50" style="${A}"/><polyline points="${poly}" style="${L}"/>`,
      step: `<path d="M6,40H20V28H34V35H48V16H62V26H76V10H90V22" style="${L}"/>`,
      band: `<polygon points="${pts.map((p) => `${p[0]},${p[1] - 8}`).join(" ")} ${pts.slice().reverse().map((p) => `${p[0]},${p[1] + 8}`).join(" ")}" style="${A};opacity:.28"/><polyline points="${poly}" style="${L}"/>`,
      bar: [[8, 22, 14], [26, 34, 24], [44, 16, 30], [62, 40, 20], [78, 26, 34]].map((b) => `<rect x="${b[0]}" y="${50 - b[1]}" width="7" height="${b[1]}" rx="2" style="fill:var(--s2)"/><rect x="${b[0] + 8}" y="${50 - b[2]}" width="7" height="${b[2]}" rx="2" style="fill:var(--s3)"/>`).join(""),
      stacked: [[8, 20, 12], [26, 28, 14], [44, 14, 26], [62, 32, 10], [78, 22, 20]].map((b) => `<rect x="${b[0]}" y="${50 - b[1]}" width="14" height="${b[1]}" style="fill:var(--s2)"/><rect x="${b[0]}" y="${50 - b[1] - b[2]}" width="14" height="${b[2]}" rx="2" style="fill:var(--s3)"/>`).join(""),
      pie: slices(0),
      donut: slices(14),
      stat: box(5, "42", "var(--s1)") + box(35, "7.1", "var(--s4)") + box(65, "230", "var(--s2)"),
    }[type];
    const axis = ["pie", "donut", "stat"].includes(type) ? "" : `<line x1="4" x2="92" y1="50" y2="50" style="stroke:var(--axis)"/>`;
    return `<svg viewBox="0 0 96 56" aria-hidden="true">${axis}${body}</svg>`;
  }

  // ---- editor with live preview
  let dlg = null;
  async function ensureData(ctx, w) {
    const d = ctx.d();
    if (!d || !d.series) return;
    let need = false;
    if (w.fields.some(needsRd) && !d.series.rd) { forced.rd = true; need = true; }
    if (isBandPanel(w)) for (const id of w.fields) if (gaugeNeedsRange(id) && !(d.series.mn && d.series.mn[id]) && !forced.ranges.has(id)) { forced.ranges.add(id); need = true; }
    if (need) await ctx.reload();
  }

  function openEditor(panel, ctx) {
    const isNew = !panel;
    const w = JSON.parse(JSON.stringify(panel || { id: "", name: "New panel", type: "line", fields: ["power_w"], scale: "auto", zero: "auto", agg: "last", values: false, width: "half", height: "medium", labels: {} }));
    w.labels = w.labels || {}; w.zero = w.zero || "auto"; w.agg = w.agg || "last";
    if (w.band && w.type === "line") w.type = "band";
    if (!dlg) { dlg = document.createElement("dialog"); dlg.className = "pdlg"; document.body.appendChild(dlg); }
    const hasData = (f) => {
      const d0 = ctx.d();
      if (!d0 || !d0.series || f.src === "calc") return null;
      if (f.src === "g") return present(d0.series[f.key]);
      if (f.src === "use") return present(d0.usage[f.key]);
      return d0.series.rd ? present(d0.series.rd[f.key]) : null;
    };
    const allDefs = FGROUPS.flatMap((gr) => gr[1]);
    const counters = FGROUPS[2][1].filter((f) => f.id !== "rd:tariff");
    const opt = (list) => list.map((f) => `<option value="${f.id}">${esc(fieldLabel(f))}</option>`).join("");
    dlg.innerHTML =
      `<div class="pform"><div class="pcol">` +
      `<h2>${isNew ? "New panel" : "Edit panel"}</h2>` +
      `<label>Name<input id="pe-name" maxlength="60"></label>` +
      `<div><div class="fl-head">Chart type</div><div class="tgrid" id="pe-types"></div></div>` +
      `<div id="pe-fields"></div><div id="pe-names"></div>` +
      `<div class="row" id="pe-valrow"><label>Value shown<select id="pe-agg">${Object.entries(AGGS).map(([k, v]) => `<option value="${k}">${v}</option>`).join("")}</select></label>` +
      `<label class="chk" id="pe-valwrap"><input type="checkbox" id="pe-values"> Show the values above the chart</label></div>` +
      `<div class="row" id="pe-axis"><label>Value axis<select id="pe-zero"><option value="auto">Automatic</option><option value="yes">Starts at zero</option><option value="no">Fits the data (not from zero)</option></select></label>` +
      `<label>Different units<select id="pe-scale"><option value="auto">Scale to 0–100 % when needed</option><option value="normalized">Always scale to 0–100 %</option></select></label></div>` +
      `<div class="row"><label>Width<select id="pe-width"><option value="half">Half</option><option value="full">Full</option></select></label>` +
      `<label>Height<select id="pe-height"><option value="small">Small</option><option value="medium">Medium</option><option value="large">Large</option></select></label></div>` +
      `</div><div class="pprev"><div class="card-head"><b>Preview</b><small>uses the data on screen</small></div><div class="stats pvals" id="pe-pvals"></div><div id="pe-prev"></div><div id="pe-msg" class="muted fine"></div></div></div>` +
      `<div class="pfoot">${isNew ? "" : '<button id="pe-del" class="danger">Delete panel</button>'}<span class="grow"></span><button id="pe-cancel">Cancel</button><button id="pe-save" class="primary">Save panel</button></div>`;
    const $e = (id) => dlg.querySelector("#" + id);
    $e("pe-name").value = w.name; $e("pe-scale").value = w.scale; $e("pe-zero").value = w.zero; $e("pe-agg").value = w.agg;
    $e("pe-values").checked = !!w.values; $e("pe-width").value = w.width; $e("pe-height").value = w.height;

    const refresh = async () => { $e("pe-msg").textContent = "Loading…"; await ensureData(ctx, w); drawFields(); drawNames(); preview(); };
    const drawTypes = () => {
      $e("pe-types").innerHTML = TYPES.map((t) => `<button type="button" class="tbtn${w.type === t[0] ? " on" : ""}" data-t="${t[0]}" title="${t[1]}">${thumb(t[0])}<span>${t[1]}</span></button>`).join("");
      $e("pe-types").querySelectorAll("button").forEach((b) => (b.onclick = async () => { w.type = b.dataset.t; w.band = w.type === "band"; drawTypes(); await ensureData(ctx, w); preview(); }));
    };
    const drawNames = () => {
      const defs = w.fields.map(defOf).filter(Boolean);
      $e("pe-names").innerHTML = defs.length ? `<div class="fl-head">Names in this chart <span class="muted fine">(leave empty to use the default; change defaults under Device → Names)</span></div>` +
        defs.map((f) => `<label class="nrow"><span>${esc(f.id.replace(/^(use|rd|calc):/, ""))}</span><input data-n="${f.id}" maxlength="40" placeholder="${esc(fieldLabel(f))}" value="${esc(w.labels[f.id] || "")}"></label>`).join("") : "";
      $e("pe-names").querySelectorAll("input[data-n]").forEach((i) => (i.oninput = () => { if (i.value.trim()) w.labels[i.dataset.n] = i.value; else delete w.labels[i.dataset.n]; preview(); }));
    };
    const drawFields = () => {
      const cols = colorsFor(w.fields.map(defOf).filter(Boolean));
      const colOf = (id) => cols[w.fields.indexOf(id)];
      const box = (f) => {
        const on = w.fields.includes(f.id), nd = hasData(f) === false;
        return `<label class="${nd ? "nodata" : ""}"><input type="checkbox" data-f="${f.id}"${on ? " checked" : ""}${!on && w.fields.length >= 8 ? " disabled" : ""}>` +
          `<i class="fsw" style="background:${on ? colOf(f.id) : "var(--axis)"}"></i>${esc(fieldLabel(f))}${nd ? '<span class="val">no data here</span>' : ""}</label>`;
      };
      const calcIds = [...new Set([...CALC_PRESETS, ...w.fields.filter((x) => x.startsWith("calc:"))])];
      $e("pe-fields").innerHTML = `<div class="fl-head">Fields <span class="muted fine">(up to 8, shown on top of each other)</span></div>` +
        FGROUPS.map((gr) => `<div class="fgroup">${esc(gr[0])}</div><div class="fgrid">${gr[1].map(box).join("")}</div>`).join("") +
        `<div class="fgroup">Calculated (from the meter readings)</div><div class="fgrid">${calcIds.map(defOf).filter(Boolean).map(box).join("")}</div>` +
        `<div class="calcbuild"><div class="fl-head">Make your own calculation</div>` +
        `<div class="row"><select id="cb-kind"><option value="delta">Change between points</option><option value="rate">Rate per hour</option></select><span>of</span><select id="cb-src">${opt(counters)}</select><button type="button" id="cb-add1">Add</button></div>` +
        `<div class="row"><span>Difference:</span><select id="cb-a">${opt(allDefs)}</select><span>−</span><select id="cb-b">${opt(allDefs)}</select><button type="button" id="cb-add2">Add</button></div>` +
        `<div class="muted fine" id="cb-msg">“Change between points” shows how much a counter grew from one point to the next (gas used). “Rate per hour” divides that by the time (gas flow, power in kW). Both depend on the Detail setting above the charts.</div></div>`;
      dlg.querySelectorAll("input[data-f]").forEach((c) => (c.onchange = async () => {
        const id = c.dataset.f;
        if (c.checked) w.fields.push(id); else { w.fields = w.fields.filter((x) => x !== id); delete w.labels[id]; }
        await refresh();
      }));
      const addCalc = async (id) => {
        if (w.fields.includes(id)) return ($e("cb-msg").textContent = "Already in this chart.");
        if (w.fields.length >= 8) return ($e("cb-msg").textContent = "At most 8 fields per chart.");
        w.fields.push(id);
        await refresh();
      };
      $e("cb-add1").onclick = () => addCalc(`calc:${$e("cb-kind").value}:${$e("cb-src").value.slice(3)}`);
      $e("cb-add2").onclick = () => {
        const a = $e("cb-a").value, b = $e("cb-b").value;
        if (a === b) return ($e("cb-msg").textContent = "Pick two different fields.");
        if (FDEFS[a].unit !== FDEFS[b].unit) return ($e("cb-msg").textContent = "A difference needs two fields with the same unit.");
        addCalc(`calc:diff:${a}|${b}`);
      };
    };
    const preview = () => {
      w.name = $e("pe-name").value; w.scale = $e("pe-scale").value; w.zero = $e("pe-zero").value; w.agg = $e("pe-agg").value;
      w.values = $e("pe-values").checked; w.width = $e("pe-width").value; w.height = $e("pe-height").value;
      const pie = isPie(w), stat = w.type === "stat", { normalized, same } = panelMode(w), bars = w.type === "bar" || w.type === "stacked";
      $e("pe-axis").style.display = pie || stat ? "none" : "";
      $e("pe-valwrap").style.display = pie || stat ? "none" : "";
      $e("pe-valrow").style.display = pie ? "none" : "";
      $e("pe-agg").parentElement.style.display = stat || w.values ? "" : "none";
      $e("pe-msg").textContent = !w.fields.length ? "Tick at least one field."
        : stat ? "Each field becomes a tile. “Value shown” picks the last value, average, lowest, highest or total over the period on screen."
        : pie ? (new Set(w.fields.map((id) => defOf(id) && defOf(id).unit)).size > 1 ? "Pie and donut charts need fields with the same unit." : "Each field becomes a slice: averages for measurements, the amount used for usage and meter readings.")
        : normalized && !same ? "These fields have different units, so each one is scaled to 0–100 % to compare shapes. Hover to see the real values."
        : bars && w.fields.some((id) => defOf(id) && !isUsageOnly(defOf(id))) ? "Bars show one bar per time bucket; they work best with “usage per period” fields."
        : w.type === "band" && !w.fields.some((id) => defOf(id) && defOf(id).src === "g") ? "The band shows lowest…highest per bucket for measurements; pick a measurement."
        : "";
      drawPanel($e("pe-prev"), w, ctx.d(), { persist: null, pvals: $e("pe-pvals") });
    };
    dlg.querySelectorAll("#pe-name, #pe-scale, #pe-zero, #pe-agg, #pe-values, #pe-width, #pe-height").forEach((el) => (el.oninput = el.onchange = preview));
    $e("pe-cancel").onclick = () => dlg.close();
    $e("pe-save").onclick = async () => {
      if (!w.fields.length) return ($e("pe-msg").textContent = "Tick at least one field.");
      preview();
      const out = { ...w, band: w.type === "band" };
      const list = isNew ? PANELS.concat([out]) : PANELS.map((p) => (p.id === w.id ? out : p));
      if (await savePanels(list, ctx)) dlg.close();
    };
    if (!isNew) $e("pe-del").onclick = async () => {
      if (!confirm(`Delete the panel “${w.name}”?`)) return;
      if (PANELS.length < 2) return toast("Keep at least one panel (or use Reset to defaults).", true);
      if (await savePanels(PANELS.filter((p) => p.id !== w.id), ctx)) dlg.close();
    };
    drawTypes(); drawFields(); drawNames(); preview();
    dlg.showModal();
  }

  async function resetPanels(ctx) {
    if (!confirm("Replace all panels with the default ones?")) return;
    const r = await api("POST", "/api/panels/reset");
    if (!r.ok) return toast(r.error, true);
    PANELS = r.panels; forced.rd = false; forced.ranges.clear();
    await ctx.reload();
  }

  const resNote = (d) =>
    `Logged every ${d.dt} s · lines: ${d.bucket ? "average per " + fmtBucket(d.bucket) : "every sample"} · bars: per ${fmtBucket(d.usage.bucket)}`;
  const covNote = (d) => {
    const cov = d.summary.coverage;
    return cov != null && cov < 0.97 && d.summary.samples
      ? `Data covers ${nf(cov * 100, 0)}% of this period. Hatched areas are gaps (the Pi, the app or the meter connection was down). Totals still include the gap, spread evenly across it; faded bars mark periods with incomplete data.`
      : "";
  };

  // ================================================================ LIVE
  let es = null;
  let live = { t: [], p: [], l1: [], l2: [], l3: [] };
  let liveWin = 900, liveSt = { active: false };

  async function enterLive() {
    await refreshInfo();
    await Promise.all([seedLive(), pollLiveStatus(), showLast()]);
    loadToday();
    every(2000, pollLiveStatus);
    every(1000, tickCountdown);
    every(15000, refreshInfo);
    every(30000, loadToday);
  }
  async function seedLive() {
    const seed = await api("GET", "/api/live?seconds=7200");
    if (seed.t) live = { t: seed.t, p: seed.power, l1: seed.l1, l2: seed.l2, l3: seed.l3 };
    renderLiveChart();
  }
  async function showLast() {
    const r = await api("GET", "/api/last");
    if (r.m) onMeasurement({ at: r.at, m: r.m }, { noBuffer: true, stale: true });
    else $("hero-sub").textContent = "No reading yet. Start live tracking, or wait for the next global tracking sample.";
  }
  function startStream() {
    if (es || document.hidden) return;
    es = new EventSource("/api/stream");
    es.onmessage = (ev) => onMeasurement(JSON.parse(ev.data));
    es.onerror = () => { streamOK = false; setPill("err", "Reconnecting…"); };
  }
  function stopStream() {
    if (es) es.close();
    es = null; streamOK = false;
  }

  async function pollLiveStatus() {
    const st = await api("GET", "/api/live/status");
    if (st.error) return;
    const was = liveSt;
    liveSt = st;
    $("live-idle").hidden = st.active;
    $("live-on").hidden = !st.active;
    if (st.active) {
      $("lv-text").textContent = `Live tracking · every ${st.intervalS} s`;
      startStream();
    } else if (was.active) {
      stopStream();
      toast(`Live tracking finished. Saved as log “${was.name}”.`);
      $("lv-msg").innerHTML = `Finished. The session is saved as the log “${esc(was.name)}” on the <a href="#logs">Logs</a> page.`;
      refreshInfo(); seedLive(); showLast();
    }
    tickCountdown();
  }
  function tickCountdown() {
    if (!liveSt.active) return;
    $("lv-left").textContent = liveSt.until ? fmtClock(liveSt.until - Date.now() / 1000) + " left" : "";
  }
  $("lv-min").onchange = () => ($("lv-custom-wrap").hidden = $("lv-min").value !== "custom");
  $("lv-start").onclick = async () => {
    const minutes = $("lv-min").value === "custom" ? +$("lv-custom").value : +$("lv-min").value;
    $("lv-msg").textContent = "";
    const r = await api("POST", "/api/live/start", { intervalS: +$("lv-int").value, minutes });
    if (r.error) { $("lv-msg").innerHTML = `<span class="err">${esc(r.error)}</span>`; return; }
    await pollLiveStatus();
  };
  $("lv-stop").onclick = async () => { await api("POST", "/api/live/stop"); await pollLiveStatus(); };
  $("lv-once").onclick = async () => {
    $("lv-once").disabled = true;
    const r = await api("GET", "/api/measurement");
    $("lv-once").disabled = false;
    if (!r.ok) return toast(r.error || "Failed", true);
    onMeasurement({ at: Date.now(), m: r.data }, { once: true });
  };
  $("live-window").onclick = (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    liveWin = +b.dataset.s;
    [...$("live-window").children].forEach((x) => x.classList.toggle("on", x === b));
    renderLiveChart();
  };

  const num = (m, k) => (typeof m[k] === "number" ? m[k] : null);

  function onMeasurement(j, opt = {}) {
    if (j.error) { streamOK = false; setPill("err", "Meter unreachable", j.error); return; }
    const m = j.m;
    const t = Math.floor(j.at / 1000);
    if (!opt.stale) { streamOK = !opt.once; if (!opt.once) setPill("ok", "Live · " + C.fmtHMS(t), "Receiving data from the meter"); }
    const l1 = num(m, "power_l1_w"), l2 = num(m, "power_l2_w"), l3 = num(m, "power_l3_w");
    let p = num(m, "power_w");
    if (p == null && (l1 != null || l2 != null || l3 != null)) p = (l1 || 0) + (l2 || 0) + (l3 || 0);
    if (!opt.noBuffer && (!live.t.length || t > live.t[live.t.length - 1])) {
      live.t.push(t); live.p.push(p); live.l1.push(l1); live.l2.push(l2); live.l3.push(l3);
      const cut = t - 7200;
      if (live.t[0] < cut) {
        let i = 0;
        while (i < live.t.length && live.t[i] < cut) i++;
        for (const k of Object.keys(live)) live[k] = live[k].slice(i);
      }
    }
    renderHero(m, p, [l1, l2, l3], opt.stale ? t : null);
    renderMeterStats(m);
    renderLiveChart();
  }

  function renderHero(m, p, ls, staleT) {
    const hero = document.querySelector(".hero");
    if (p == null) { $("hero-num").textContent = "–"; return; }
    const [n, u] = fmtPower(p);
    $("hero-num").textContent = n;
    $("hero-unit").textContent = u;
    hero.classList.toggle("imp", p > 5);
    hero.classList.toggle("exp", p < -5);
    const tar = num(m, "tariff");
    $("hero-sub").textContent =
      (p > 5 ? "Drawing from the grid" : p < -5 ? "Feeding back to the grid" : "Balanced") + (tar ? " · " + tName(tar, true) : "") +
      (staleT ? ` · last reading ${fmtDur(Date.now() / 1000 - staleT)} ago` : "");
    const volts = [num(m, "voltage_l1_v") ?? num(m, "voltage_v"), num(m, "voltage_l2_v"), num(m, "voltage_l3_v")];
    const amps = [num(m, "current_l1_a") ?? num(m, "current_a"), num(m, "current_l2_a"), num(m, "current_l3_a")];
    const peak = Math.max(1500, ...live.l1.concat(live.l2, live.l3).slice(-1500).map((v) => Math.abs(v || 0)), ...ls.map((v) => Math.abs(v || 0))) * 1.15;
    $("phases").innerHTML = ls.map((w, i) => {
      if (w == null) return "";
      const pct = Math.min(50, (Math.abs(w) / peak) * 50);
      const bar = w >= 0 ? `left:50%;width:${pct}%;background:var(--s2)` : `left:${50 - pct}%;width:${pct}%;background:var(--s3)`;
      return `<div class="phase"><b>L${i + 1}</b><div class="pbar"><i style="${bar}"></i></div>` +
        `<span class="meta">${nf(w, 0)} W${amps[i] != null ? " · " + nf(Math.abs(amps[i]), 1) + " A" : ""}${volts[i] != null ? " · " + nf(volts[i], 0) + " V" : ""}</span></div>`;
    }).join("");
  }

  function renderLiveChart() {
    const now = live.t.length ? Math.max(Math.floor(Date.now() / 1000), live.t[live.t.length - 1]) : Math.floor(Date.now() / 1000);
    const from = now - liveWin;
    let i = 0;
    const n = live.t.length;
    while (i < n && live.t[i] < from) i++;
    if (i > 0) i--;
    const sl = (a) => a.slice(i);
    const ser = [
      { key: "p", name: "Total", color: "var(--s1)", data: sl(live.p), width: 2.2 },
      { key: "l1", name: "L1", color: "var(--s2)", data: sl(live.l1), hidden: true, width: 1.5 },
      { key: "l2", name: "L2", color: "var(--s3)", data: sl(live.l2), hidden: true, width: 1.5 },
      { key: "l3", name: "L3", color: "var(--s4)", data: sl(live.l3), hidden: true, width: 1.5 },
    ].filter((s) => present(s.data));
    if (!ser.length) return empty($("chart-live"), "No power data yet.");
    C.line($("chart-live"), {
      x: sl(live.t), xDomain: [from, now], height: 240, unit: "W", zoomable: false, table: false, title: "Live power",
      gap: Math.max(60, (info.tracking ? info.tracking.intervalS : 5) * 4), persist: "live", series: ser, fmt: (v) => nf(v, 0) + " W",
    });
  }

  function renderMeterStats(m) {
    const gas = (m.external || []).find((e) => e.type === "gas_meter");
    const water = (m.external || []).find((e) => e.type === "water_meter");
    const o = [];
    if (num(m, "energy_import_kwh") != null) o.push(tile("Import meter reading", nf(m.energy_import_kwh, 3), "kWh", num(m, "energy_import_t1_kwh") != null ? `${tName(1)} ${nf(m.energy_import_t1_kwh, 1)} · ${tName(2)} ${nf(m.energy_import_t2_kwh, 1)}` : ""));
    if (num(m, "energy_export_kwh") != null && m.energy_export_kwh > 0) o.push(tile("Export meter reading", nf(m.energy_export_kwh, 3), "kWh", num(m, "energy_export_t1_kwh") != null ? `${tName(1)} ${nf(m.energy_export_t1_kwh, 1)} · ${tName(2)} ${nf(m.energy_export_t2_kwh, 1)}` : ""));
    if (gas) o.push(tile("Gas meter reading", nf(gas.value, 3), gas.unit === "m3" ? "m³" : gas.unit, "updated " + String(gas.timestamp || "").slice(11, 16)));
    if (water) o.push(tile("Water meter reading", nf(water.value, 3), water.unit === "m3" ? "m³" : water.unit));
    if (num(m, "frequency_hz") != null) o.push(tile("Grid frequency", nf(m.frequency_hz, 2), "Hz"));
    if (num(m, "average_power_15m_w") != null) o.push(tile("15-min average", nf(m.average_power_15m_w, 0), "W", "capacity tariff"));
    if (num(m, "monthly_power_peak_w") != null) o.push(tile("Monthly peak", nf(m.monthly_power_peak_w, 0), "W", String(m.monthly_power_peak_timestamp || "").replace("T", " ").slice(0, 16)));
    if (num(m, "any_power_fail_count") != null) o.push(tile("Power failures", nf(m.any_power_fail_count, 0), "", `${nf(m.long_power_fail_count, 0)} long`));
    const sags = [1, 2, 3].reduce((a, i) => a + (num(m, `voltage_sag_l${i}_count`) || 0), 0);
    const swells = [1, 2, 3].reduce((a, i) => a + (num(m, `voltage_swell_l${i}_count`) || 0), 0);
    if (Object.keys(m).some((k) => k.startsWith("voltage_sag"))) o.push(tile("Voltage sags / swells", `${sags} / ${swells}`, "", "since meter start"));
    $("meter-stats").innerHTML = o.join("");
  }

  async function loadToday() {
    const d = await api("GET", "/api/history?range=today&lite=1&compare=1");
    if (!d.usage) return;
    $("today-stats").innerHTML = summaryTiles(d.summary, d.prev, "yesterday so far");
    const u = d.usage, dom = [d.from, d.to];
    elecBars($("chart-today-el"), u, 210, dom);
    gasBars($("chart-today-gas"), u, 210, dom);
  }

  // ================================================================ HISTORY
  const saved = store.get("hrange", "yesterday");
  const H = { range: ["date", "custom", "window"].includes(saved) ? "yesterday" : saved, date: null, from: null, to: null, t0: null, t1: null, res: "auto", data: null, stack: [], loadedAt: 0 };
  const hKey = () => ({ range: H.range, date: H.date, from: H.from, to: H.to, t0: H.t0, t1: H.t1 });

  const histCtx = { persist: "h", onZoom: (a, b) => histZoom(a, b), d: () => H.data, reload: () => loadHistory(true) };

  function enterHistory() {
    Promise.all([refreshInfo(), panelsReady]).then(() => loadHistory());
    every(5000, () => {
      const d = H.data;
      if (!d || document.hidden) return;
      const live = d.to >= d.now - 5; // the period reaches "now"
      const every = ["10m", "1h"].includes(H.range) ? 10000 : 60000;
      if (live && Date.now() - H.loadedAt > every) loadHistory(true);
    });
  }
  function histQuery() {
    let q;
    if (H.range === "date") q = `range=date&date=${H.date}`;
    else if (H.range === "custom") q = `range=custom&from=${H.from}&to=${H.to}`;
    else if (H.range === "window") q = `range=window&t0=${H.t0}&t1=${H.t1}`;
    else q = "range=" + H.range;
    return q + (H.res !== "auto" ? "&res=" + H.res : "") + extraQuery();
  }
  async function loadHistory(quiet) {
    if (!quiet) $("hist-label").textContent = "Loading…";
    const d = await api("GET", "/api/history?compare=1&" + histQuery());
    if (!d.usage) {
      $("hist-label").innerHTML = `<span class="err">${esc(d.error || "Failed")}</span>`;
      return;
    }
    H.data = d; H.loadedAt = Date.now();
    renderHistory();
  }
  const dayShift = (s, n) => { const [y, m, dd] = s.split("-").map(Number); return ymd(new Date(y, m - 1, dd + n)); };

  function histZoom(a, b) {
    a = Math.floor(a); b = Math.ceil(b);
    if (b - a > 86400 * 400) return toast("That is more than the 400 days that can be shown at once.", true);
    if (b - a < 6) b = a + 6;
    H.stack.push(hKey());
    H.range = "window"; H.t0 = a; H.t1 = b;
    loadHistory();
  }

  function renderHistory() {
    const d = H.data;
    document.querySelectorAll("#range-seg button").forEach((b) => b.classList.toggle("on", b.dataset.r === H.range));
    document.querySelectorAll("#res-seg button").forEach((b) => b.classList.toggle("on", b.dataset.res === H.res));
    const sameDay = ymd(new Date(d.from * 1000)) === ymd(new Date(d.to * 1000));
    $("hist-label").textContent = H.range === "window" ? `${C.fmtDate(d.from)} ${C.fmtHMS(d.from)} – ${sameDay ? "" : C.fmtDate(d.to) + " "}${C.fmtHMS(d.to)}` : d.label;
    $("zoom-bar").hidden = !H.stack.length;
    const dayMode = !!d.date;
    $("day-pick").value = dayMode ? d.date : "";
    $("day-prev").disabled = !dayMode;
    $("day-next").disabled = !dayMode || d.date >= ymd(new Date(d.now * 1000));
    $("day-pick").max = ymd(new Date(d.now * 1000));
    const prevLabel = dayMode ? (d.range === "today" ? "yesterday so far" : "day before") : H.range === "24h" ? "previous 24 h" : "previous period";
    $("hist-stats").innerHTML = summaryTiles(d.summary, d.prev, prevLabel);
    $("res-note").textContent = resNote(d);
    renderPanels($("panels"), d, histCtx);
    $("hist-foot").textContent = covNote(d);
  }

  $("range-seg").onclick = (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    H.range = b.dataset.r; H.stack = [];
    store.set("hrange", H.range);
    loadHistory();
  };
  $("res-seg").onclick = (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    H.res = b.dataset.res;
    loadHistory();
  };
  $("zoom-back").onclick = () => { Object.assign(H, H.stack.pop()); loadHistory(); };
  $("zoom-reset").onclick = () => { Object.assign(H, H.stack[0]); H.stack = []; loadHistory(); };
  const gotoDay = (s) => { H.range = "date"; H.date = s; H.stack = []; loadHistory(); };
  $("day-prev").onclick = () => H.data && H.data.date && gotoDay(dayShift(H.data.date, -1));
  $("day-next").onclick = () => H.data && H.data.date && gotoDay(dayShift(H.data.date, 1));
  $("day-pick").onchange = (e) => e.target.value && gotoDay(e.target.value);
  $("rg-go").onclick = () => {
    const a = $("rg-from").value, b = $("rg-to").value;
    if (!a || !b) return toast("Pick both dates", true);
    H.range = "custom"; H.from = a; H.to = b; H.stack = [];
    loadHistory();
  };
  $("panel-add").onclick = () => openEditor(null, histCtx);
  $("panel-reset").onclick = () => resetPanels(histCtx);

  // ================================================================ LOGS
  let logs = [], files = [], openId = null;

  function enterLogs() {
    refreshInfo();
    if (!$("track-card")) {
      const c = document.createElement("div");
      c.className = "card"; c.id = "track-card";
      $("master-card").insertAdjacentElement("afterend", c);
    }
    loadTracking();
    loadLogs();
    every(2000, loadLogs);
  }

  // ---- global tracking settings
  // Off by default. "Start" first fetches a test reading and switches off every field the meter has no data for.
  const TR = { enabled: false, setup: false, intervalS: 5, fields: new Set(), all: [], test: null };
  const INTERVALS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600];
  const ivLabel = (s) => (s < 60 ? s + " s" : s < 3600 ? s / 60 + " min" : "1 hour");
  const noData = (n) => TR.test && TR.test.some((x) => x.name === n && x.value == null);

  async function loadTracking() {
    const r = await api("GET", "/api/tracking");
    if (!r.ok) return;
    Object.assign(TR, { enabled: r.enabled, intervalS: r.intervalS, fields: new Set(r.fields), all: r.all });
    if (r.enabled) TR.setup = false;
    renderTracking();
  }
  async function runTest() {
    const msg = $("tr-msg");
    if (msg) msg.textContent = "Asking the meter…";
    document.querySelectorAll("#track-card button").forEach((b) => (b.disabled = true));
    const r = await api("POST", "/api/tracking/test");
    if (!r.ok) {
      renderTracking();
      $("tr-msg").innerHTML = `<span class="err">${esc(r.error)}</span>`;
      return false;
    }
    TR.test = r.fields;
    let off = 0;
    for (const f of r.fields) if (f.value == null && TR.fields.delete(f.name)) off++;
    renderTracking();
    const have = r.fields.filter((f) => f.value != null).length;
    $("tr-msg").innerHTML = `<span class="ok">${have} of ${r.fields.length} fields have data${off ? `; switched off the ${off} without data` : ""}.</span>`;
    return true;
  }
  async function saveTracking(enabled) {
    const fields = TR.all.filter((n) => TR.fields.has(n) && !noData(n));
    const r = await api("POST", "/api/tracking", { enabled, intervalS: TR.intervalS, fields });
    if (!r.ok) return toast(r.error, true);
    Object.assign(TR, { enabled: r.enabled, intervalS: r.intervalS, fields: new Set(r.fields), setup: false });
    toast(enabled ? "Global tracking is running" : "Global tracking stopped");
    renderTracking(); loadLogs();
  }

  function renderTracking() {
    const card = $("track-card");
    if (!TR.enabled && !TR.setup) {
      card.innerHTML = `<div class="card-head"><h2>Global tracking settings</h2></div>` +
        `<p class="muted" style="margin-top:0">Global tracking is <b>off</b>: nothing is recorded in the background. Live and History only show data once it has run.</p>` +
        `<p class="muted fine">Starting it first asks the meter for a test reading and switches off every field your meter has no data for. You can then adjust the fields and the interval before recording begins.</p>` +
        `<div class="row"><button id="tr-begin" class="primary">Start global tracking…</button><span id="tr-msg" class="status" style="margin:0"></span></div>`;
      $("tr-begin").onclick = async () => { TR.setup = true; TR.test = null; renderTracking(); await runTest(); };
      return;
    }
    const ivs = INTERVALS.includes(TR.intervalS) ? INTERVALS : [...INTERVALS, TR.intervalS].sort((a, b) => a - b);
    const rows = TR.all.map((n) => {
      const unit = (FIELDS[n] || ["", ""])[1];
      const fd = FDEFS[FGROUPS[0][1].some((x) => x.id === n) ? n : "rd:" + n];
      const label = fd ? fieldLabel(fd) : (FIELDS[n] || [n])[0];
      const t = TR.test ? TR.test.find((x) => x.name === n) : null;
      const nd = t && t.value == null;
      const val = t ? (nd ? "no data" : nf(t.value, 3).replace(/\.?0+$/, "") + (unit ? " " + unit : "")) : "";
      return `<label class="${nd ? "nodata" : ""}"><input type="checkbox" data-f="${n}"${TR.fields.has(n) && !nd ? " checked" : ""}${nd ? " disabled" : ""}> ${esc(label)}<span class="val">${esc(val)}</span></label>`;
    }).join("");
    const nsel = TR.all.filter((n) => TR.fields.has(n) && !noData(n)).length;
    const perDay = (86400 / TR.intervalS) * (26 + 7 * Math.max(1, nsel)) / 1048576;
    card.innerHTML =
      `<div class="card-head"><h2>Global tracking settings</h2>${TR.enabled ? '<span class="badge run">● running</span>' : '<span class="badge warn">not started yet</span>'}</div>` +
      `<div class="form-row"><label>Interval<select id="tr-int">${ivs.map((s) => `<option value="${s}"${s === TR.intervalS ? " selected" : ""}>${ivLabel(s)}</option>`).join("")}</select></label>` +
      `<label>…or custom (seconds)<input id="tr-custom" type="number" min="1" max="3600" placeholder="e.g. 20" style="width:7rem"></label></div>` +
      `<div class="cfg"><div class="row"><b>Fields to record</b><button id="tr-test">${TR.test ? "Test again" : "Test: fetch data from the meter"}</button><span id="tr-msg" class="status" style="margin:0"></span></div>` +
      `<div class="muted fine">Only ticked fields are written to the CSV files, as columns. Fields without data are switched off automatically.</div>` +
      `<div class="track-fields">${rows}</div>` +
      `<div class="row">` +
      (TR.enabled
        ? `<button id="tr-save" class="primary">Save changes</button><button id="tr-stop" class="danger">Stop tracking</button>`
        : `<button id="tr-start" class="primary">Start tracking</button><button id="tr-cancel">Cancel</button>`) +
      `<span class="muted fine">${nsel} fields every ${ivLabel(TR.intervalS)} ≈ ${nf(perDay, 1)} MB per day. ${TR.enabled ? "Saving trims today's file to exactly these columns (data of removed columns from today is dropped); older days are not touched." : ""}</span></div></div>`;
    $("tr-int").onchange = () => { TR.intervalS = +$("tr-int").value; renderTracking(); };
    $("tr-custom").onchange = () => { const v = Math.round(+$("tr-custom").value); if (v >= 1 && v <= 3600) { TR.intervalS = v; renderTracking(); } };
    card.querySelectorAll("input[data-f]").forEach((c) => (c.onchange = () => {
      c.checked ? TR.fields.add(c.dataset.f) : TR.fields.delete(c.dataset.f);
      renderTracking();
    }));
    $("tr-test").onclick = runTest;
    if (TR.enabled) {
      $("tr-save").onclick = () => saveTracking(true);
      $("tr-stop").onclick = () => confirm("Stop global tracking? Nothing is recorded in the background until you start it again.") && saveTracking(false);
    } else {
      $("tr-start").onclick = () => saveTracking(true);
      $("tr-cancel").onclick = () => { TR.setup = false; renderTracking(); };
    }
  }

  async function loadLogs() {
    const j = await api("GET", "/api/logs");
    if (!j.logs) return;
    logs = j.logs; files = j.masterFiles || [];
    renderMaster();
    renderLogList();
    if (openId) {
      const l = logs.find((x) => x.id === openId);
      if (!l) closeLog();
      else if (l.running && L.t0 == null && (!detailAt || Date.now() - detailAt > 3000)) loadDetail();
    }
  }
  const ago = (t) => (t ? fmtDur(Date.now() / 1000 - t) + " ago" : "never");

  function renderMaster() {
    const m = logs.find((l) => l.master);
    if (!m) return;
    const open = $("master-card").querySelector("details")?.open;
    $("master-card").innerHTML =
      `<div class="log"><div><h3>Global tracking ${m.running ? '<span class="badge run">● on</span>' : '<span class="badge warn">off</span>'}</h3>` +
      `<div class="meta">${m.running ? `Every ${ivLabel(m.intervalS)} · ${nf(m.rows)} samples today · today's file ${fmtBytes(m.bytes)} · last sample ${ago(m.lastT)}` : "Not recording. Start it below."}</div>` +
      (m.lastError ? `<div class="err fine">${esc(m.lastError)}</div>` : "") +
      `</div><div class="muted fine" style="max-width:26rem">One CSV per day in data/logs/master/. Live and History read from it.</div></div>` +
      `<details class="files"${open ? " open" : ""}><summary>Daily CSV files (${files.length})</summary><div class="daylist">` +
      files.map((f) => `<a href="/api/master/download?date=${f.date}" download>${f.date} · ${fmtBytes(f.size)}</a>`).join("") + `</div></details>`;
  }

  function renderLogList() {
    const us = logs.filter((l) => !l.master);
    if (!us.length) {
      $("log-list").innerHTML = '<div class="card muted">No custom logs yet. Start one above to record a specific appliance or period at its own interval. Live tracking sessions appear here too.</div>';
      return;
    }
    $("log-list").innerHTML = us.map((l) => {
      const dur = l.running ? Date.now() / 1000 - Date.parse(l.runStartedAt) / 1000 : (l.stoppedAt ? (Date.parse(l.stoppedAt) - Date.parse(l.createdAt)) / 1000 : 0);
      const lv = l.last;
      return `<div class="card log" data-id="${l.id}"><div><h3>${esc(l.name)} ` +
        (l.running ? '<span class="badge run">● running</span>' : '<span class="badge">stopped</span>') +
        (l.live ? '<span class="badge">live session</span>' : "") +
        (l.stopAfterMin && l.running ? `<span class="badge">stops after ${l.stopAfterMin} min</span>` : "") + `</h3>` +
        `<div class="meta">every ${l.intervalS} s · ${nf(l.rows)} rows · ${fmtBytes(l.bytes)} · started ${new Date(l.createdAt).toLocaleString()}${l.running ? " · running " + fmtDur(dur) : ""}` +
        (lv && l.running ? ` · now ${nf(lv.power, 0)} W` : "") + `</div>` +
        (l.note ? `<div class="meta">${esc(l.note)}</div>` : "") +
        (l.lastError ? `<div class="err fine">${esc(l.lastError)}</div>` : "") + `</div>` +
        `<div class="btns"><button data-act="open">${openId === l.id ? "Refresh" : "View"}</button>` +
        (l.running ? '<button data-act="stop">Stop</button>' : '<button data-act="resume">Resume</button>') +
        `<a href="/api/logs/${l.id}/download"><button>Download CSV</button></a><button data-act="delete" class="danger">Delete</button></div></div>`;
    }).join("");
  }
  $("log-list").onclick = async (e) => {
    const b = e.target.closest("button[data-act]");
    if (!b) return;
    const id = b.closest("[data-id]").dataset.id, act = b.dataset.act;
    if (act === "open") { openLog(id); return; }
    if (act === "delete") {
      if (!confirm("Delete this log and its CSV file? This cannot be undone.")) return;
      const r = await api("DELETE", "/api/logs/" + id);
      if (!r.ok) return toast(r.error, true);
      if (openId === id) closeLog();
    } else {
      const r = await api("POST", `/api/logs/${id}/${act}`);
      if (!r.ok) return toast(r.error, true);
    }
    loadLogs();
  };

  $("lf-int").onchange = () => ($("lf-custom-wrap").hidden = $("lf-int").value !== "custom");
  $("lf-stop").onchange = () => ($("lf-stop-wrap").hidden = $("lf-stop").value !== "custom");
  $("log-form").onsubmit = async (e) => {
    e.preventDefault();
    const iv = $("lf-int").value === "custom" ? +$("lf-custom").value : +$("lf-int").value;
    const stop = $("lf-stop").value === "custom" ? +$("lf-stop-custom").value : +$("lf-stop").value;
    const r = await api("POST", "/api/logs", { name: $("lf-name").value, note: $("lf-note").value, intervalS: iv, stopAfterMin: stop });
    $("lf-msg").innerHTML = r.ok ? '<span class="ok">Started.</span>' : `<span class="err">${esc(r.error)}</span>`;
    if (r.ok) { $("lf-name").value = ""; $("lf-note").value = ""; await loadLogs(); openLog(r.log.id); }
  };

  // ---- log detail
  const L = { res: "auto", t0: null, t1: null, stack: [], data: null };
  const logCtx = { persist: "l", onZoom: (a, b) => logZoom(a, b), d: () => L.data, reload: () => { detailAt = 0; return loadDetail(); } };
  let detailAt = 0;
  async function openLog(id) {
    await panelsReady;
    openId = id;
    Object.assign(L, { res: "auto", t0: null, t1: null, stack: [], data: null });
    $("log-detail").innerHTML = "";
    loadDetail();
  }
  function closeLog() { openId = null; $("log-detail").innerHTML = ""; }
  const lKey = () => ({ t0: L.t0, t1: L.t1 });
  function logZoom(a, b) {
    a = Math.floor(a); b = Math.ceil(b);
    if (b - a > 86400 * 400) return;
    if (b - a < 3) b = a + 3;
    L.stack.push(lKey()); L.t0 = a; L.t1 = b;
    detailAt = 0; loadDetail();
  }
  function buildDetail(id) {
    const el = $("log-detail");
    el.dataset.id = id;
    el.innerHTML =
      `<div class="card"><div class="card-head"><h2 id="ld-title"></h2><button id="ld-close" class="tool">Close</button></div>` +
      `<div class="stats" id="ld-stats"></div>` +
      `<div class="row"><span class="muted fine">Detail</span><div class="seg scroll" id="ld-res">` +
      `<button data-res="auto">Auto</button><button data-res="raw">Raw</button><button data-res="60">1 min</button><button data-res="300">5 min</button><button data-res="900">15 min</button><button data-res="3600">1 h</button></div>` +
      `<span id="ld-note" class="muted fine"></span>` +
      `<span id="ld-zoom" hidden style="margin-left:auto"><span class="pill"><i></i><span>Zoomed in</span></span> <button id="ld-back" class="tool">Back</button> <button id="ld-whole" class="tool">Show whole log</button></span></div></div>` +
      `<div class="panel-bar"><b>Panels</b><span class="muted fine">same panels as History · drag on a chart to zoom in</span><span class="grow"></span><button id="ld-padd" class="primary">+ Add panel</button><button id="ld-preset" class="tool">Reset to defaults</button></div>` +
      `<div id="ld-panels" class="panels"></div>` +
      `<p class="muted fine" id="ld-foot"></p>`;
    $("ld-close").onclick = closeLog;
    $("ld-res").onclick = (e) => { const b = e.target.closest("button"); if (b) { L.res = b.dataset.res; detailAt = 0; loadDetail(); } };
    $("ld-back").onclick = () => { Object.assign(L, L.stack.pop()); detailAt = 0; loadDetail(); };
    $("ld-whole").onclick = () => { Object.assign(L, L.stack[0] || {}); L.stack = []; L.t0 = L.t1 = null; detailAt = 0; loadDetail(); };
    $("ld-padd").onclick = () => openEditor(null, logCtx);
    $("ld-preset").onclick = () => resetPanels(logCtx);
    el.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }
  async function loadDetail() {
    if (!openId) return;
    detailAt = Date.now();
    const id = openId;
    let q = "res=" + L.res + extraQuery();
    if (L.t0 != null) q += `&t0=${L.t0}&t1=${L.t1}`;
    const r = await api("GET", `/api/logs/${id}/data?${q}`);
    if (id !== openId || !r.meta) return;
    if ($("log-detail").dataset.id !== id || !$("ld-title")) buildDetail(id);
    $("ld-title").textContent = r.meta.name;
    document.querySelectorAll("#ld-res button").forEach((b) => b.classList.toggle("on", b.dataset.res === L.res));
    $("ld-zoom").hidden = !L.stack.length;
    if (!r.view) { $("ld-stats").innerHTML = ""; $("ld-panels").dataset.sig = ""; empty($("ld-panels"), "No rows yet."); return; }
    const d = Object.assign({}, r.view, { from: r.from, to: r.to, now: Math.floor(Date.now() / 1000) });
    L.data = d;
    const sm = d.summary;
    $("ld-stats").innerHTML = [
      tile("Duration", fmtDur(r.last - r.first), "", `${nf(r.rows)} rows · ${C.fmtHMS(r.first)} → ${C.fmtHMS(r.last)}`),
      sm.import_kwh != null ? tile("Imported", nf(sm.import_kwh, 3), "kWh", money(cost(sm.import_kwh, 0, 0)), "var(--s2)") : "",
      sm.export_kwh > 0.0005 ? tile("Fed back", nf(sm.export_kwh, 3), "kWh", "", "var(--s3)") : "",
      sm.gas_m3 != null ? tile("Gas", nf(sm.gas_m3, 3), "m³", money(sm.gas_m3 * info.prices.gas), "var(--s4)") : "",
      tile("Average", nf(sm.avg_w, 0), "W"), tile("Peak", nf(sm.peak_w, 0), "W", "at " + C.fmtHMS(sm.peak_t)),
      sm.min_w != null && sm.min_w < 0 ? tile("Lowest", nf(sm.min_w, 0), "W", "at " + C.fmtHMS(sm.min_t)) : "",
    ].join("");
    $("ld-note").textContent = resNote(d);
    renderPanels($("ld-panels"), d, logCtx);
    $("ld-foot").textContent = covNote(d).replace("the Pi, the app or the meter connection was down", "the log was stopped, or the Pi, app or meter connection was down");
  }

  // ================================================================ DEVICE
  async function enterDevice() {
    await refreshInfo();
    renderConn(); renderLabelForm();
    $("pr-cur").value = info.prices.currency; $("pr-imp").value = info.prices.import;
    $("pr-exp").value = info.prices.export; $("pr-gas").value = info.prices.gas;
    $("app-foot").textContent = `Version ${info.version} · server time zone ${info.tz} · data folder holds config.json and logs/.`;
    loadDevice();
  }
  function renderLabelForm() {
    $("lb-t1").value = LABELS["tariff:1"] || ""; $("lb-t2").value = LABELS["tariff:2"] || "";
    $("lb-list").innerHTML = FGROUPS.flatMap((gr) => gr[1]).map((f) =>
      `<label class="nrow"><span>${esc(f.id)}</span><input data-l="${f.id}" maxlength="40" placeholder="${esc(f.tn ? f.base + ", " + tName(f.tn, true) : f.base)}" value="${esc(LABELS[f.id] || "")}"></label>`).join("");
  }
  $("label-form").onsubmit = async (e) => {
    e.preventDefault();
    const labels = {};
    if ($("lb-t1").value.trim()) labels["tariff:1"] = $("lb-t1").value.trim();
    if ($("lb-t2").value.trim()) labels["tariff:2"] = $("lb-t2").value.trim();
    document.querySelectorAll("#lb-list input").forEach((i) => { if (i.value.trim()) labels[i.dataset.l] = i.value.trim(); });
    const r = await api("PUT", "/api/labels", { labels });
    $("lb-msg").innerHTML = r.ok ? '<span class="ok">Saved</span>' : errHtml(r);
    if (r.ok) { LABELS = r.labels; renderLabelForm(); }
  };
  function renderConn() {
    if (document.activeElement !== $("host")) $("host").value = info.host || "";
    $("conn").innerHTML = info.paired
      ? `<span class="ok">● Paired</span> as ${esc(info.userName)}${info.pairedAt ? " (" + new Date(info.pairedAt).toLocaleString() + ")" : ""}`
      : "● Not paired yet";
  }
  $("save-host").onclick = async () => {
    const r = await api("POST", "/api/config", { host: $("host").value });
    if (!r.ok) $("conn").innerHTML = `<span class="err">${esc(r.error)}</span>`;
    else { await refreshInfo(); renderConn(); loadDevice(); }
  };
  $("pair").onclick = async () => {
    if (info.paired && !confirm("Already paired. Pairing again replaces the stored token. Continue?")) return;
    const s = await api("POST", "/api/config", { host: $("host").value });
    if (!s.ok) return ($("conn").innerHTML = `<span class="err">${esc(s.error)}</span>`);
    $("pair").disabled = true;
    $("conn").innerHTML = "👉 <b>Press the button on your P1 meter now</b> (waiting up to 60 s)…";
    const r = await api("POST", "/api/pair");
    $("pair").disabled = false;
    await refreshInfo(); renderConn();
    if (!r.ok) $("conn").innerHTML += ` <span class="err">${esc(r.error)}</span>`;
    else { toast("Paired"); loadDevice(); }
  };
  $("forget").onclick = async () => {
    if (!confirm("Delete the stored token from this app? (The user stays on the meter until you delete it below.)")) return;
    await api("POST", "/api/forget");
    await refreshInfo(); renderConn(); loadDevice();
  };

  const unpaired = () => !(info.host && info.paired);
  const notPaired = '<span class="muted">Enter the meter address and pair first.</span>';
  const errHtml = (r) => `<span class="err">${esc(r.error || "Failed")}</span>`;

  async function loadDevice() {
    if (unpaired()) {
      for (const id of ["info-body", "sys-body", "bat-body", "users-body"]) $(id).innerHTML = notPaired;
      return;
    }
    await loadInfo(); await loadSystem(); await loadBatteries(); await loadUsers();
  }
  async function loadInfo() {
    const [d, s] = [await api("GET", "/api/device"), await api("GET", "/api/system")];
    if (!d.ok) return ($("info-body").innerHTML = errHtml(d));
    const x = d.data, y = s.ok ? s.data : {};
    const rows = [["Product", x.product_name], ["Type", x.product_type], ["Serial / MAC", x.serial], ["Firmware", x.firmware_version], ["API version", x.api_version],
      ["Wi-Fi network", y.wifi_ssid], ["Wi-Fi signal", y.wifi_rssi_db != null ? y.wifi_rssi_db + " dBm" : null], ["Uptime", y.uptime_s != null ? fmtDur(y.uptime_s) : null]];
    $("info-body").innerHTML = rows.filter((r) => r[1] != null).map((r) => `<span class="k">${r[0]}</span><span>${esc(r[1])}</span>`).join("");
  }
  $("info-reload").onclick = loadInfo;
  $("users-reload").onclick = loadUsers;

  const sw = (id, on, label, desc) =>
    `<div class="setting"><div><div>${label}</div><div class="d">${desc}</div></div><label class="switch"><input type="checkbox" id="${id}"${on ? " checked" : ""}><span></span></label></div>`;

  async function loadSystem() {
    const r = await api("GET", "/api/system");
    if (!r.ok) return ($("sys-body").innerHTML = errHtml(r));
    renderSystem(r.data);
  }
  async function putSystem(body, undo) {
    const r = await api("PUT", "/api/system", body);
    if (!r.ok) { toast(r.error, true); if (undo) undo(); return; }
    toast("Saved");
    renderSystem(r.data);
  }
  function renderSystem(s) {
    const led = s.status_led_brightness_pct;
    $("sys-body").innerHTML =
      (led != null ? `<div class="setting"><div><div>Status LED brightness</div><div class="d">Dims the LED in normal operation; errors always show at full brightness.</div></div>` +
        `<div class="row"><input type="range" id="s-led" min="0" max="100" step="5" value="${led}"><b id="s-led-v">${led}%</b></div></div>` : "") +
      (s.cloud_enabled != null ? sw("s-cloud", s.cloud_enabled, "Cloud communication", "Needed for the HomeWizard app away from home, firmware updates and predictive battery mode. Off = fully local.") : "") +
      (s.api_v1_enabled != null ? sw("s-v1", s.api_v1_enabled, "Legacy API v1", "Other tools that still use the unauthenticated v1 API stop working when this is off.") : "") +
      `<div class="setting"><div><div>Actions</div><div class="d">Identify blinks the LED for a few seconds. Reboot restarts the meter (logging has a short gap).</div></div>` +
      `<div class="row"><button id="s-ident">Identify</button><button id="s-reboot" class="danger">Reboot</button></div></div>`;
    const led$ = $("s-led");
    if (led$) {
      led$.oninput = () => ($("s-led-v").textContent = led$.value + "%");
      led$.onchange = () => putSystem({ status_led_brightness_pct: +led$.value });
    }
    const cl = $("s-cloud");
    if (cl) cl.onchange = () => {
      if (!cl.checked && !confirm("Turn off cloud communication?\n\nThe HomeWizard app can no longer reach this meter and it stops receiving firmware updates. Local access (this app) keeps working.")) { cl.checked = true; return; }
      putSystem({ cloud_enabled: cl.checked }, () => (cl.checked = !cl.checked));
    };
    const v1 = $("s-v1");
    if (v1) v1.onchange = () => {
      if (!v1.checked && !confirm("Turn off API v1? Anything still using the v1 API will stop getting data.")) { v1.checked = true; return; }
      putSystem({ api_v1_enabled: v1.checked }, () => (v1.checked = !v1.checked));
    };
    $("s-ident").onclick = async () => { const r = await api("POST", "/api/system/identify"); toast(r.ok ? "LED is blinking" : r.error, !r.ok); };
    $("s-reboot").onclick = async () => {
      if (!confirm("Reboot the meter now?")) return;
      const r = await api("POST", "/api/system/reboot");
      toast(r.ok ? "Rebooting… it will be back in a minute" : r.error, !r.ok);
    };
  }

  async function loadBatteries() {
    const r = await api("GET", "/api/batteries");
    if (!r.ok || !r.data || typeof r.data !== "object") {
      $("bat-body").innerHTML = '<span class="muted">No Plug-In Battery group is available on this meter' + (r.status ? ` (HTTP ${r.status})` : "") + ".</span>";
      return;
    }
    renderBatteries(r.data);
  }
  async function putBat(body) {
    const r = await api("PUT", "/api/batteries", body);
    if (!r.ok) toast(r.error, true); else toast("Saved");
    if (r.data && typeof r.data === "object" && r.ok) renderBatteries(r.data); else loadBatteries();
  }
  function renderBatteries(b) {
    const perms = b.permissions || [];
    const lock = b.charge_to_full === true || b.mode === "to_full";
    const stat = (k, v, u) => `<span class="k">${k}</span><span>${v == null ? "–" : nf(v, 0) + " " + u}</span>`;
    $("bat-body").innerHTML =
      `<div class="kv" style="margin-bottom:.6rem"><span class="k">Batteries</span><span>${b.battery_count ?? "–"}</span>` +
      stat("Power now", b.power_w, "W") + stat("Target", b.target_power_w, "W") + stat("Max charge", b.max_consumption_w, "W") + stat("Max discharge", b.max_production_w, "W") + `</div>` +
      `<div class="setting"><div><div>Mode</div><div class="d">Zero keeps net power at zero; predictive plans with prices (needs cloud); standby does nothing.</div></div>` +
      `<select id="b-mode">${["zero", "predictive", "standby"].map((m) => `<option${b.mode === m ? " selected" : ""}>${m}</option>`).join("")}${b.mode === "to_full" ? "<option selected>to_full</option>" : ""}</select></div>` +
      `<div class="setting"><div><div>Allowed to</div><div class="d">${lock ? "Locked while charging to full." : "Fine-grained control of charging and discharging."}</div></div>` +
      `<div class="row"><label><input type="checkbox" id="b-ch"${perms.includes("charge_allowed") ? " checked" : ""}${lock ? " disabled" : ""}> charge</label>` +
      `<label><input type="checkbox" id="b-dis"${perms.includes("discharge_allowed") ? " checked" : ""}${lock ? " disabled" : ""}> discharge</label></div></div>` +
      sw("b-full", b.charge_to_full === true, "Charge to full", "Charges all batteries to 100 %, then returns to the previous mode.");
    $("b-mode").onchange = () => putBat({ mode: $("b-mode").value });
    const sendPerms = () => putBat({ permissions: [$("b-ch").checked && "charge_allowed", $("b-dis").checked && "discharge_allowed"].filter(Boolean) });
    $("b-ch").onchange = sendPerms; $("b-dis").onchange = sendPerms;
    $("b-full").onchange = () => putBat({ charge_to_full: $("b-full").checked });
  }

  async function loadUsers() {
    const r = await api("GET", "/api/users");
    if (!r.ok) return ($("users-body").innerHTML = errHtml(r));
    const us = Array.isArray(r.data) ? r.data : [];
    $("users-body").innerHTML = `<table class="t"><tr><th>Name</th><th></th></tr>` +
      us.map((u) => `<tr><td>${esc(u.name)}${u.current ? '<span class="badge run">this app</span>' : ""}</td>` +
        `<td style="text-align:right"><button class="danger" data-name="${esc(u.name)}" data-cur="${u.current ? 1 : 0}">Delete</button></td></tr>`).join("") + `</table>` +
      `<p class="muted fine">Each paired app has its own user. <code>cloud/…</code> users belong to HomeWizard's app. Deleting a user revokes its token.</p>`;
    $("users-body").querySelectorAll("button[data-name]").forEach((b) => (b.onclick = async () => {
      const cur = b.dataset.cur === "1", name = b.dataset.name;
      if (!confirm(cur ? `“${name}” is the user THIS app uses. Deleting it logs this app out and you must pair again. Delete anyway?` : `Delete user “${name}” from the meter? Whatever app uses it loses access.`)) return;
      const d = await api("DELETE", "/api/users", { name, force: cur });
      if (!d.ok) return toast(d.error, true);
      toast("User deleted");
      if (cur) { await refreshInfo(); renderConn(); }
      loadDevice();
    }));
  }

  function rawCards(d) {
    const items = [["Power", d.power_w, "W"], ["L1", d.power_l1_w, "W"], ["L2", d.power_l2_w, "W"], ["L3", d.power_l3_w, "W"], ["Import", d.energy_import_kwh, "kWh"], ["Export", d.energy_export_kwh, "kWh"], ["Voltage", d.voltage_v ?? d.voltage_l1_v, "V"], ["Tariff", d.tariff, ""]];
    for (const e of d.external || []) items.push([e.type, e.value, e.unit]);
    return items.filter((i) => i[1] != null).map((i) => `<div class="c"><small>${esc(i[0])}</small><b>${nf(i[1], 3).replace(/\.?0+$/, "")} ${esc(i[2])}</b></div>`).join("");
  }
  document.querySelectorAll("[data-get]").forEach((b) => (b.onclick = async () => {
    const what = b.dataset.get;
    b.disabled = true; $("cards").innerHTML = ""; $("dstatus").textContent = "Loading…";
    const r = await api("GET", "/api/" + what);
    b.disabled = false;
    $("dstatus").innerHTML = r.ok ? `<span class="ok">OK</span> (${r.status})` : errHtml(r);
    $("out").textContent = typeof r.data === "string" ? r.data : JSON.stringify(r.data ?? r, null, 2);
    if (r.ok && what === "measurement") $("cards").innerHTML = rawCards(r.data);
  }));
  $("all").onclick = async () => {
    $("all").disabled = true; $("cards").innerHTML = ""; $("dstatus").textContent = "Loading everything…";
    const r = await api("GET", "/api/all");
    $("all").disabled = false;
    if (!r.results) { $("dstatus").innerHTML = errHtml(r); return; }
    const parts = Object.entries(r.results);
    $("dstatus").textContent = `${parts.filter(([, v]) => v.ok).length} of ${parts.length} endpoints returned data`;
    $("out").textContent = parts.map(([n, v]) => `=== ${n} (${v.path}) ${v.ok ? "OK" : "FAILED"} ${v.status} ===\n` + (v.error && !v.ok ? v.error + "\n" : "") + (typeof v.data === "string" ? v.data : JSON.stringify(v.data, null, 2))).join("\n\n");
    if (r.results.measurement?.ok) $("cards").innerHTML = rawCards(r.results.measurement.data);
  };

  $("price-form").onsubmit = async (e) => {
    e.preventDefault();
    const r = await api("PUT", "/api/prices", { currency: $("pr-cur").value, import: +$("pr-imp").value, export: +$("pr-exp").value, gas: +$("pr-gas").value });
    $("pr-msg").innerHTML = r.ok ? '<span class="ok">Saved</span>' : errHtml(r);
    if (r.ok) info.prices = r.prices;
  };

  // ---------------------------------------------------------------- boot
  setInterval(() => { if (tab !== "live") refreshInfo(); }, 15000);
  showTab(location.hash.slice(1) || "live");
})();
