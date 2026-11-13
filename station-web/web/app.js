const $ = (id) => document.getElementById(id);
const MB = 1048576, GB = 1073741824;

const titles = {
  overview: ["Моя Станция Макс", "Панель работает на самой станции"],
  access: ["Доступ", "root, ADB, SSH и защита от OTA"],
  device: ["Устройство", "Wi-Fi, Bluetooth, экран и питание"],
  light: ["Свет", "кольцо и подсветка станции"],
  journal: ["Журнал", "логи сервисов и системы"],
  backups: ["Резервные копии", "конфиги, ключи и доступы"],
  features: ["Возможности", "что реально работает"],
  settings: ["Настройки", "логин, пароль и параметры панели"],
  console: ["Консоль", "root shell на станции"],
};

function setText(id, v) {
  const el = $(id);
  if (el) el.textContent = v;
}

function fmtMB(bytes) { return (bytes / MB).toFixed(0); }
function fmtGB(bytes) { return (bytes / GB).toFixed(2); }

const RING = 213.63;
function renderCores(cores) {
  const box = $("core-line");
  if (!box) return;
  if (!cores || !cores.length) {
    box.textContent = "—";
    return;
  }
  box.textContent = "ядра: " + cores.map((v) => Math.round(v) + "%").join(" · ");
  box.title = cores.map((v, i) => "cpu" + i + ": " + v.toFixed(1) + "%").join("\n");
}

function setRing(ringId, textId, pct) {
  const p = Math.max(0, Math.min(100, pct || 0));
  const ring = $(ringId);
  if (ring) ring.style.strokeDashoffset = String(RING * (1 - p / 100));
  setText(textId, Math.round(p) + "%");
}

function showView(name) {
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.view === name));
  document.querySelectorAll(".view").forEach((v) => v.classList.toggle("hidden", v.id !== "view-" + name));
  const [title, sub] = titles[name] || titles.overview;
  setText("page-title", title);
  setText("page-subtitle", sub);
  if (name === "backups") loadBackups();
  if (name === "features") loadFeatures();
  if (name === "journal") loadJournal();
}

document.querySelectorAll(".nav-item").forEach((b) => {
  b.addEventListener("click", () => showView(b.dataset.view));
});

async function api(path, options) {
  const opts = Object.assign({ cache: "no-store" }, options || {});
  const r = await fetch(path, opts);
  if (r.status === 401) {
    window.location = "/login";
    throw new Error("unauthorized");
  }
  return r;
}

let lastMode = "steady";
let ringDirty = false;
let liveViewEnabled = localStorage.getItem("sw-liveview") !== "0";
let lastAuthUser = null;

const serviceInfo = {
  rootd: "root и защита SELinux",
  adbd: "ADB на порту 5555",
  absshd: "SSH на порту 2222",
  adbguard: "сторож ADB и hosts",
  stationweb: "эта веб-панель",
};

function renderServices(services) {
  const box = $("services");
  box.innerHTML = "";
  Object.entries(services || {}).forEach(([name, state]) => {
    const el = document.createElement("div");
    const running = state === "running";
    el.className = "svc-tile" + (running ? " run" : "");
    el.innerHTML = '<span class="svc-dot"></span><div class="svc-copy"><strong>' + name + '</strong><small>' +
      (serviceInfo[name] || "сервис") + '</small></div><span class="svc-state">' + (state || "—") + '</span>';
    box.appendChild(el);
  });
}

function renderStatus(s) {
  setText("heading-model", s.model || "YANDEXSTATION_2");
  setText("device-address", s.ip || "—");
  setText("android-chip", "Android " + (s.android || "—"));
  setText("build-chip", "build " + (s.build || "—"));
  setText("selinux-chip", "SELinux " + (s.selinux || "—"));
  setText("device-summary", s.wifi && s.wifi.ssid ? "подключена к " + s.wifi.ssid : "подключена по Wi-Fi");
  setText("last-updated", "обновлено " + new Date().toLocaleTimeString("ru-RU"));
  setText("transport-status", s.adb_allowed ? "SSH 2222 · ADB 5555" : "SSH 2222 · ADB выкл");
  const adbSw = $("adb-toggle");
  if (adbSw) {
    const adbOn = !!s.adb_allowed;
    const adbRun = !!s.adb_running;
    adbSw.dataset.on = adbOn ? "1" : "0";
    adbSw.setAttribute("aria-checked", adbOn ? "true" : "false");
    setText("adb-toggle-state", !adbOn
      ? "выключен"
      : (adbRun ? "включён · сервис запущен" : "включён · сервис запускается"));
  }
  if (s.panel_version) {
    setText("app-version", "v" + s.panel_version);
    setText("version-current", "v" + s.panel_version);
  }
  if (s.panel_build) setText("version-built", "сборка " + String(s.panel_build).replace("T", " "));
  const defaultPwd = $("cred-default-warning");
  if (defaultPwd) defaultPwd.hidden = !s.default_password;

  const used = s.mem_total - s.mem_avail;
  setText("memory-used", fmtMB(used));
  setText("memory-total", fmtMB(s.mem_total));
  setText("memory-available", "свободно " + fmtMB(s.mem_avail) + " МБ");
  setRing("memory-ring", "memory-percent", s.mem_total ? (used / s.mem_total) * 100 : 0);
  setText("disk-free", fmtGB(s.disk_free));
  setText("disk-total", fmtGB(s.disk_total));
  setText("disk-used", "занято " + fmtGB(s.disk_total - s.disk_free) + " ГБ");
  setRing("disk-ring", "disk-percent", s.disk_total ? ((s.disk_total - s.disk_free) / s.disk_total) * 100 : 0);
  setText("cpu-pct", typeof s.cpu_pct === "number" ? s.cpu_pct.toFixed(1) : "—");
  setRing("cpu-ring", "cpu-percent", s.cpu_pct || 0);
  renderCores(s.cpu_cores || []);
  setText("cpu-temperature", s.temp_c ? s.temp_c.toFixed(1) : "—");
  setText("temp-detail", s.temp_c ? "норма" : "нет данных");
  setText("uptime", s.uptime || "—");
  setText("load-detail", "load " + (s.load || "—"));

  renderServices(s.services);

  const wifi = s.wifi || {};
  setText("wifi-current", (wifi.ssid || "нет сети") + (wifi.ip ? " · " + wifi.ip : ""));
  renderSignalBars($("wifi-bars"), wifi.signal);
  setText("wifi-modal-current", wifi.ssid || (wifi.enabled ? "нет сети" : "Wi-Fi выключен"));
  setText("wifi-modal-sub", wifi.ip ? wifi.ip : (wifi.state || "—"));
  setText("wifi-modal-enabled", wifi.enabled ? "включён" : "выключен");
  $("wifi-dot").className = "status-dot " + (wifi.enabled ? "ok" : "pending");

  const bt = s.bt || {};
  setText("bluetooth-state", bt.enabled ? "включён" : "выключен");
  setText("d-bt-name", bt.name || "—");
  setText("d-bt-addr", bt.address || "—");
  ["bluetooth-toggle", "bluetooth-toggle-2"].forEach((id) => {
    const el = $(id);
    if (el) {
      el.setAttribute("aria-checked", bt.enabled ? "true" : "false");
      el.dataset.on = bt.enabled ? "1" : "0";
    }
  });

  const vol = s.volume || {};
  const slider = $("media-volume");
  if (vol.max > 0) {
    slider.max = vol.max;
    if (document.activeElement !== slider) slider.value = vol.current;
    setText("media-volume-value", Math.round((vol.current / vol.max) * 100) + "%");
    setText("media-detail", "STREAM_MUSIC: " + vol.current + " из " + vol.max);
  }

  renderMedia(s.media || {});

  setText("root-status", (s.services || {}).rootd === "running" ? "работает" : "нет");
  setText("adb-status", (s.services || {}).adbd === "running" ? "работает" : "нет");
  setText("ssh-status", (s.services || {}).absshd === "running" ? "работает" : "нет");
  setText("hosts-status", s.hosts_blocked ? "включено" : "выключено");

  setText("a-root", (s.services || {}).rootd === "running" ? "running" : "—");
  setText("a-selinux", s.selinux || "—");
  setText("a-adb", (s.services || {}).adbd === "running" ? "5555 · running" : "—");
  setText("a-ssh", (s.services || {}).absshd === "running" ? "2222 · running" : "—");
  setText("a-keys", String(s.adb_keys || 0));
  setText("a-hosts", s.hosts_blocked ? "включено" : "выключено");
  setText("a-updater", s.updater_installed ? "установлен" : "удалён для user 0");
  setText("a-ip", s.ip || "—");
  setText("adb-address", "adb connect " + (s.ip || "IP") + ":5555");
  setText("ssh-address", "ssh -p 2222 root@" + (s.ip || "IP"));

  setText("d-ssid", wifi.ssid || "—");
  setText("d-ip", wifi.ip || s.ip || "—");
  setText("d-wifi", wifi.enabled ? "включён" : "выключен");
  setText("d-dns", s.dns || "—");

  $("conn-dot").className = "status-dot ok";
  $("sidebar-dot").className = "status-dot ok";
  setText("conn-text", "на связи");
  $("device-dot").className = "status-dot ok";
  setText("device-state", "станция на связи");

  const ring = s.ring || {};
  if (!ringDirty) {
    if (ring.mode) lastMode = ring.mode;
    if (ring.mode === "anim" && ring.anim) {
      const name = document.querySelector('#ring-anims .ring-anim-card[data-anim="' + ring.anim + '"] strong')?.textContent || ring.anim;
      if (!ringAnimState || ringAnimState.id !== ring.anim) {
        ensureRingAnim(ring.anim).then((data) => {
          if (!data) return;
          ringAnimState = { id: ring.anim, name, data, started: performance.now() };
          drawRingCanvas();
        });
      }
    }
    const colorInput = $("ring-color");
    const color2Input = $("ring-color2");
    const levelInput = $("ring-level");
    if (document.activeElement !== colorInput && ring.color) colorInput.value = ring.color;
    if (color2Input && document.activeElement !== color2Input && ring.color2) color2Input.value = ring.color2;
    if (document.activeElement !== levelInput && typeof ring.level === "number") {
      levelInput.value = ring.level;
      setText("ring-level-value", ring.level);
    }
    const speedInput = $("ring-speed");
    if (speedInput && document.activeElement !== speedInput && typeof ring.speed === "number") {
      speedInput.value = ring.speed;
      setText("ring-speed-value", ring.speed);
    }
    setText("ring-state", ring.mode === "native" ? "штатно" : (ring.mode || "—"));
    if (ring.mode === "native") setText("ring-modal-state", "Кольцо под штатным управлением станции");
    document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.toggle("active", x.dataset.mode === lastMode));
    updateRingPreview();
  }

  const credUser = $("cred-user");
  if (credUser && s.auth_user) {
    if (!credUser.value || credUser.value === lastAuthUser) credUser.value = s.auth_user;
    lastAuthUser = s.auth_user;
  }
}

let failures = 0;
async function refresh() {
  try {
    const r = await api("/api/status", { cache: "no-store" });
    renderStatus(await r.json());
    failures = 0;
  } catch (e) {
    failures++;
    if (failures > 1) {
      setText("conn-text", "нет связи");
      $("conn-dot").className = "status-dot pending";
      $("sidebar-dot").className = "status-dot pending";
    }
  }
}

async function postJSON(path, body) {
  const r = await api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  return r.json();
}

function hexRgb(hex) {
  const s = String(hex || "").replace("#", "");
  if (s.length !== 6) return [184, 243, 107];
  return [parseInt(s.slice(0, 2), 16), parseInt(s.slice(2, 4), 16), parseInt(s.slice(4, 6), 16)];
}

function mixRgb(a, b, t) {
  return [Math.round(a[0] + (b[0] - a[0]) * t), Math.round(a[1] + (b[1] - a[1]) * t), Math.round(a[2] + (b[2] - a[2]) * t)];
}

function hsvRgb(h, s, v) {
  h = ((h % 360) + 360) % 360;
  const c = v * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = v - c;
  let r = 0, g = 0, b = 0;
  if (h < 60) [r, g, b] = [c, x, 0];
  else if (h < 120) [r, g, b] = [x, c, 0];
  else if (h < 180) [r, g, b] = [0, c, x];
  else if (h < 240) [r, g, b] = [0, x, c];
  else if (h < 300) [r, g, b] = [x, 0, c];
  else [r, g, b] = [c, 0, x];
  return [Math.round((r + m) * 255), Math.round((g + m) * 255), Math.round((b + m) * 255)];
}

let ringPreviewT0 = performance.now();
let ringPreviewRAF = null;
let ringAnimState = null;
const ringAnimFrames = {};

async function ensureRingAnim(id) {
  if (!id) return null;
  if (ringAnimFrames[id]) return ringAnimFrames[id];
  try {
    const r = await api("/api/ring/animations/frames?id=" + encodeURIComponent(id), { cache: "no-store" });
    if (!r.ok) return null;
    const j = await r.json();
    ringAnimFrames[id] = j;
    return j;
  } catch (e) {
    return null;
  }
}

function animFrameAt(data, ms) {
  if (!data || !data.frames || !data.frames.length) return null;
  const total = data.total || 1;
  let t = ms;
  if (t >= total) {
    if (!data.loop) return data.frames[data.frames.length - 1];
    t = t % total;
  }
  let acc = 0;
  for (let i = 0; i < data.frames.length; i++) {
    acc += data.delays[i] || 20;
    if (t < acc) return data.frames[i];
  }
  return data.frames[data.frames.length - 1];
}

function drawRingCanvas() {
  const canvas = $("ring-canvas");
  if (!canvas) return;
  const ctx = canvas.getContext("2d");
  const w = canvas.width, h = canvas.height;
  const cx = w / 2, cy = h / 2, radius = Math.min(w, h) * 0.34;
  const level = Number($("ring-level").value) / 100;
  const speed = Number($("ring-speed").value) / 100;
  const speedFactor = 0.3 + speed * 2.2;
  const t = (performance.now() - ringPreviewT0) / 1000;
  const base = hexRgb($("ring-color").value);
  const second = hexRgb($("ring-color2").value);

  let animCols = null;
  if (lastMode === "anim" && ringAnimState) {
    animCols = animFrameAt(ringAnimState.data, performance.now() - ringAnimState.started);
  }

  ctx.clearRect(0, 0, w, h);
  ctx.lineCap = "butt";
  ctx.lineWidth = Math.max(12, w * 0.06);
  const step = (Math.PI * 2) / 24;
  // как у живого кольца на обзоре: сегмент 0 внизу, дальше против часовой
  const start = Math.PI / 2;
  const dir = -1;
  for (let i = 0; i < 24; i++) {
    let rgb = base;
    if (lastMode === "off") rgb = [90, 100, 95];
    else if (lastMode === "native") rgb = [120, 130, 125];
    else if (lastMode === "rainbow") {
      const hue = (t * 45 * speedFactor + i * 15) % 360;
      rgb = hsvRgb(hue, 1, 0.85);
    } else if (lastMode === "gradient") {
      const phase = (t * 0.12 * speedFactor) % 1;
      rgb = mixRgb(base, second, (i / 24 + phase) % 1);
    } else if (lastMode === "breathe") {
      const k = 0.15 + 0.85 * (0.5 + 0.5 * Math.sin(t * 1.7 * speedFactor));
      rgb = base.map((v) => Math.round(v * k));
    } else if (lastMode === "candle") {
      const k = (0.35 + 0.65 * Math.random()) * (0.75 + 0.25 * Math.sin(t * 2.2 + i * 0.7));
      rgb = [Math.round(255 * k), Math.round(150 * k), Math.round(60 * k)];
    } else if (lastMode === "anim" && animCols) {
      // физический порядок сегментов отражён так же, как на кольце
      const hex = animCols[(24 - i) % 24];
      rgb = [parseInt(hex.slice(0, 2), 16), parseInt(hex.slice(2, 4), 16), parseInt(hex.slice(4, 6), 16)];
    }
    ctx.globalAlpha = (lastMode === "off" || lastMode === "native") ? 0.18 : 0.25 + level * 0.75;
    ctx.strokeStyle = "rgb(" + rgb.join(",") + ")";
    const from = start + dir * (i * step + 0.015);
    const to = start + dir * ((i + 1) * step - 0.015);
    ctx.beginPath();
    ctx.arc(cx, cy, radius, Math.min(from, to), Math.max(from, to));
    ctx.stroke();
  }
  ctx.globalAlpha = 1;
  ctx.fillStyle = "rgba(255,255,255,.35)";
  ctx.font = "12px 'Segoe UI', sans-serif";
  ctx.textAlign = "center";
  const label = lastMode === "native"
    ? "штатно"
    : lastMode === "anim"
      ? ((ringAnimState && ringAnimState.name) || "анимация")
      : "24 сегмента";
  ctx.fillText(label, cx, cy + 4);

  const label2 = document.querySelector('label[for="ring-color2"]');
  const input2 = $("ring-color2");
  const showSecond = lastMode === "gradient";
  if (label2) label2.style.display = showSecond ? "" : "none";
  if (input2) input2.style.display = showSecond ? "" : "none";
}

function startRingPreview() {
  stopRingPreview();
  ringPreviewT0 = performance.now();
  const tick = () => {
    drawRingCanvas();
    ringPreviewRAF = requestAnimationFrame(tick);
  };
  ringPreviewRAF = requestAnimationFrame(tick);
}

function stopRingPreview() {
  if (ringPreviewRAF) {
    cancelAnimationFrame(ringPreviewRAF);
    ringPreviewRAF = null;
  }
}

let ringRaf = 0;
function scheduleRingDraw() {
  if (ringRaf) return;
  ringRaf = requestAnimationFrame(() => {
    ringRaf = 0;
    drawRingCanvas();
  });
}

function updateRingPreview() {
  const color = $("ring-color").value;
  const level = Number($("ring-level").value);
  const el = $("ring-preview");
  if (el) {
    el.style.background = color;
    el.style.boxShadow = "0 0 " + Math.max(4, level / 2) + "px " + color;
    el.style.opacity = level ? 0.25 + level / 150 : 0;
  }
  setText("ring-level-value", level);
  const speed = $("ring-speed");
  if (speed) setText("ring-speed-value", speed.value);
  scheduleRingDraw();
}

function ringEdited() {
  ringDirty = true;
  updateRingPreview();
}
$("ring-color").addEventListener("input", ringEdited);
$("ring-color2").addEventListener("input", ringEdited);
$("ring-level").addEventListener("input", ringEdited);
$("ring-speed").addEventListener("input", ringEdited);

let ringProfiles = [];

async function refreshRingProfiles(selectName) {
  try {
    const r = await api("/api/ring/profiles");
    ringProfiles = await r.json();
  } catch (e) {
    ringProfiles = [];
  }
  const sel = $("ring-profile");
  if (!sel) return;
  const current = selectName || sel.value;
  sel.innerHTML = "";
  const empty = document.createElement("option");
  empty.value = "";
  empty.textContent = "Профили…";
  sel.appendChild(empty);
  for (const p of ringProfiles) {
    const o = document.createElement("option");
    o.value = p.name;
    o.textContent = p.name;
    sel.appendChild(o);
  }
  if (current) sel.value = current;
}

function loadRingProfile(name) {
  const p = ringProfiles.find((x) => x.name === name);
  if (!p) return;
  $("ring-color").value = p.color || "#b8f36b";
  $("ring-color2").value = p.color2 || "#6e62ff";
  $("ring-level").value = String(p.level ?? 70);
  $("ring-speed").value = String(p.speed || 35);
  $("ring-profile-name").value = p.name;
  ringDirty = true;
  lastMode = p.mode || "steady";
  document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.toggle("active", x.dataset.mode === lastMode));
  setText("ring-state", lastMode);
  updateRingPreview();
  setText("ring-modal-state", "профиль «" + p.name + "» — нажмите «Применить»");
}

$("ring-profile").addEventListener("change", () => {
  if ($("ring-profile").value) loadRingProfile($("ring-profile").value);
});

$("ring-profile-save").addEventListener("click", async () => {
  const name = ($("ring-profile-name").value || $("ring-profile").value || "").trim();
  if (!name) {
    setText("ring-modal-state", "введите название профиля");
    $("ring-profile-name").focus();
    return;
  }
  try {
    await postJSON("/api/ring/profiles", {
      name,
      mode: lastMode,
      color: $("ring-color").value,
      color2: $("ring-color2").value,
      level: Number($("ring-level").value),
      speed: Number($("ring-speed").value),
    });
    await refreshRingProfiles(name);
    setText("ring-modal-state", "профиль «" + name + "» сохранён");
  } catch (e) {
    setText("ring-modal-state", "ошибка: " + e.message);
  }
});

$("ring-profile-delete").addEventListener("click", async () => {
  const name = $("ring-profile").value || $("ring-profile-name").value.trim();
  if (!name) {
    setText("ring-modal-state", "выберите профиль");
    return;
  }
  if (!confirm("Удалить профиль «" + name + "»?")) return;
  try {
    const r = await api("/api/ring/profiles?name=" + encodeURIComponent(name), { method: "DELETE" });
    ringProfiles = await r.json();
    $("ring-profile-name").value = "";
    await refreshRingProfiles("");
    setText("ring-modal-state", "профиль «" + name + "» удалён");
  } catch (e) {
    setText("ring-modal-state", "ошибка: " + e.message);
  }
});

document.querySelectorAll(".mode-buttons button").forEach((b) => {
  b.addEventListener("click", () => {
    ringDirty = true;
    lastMode = b.dataset.mode;
    ringAnimState = null;
    document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.toggle("active", x === b));
    setText("ring-state", lastMode);
    drawRingCanvas();
  });
});

$("ring-apply").addEventListener("click", async () => {
  setText("ring-modal-state", "применяю…");
  try {
    await postJSON("/api/ring", {
      mode: lastMode,
      anim: ringAnimState?.id || "",
      color: $("ring-color").value,
      color2: $("ring-color2").value,
      level: Number($("ring-level").value),
      speed: Number($("ring-speed").value),
    });
    setText("ring-modal-state", "применено");
    ringDirty = false;
    await refresh();
  } catch (e) {
    setText("ring-modal-state", "ошибка: " + e.message);
  }
});

$("ring-off").addEventListener("click", async () => {
  try {
    await postJSON("/api/ring", { mode: "off", color: $("ring-color").value, color2: $("ring-color2").value, level: 0 });
    lastMode = "off";
    ringDirty = false;
    document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.toggle("active", x.dataset.mode === "off"));
    drawRingCanvas();
    setText("ring-modal-state", "выключено");
    await refresh();
  } catch (e) {}
});

$("ring-native").addEventListener("click", async () => {
  try {
    await postJSON("/api/ring", {
      mode: "native",
      color: $("ring-color").value,
      color2: $("ring-color2").value,
      level: Number($("ring-level").value),
      speed: Number($("ring-speed").value),
    });
    lastMode = "native";
    ringDirty = false;
    document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.toggle("active", x.dataset.mode === "native"));
    drawRingCanvas();
    setText("ring-modal-state", "кольцо под штатным управлением станции");
    await refresh();
  } catch (e) {
    setText("ring-modal-state", "ошибка: " + e.message);
  }
});

// Кольцо: вкладки «Ручное» / «Анимации» и родные .led-шаблоны.
let ringAnimsLoaded = false;
async function loadRingAnims() {
  const box = $("ring-anims");
  if (!box || ringAnimsLoaded) return;
  ringAnimsLoaded = true;
  try {
    const r = await api("/api/ring/animations", { cache: "no-store" });
    const j = await r.json();
    box.innerHTML = "";
    for (const a of j.animations || []) {
      const card = document.createElement("button");
      card.type = "button";
      card.className = "ring-anim-card";
      card.dataset.anim = a.id;
      const strip = document.createElement("span");
      strip.className = "ring-anim-strip";
      for (const c of a.preview || []) {
        const seg = document.createElement("i");
        seg.style.background = c;
        strip.appendChild(seg);
      }
      const name = document.createElement("strong");
      name.textContent = a.name;
      card.append(strip, name);
      card.addEventListener("click", () => applyRingAnim(a.id, card));
      box.appendChild(card);
    }
  } catch (e) {
    box.textContent = "Не удалось загрузить анимации";
    ringAnimsLoaded = false;
  }
}

async function applyRingAnim(id, card) {
  try {
    const st = await postJSON("/api/ring/animation", { id });
    const name = (card && card.querySelector("strong")?.textContent) || id;
    const data = await ensureRingAnim(id);
    ringAnimState = { id, name, data, started: performance.now() };
    const box = $("ring-anims");
    if (box) box.querySelectorAll(".ring-anim-card").forEach((c) => c.classList.toggle("active", c === card));
    lastMode = "anim";
    ringDirty = false;
    document.querySelectorAll(".mode-buttons button").forEach((x) => x.classList.remove("active"));
    setText("ring-state", "анимация");
    setText("ring-modal-state", "Анимация: " + name);
    updateRingPreview();
  } catch (e) {
    setText("ring-modal-state", "Ошибка: " + (e && e.message ? e.message : "запрос"));
  }
}

document.querySelectorAll("#ring-tabs button").forEach((b) => {
  b.addEventListener("click", () => {
    document.querySelectorAll("#ring-tabs button").forEach((x) => x.classList.toggle("active", x === b));
    document.querySelectorAll(".ring-tab").forEach((t) => t.classList.toggle("hidden", t.id !== "rtab-" + b.dataset.rtab));
    if (b.dataset.rtab === "anim") loadRingAnims();
  });
});

function openRing() {
  ringDirty = false;
  startRingPreview();
  if (lastMode === "anim" && ringAnimState && !ringAnimState.data) {
    ensureRingAnim(ringAnimState.id).then((data) => {
      if (data) {
        ringAnimState.data = data;
        ringAnimState.started = performance.now();
      }
    });
  }
  drawRingCanvas();
  refreshRingProfiles();
  $("ring-dialog").showModal();
}
["ring-open", "ring-open-light"].forEach((id) => {
  const el = $(id);
  if (el) el.addEventListener("click", openRing);
});
$("ring-close").addEventListener("click", () => $("ring-dialog").close());
$("ring-dialog").addEventListener("close", () => {
  stopRingPreview();
  ringDirty = false;
  refresh();
});

/* ---------- wifi ---------- */

let wifiSelected = null;
let wifiScanning = false;

function wifiSecLabel(sec) {
  switch (sec) {
    case "open": return "открытая";
    case "wpa": return "WPA";
    case "wpa2": return "WPA2";
    case "wpa3": return "WPA3";
    case "wep": return "WEP";
    case "enterprise": return "802.1X";
    default: return sec || "—";
  }
}

function wifiSignalText(dbm) {
  if (typeof dbm !== "number") return "";
  return dbm + " dBm";
}

function signalLevel(dbm) {
  if (typeof dbm !== "number" || dbm === 0) return 0;
  if (dbm >= -55) return 4;
  if (dbm >= -67) return 3;
  if (dbm >= -78) return 2;
  if (dbm >= -90) return 1;
  return 0;
}

function renderSignalBars(el, dbm) {
  if (!el) return;
  const level = signalLevel(dbm);
  el.innerHTML = "";
  for (let i = 1; i <= 4; i++) {
    const bar = document.createElement("i");
    if (i <= level) bar.className = "on";
    el.appendChild(bar);
  }
  el.title = dbm ? dbm + " dBm" : "нет сигнала";
  el.setAttribute("aria-label", dbm ? "сигнал " + level + " из 4" : "нет сигнала");
}

async function wifiRefresh() {
  try {
    const r = await api("/api/wifi");
    const w = await r.json();
    setText("wifi-modal-current", w.ssid || (w.enabled ? "нет сети" : "Wi-Fi выключен"));
    setText("wifi-modal-sub", w.ip ? w.ip : (w.state || "—"));
    setText("wifi-modal-enabled", w.enabled ? "включён" : "выключен");
    renderSignalBars($("wifi-bars"), w.signal);
    return w;
  } catch (e) {
    return null;
  }
}

async function refreshWifiSaved() {
  const box = $("wifi-saved-list");
  if (!box) return;
  try {
    const r = await api("/api/wifi/saved");
    const j = await r.json();
    const list = j.networks || [];
    box.innerHTML = "";
    if (!list.length) {
      const p = document.createElement("p");
      p.className = "note";
      p.textContent = "Нет сохранённых сетей";
      box.appendChild(p);
      return;
    }
    for (const ssid of list) {
      const row = document.createElement("div");
      row.className = "wifi-saved-row";
      const name = document.createElement("span");
      name.textContent = ssid;
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "button subtle";
      btn.textContent = "Забыть";
      btn.addEventListener("click", async () => {
        if (!confirm("Забыть сеть «" + ssid + "»?")) return;
        try {
          const res = await postJSON("/api/wifi/forget", { ssid });
          if (res.error) throw new Error(res.error);
          setText("wifi-modal-state", "сеть забыта: " + ssid);
          await refresh();
          await refreshWifiSaved();
          await wifiScanNow();
        } catch (e) {
          setText("wifi-modal-state", "ошибка: " + e.message);
        }
      });
      row.append(name, btn);
      box.appendChild(row);
    }
  } catch (e) {
    box.textContent = "не удалось получить список";
  }
}

function renderWifiNetworks(networks) {
  const box = $("wifi-list");
  box.innerHTML = "";
  if (!networks || !networks.length) {
    const p = document.createElement("p");
    p.className = "note";
    p.textContent = "Сети не найдены";
    box.appendChild(p);
    return;
  }
  for (const n of networks) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "wifi-network";
    const name = document.createElement("span");
    name.className = "wifi-name";
    const strong = document.createElement("strong");
    strong.textContent = n.ssid;
    if (n.saved) {
      const saved = document.createElement("span");
      saved.className = "wifi-saved";
      saved.textContent = " · сохранена";
      strong.appendChild(saved);
    }
    const sub = document.createElement("small");
    sub.textContent = wifiSecLabel(n.security);
    name.append(strong, sub);
    const sig = document.createElement("small");
    sig.className = "wifi-signal";
    const bars = document.createElement("span");
    bars.className = "signal-bars";
    renderSignalBars(bars, n.signal);
    sig.append(bars, document.createTextNode(" " + wifiSignalText(n.signal)));
    b.append(name, sig);
    b.addEventListener("click", () => selectWifiNetwork(n, b));
    box.appendChild(b);
  }
}

function selectWifiNetwork(n, btn) {
  wifiSelected = n;
  document.querySelectorAll(".wifi-network").forEach((x) => x.classList.remove("selected"));
  if (btn) btn.classList.add("selected");
  $("wifi-selection").classList.remove("hidden");
  const open = n.security === "open";
  $("wifi-password").value = "";
  $("wifi-password").disabled = open;
  $("wifi-password").placeholder = open ? "сеть без пароля" : (n.saved ? "оставьте пустым — сохранённый пароль" : "пароль от сети");
  $("wifi-forget").style.display = n.saved ? "" : "none";
  $("wifi-connect").textContent = n.saved ? "Переподключиться" : "Подключить";
  setText("wifi-modal-state", n.ssid + " · " + wifiSecLabel(n.security) + " · " + wifiSignalText(n.signal));
  if (!open) $("wifi-password").focus();
}

async function wifiScanNow() {
  if (wifiScanning) return;
  wifiScanning = true;
  const btn = $("wifi-scan");
  btn.disabled = true;
  setText("wifi-modal-state", "сканирую сети…");
  try {
    const r = await api("/api/wifi/scan");
    const j = await r.json();
    if (!r.ok || j.error) throw new Error(j.error || "ошибка сканирования");
    renderWifiNetworks(j.networks || []);
    setText("wifi-modal-state", "найдено сетей: " + (j.networks || []).length);
  } catch (e) {
    setText("wifi-modal-state", "ошибка: " + e.message);
  } finally {
    wifiScanning = false;
    btn.disabled = false;
  }
}

function openWifi() {
  $("wifi-dialog").showModal();
  wifiRefresh();
  refreshWifiSaved();
  wifiScanNow();
}

$("wifi-open").addEventListener("click", openWifi);
const wifiOpenOverview = $("wifi-open-overview");
if (wifiOpenOverview) wifiOpenOverview.addEventListener("click", openWifi);
$("wifi-close").addEventListener("click", () => $("wifi-dialog").close());
$("wifi-scan").addEventListener("click", wifiScanNow);

$("wifi-cancel").addEventListener("click", () => {
  wifiSelected = null;
  $("wifi-selection").classList.add("hidden");
  document.querySelectorAll(".wifi-network").forEach((x) => x.classList.remove("selected"));
  setText("wifi-modal-state", "выберите сеть");
});

$("wifi-connect").addEventListener("click", async () => {
  if (!wifiSelected) return;
  const n = wifiSelected;
  const pass = n.security === "open" ? "" : $("wifi-password").value;
  if (n.security !== "open" && !n.saved && pass.length < 8) {
    setText("wifi-modal-state", "пароль должен быть не короче 8 символов");
    return;
  }
  if (n.security !== "open" && pass.length > 0 && pass.length < 8) {
    setText("wifi-modal-state", "пароль должен быть не короче 8 символов");
    return;
  }
  const btn = $("wifi-connect");
  btn.disabled = true;
  setText("wifi-modal-state", "подключаюсь к " + n.ssid + "…");
  try {
    const j = await postJSON("/api/wifi/connect", { ssid: n.ssid, password: pass, security: n.security });
    if (j.error) throw new Error(j.error);
    setText("wifi-modal-state", j.saved ? "подключено: " + n.ssid : "подключено, но может не сохраниться после перезагрузки");
    await refresh();
    await wifiRefresh();
    await wifiScanNow();
    await refreshWifiSaved();
  } catch (e) {
    const msg = e && e.message ? e.message : "запрос";
    if (/failed to fetch|networkerror|load failed|network/i.test(msg)) {
      setText("wifi-modal-state", "Станция переключается на новую сеть. Откройте панель по её новому IP.");
    } else {
      setText("wifi-modal-state", "ошибка: " + msg);
    }
    const st = $("wifi-modal-state");
    if (st) st.scrollIntoView({ block: "nearest", behavior: "smooth" });
  } finally {
    btn.disabled = false;
  }
});

$("wifi-forget").addEventListener("click", async () => {
  if (!wifiSelected || !wifiSelected.saved) return;
  const ssid = wifiSelected.ssid;
  if (!confirm("Забыть сеть «" + ssid + "»?")) return;
  try {
    const j = await postJSON("/api/wifi/forget", { ssid });
    if (j.error) throw new Error(j.error);
    setText("wifi-modal-state", "сеть забыта: " + ssid);
    wifiSelected = null;
    $("wifi-selection").classList.add("hidden");
    await refresh();
    await wifiScanNow();
    await refreshWifiSaved();
  } catch (e) {
    setText("wifi-modal-state", "ошибка: " + e.message);
  }
});

/* ---------- versions ---------- */
function openVersions() {
  const dialog = $("version-dialog");
  if (dialog && !dialog.open) dialog.showModal();
}
const versionBadge = $("app-version");
if (versionBadge) {
  versionBadge.addEventListener("click", openVersions);
  versionBadge.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openVersions();
    }
  });
}
$("version-close").addEventListener("click", () => $("version-dialog").close());

/* ---------- LED matrix ---------- */
const MW = 25, MH = 16, MCELL = 20;
let matrixPixels = new Array(MW * MH).fill(null);
let matrixPaint = null;
let matrixPainting = false;
let matrixTool = "brush";
let matrixTab = "draw";

const matrixCanvas = $("matrix-canvas");
const matrixCtx = matrixCanvas.getContext("2d");
const matrixBG = document.createElement("canvas");
matrixBG.width = matrixCanvas.width;
matrixBG.height = matrixCanvas.height;
const matrixBGCtx = matrixBG.getContext("2d");

function buildMatrixBG() {
  matrixBGCtx.clearRect(0, 0, matrixBG.width, matrixBG.height);
  for (let y = 0; y < MH; y++) {
    for (let x = 0; x < MW; x++) {
      matrixBGCtx.fillStyle = "#000000";
      matrixBGCtx.fillRect(x * MCELL, y * MCELL, MCELL, MCELL);
      matrixBGCtx.strokeStyle = "rgba(255,255,255,.07)";
      matrixBGCtx.strokeRect(x * MCELL + .5, y * MCELL + .5, MCELL - 1, MCELL - 1);
    }
  }
}
buildMatrixBG();

function brushValue() {
  return Number($("brush-brightness").value);
}

function paintCell(idx) {
  const x = idx % MW, y = Math.floor(idx / MW);
  const px = matrixPixels[idx];
  matrixCtx.drawImage(matrixBG, x * MCELL, y * MCELL, MCELL, MCELL, x * MCELL, y * MCELL, MCELL, MCELL);
  if (px) {
    matrixCtx.fillStyle = "rgb(" + px.join(",") + ")";
    matrixCtx.fillRect(x * MCELL + 1, y * MCELL + 1, MCELL - 2, MCELL - 2);
  }
}

function drawMatrix() {
  matrixCtx.drawImage(matrixBG, 0, 0);
  for (let i = 0; i < matrixPixels.length; i++) {
    const px = matrixPixels[i];
    if (!px) continue;
    const x = i % MW, y = Math.floor(i / MW);
    matrixCtx.fillStyle = "rgb(" + px.join(",") + ")";
    matrixCtx.fillRect(x * MCELL + 1, y * MCELL + 1, MCELL - 2, MCELL - 2);
  }
}

function matrixCell(ev) {
  const r = matrixCanvas.getBoundingClientRect();
  const x = Math.floor(((ev.clientX - r.left) / r.width) * MW);
  const y = Math.floor(((ev.clientY - r.top) / r.height) * MH);
  if (x < 0 || x >= MW || y < 0 || y >= MH) return -1;
  return y * MW + x;
}

let matrixLastCell = -1;

function applyMatrixPaint(idx) {
  if (idx < 0) return;
  matrixPixels[idx] = matrixPaint;
  paintCell(idx);
}

matrixCanvas.addEventListener("pointerdown", (ev) => {
  ev.preventDefault();
  const idx = matrixCell(ev);
  if (idx < 0) return;
  matrixPaint = matrixTool === "eraser" ? null : [brushValue(), brushValue(), brushValue()];
  matrixPainting = true;
  matrixLastCell = idx;
  applyMatrixPaint(idx);
  matrixCanvas.setPointerCapture(ev.pointerId);
});
matrixCanvas.addEventListener("pointermove", (ev) => {
  if (!matrixPainting || ev.buttons === 0) return;
  const idx = matrixCell(ev);
  if (idx === matrixLastCell) return;
  matrixLastCell = idx;
  applyMatrixPaint(idx);
});
matrixCanvas.addEventListener("pointerup", () => { matrixPainting = false; matrixLastCell = -1; });
matrixCanvas.addEventListener("pointercancel", () => { matrixPainting = false; matrixLastCell = -1; });

document.querySelectorAll("[data-mtool]").forEach((b) => {
  b.addEventListener("click", () => {
    matrixTool = b.dataset.mtool;
    document.querySelectorAll("[data-mtool]").forEach((x) => x.classList.toggle("active", x === b));
  });
});

$("brush-brightness").addEventListener("input", () => {
  setText("brush-brightness-value", $("brush-brightness").value);
});

$("matrix-display-brightness").addEventListener("input", () => {
  setText("matrix-display-brightness-value", $("matrix-display-brightness").value + "%");
});

document.querySelectorAll("#matrix-tabs button").forEach((b) => {
  b.addEventListener("click", () => {
    matrixTab = b.dataset.mtab;
    document.querySelectorAll("#matrix-tabs button").forEach((x) => x.classList.toggle("active", x === b));
    document.querySelectorAll(".matrix-tab").forEach((t) => t.classList.toggle("hidden", t.id !== "mtab-" + matrixTab));
    syncMatrixTab();
    if (matrixTab === "anim") loadMatrixAnims();
  });
});

// Настройки и кнопки показываем только там, где они реально работают.
function syncMatrixTab() {
  const tab = matrixTab;
  const show = (id, on) => {
    const el = $(id);
    if (el) el.classList.toggle("hidden", !on);
  };
  show("matrix-brush-row", tab === "draw");
  const settings = $("matrix-settings");
  if (settings) settings.classList.toggle("hidden", tab === "anim");
  show("matrix-duration-row", tab !== "anim");
  show("matrix-display-row", tab !== "anim");
  show("matrix-send", tab !== "anim");
}

syncMatrixTab();

// Родные анимации станции: GIF отдаются статикой, браузер сам их проигрывает.
let animsLoaded = false;
async function loadMatrixAnims() {
  const box = $("matrix-anims");
  if (!box || animsLoaded) return;
  animsLoaded = true;
  try {
    const r = await api("/api/matrix/animations", { cache: "no-store" });
    const j = await r.json();
    box.innerHTML = "";
    for (const a of j.animations || []) {
      const card = document.createElement("button");
      card.type = "button";
      card.className = "anim-card";
      card.dataset.anim = a.id;
      const img = document.createElement("img");
      img.alt = a.name;
      img.loading = "lazy";
      img.src = "/matrix-anim/" + a.file;
      const name = document.createElement("strong");
      name.textContent = a.name;
      card.append(img, name);
      card.addEventListener("click", () => playMatrixAnim(a.id, card));
      box.appendChild(card);
    }
  } catch (e) {
    box.textContent = "Не удалось загрузить анимации";
    animsLoaded = false;
  }
}

async function playMatrixAnim(id, card) {
  // Анимации запускаются сразу; длительность не выбираем — 5 минут по умолчанию.
  try {
    await postJSON("/api/matrix/animation", { id, duration: 0 });
    const box = $("matrix-anims");
    if (box) box.querySelectorAll(".anim-card").forEach((c) => c.classList.toggle("active", c === card));
    setText("matrix-engine", "движок: анимация");
  } catch (e) {
    setText("matrix-engine", "Ошибка: " + (e && e.message ? e.message : "запрос"));
  }
}

function matrixHeart() {
  const rows = [
    "01100000110",
    "11110001111",
    "11111111111",
    "11111111111",
    "01111111110",
    "00111111100",
    "00011111000",
    "00001110000",
    "00000100000",
  ];
  const offX = Math.floor((MW - rows[0].length) / 2), offY = Math.floor((MH - rows.length) / 2);
  matrixPixels = new Array(MW * MH).fill(null);
  rows.forEach((row, y) => {
    for (let x = 0; x < row.length; x++) {
      if (row[x] === "1") matrixPixels[(offY + y) * MW + offX + x] = [255, 255, 255];
    }
  });
  drawMatrix();
}

function matrixPattern() {
  matrixPixels = new Array(MW * MH).fill(null);
  for (let y = 0; y < MH; y++) {
    for (let x = 0; x < MW; x++) {
      let v;
      if (y < 8) {
        v = Math.round((x / (MW - 1)) * 255);
      } else {
        v = (x + y) % 2 === 0 ? 255 : 0;
      }
      matrixPixels[y * MW + x] = [v, v, v];
    }
  }
  drawMatrix();
}

function matrixB64() {
  const buf = new Uint8Array(MW * MH * 3);
  for (let i = 0; i < MW * MH; i++) {
    const px = matrixPixels[i];
    if (!px) continue;
    buf[i * 3] = px[2];
    buf[i * 3 + 1] = px[1];
    buf[i * 3 + 2] = px[0];
  }
  let s = "";
  for (let i = 0; i < buf.length; i++) s += String.fromCharCode(buf[i]);
  return btoa(s);
}

function matrixState(text) { setText("matrix-state", text); }

async function matrixRefreshEngine() {
  try {
    const r = await api("/api/glagol/status");
    const s = await r.json();
    setText("matrix-engine", s.ready ? "движок: Glagol" : "движок: fb4");
    const el = $("matrix-engine");
    if (el) el.title = s.ready ? "Вендорный API станции" : (s.error || "Локальная отрисовка");
  } catch (e) {}
}

async function matrixRefreshStatus() {
  try {
    const r = await api("/api/matrix/status");
    const s = await r.json();
    matrixState("режим: " + (s.mode || "—") + (s.label ? " · " + s.label : ""));
  } catch (e) {}
}

async function matrixApplyBrightness() {
  try {
    await postJSON("/api/matrix/brightness", { value: Number($("matrix-display-brightness").value) });
  } catch (e) {}
}

function matrixDuration() {
  return Number($("matrix-duration").value) || 60;
}

function bytesToBase64(bytes) {
  let out = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    out += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
  }
  return btoa(out);
}

function seekVideo(video, t) {
  return new Promise((resolve, reject) => {
    if (Math.abs(video.currentTime - t) < 0.001 && video.readyState >= 2) {
      resolve();
      return;
    }
    const timer = setTimeout(() => {
      video.onseeked = null;
      resolve();
    }, 1500);
    video.onseeked = () => {
      clearTimeout(timer);
      video.onseeked = null;
      resolve();
    };
    video.onerror = () => {
      clearTimeout(timer);
      reject(new Error("ошибка декодирования видео"));
    };
    try {
      video.currentTime = t;
    } catch (e) {
      clearTimeout(timer);
      reject(e);
    }
  });
}

async function captureVideoFrames(file) {
  const url = URL.createObjectURL(file);
  const video = document.createElement("video");
  video.src = url;
  video.muted = true;
  video.playsInline = true;
  video.preload = "auto";
  try {
    await new Promise((resolve, reject) => {
      video.onloadedmetadata = () => resolve();
      video.onerror = () => reject(new Error("браузер не смог открыть видео"));
    });
    const limit = Math.min(Number.isFinite(video.duration) ? video.duration : 10, 10);
    if (!(limit > 0.05)) throw new Error("не удалось определить длительность");
    const fps = 12;
    const W = 25, H = 16;
    const canvas = document.createElement("canvas");
    canvas.width = W;
    canvas.height = H;
    const ctx = canvas.getContext("2d", { willReadFrequently: true });
    const frames = [];
    const total = Math.min(120, Math.max(2, Math.ceil(limit * fps)));
    for (let n = 0; n < total; n++) {
      const t = Math.min(n / fps, Math.max(0, limit - 0.001));
      await seekVideo(video, t);
      ctx.fillStyle = "#000";
      ctx.fillRect(0, 0, W, H);
      const vw = video.videoWidth || 16;
      const vh = video.videoHeight || 9;
      const scale = Math.min(W / vw, H / vh);
      const dw = Math.max(1, Math.round(vw * scale));
      const dh = Math.max(1, Math.round(vh * scale));
      ctx.drawImage(video, Math.floor((W - dw) / 2), Math.floor((H - dh) / 2), dw, dh);
      const img = ctx.getImageData(0, 0, W, H).data;
      const rgb = new Uint8Array(W * H * 3);
      for (let i = 0, j = 0; i < img.length; i += 4, j += 3) {
        rgb[j] = img[i];
        rgb[j + 1] = img[i + 1];
        rgb[j + 2] = img[i + 2];
      }
      frames.push(rgb);
    }
    const all = new Uint8Array(frames.length * W * H * 3);
    frames.forEach((f, i) => all.set(f, i * W * H * 3));
    return { frames: all, fps, duration: limit };
  } finally {
    URL.revokeObjectURL(url);
    video.removeAttribute("src");
    try { video.load(); } catch (e) {}
  }
}

async function matrixShowVideo() {
  const file = $("matrix-video").files[0];
  if (!file) {
    matrixState("выберите видео");
    return;
  }
  matrixState("разбираю видео на кадры…");
  try {
    const cap = await captureVideoFrames(file);
    const count = cap.frames.length / (25 * 16 * 3);
    matrixState("отправляю " + count + " кадров…");
    const j = await postJSON("/api/matrix/video", {
      frames: bytesToBase64(cap.frames),
      fps: cap.fps,
      duration: matrixDuration(),
    });
    matrixState(j.ok ? "видео запущено (" + (j.engine === "glagol" ? "Glagol" : "fb4") + ", " + j.frames + " кадров)" : "ошибка: " + (j.error || ""));
    matrixRefreshEngine();
  } catch (e) {
    matrixState("ошибка: " + e.message);
  }
}

async function matrixShow() {
  if (matrixTab === "anim") return;
  await matrixApplyBrightness();
  const duration = matrixDuration();
  if (matrixTab === "draw") {
    matrixState("отправляю рисунок…");
    try {
      const j = await postJSON("/api/matrix/frame", { pixels: matrixB64(), duration });
      matrixState(j.engine === "glagol" ? "рисунок отправлен (Glagol)" : "рисунок отправлен");
      matrixRefreshEngine();
    } catch (e) { matrixState("ошибка: " + e.message); }
    return;
  }
  if (matrixTab === "text") {
    const text = $("matrix-text").value.trim();
    if (!text) { matrixState("введите текст"); return; }
    matrixState("отправляю текст…");
    try {
      const j = await postJSON("/api/matrix/text", {
        text,
        color: "#ffffff",
        speed: Number($("matrix-speed").value),
        duration,
      });
      matrixState((j.engine === "glagol" ? "текст отправлен (Glagol): " : "текст отправлен: ") + text);
      matrixRefreshEngine();
    } catch (e) { matrixState("ошибка: " + e.message); }
    return;
  }
  if (matrixTab === "gif") {
    const file = $("matrix-gif").files[0];
    if (!file) { matrixState("выберите GIF"); return; }
    matrixState("загружаю GIF…");
    try {
      const fd = new FormData();
      fd.append("file", file);
      fd.append("duration", String(duration));
      const r = await api("/api/matrix/gif", { method: "POST", body: fd });
      const j = await r.json();
      matrixState(j.ok ? (j.engine === "glagol" ? "GIF запущен (Glagol)" : "GIF запущен: " + j.frames + " кадров") : "ошибка: " + (j.error || ""));
      matrixRefreshEngine();
    } catch (e) { matrixState("ошибка: " + e.message); }
    return;
  }
  if (matrixTab === "video") {
    await matrixShowVideo();
    return;
  }
  matrixState("неизвестная вкладка");
}

$("matrix-send").addEventListener("click", matrixShow);

$("matrix-clear").addEventListener("click", async () => {
  matrixPixels = new Array(MW * MH).fill(null);
  drawMatrix();
  try {
    await postJSON("/api/matrix/clear");
    matrixState("матрица очищена");
  } catch (e) { matrixState("ошибка: " + e.message); }
});

$("matrix-heart").addEventListener("click", matrixHeart);
$("matrix-pattern").addEventListener("click", matrixPattern);

$("matrix-stop").addEventListener("click", async () => {
  matrixPixels = new Array(MW * MH).fill(null);
  drawMatrix();
  try {
    await postJSON("/api/matrix/clear");
    matrixState("остановлено и очищено");
  } catch (e) { matrixState("ошибка: " + e.message); }
});

$("matrix-native").addEventListener("click", async () => {
  try {
    const j = await postJSON("/api/matrix/native");
    matrixState(j.clock ? "часы возвращены" : "матрица под штатным управлением станции");
  } catch (e) { matrixState("ошибка: " + e.message); }
});

$("matrix-read").addEventListener("click", async () => {
  matrixState("читаю кадр…");
  try {
    const r = await api("/api/matrix/frame");
    const j = await r.json();
    const raw = atob(j.pixels || "");
    if (raw.length !== MW * MH * 3) throw new Error("bad frame");
    matrixPixels = new Array(MW * MH).fill(null);
    for (let i = 0; i < MW * MH; i++) {
      const v = raw.charCodeAt(i * 3);
      if (v > 0) matrixPixels[i] = [v, v, v];
    }
    drawMatrix();
    matrixState("кадр считан");
  } catch (e) { matrixState("ошибка: " + e.message); }
});

let matrixLiveTimer = null;

function matrixLiveTick() {
  const img = $("matrix-live-img");
  if (img) img.src = "/api/matrix/live?t=" + Date.now();
}

function startMatrixLive() {
  if (!liveViewEnabled) return;
  stopMatrixLive();
  matrixLiveTick();
  matrixLiveTimer = setInterval(matrixLiveTick, 300);
}

function stopMatrixLive() {
  if (matrixLiveTimer) {
    clearInterval(matrixLiveTimer);
    matrixLiveTimer = null;
  }
}

function openMatrix() {
  drawMatrix();
  matrixRefreshStatus();
  matrixRefreshEngine();
  syncMatrixTab();
  $("matrix-dialog").showModal();
  startMatrixLive();
}
["matrix-open", "matrix-open-light"].forEach((id) => {
  const el = $(id);
  if (el) el.addEventListener("click", openMatrix);
});
$("matrix-close").addEventListener("click", () => $("matrix-dialog").close());
$("matrix-dialog").addEventListener("close", stopMatrixLive);

const stationMatrixCanvas = $("station-matrix-live");
const stationMatrixCtx = stationMatrixCanvas ? stationMatrixCanvas.getContext("2d") : null;
const SMW = 25, SMH = 16, SMCELL = 20;

function drawStationMatrix(raw) {
  if (!stationMatrixCtx) return;
  const ctx = stationMatrixCtx;
  const w = stationMatrixCanvas.width, h = stationMatrixCanvas.height;
  ctx.clearRect(0, 0, w, h);
  for (let y = 0; y < SMH; y++) {
    for (let x = 0; x < SMW; x++) {
      const v = raw.length > (y * SMW + x) * 3 ? raw.charCodeAt((y * SMW + x) * 3) : 0;
      if (v <= 0) continue;
      const cx = x * SMCELL + SMCELL / 2;
      const cy = y * SMCELL + SMCELL / 2;
      ctx.beginPath();
      ctx.arc(cx, cy, 6, 0, Math.PI * 2);
      ctx.fillStyle = "rgba(255,255,255," + (0.15 + 0.85 * v / 255) + ")";
      ctx.fill();
    }
  }
}

let lastStationRaw = "";
let liveStream = null;

function handleStationRaw(raw) {
  if (!raw || raw === lastStationRaw) return;
  lastStationRaw = raw;
  drawStationMatrix(raw);
}

// Один SSE-поток на матрицу и кольцо: меньше соединений к станции.
function startStationStream() {
  if (!liveViewEnabled) return;
  if (liveStream && liveStream.readyState !== EventSource.CLOSED) return;
  if (liveStream) {
    try { liveStream.close(); } catch (e) {}
    liveStream = null;
  }
  lastStationRaw = "";
  liveStream = new EventSource("/api/live/stream");
  liveStream.onmessage = (ev) => {
    try {
      const data = JSON.parse(ev.data || "{}");
      if (data.m) handleStationRaw(atob(data.m));
      if (data.r) handleRingLive(atob(data.r));
    } catch (e) {}
  };
  liveStream.onerror = () => {};
}

function stopStationStream() {
  if (liveStream) {
    liveStream.close();
    liveStream = null;
  }
}

document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    stopStationStream();
    return;
  }
  if (liveViewEnabled) startStationStream();
});

/* ---------- live ring on the station image ---------- */
// Кольцо лежит вокруг верхней площадки; ring1 — на лицевой стороне,
// дальше против часовой стрелки.
const RING_GEO = { cx: 626, cy: 176, rx: 340, ry: 80, width: 40, n: 24, start: Math.PI / 2, dir: -1 };
const ringLiveCanvas = $("ring-live");
let ringLiveLast = "";

function drawRingLive(raw) {
  if (!ringLiveCanvas) return;
  const ctx = ringLiveCanvas.getContext("2d");
  const w = ringLiveCanvas.width, h = ringLiveCanvas.height;
  ctx.clearRect(0, 0, w, h);
  if (!raw || raw.length < 72) return;
  const step = (Math.PI * 2) / RING_GEO.n;
  const half = RING_GEO.width / 2;
  // Вендор на максимуме громкости зажигает 23 сегмента и оставляет ring1
  // погашенным — замыкаем круг, чтобы он выглядел целым.
  const segs = [];
  for (let i = 0; i < RING_GEO.n; i++) {
    segs.push([raw.charCodeAt(i * 3), raw.charCodeAt(i * 3 + 1), raw.charCodeAt(i * 3 + 2)]);
  }
  const isLit = (c) => c[0] > 2 || c[1] > 2 || c[2] > 2;
  const litCount = segs.filter(isLit).length;
  if (litCount === RING_GEO.n - 1 && !isLit(segs[0])) {
    segs[0] = segs[1].slice();
  }
  for (let i = 0; i < RING_GEO.n; i++) {
    const r = segs[i][0], g = segs[i][1], b = segs[i][2];
    const v = Math.max(r, g, b);
    if (v <= 2) continue;
    // нахлёст в 6° — соседние сегменты стыкуются без шва
    const a0 = RING_GEO.start + RING_GEO.dir * (i * step - step * 0.06);
    const a1 = RING_GEO.start + RING_GEO.dir * ((i + 1) * step + step * 0.06);
    const from = Math.min(a0, a1), to = Math.max(a0, a1);
    const color = "rgb(" + r + "," + g + "," + b + ")";
    for (let pass = 0; pass < 3; pass++) {
      ctx.beginPath();
      ctx.ellipse(RING_GEO.cx, RING_GEO.cy, RING_GEO.rx, RING_GEO.ry, 0, from, to);
      ctx.lineCap = "butt";
      ctx.strokeStyle = color;
      if (pass === 0) {
        ctx.globalCompositeOperation = "lighter";
        ctx.globalAlpha = 0.16 + 0.30 * (v / 255);
        ctx.lineWidth = half * 1.9;
        ctx.shadowBlur = 52;
        ctx.shadowColor = color;
      } else if (pass === 1) {
        ctx.globalCompositeOperation = "lighter";
        ctx.globalAlpha = 0.30 + 0.45 * (v / 255);
        ctx.lineWidth = half * 0.9;
        ctx.shadowBlur = 26;
        ctx.shadowColor = color;
      } else {
        ctx.globalCompositeOperation = "source-over";
        ctx.globalAlpha = 0.55 + 0.45 * (v / 255);
        ctx.lineWidth = half * 0.42;
        ctx.shadowBlur = 0;
      }
      ctx.stroke();
    }
  }
  ctx.globalAlpha = 1;
  ctx.globalCompositeOperation = "source-over";
  ctx.shadowBlur = 0;
}

let ringLivePending = null;
let ringLiveRaf = 0;

function handleRingLive(raw) {
  if (!raw) return;
  ringLivePending = raw;
  if (ringLiveRaf) return;
  ringLiveRaf = requestAnimationFrame(() => {
    ringLiveRaf = 0;
    const next = ringLivePending;
    ringLivePending = null;
    if (!next || next === ringLiveLast) return;
    ringLiveLast = next;
    drawRingLive(next);
  });
}

function startRingStream() {
  startStationStream();
}

function stopRingStream() {
  stopStationStream();
}

function setLiveView(on, persist) {
  liveViewEnabled = !!on;
  if (persist !== false) localStorage.setItem("sw-liveview", liveViewEnabled ? "1" : "0");
  const btn = $("liveview-toggle");
  if (btn) {
    btn.setAttribute("aria-checked", liveViewEnabled ? "true" : "false");
    btn.dataset.on = liveViewEnabled ? "1" : "0";
  }
  setText("liveview-state", liveViewEnabled ? "включён" : "выключен");
  if (liveViewEnabled) {
    stopStationStream();
    startStationStream();
    startRingStream();
    if ($("matrix-dialog") && $("matrix-dialog").open) startMatrixLive();
  } else {
    stopStationStream();
    stopRingStream();
    stopMatrixLive();
    if (stationMatrixCtx) stationMatrixCtx.clearRect(0, 0, stationMatrixCanvas.width, stationMatrixCanvas.height);
    if (ringLiveCanvas) ringLiveCanvas.getContext("2d").clearRect(0, 0, ringLiveCanvas.width, ringLiveCanvas.height);
    ringLiveLast = "";
    ringLivePending = null;
  }
  if ($("screen-dialog") && $("screen-dialog").open) {
    if (liveViewEnabled) screenStartLive();
    else screenSnapshot();
  }
}

$("liveview-toggle").addEventListener("click", () => setLiveView(!liveViewEnabled));
setLiveView(liveViewEnabled, false);

document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    stopRingStream();
    return;
  }
  if (liveViewEnabled) startRingStream();
});

let mediaDuration = 0;
let mediaProgressBase = 0;
let mediaProgressAt = 0;
let mediaPlaying = false;
let mediaSeeking = false;

let lastMedia = {};
let mediaSource = "glagol";

function renderMedia(media) {
  media = media || {};
  lastMedia = media;
  const ap = airplayState || {};
  const apSession = !!ap.session;
  const apPlaying = !!ap.playing;
  // Начался AirPlay — управление автоматически переходит к нему.
  if (apPlaying) mediaSource = "airplay";
  if (!apSession && mediaSource === "airplay") mediaSource = "glagol";
  const airplayOn = mediaSource === "airplay" && apSession;
  // После остановки штатного плеера Glagol ещё какое-то время отдаёт
  // заголовок нашего же WAV-потока. Не показываем его как трек Я.Музыки.
  const staleNative = !airplayOn && media.title === "AirPlay" &&
    (!media.artist || media.artist === "Native visualizer");
  const m = staleNative ? {} : media;

  const view = airplayOn
    ? {
        title: ap.title || "AirPlay",
        artist: ap.artist || "",
        album: ap.album || "",
        cover: ap.cover || "",
        playing: apPlaying,
        source: ap.source || "",
      }
    : {
        title: m.title || "",
        artist: m.artist || "",
        album: "",
        cover: m.cover || "",
        playing: !!m.playing,
        source: "",
      };

  const srcBox = $("media-source");
  if (srcBox) {
    srcBox.hidden = !(apSession || m.title);
    srcBox.querySelectorAll("button").forEach((b) => {
      b.classList.toggle("active", b.dataset.source === mediaSource);
      if (b.dataset.source === "airplay") b.disabled = !apSession;
    });
  }

  const art = $("music-art");
  if (art) {
    if (view.cover) {
      let img = art.querySelector("img");
      if (!img) {
        art.textContent = "";
        img = document.createElement("img");
        img.alt = "";
        art.appendChild(img);
      }
      if (img.getAttribute("src") !== view.cover) img.src = view.cover;
    } else {
      const img = art.querySelector("img");
      if (img) img.remove();
      art.textContent = "♪";
    }
  }
  setText("track-title", view.title || "Активная сессия не найдена");
  let sub = view.artist || "";
  if (!sub && view.album && view.album !== view.title) sub = view.album;
  if (!sub && airplayOn) sub = view.source || "AirPlay";
  if (!sub && m.type === "FmRadio") sub = "Радио";
  if (!sub) sub = view.playing ? "играет" : "управление через Glagol";
  setText("track-artists", sub);
  const toggleBtn = $("track-toggle");
  if (toggleBtn) {
    toggleBtn.textContent = view.playing ? "⏸" : "▶";
    toggleBtn.title = view.playing ? "Пауза" : "Воспроизвести";
  }
  if (airplayOn) {
    setText("media-state", view.playing ? "Играет: AirPlay" : "Пауза: AirPlay");
    const src = view.source && view.source !== "AirPlay" ? " · " + view.source : "";
    setText("media-detail", "AirPlay" + src);
  } else if (m.title) {
    let state = m.playing ? "играет" : "пауза";
    if (m.duration > 0) state += " · " + fmtTime(m.progress) + " / " + fmtTime(m.duration);
    setText("media-detail", state);
    setText("media-state", m.playing ? "Играет: Я.Музыка" : "Пауза: Я.Музыка");
  } else {
    setText("media-state", "локальное управление");
  }
  if (m.alice && m.alice !== "IDLE") {
    const amap = { LISTENING: "Алиса слушает", THINKING: "Алиса думает", SPEAKING: "Алиса говорит" };
    setText("media-state", amap[m.alice] || "Алиса: " + m.alice);
  }

  mediaDuration = airplayOn ? 0 : Math.max(0, Math.floor(Number(m.duration) || 0));
  mediaProgressBase = airplayOn ? 0 : Math.max(0, Math.floor(Number(m.progress) || 0));
  mediaProgressAt = Date.now();
  mediaPlaying = airplayOn ? view.playing : !!m.playing;
  const progressBlock = $("music-progress-block");
  const progressRange = $("music-progress");
  if (progressBlock && progressRange) {
    if (!airplayOn && mediaDuration > 0 && m.title) {
      progressBlock.hidden = false;
      progressRange.max = String(mediaDuration);
      if (!mediaSeeking) {
        progressRange.value = String(Math.min(mediaProgressBase, mediaDuration));
        setText("music-time-current", fmtTime(mediaProgressBase));
        setText("music-time-total", fmtTime(mediaDuration));
      }
    } else {
      progressBlock.hidden = true;
      if (!mediaSeeking) mediaDuration = 0;
    }
  }
}

async function mediaPoll(times) {
  const n = times || 6;
  for (let i = 0; i < n; i++) {
    await new Promise((r) => setTimeout(r, 500));
    try {
      const r = await api("/api/media");
      renderMedia(await r.json());
    } catch (e) {}
  }
}

function fmtTime(sec) {
  sec = Number(sec) || 0;
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  return m + ":" + String(s).padStart(2, "0");
}

// Пока идёт трек, двигаем ползунок локально между опросами /api/media.
setInterval(() => {
  if (mediaSeeking || !mediaPlaying || mediaDuration <= 0) return;
  const range = $("music-progress");
  if (!range || range.closest("[hidden]")) return;
  const pos = Math.min(mediaDuration, mediaProgressBase + (Date.now() - mediaProgressAt) / 1000);
  range.value = String(Math.floor(pos));
  setText("music-time-current", fmtTime(pos));
}, 500);

const seekRange = $("music-progress");
if (seekRange) {
  seekRange.addEventListener("pointerdown", () => {
    mediaSeeking = true;
  });
  seekRange.addEventListener("input", () => {
    mediaSeeking = true;
    setText("music-time-current", fmtTime(Number(seekRange.value) || 0));
  });
  seekRange.addEventListener("pointercancel", () => {
    mediaSeeking = false;
  });
  seekRange.addEventListener("change", async () => {
    const pos = Math.max(0, Math.round(Number(seekRange.value) || 0));
    mediaSeeking = false;
    try {
      await postJSON("/api/media/seek", { position: pos });
      mediaPoll(4);
    } catch (e) {}
  });
}

let airplayState = null;
let airplayPollTick = 0;

function setViz(enabled, active, style, label, err) {
  const nativeMode = !!label;
  let text = "выключена";
  if (enabled) {
    if (nativeMode) text = active ? "штатный рендерер · матрица" : "включена · штатный рендерер";
    else text = active ? "работает · матрица" : "включена";
    if (err) text += " · ошибка";
  }
  setText("viz-state", text);
  const modeEl = $("viz-mode");
  if (modeEl) {
    modeEl.hidden = !nativeMode;
    modeEl.title = "Режим выбирается в приложении «Умный Дом»";
    if (nativeMode) modeEl.textContent = label;
  }
}

function renderAirplay(a) {
  a = a || {};
  airplayState = a;
  const sw = $("airplay-toggle");
  if (!sw) return;
  const on = !!a.enabled || !!a.running;
  sw.setAttribute("aria-checked", on ? "true" : "false");
  sw.dataset.on = on ? "1" : "0";
  let state = "выключен";
  if (!a.installed) state = "приложение не установлено";
  else if (a.error) state = "нет ответа";
  else if (a.running) {
    const map = { playing: "играет", paused: "пауза", idle: "готов" };
    state = map[a.state] || "готов";
    state += a.source && a.source !== "AirPlay" ? " · " + a.source : "";
  }
  setText("airplay-state", state);
  setViz(!!a.viz_enabled, !!a.viz_active, a.viz_style, a.native ? (a.native_label || "режим из Умного Дома") : "", a.native_error || "");
  const play = $("airplay-play");
  if (play) {
    const active = a.running && (a.state === "playing" || a.state === "paused" || a.title);
    play.hidden = !active;
    play.textContent = a.state === "playing" ? "⏸" : "▶";
    play.title = a.state === "playing" ? "Пауза" : "Продолжить";
  }
  renderMedia(lastMedia);
}

async function refreshAirplay() {
  try {
    const r = await api("/api/airplay", { cache: "no-store" });
    renderAirplay(await r.json());
  } catch (e) {}
}

const airplayToggle = $("airplay-toggle");
if (airplayToggle) {
  airplayToggle.addEventListener("click", async () => {
    const on = airplayToggle.dataset.on === "1";
    airplayToggle.disabled = true;
    setText("airplay-state", on ? "выключаю…" : "включаю…");
    try {
      renderAirplay(await postJSON("/api/airplay", { on: !on }));
    } catch (e) {
      setText("airplay-state", "ошибка");
    } finally {
      airplayToggle.disabled = false;
    }
  });
}

async function openAirplayName() {
  const dialog = $("airplay-name-dialog");
  if (!dialog) return;
  setText("airplay-name-state", "Загружаю…");
  if (!dialog.open) dialog.showModal();
  try {
    const r = await api("/api/airplay/name", { cache: "no-store" });
    const j = await r.json();
    if (!r.ok) {
      setText("airplay-name-state", "Ошибка: " + (j.error || r.status));
      return;
    }
    const mode = j.mode === "custom" ? "custom" : "smart";
    document.querySelectorAll('input[name="airplay-name-mode"]').forEach((x) => {
      x.checked = x.value === mode;
    });
    $("airplay-custom-name").value = j.custom || "";
    const display = (j.smart && j.smart.display) || j.current || "—";
    $("airplay-smart-name").textContent = display;
    const full = (j.smart && j.smart.full) || "";
    const room = (j.smart && j.smart.room) || "";
    setText("airplay-smart-full", full && full !== display ? "В Умном доме: " + full : "");
    setText("airplay-smart-note", room
      ? "Имя и комната из Умного дома; бренд убран, чтобы влезло в список AirPlay."
      : "Комната в Умном доме не назначена — будет только имя.");
    setText("airplay-current-name", j.current || "—");
    setText("airplay-video-name", j.video || "—");
    setText("airplay-name-state", j.error ? "Умный дом: " + j.error : "—");
    syncAirplayNameMode();
  } catch (e) {
    setText("airplay-name-state", "Ошибка: " + (e && e.message ? e.message : "запрос"));
  }
}

function syncAirplayNameMode() {
  const mode = document.querySelector('input[name="airplay-name-mode"]:checked')?.value;
  const input = $("airplay-custom-name");
  if (input) input.disabled = mode !== "custom";
}

async function saveAirplayName() {
  const mode = document.querySelector('input[name="airplay-name-mode"]:checked')?.value || "smart";
  const custom = ($("airplay-custom-name").value || "").trim();
  setText("airplay-name-state", "Сохраняю…");
  try {
    const r = await api("/api/airplay/name", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode, custom }),
    });
    const j = await r.json();
    if (!r.ok) {
      setText("airplay-name-state", "Ошибка: " + (j.error || r.status));
      return;
    }
    setText("airplay-name-state", "Сохранено: " + (j.current || ""));
    await refreshAirplay();
    setTimeout(() => $("airplay-name-dialog").close(), 400);
  } catch (e) {
    setText("airplay-name-state", "Ошибка: " + (e && e.message ? e.message : "запрос"));
  }
}

if ($("airplay-name-open")) $("airplay-name-open").addEventListener("click", openAirplayName);
if ($("airplay-name-close")) $("airplay-name-close").addEventListener("click", () => $("airplay-name-dialog").close());
if ($("airplay-name-cancel")) $("airplay-name-cancel").addEventListener("click", () => $("airplay-name-dialog").close());
if ($("airplay-name-save")) $("airplay-name-save").addEventListener("click", saveAirplayName);
document.querySelectorAll('input[name="airplay-name-mode"]').forEach((x) => x.addEventListener("change", syncAirplayNameMode));

const airplayPlay = $("airplay-play");
if (airplayPlay) {  airplayPlay.addEventListener("click", async () => {
    try {
      renderAirplay(await postJSON("/api/airplay/action", { action: "play_pause" }));
      setTimeout(refreshAirplay, 900);
    } catch (e) {}
  });
}

document.querySelectorAll("#media-source button").forEach((b) => {
  b.addEventListener("click", () => {
    if (b.dataset.source === "airplay" && !(airplayState && airplayState.session)) return;
    mediaSource = b.dataset.source;
    renderMedia(lastMedia);
  });
});

document.querySelectorAll(".music-controls button").forEach((b) => {
  b.addEventListener("click", async () => {
    const ap = airplayState || {};
    if (mediaSource === "airplay" && ap.session) {
      const action = b.id === "track-next" ? "next" : b.id === "track-previous" ? "prev" : "play_pause";
      try {
        await postJSON("/api/airplay/action", { action });
        setTimeout(refreshAirplay, 900);
      } catch (e) {}
      return;
    }
    const map = { "track-previous": "prev", "track-toggle": "play_pause", "track-next": "next" };
    try {
      await postJSON("/api/media/action", { action: map[b.id] });
      await refresh();
      mediaPoll(6);
    } catch (e) {}
  });
});

async function aliceSend() {
  const input = $("alice-text");
  const text = (input.value || "").trim();
  if (!text) return;
  const btn = $("alice-send");
  btn.disabled = true;
  setText("media-state", "отправляю Алисе…");
  try {
    await postJSON("/api/alice/text", { text: text });
    input.value = "";
    setText("media-state", "отправлено Алисе");
    mediaPoll(8);
  } catch (e) {
    setText("media-state", "Алиса недоступна");
  } finally {
    btn.disabled = false;
  }
}
$("alice-send").addEventListener("click", aliceSend);
$("alice-text").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    aliceSend();
  }
});

$("media-volume").addEventListener("change", async () => {
  try {
    await postJSON("/api/volume", { level: Number($("media-volume").value) });
    await refresh();
  } catch (e) {}
});

async function toggleBluetooth() {
  const on = $("bluetooth-toggle").dataset.on === "1";
  try {
    await postJSON("/api/bluetooth", { on: !on });
    await refresh();
  } catch (e) {}
}
$("bluetooth-toggle").addEventListener("click", toggleBluetooth);
$("bluetooth-toggle-2").addEventListener("click", toggleBluetooth);

/* ---------- Bluetooth devices ---------- */

let btTimer = null;

function btTypeLabel(t) {
  if (t === 2) return "LE";
  if (t === 1) return "BR/EDR";
  if (t === 3) return "dual";
  return "—";
}

function btDeviceRow(d) {
  const row = document.createElement("div");
  row.className = "bt-row";
  const main = document.createElement("div");
  main.className = "bt-main";
  const name = document.createElement("strong");
  name.textContent = d.name || d.mac;
  const sub = document.createElement("small");
  sub.textContent = [d.mac, btTypeLabel(d.type)].join(" · ");
  main.append(name, sub);
  row.appendChild(main);

  const status = document.createElement("span");
  status.className = "bt-status" + (d.connected ? " on" : "");
  status.textContent = d.connected ? "подключено" : "не подключено";
  row.appendChild(status);
  return row;
}

function btRender(j) {
  const info = j.info || {};
  setText("bt-modal-name", info.name || "Bluetooth");
  setText("bt-modal-sub", info.address || "—");
  setText("bt-modal-enabled", info.enabled ? "включён" : "выключен");

  const box = $("bt-list");
  box.innerHTML = "";
  const bonded = Array.isArray(info.devices) ? info.devices : [];
  if (bonded.length) {
    const h = document.createElement("div");
    h.className = "bt-section";
    h.textContent = "Сопряжённые";
    box.appendChild(h);
    bonded.forEach((d) => box.appendChild(btDeviceRow(d)));
  } else {
    const p = document.createElement("p");
    p.className = "note";
    p.textContent = "Сопряжённых устройств нет. Сопряжение — на экране станции.";
    box.appendChild(p);
  }
  setText("bt-modal-state", info.error ? "helper: " + info.error : "—");
}

async function btRefresh() {
  try {
    const r = await api("/api/bluetooth");
    btRender(await r.json());
  } catch (e) {}
}

function btOpen() {
  $("bt-dialog").showModal();
  btRefresh();
  if (btTimer) clearInterval(btTimer);
  btTimer = setInterval(() => {
    if (!document.hidden) btRefresh();
  }, 1500);
}
function btStopPoll() {
  if (btTimer) clearInterval(btTimer);
  btTimer = null;
}
$("bt-open").addEventListener("click", btOpen);
const btOpenOverview = $("bt-open-overview");
if (btOpenOverview) btOpenOverview.addEventListener("click", btOpen);
$("bt-close").addEventListener("click", () => $("bt-dialog").close());
$("bt-dialog").addEventListener("close", btStopPoll);

$("screen-refresh").addEventListener("click", () => {
  const img = $("screen-img");
  $("screen-placeholder").classList.remove("hidden");
  $("screen-placeholder").textContent = "Снимаю кадр…";
  img.src = "/api/screen?t=" + Date.now();
});
$("screen-img").addEventListener("load", () => {
  $("screen-img").classList.add("loaded");
  $("screen-placeholder").classList.add("hidden");
});
$("screen-img").addEventListener("error", () => {
  $("screen-img").classList.remove("loaded");
  $("screen-placeholder").classList.remove("hidden");
  $("screen-placeholder").textContent = "Не удалось получить снимок экрана";
});

/* ---------- screen + remote ---------- */

const SCREEN_BLANK = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7";
let screenInfo = { w: 1920, h: 1080 };
let screenTouch = null;

async function screenLoadInfo() {
  try {
    const r = await api("/api/screen/info");
    const j = await r.json();
    if (j.width > 0 && j.height > 0) screenInfo = { w: j.width, h: j.height };
  } catch (e) {}
}

function screenSetState(text) { setText("screen-state", text); }

function screenStartLive() {
  const img = $("screen-live-img");
  if (!img) return;
  if (!liveViewEnabled) {
    screenSnapshot();
    return;
  }
  img.src = "/api/screen/live?t=" + Date.now();
  setText("screen-live-state", "live-view");
}

function screenSnapshot() {
  const img = $("screen-live-img");
  if (!img) return;
  img.src = "/api/screen?t=" + Date.now();
  setText("screen-live-state", liveViewEnabled ? "снимок" : "live-view выключен");
}

function screenStopLive() {
  const img = $("screen-live-img");
  if (img) img.src = SCREEN_BLANK;
  setText("screen-live-state", "—");
}

async function openScreen() {
  $("screen-dialog").showModal();
  screenSetState("подключаюсь к экрану…");
  await screenLoadInfo();
  if (liveViewEnabled) screenStartLive();
  else screenSnapshot();
}

$("screen-open").addEventListener("click", openScreen);
const screenOpenOverview = $("screen-open-overview");
if (screenOpenOverview) screenOpenOverview.addEventListener("click", openScreen);
const tvOpen = $("tv-open");
if (tvOpen) tvOpen.addEventListener("click", async () => {
  tvOpen.disabled = true;
  try {
    await postJSON("/api/airplay/tv", {});
  } catch (e) {}
  tvOpen.disabled = false;
});
const apVideo = $("ap-video");
if (apVideo) apVideo.addEventListener("click", async () => {
  apVideo.disabled = true;
  try {
    await postJSON("/api/airplay/video", {});
  } catch (e) {}
  apVideo.disabled = false;
});
const screenCardImg = $("screen-img");
if (screenCardImg) screenCardImg.addEventListener("click", openScreen);
$("screen-close").addEventListener("click", () => $("screen-dialog").close());
$("screen-dialog").addEventListener("close", screenStopLive);
$("screen-snapshot").addEventListener("click", screenSnapshot);

$("screen-live-img").addEventListener("load", () => {
  if (liveViewEnabled) setText("screen-live-state", "live-view");
});
$("screen-live-img").addEventListener("error", () => {
  setText("screen-live-state", "нет сигнала");
});

$("screen-fullscreen").addEventListener("click", () => {
  const v = $("screen-viewport");
  if (v && v.requestFullscreen) v.requestFullscreen();
});
$("screen-fullscreen-exit").addEventListener("click", () => {
  if (document.fullscreenElement) document.exitFullscreen();
});

function screenToDevice(clientX, clientY) {
  const img = $("screen-live-img");
  const rect = img.getBoundingClientRect();
  const x = Math.round(((clientX - rect.left) / Math.max(1, rect.width)) * screenInfo.w);
  const y = Math.round(((clientY - rect.top) / Math.max(1, rect.height)) * screenInfo.h);
  return {
    x: Math.max(0, Math.min(screenInfo.w - 1, x)),
    y: Math.max(0, Math.min(screenInfo.h - 1, y)),
  };
}

const screenImgEl = $("screen-live-img");
screenImgEl.addEventListener("pointerdown", (e) => {
  if (e.button !== 0) return;
  e.preventDefault();
  try { screenImgEl.setPointerCapture(e.pointerId); } catch (err) {}
  screenTouch = { x: e.clientX, y: e.clientY, t: performance.now() };
});
screenImgEl.addEventListener("pointerup", async (e) => {
  if (!screenTouch) return;
  const start = screenTouch;
  screenTouch = null;
  const a = screenToDevice(start.x, start.y);
  const b = screenToDevice(e.clientX, e.clientY);
  const dist = Math.hypot(b.x - a.x, b.y - a.y);
  const duration = Math.max(80, Math.min(800, Math.round(performance.now() - start.t)));
  try {
    if (dist < Math.max(12, screenInfo.w * 0.012)) {
      const res = await postJSON("/api/input/tap", { x: b.x, y: b.y });
      if (res.error) throw new Error(res.error);
      screenSetState("тап " + b.x + "×" + b.y);
    } else {
      const res = await postJSON("/api/input/swipe", { x1: a.x, y1: a.y, x2: b.x, y2: b.y, duration });
      if (res.error) throw new Error(res.error);
      screenSetState("свайп " + duration + " мс");
    }
  } catch (err) {
    screenSetState("ошибка: " + err.message);
  }
});
screenImgEl.addEventListener("pointercancel", () => { screenTouch = null; });

document.querySelectorAll("#screen-dialog [data-key]").forEach((btn) => {
  btn.addEventListener("click", async () => {
    const key = btn.dataset.key;
    try {
      const res = await postJSON("/api/key", { key });
      if (res.error) throw new Error(res.error);
      screenSetState("кнопка: " + key);
      btn.classList.add("pressed");
      setTimeout(() => btn.classList.remove("pressed"), 120);
    } catch (e) {
      screenSetState("ошибка: " + e.message);
    }
  });
});

async function screenSendText() {
  const input = $("screen-text-input");
  if (!input) return;
  const text = input.value.trim();
  if (!text) return;
  try {
    const res = await postJSON("/api/input/text", { text });
    if (res.error) throw new Error(res.error);
    input.value = "";
    screenSetState("текст отправлен");
  } catch (e) {
    screenSetState("ошибка: " + e.message);
  }
}
$("screen-text-send").addEventListener("click", screenSendText);
$("screen-text-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    screenSendText();
  }
});

$("reboot").addEventListener("click", async () => {
  if (!confirm("Перезагрузить станцию?")) return;
  try { await postJSON("/api/reboot"); } catch (e) {}
});

let logMode = "human";

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function renderEvents(items) {
  const box = $("events-view");
  if (!items || !items.length) {
    box.innerHTML = '<p class="note">Событий пока нет.</p>';
    return;
  }
  box.innerHTML = items.map((e) => {
    const dt = new Date(e.time);
    const when = isNaN(dt) ? e.time : dt.toLocaleString("ru-RU", { day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit", second: "2-digit" });
    const lvl = ["warn", "error", "info"].includes(e.level) ? e.level : "ok";
    return '<div class="event ' + lvl + '"><span class="dot"></span><div><div class="title">' + esc(e.title) + '</div>' +
      (e.detail ? '<div class="detail">' + esc(e.detail) + '</div>' : '') + '</div><div class="time">' + esc(when) + '</div></div>';
  }).join("");
}

async function loadEvents() {
  try {
    const src = $("log-source").value;
    const r = await api("/api/journal?src=" + encodeURIComponent(src), { cache: "no-store" });
    renderEvents(await r.json());
  } catch (e) {
    $("events-view").innerHTML = '<p class="note">Ошибка загрузки журнала.</p>';
  }
}

async function loadLogs() {
  const src = $("log-source").value;
  $("log-view").textContent = "Загрузка…";
  try {
    const r = await api("/api/logs?src=" + encodeURIComponent(src));
    $("log-view").textContent = await r.text() || "(пусто)";
  } catch (e) {
    $("log-view").textContent = "Ошибка загрузки журнала";
  }
}

function loadJournal() {
  if (logMode === "human") loadEvents();
  else loadLogs();
}

document.querySelectorAll("#log-modes button").forEach((b) => {
  b.addEventListener("click", () => {
    logMode = b.dataset.logmode;
    document.querySelectorAll("#log-modes button").forEach((x) => x.classList.toggle("active", x === b));
    $("events-view").classList.toggle("hidden", logMode !== "human");
    $("verbose-box").classList.toggle("hidden", logMode !== "verbose");
    loadJournal();
  });
});

$("log-refresh").addEventListener("click", loadJournal);
$("log-source").addEventListener("change", loadJournal);
setInterval(() => {
  const view = $("view-journal");
  if (logMode === "human" && view && !view.classList.contains("hidden")) loadEvents();
}, 8000);

function renderBackups(items) {
  const tbody = $("backup-rows");
  tbody.innerHTML = "";
  if (!items || !items.length) {
    tbody.innerHTML = '<tr><td colspan="4">Копий пока нет</td></tr>';
    setText("backup-count-nav", "06");
    return;
  }
  setText("backup-count-nav", String(items.length));
  items.forEach((it) => {
    const tr = document.createElement("tr");
    const size = (it.size / MB).toFixed(1) + " МБ";
    tr.innerHTML = '<td>' + esc(it.name) + '</td><td>' + size + '</td><td>' + esc(it.time) + '</td>' +
      '<td><button class="text-button" data-restore="' + esc(it.name) + '">восстановить</button> ' +
      '<a href="/api/backups/download?name=' + encodeURIComponent(it.name) + '">скачать</a> ' +
      '<button class="text-button danger-text" data-del="' + esc(it.name) + '">удалить</button></td>';
    tbody.appendChild(tr);
  });
}

async function loadBackups() {
  try {
    const r = await api("/api/backups", { cache: "no-store" });
    renderBackups(await r.json());
  } catch (e) {}
}

$("backup-create").addEventListener("click", async () => {
  $("backup-create").disabled = true;
  try {
    const r = await postJSON("/api/backups");
    renderBackups(r.items || []);
  } catch (e) {}
  $("backup-create").disabled = false;
});

$("access-backup").addEventListener("click", async () => {
  setText("access-backup-state", "создаю…");
  try {
    const r = await postJSON("/api/backups");
    setText("access-backup-state", "готово: " + (r.name || ""));
  } catch (e) {
    setText("access-backup-state", "ошибка");
  }
});

const adbToggle = $("adb-toggle");
if (adbToggle) {
  adbToggle.addEventListener("click", async () => {
    const on = adbToggle.dataset.on !== "1";
    adbToggle.disabled = true;
    setText("adb-toggle-state", on ? "включаю…" : "выключаю…");
    try {
      let j = await postJSON("/api/adb", { enabled: on });
      // adbd стартует не мгновенно: ждём, пока сервис догонит настройку
      for (let i = 0; i < 12; i++) {
        if (!j.allowed || j.running) break;
        await new Promise((r) => setTimeout(r, 600));
        j = await (await api("/api/adb", { cache: "no-store" })).json();
      }
      if (!on) {
        for (let i = 0; i < 8 && j.running; i++) {
          await new Promise((r) => setTimeout(r, 500));
          j = await (await api("/api/adb", { cache: "no-store" })).json();
        }
      }
      adbToggle.dataset.on = j.allowed ? "1" : "0";
      adbToggle.setAttribute("aria-checked", j.allowed ? "true" : "false");
      setText("adb-toggle-state", !j.allowed
        ? "выключен"
        : (j.running ? "включён · сервис запущен" : "включён · сервис запускается"));
      await refresh();
    } catch (e) {
      setText("adb-toggle-state", "ошибка: " + (e && e.message ? e.message : "запрос"));
    } finally {
      adbToggle.disabled = false;
    }
  });
}

let toastTimer = null;
let undoToken = null;

function showToast(text, token) {
  undoToken = token || null;
  setText("toast-text", text);
  $("toast").classList.remove("hidden");
  $("toast-undo").style.display = token ? "" : "none";
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => $("toast").classList.add("hidden"), 20000);
}

$("toast-undo").addEventListener("click", async () => {
  if (!undoToken) return;
  try {
    const r = await postJSON("/api/backups/undo", { token: undoToken });
    if (r.items) renderBackups(r.items);
  } catch (e) {}
  undoToken = null;
  $("toast").classList.add("hidden");
  setText("backup-state", "удаление отменено");
});

$("backup-rows").addEventListener("click", async (ev) => {
  const restore = ev.target.closest("[data-restore]");
  if (restore) {
    const name = restore.dataset.restore;
    const reset = $("restore-reset-creds").checked;
    const extra = reset ? "\n\nЛогин и пароль панели будут сброшены на admin/admin." : "";
    if (!confirm("Восстановить конфиги из «" + name + "»?\nТекущие /odm/etc и adb_keys будут перезаписаны. После этого нужна перезагрузка." + extra)) return;
    setText("backup-state", "восстанавливаю…");
    try {
      const r = await postJSON("/api/backups/restore", { name, reset_credentials: reset });
      if (r.ok) {
        if (r.relogin) {
          setText("backup-state", "восстановлено. Доступ сброшен, войдите заново как admin/admin.");
          showToast("Доступ панели сброшен на admin/admin", null);
          setTimeout(() => { window.location = "/login"; }, 1500);
        } else {
          setText("backup-state", "восстановлено файлов: " + r.files + ". Перезагрузите станцию, чтобы применить конфиги.");
        }
      } else {
        setText("backup-state", "ошибка: " + (r.error || "неизвестно"));
      }
    } catch (e) {
      setText("backup-state", "ошибка: " + e.message);
    }
    return;
  }
  const del = ev.target.closest("[data-del]");
  if (del) {
    const name = del.dataset.del;
    try {
      const r = await postJSON("/api/backups/delete", { name });
      if (r.ok) {
        renderBackups(r.items || []);
        setText("backup-state", "копия удалена");
        showToast("Копия «" + name + "» удалена", r.token);
      }
    } catch (e) {
      setText("backup-state", "ошибка удаления: " + e.message);
    }
  }
});

$("backup-upload").addEventListener("click", () => $("backup-file").click());
$("backup-file").addEventListener("change", async () => {
  const file = $("backup-file").files[0];
  if (!file) return;
  $("backup-file").value = "";
  setText("backup-state", "загружаю " + file.name + "…");
  try {
    const fd = new FormData();
    fd.append("file", file);
    const r = await api("/api/backups/upload", { method: "POST", body: fd });
    const j = await r.json();
    if (j.items) renderBackups(j.items);
    setText("backup-state", j.ok ? "загружено: " + j.name : "ошибка: " + (j.error || ""));
  } catch (e) {
    setText("backup-state", "ошибка загрузки: " + e.message);
  }
});

async function loadFeatures() {
  try {
    const r = await api("/api/features", { cache: "no-store" });
    const items = await r.json();
    const tbody = $("feature-rows");
    tbody.innerHTML = "";
    items.forEach((f) => {
      const tr = document.createElement("tr");
      tr.innerHTML = '<td>' + f.name + '</td><td class="' + (f.ok ? "ok" : "bad") + '">' + f.state + '</td><td>' + (f.ok ? "✓" : "—") + '</td>';
      tbody.appendChild(tr);
    });
  } catch (e) {}
}

$("cred-save").addEventListener("click", async () => {
  const user = $("cred-user").value.trim();
  const cur = $("cred-current").value;
  const pw = $("cred-new").value;
  const confirm = $("cred-confirm").value;
  const state = $("cred-state");
  if (!user) { state.textContent = "укажите логин"; return; }
  if (pw.length < 4) { state.textContent = "пароль минимум 4 символа"; return; }
  if (pw !== confirm) { state.textContent = "пароли не совпадают"; return; }
  state.textContent = "сохраняю…";
  try {
    const r = await postJSON("/api/credentials", { current_password: cur, user, password: pw });
    if (r.ok) {
      state.textContent = "сохранено: " + r.user;
      lastAuthUser = r.user;
      $("cred-current").value = "";
      $("cred-new").value = "";
      $("cred-confirm").value = "";
    } else {
      state.textContent = r.error || "не удалось сохранить";
    }
  } catch (e) {
    state.textContent = "ошибка: " + e.message;
  }
});

async function runCmd() {
  const cmd = $("cmd").value.trim();
  if (!cmd) return;
  $("out").textContent = "…";
  try {
    const r = await postJSON("/api/exec", { cmd });
    $("out").textContent = (r.out || "") + "\n[exit " + r.code + "]";
  } catch (e) {
    $("out").textContent = "Ошибка: " + e.message;
  }
}
$("run").addEventListener("click", runCmd);
$("cmd").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) runCmd();
});

function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); } catch (e) {}
  document.body.removeChild(ta);
}

document.querySelectorAll("[data-copy]").forEach((b) => {
  b.addEventListener("click", () => copyText($(b.dataset.copy).textContent));
});
$("copy-address").addEventListener("click", () => copyText($("device-address").textContent));

$("refresh-button").addEventListener("click", refresh);
setText("port", location.host);
setText("server-address", location.host);

const themeSelect = $("theme-select");

function setAccent(accent, persist = true) {
  const value = accent || "lime";
  document.documentElement.dataset.accent = value;
  document.documentElement.removeAttribute("data-mode");
  localStorage.setItem("sw-accent", value);
  localStorage.removeItem("sw-theme");
  localStorage.removeItem("sw-mode");
  if (themeSelect) themeSelect.value = value;
  document.querySelectorAll(".theme-swatch").forEach((x) => x.classList.toggle("active", x.dataset.accent === value));
  setFavicon();
  // при загрузке страницы не перезаписываем акцент станции: иначе вход
  // с другого браузера молча меняет тему видео-приёмника
  if (persist) postJSON("/api/theme", { accent: value }).catch(() => {});
}

function setFavicon() {
  const accent = getComputedStyle(document.documentElement).getPropertyValue("--accent").trim() || "#b8f36b";
  const svg = "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><rect width='32' height='32' rx='9' fill='#121a1c'/><path d='M9 22V10h14v12M13 16h6' fill='none' stroke='" + accent + "' stroke-width='3'/></svg>";
  let link = document.querySelector('link[rel="icon"]');
  if (!link) {
    link = document.createElement("link");
    link.rel = "icon";
    document.head.appendChild(link);
  }
  link.href = "data:image/svg+xml," + encodeURIComponent(svg);
  let meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) {
    meta = document.createElement("meta");
    meta.name = "theme-color";
    document.head.appendChild(meta);
  }
  meta.content = accent;
}

const legacyTheme = localStorage.getItem("sw-theme");
let savedAccent = localStorage.getItem("sw-accent");
if (!savedAccent && legacyTheme) savedAccent = legacyTheme === "light" ? "lime" : legacyTheme;
setAccent(savedAccent || "lime", false);

if (themeSelect) themeSelect.addEventListener("change", () => setAccent(themeSelect.value));

const themePop = $("theme-pop");
$("theme-button").addEventListener("click", (ev) => {
  ev.stopPropagation();
  themePop.classList.toggle("hidden");
});
document.querySelectorAll(".theme-swatch").forEach((b) => {
  b.addEventListener("click", (ev) => {
    ev.stopPropagation();
    setAccent(b.dataset.accent);
  });
});
document.addEventListener("click", (ev) => {
  if (!themePop.classList.contains("hidden") && !themePop.contains(ev.target)) {
    themePop.classList.add("hidden");
  }
});

updateRingPreview();
refresh();
refreshAirplay();
setInterval(() => {
  if (!document.hidden) refresh();
}, 10000);
setInterval(async () => {
  if (document.hidden) return;
  const overview = document.getElementById("view-overview");
  if (!overview || overview.classList.contains("hidden")) return;
  try {
    const r = await api("/api/media");
    renderMedia(await r.json());
  } catch (e) {}
  if (airplayPollTick++ % 2 === 0) refreshAirplay();
}, 1000);
