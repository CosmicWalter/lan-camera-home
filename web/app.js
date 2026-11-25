import { startLive } from "./live.js";

const icons = {
  lens: '<path d="M4 8V5h3M17 5h3v3M4 16v3h3m10 0h3v-3"/><circle cx="12" cy="12" r="4"/>',
  camera: '<rect x="3" y="5" width="13" height="14" rx="2"/><path d="m16 10 5-3v10l-5-3"/>',
  archive: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18m-12 4h6"/>',
  bell: '<path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9m-8 12a2 2 0 0 0 4 0"/>',
  server: '<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01M11 7.5h6m-6 9h6"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M10 3h4l.6 2.3 2 .9 2.1-1.1 2.8 2.8-1.1 2.1.9 2L23 13v2l-2.7.6-.9 2 1.1 2.1-2.8 2.8-2.1-1.1-2 .9L13 24h-2l-.6-2.7-2-.9-2.1 1.1-2.8-2.8 1.1-2.1-.9-2L1 14v-2l2.7-.6.9-2-1.1-2.1 2.8-2.8 2.1 1.1 2-.9z" transform="translate(2 1) scale(.83)"/>',
  code: '<path d="m8 7-5 5 5 5m8-10 5 5-5 5m-3-13-2 16"/>',
  link: '<path d="M10 13a4 4 0 0 0 6 .5l3-3a4 4 0 0 0-5.7-5.7L12 6m2 5a4 4 0 0 0-6-.5l-3 3a4 4 0 0 0 5.7 5.7L12 18"/>',
  record: '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="3" fill="currentColor"/>',
  grid: '<rect x="4" y="4" width="6" height="6" rx="1"/><rect x="14" y="4" width="6" height="6" rx="1"/><rect x="4" y="14" width="6" height="6" rx="1"/><rect x="14" y="14" width="6" height="6" rx="1"/>',
  square: '<rect x="4" y="4" width="16" height="16" rx="2"/>',
  expand: '<path d="M9 4H4v5m11-5h5v5M4 15v5h5m11-5v5h-5"/>',
  search: '<circle cx="10.5" cy="10.5" r="6"/><path d="m15 15 5 5"/>',
  refresh: '<path d="M20 10a8 8 0 0 0-14-4L3 9m0-5v5h5m-4 5a8 8 0 0 0 14 4l3-3m0 5v-5h-5"/>',
  close: '<path d="m6 6 12 12M6 18 18 6"/>',
  play: '<path d="m8 5 12 7-12 7z"/>',
  pause: '<path d="M9 5v14m6-14v14"/>',
  download: '<path d="M12 3v12m-4-4 4 4 4-4M4 16v4h16v-4"/>',
  motion: '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  add: '<path d="M12 5v14M5 12h14"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M15 5H6a2 2 0 0 0-2 2v9"/>',
};

const $ = id => document.getElementById(id);
const ticks = 90_000;
const hour = 60 * 60 * ticks;
const liveCards = new Map();
const demoCameras = [
  ["front", "前门", "室外"],
  ["living", "客厅", "室内"],
  ["patio", "后院", "室外"],
  ["garage", "车道", "室外"],
].map(([uuid, shortName, description], index) => ({ uuid, shortName, description, streams: { main: { record: index < 3 } } }));
const knownViews = { live: "实时预览", recordings: "录像回放", alerts: "告警事件", devices: "添加摄像头" };
const localDashboard = location.protocol === "http:" && location.hostname === "127.0.0.1";
const streamNames = { main: "主码流", sub: "子码流", ext: "扩展码流" };
let page = "live";
let session;
let snapshot;
let alertRequest;
let recordRequest;
let connectRequest;
let scanRequest;
let profileRequest;
let cameraWriteRequest;
let toastTimer;

function icon(name) {
  return '<svg class="icon" width="24" height="24" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">' +
    icons[name] + '</svg>';
}

function decorate(parent) {
  parent.querySelectorAll("[data-icon]").forEach(node => { node.innerHTML = icon(node.dataset.icon); });
}

function node(tag, cls, text) {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text !== undefined) element.textContent = text;
  return element;
}

function button(label, iconName, handler, text = false) {
  const element = node("button", text ? "secondary" : "icon-button");
  element.type = "button";
  element.setAttribute("aria-label", label);
  element.title = label;
  element.innerHTML = icon(iconName);
  if (text) element.append(label);
  element.addEventListener("click", handler);
  return element;
}

function empty(parent, message) {
  parent.replaceChildren(node("p", "empty", message));
}

function toast(message) {
  clearTimeout(toastTimer);
  $("toast").textContent = message;
  $("toast").hidden = false;
  toastTimer = setTimeout(() => { $("toast").hidden = true; }, 5000);
}

function cameras() {
  return snapshot?.cameras ?? demoCameras;
}

async function request(active, path, { signal, payload } = {}) {
  let response;
  try {
    const auth = { Authorization: "Bearer " + active.token };
    response = await fetch(active.base + path, {
      method: payload === undefined ? "GET" : "POST",
      headers: payload === undefined ? auth : { ...auth, "Content-Type": "application/json" },
      body: payload === undefined ? undefined : JSON.stringify(payload),
      cache: "no-store",
      credentials: "omit",
      redirect: "error",
      signal,
    });
  } catch (error) {
    if (signal?.aborted) throw signal.reason;
    throw new Error("无法访问本地桥接。请检查桥接程序、端口和浏览器的本地网络权限。");
  }
  if (!response.ok) throw new Error((await response.text()).trim() || "请求失败");
  return response;
}

async function data(active, path, signal) {
  return (await request(active, path, { signal })).json();
}

function query(values) {
  return "?" + new URLSearchParams(values);
}

function cameraPath(uuid, stream, resource) {
  return "/api/cameras/" + encodeURIComponent(uuid) + "/" + encodeURIComponent(stream) + "/" + resource;
}

function updateConnection() {
  const connected = Boolean(session);
  $("session-status").classList.toggle("connected", connected);
  $("session-label").textContent = connected ? (snapshot?.setupOffline ? "已配对 · 录像服务待启动" : "本地已连接") : "演示模式";
  $("local-state").textContent = connected ? "桥接已连接" : "等待连接";
  $("reload-cameras").hidden = !connected;
  $("disconnect").hidden = !connected;
  document.querySelectorAll("[data-connect-label]").forEach(label => {
    label.textContent = connected ? "管理连接" : "连接本地设备";
  });
  $("live-subtitle").textContent = connected ? "画面与录像由你家里的 Moonfire NVR 提供。" : "在一个页面查看家里的摄像头。";
  $("camera-count").textContent = String(cameras().length);
  $("record-count").textContent = String(cameras().filter(camera => Object.values(camera.streams).some(stream => stream.record)).length);
}

function closeLive() {
  for (const card of liveCards.values()) card.stop();
  liveCards.clear();
}

function makeCard(camera) {
  const card = node("article", "camera-card");
  const feed = node("div", "feed");
  const badge = node("span", "feed-badge");
  const badgeLabel = node("span");
  badge.append(node("span", "dot"), badgeLabel);
  const message = node("div", "feed-message");
  const glyph = node("span");
  glyph.innerHTML = icon("camera");
  const messageLabel = node("span");
  message.append(glyph, messageLabel);
  const stream = ["sub", "main", "ext"].find(key => camera.streams[key]?.record);
  const isDemo = !session;
  const media = node(isDemo ? "img" : "video");
  let stop;
  if (isDemo) {
    media.src = "./demo/" + camera.uuid + ".webp";
    media.alt = camera.shortName + "的演示画面";
    media.loading = "lazy";
  } else {
    media.muted = true;
    media.playsInline = true;
    media.autoplay = true;
    media.disablePictureInPicture = true;
    media.setAttribute("aria-label", camera.shortName + "的实时视频");
  }
  feed.append(media, badge, message);
  const metadata = node("div", "camera-meta");
  const description = node("div");
  const recording = Object.values(camera.streams).some(source => source.record);
  description.append(node("h3", "", camera.shortName), node("p", "", [
    isDemo ? camera.description : streamNames[stream],
    recording ? "已启用录像" : "未启用录像",
  ].filter(Boolean).join(" · ")));
  const actions = node("div", "card-actions");
  const archive = button(camera.shortName + "录像", "archive", () => {
    fillRecordingFilters(camera.uuid, false);
    if (location.hash === "#recordings") void loadRecordings();
    else location.hash = "#recordings";
  });
  archive.hidden = !Object.keys(camera.streams).length;
  const fullscreen = button(camera.shortName + "全屏", "expand", async () => {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else if (feed.requestFullscreen) await feed.requestFullscreen();
      else if (!isDemo && !media.hidden && media.webkitEnterFullscreen) media.webkitEnterFullscreen();
      else toast("当前浏览器不支持此画面全屏");
    } catch {
      toast("当前无法进入全屏");
    }
  });
  const toggle = button(camera.shortName + "开始预览", "play", () => {
    if (stop) pause();
    else void play();
  });
  toggle.hidden = isDemo || !stream;
  actions.append(toggle, archive, fullscreen);
  metadata.append(description, actions);
  card.append(feed, metadata);

  function mark(state, text) {
    badge.className = "feed-badge " + (state === "waiting" ? "wait" : state === "stop" || state === "error" ? "stop" : "");
    badgeLabel.textContent = ({ demo: "演示画面", live: "实时", waiting: "正在连接", stop: "预览已暂停", error: "预览不可用" })[state];
    message.hidden = state === "live" || state === "demo";
    media.hidden = !message.hidden;
    messageLabel.textContent = text || "";
  }

  function changeToggle(playing) {
    const name = camera.shortName + (playing ? "暂停预览" : "开始预览");
    toggle.innerHTML = icon(playing ? "pause" : "play");
    toggle.setAttribute("aria-label", name);
    toggle.title = name;
  }

  function stopVideo() {
    if (stop) stop();
    stop = undefined;
    media.pause();
    media.removeAttribute("src");
    media.load();
    changeToggle(false);
  }

  function pause() {
    stopVideo();
    mark("stop", "点击播放以预览");
  }

  function play() {
    if (stop || !stream || isDemo || !session) return;
    const active = session;
    const playing = [...liveCards.values()].filter(entry => entry.playing());
    if (playing.length >= 4) playing[0].pause();
    mark("waiting", "等待摄像头画面");
    changeToggle(true);
    stop = startLive(media, (path, options) => request(active, path, options),
      cameraPath(camera.uuid, stream, "live.m4s"), (state, text) => {
        if (state === "error") stopVideo();
        mark(state, text);
      });
  }

  if (isDemo) mark("demo");
  else mark("stop", stream ? "点击播放以预览" : "请在 Moonfire 启用码流录像");
  return { card, play, pause, playing: () => Boolean(stop), stop: isDemo ? () => {} : stopVideo, canPlay: Boolean(stream) };
}

function renderCameras() {
  closeLive();
  const grid = $("camera-grid");
  grid.replaceChildren();
  for (const camera of cameras()) {
    const card = makeCard(camera);
    liveCards.set(camera.uuid, card);
    grid.append(card.card);
  }
  if (!liveCards.size) empty(grid, snapshot?.setupOffline ? "启动 Moonfire 后，点击刷新获取摄像头。" : "Moonfire 还没有配置摄像头。添加摄像头后，点击刷新。");
  if (session && page === "live") [...liveCards.values()].filter(card => card.canPlay).slice(0, 4).forEach(card => card.play());
}

function zone() {
  return snapshot?.timeZoneName || Intl.DateTimeFormat().resolvedOptions().timeZone;
}

function today() {
  try {
    const parts = new Intl.DateTimeFormat("en", { timeZone: zone(), year: "numeric", month: "2-digit", day: "2-digit" }).formatToParts(new Date());
    const read = type => parts.find(part => part.type === type).value;
    return read("year") + "-" + read("month") + "-" + read("day");
  } catch {
    return new Date().toISOString().slice(0, 10);
  }
}

function formatTime(value, withDate = false) {
  const options = { hour: "2-digit", minute: "2-digit", hourCycle: "h23" };
  if (withDate) Object.assign(options, { month: "2-digit", day: "2-digit", second: "2-digit" });
  const date = new Date(value / 90);
  try {
    return new Intl.DateTimeFormat("zh-CN", { ...options, timeZone: zone() }).format(date);
  } catch {
    return new Intl.DateTimeFormat("zh-CN", options).format(date);
  }
}

function fillRecordingFilters(preferred, keepStream = true) {
  const camera = $("record-camera");
  const selected = preferred || camera.value;
  camera.replaceChildren(...cameras().map(item => new Option(item.shortName, item.uuid)));
  if (cameras().some(item => item.uuid === selected)) camera.value = selected;
  fillStreams(keepStream && camera.value === selected);
  if (!$("record-date").value) $("record-date").value = today();
  $("record-zone").textContent = "时区 " + zone();
}

function fillStreams(keepPrevious) {
  const camera = cameras().find(item => item.uuid === $("record-camera").value);
  const streams = $("record-stream");
  const previous = streams.value;
  const keys = Object.keys(camera?.streams ?? {});
  streams.replaceChildren(...keys.map(key => new Option(streamNames[key] || key, key)));
  const available = keys.sort((a, b) => Number(camera.streams[b].record) - Number(camera.streams[a].record));
  streams.value = keepPrevious && available.includes(previous) ? previous : available[0] ?? "";
}

function demoRecords(day) {
  if (day !== today()) return [];
  const midnight = new Date(day + "T00:00:00").getTime() * 90;
  return [8, 12, 17].map(offset => ({
    startTime90k: midnight + offset * hour, endTime90k: midnight + offset * hour + 6 * ticks,
  }));
}

async function loadRecordings() {
  recordRequest?.abort();
  const run = new AbortController();
  recordRequest = run;
  const camera = cameras().find(item => item.uuid === $("record-camera").value);
  const stream = $("record-stream").value;
  const date = $("record-date").value;
  const result = $("record-result");
  if (!camera || !stream || !date) {
    empty(result, "选择有可用码流的摄像头和日期。");
    return;
  }
  empty(result, "正在查询录像…");
  try {
    let rows;
    const active = session;
    if (!active) {
      rows = camera.streams[stream].record ? demoRecords(date) : [];
    } else {
      const inventory = await data(active, "/api/", run.signal);
      if (run.signal.aborted) return;
      snapshot = inventory;
      const day = inventory.cameras.find(item => item.uuid === camera.uuid)?.streams[stream]?.days?.[date];
      if (day) {
        const params = query({ startTime90k: day.startTime90k, endTime90k: day.endTime90k, split90k: 10 * 60 * ticks });
        const payload = await data(active, cameraPath(camera.uuid, stream, "recordings") + params, run.signal);
        rows = payload.recordings.filter(record => record.endTime90k > day.startTime90k && record.startTime90k < day.endTime90k)
          .map(record => ({ ...record, day }));
      } else {
        rows = [];
      }
    }
    if (run.signal.aborted) return;
    if (!rows.length) {
      empty(result, "这一日期没有可用录像。");
      return;
    }
    rows.sort((a, b) => b.startTime90k - a.startTime90k);
    result.replaceChildren(...rows.map(record => recordRow(active, camera, stream, record)));
  } catch (error) {
    if (!run.signal.aborted) empty(result, error.message);
  }
}

function recordRow(active, camera, stream, record) {
  const start = Math.max(record.startTime90k, record.day?.startTime90k ?? 0);
  const end = Math.min(record.endTime90k, record.day?.endTime90k ?? Infinity);
  const summary = node("div");
  const duration = Math.max(1, Math.round((end - start) / ticks));
  const status = active ? (record.growing ? " · 录制中" : "") : " · 演示片段";
  summary.append(node("time", "", formatTime(start, true) + " — " + formatTime(end, true)), node("small", "", duration + " 秒" + status));
  const actions = node("div", "card-actions");
  for (const [label, image, downloading] of [["播放", "play", false], ["下载", "download", true]]) {
    const control = button(label, image, () => {
      control.disabled = true;
      void openRecord(active, camera, stream, record, start, end, downloading).finally(() => { control.disabled = false; });
    }, true);
    actions.append(control);
  }
  const row = node("div", "record-row");
  row.append(summary, actions);
  return row;
}

async function openRecord(active, camera, stream, record, start, end, downloading) {
  try {
    let href;
    if (active) {
      let clip = String(record.startId);
      if (record.endId !== undefined) clip += "-" + record.endId;
      if (record.firstUncommitted !== undefined || record.growing) clip += "@" + record.openId;
      if (start !== record.startTime90k || end !== record.endTime90k || record.growing) {
        clip += "." + (start - record.startTime90k) + "-" + (end - record.startTime90k);
      }
      const path = cameraPath(camera.uuid, stream, "view.mp4");
      const params = query({ path, s: clip, download: downloading ? "1" : "" });
      const value = await data(active, "/api/media-ticket" + params, AbortSignal.timeout(15000));
      href = active.base + value.url;
      if (active !== session) return;
    } else {
      href = "./demo/" + camera.uuid + ".mp4";
    }
    if (downloading) {
      const link = node("a");
      link.href = href;
      if (!active) link.download = camera.uuid + "-demo.mp4";
      link.referrerPolicy = "no-referrer";
      document.body.append(link);
      link.click();
      link.remove();
      return;
    }
    $("player-heading").textContent = camera.shortName + " · " + formatTime(start, true);
    $("player-message").textContent = "";
    const player = $("record-player");
    if (!$("player-dialog").open) $("player-dialog").showModal();
    player.src = href;
    player.play().catch(() => {});
  } catch (error) {
    toast(error.message);
  }
}

function displayAlerts(events, message) {
  $("alert-count").textContent = message ? "—" : String(events.length);
  for (const [id, rows, showDate] of [["recent-alerts", events.slice(0, 4), false], ["all-alerts", events, true]]) {
    const list = $(id);
    if (!rows.length) {
      list.replaceChildren(node("p", "event-note", message || "近 24 小时没有告警。"));
      continue;
    }
    list.replaceChildren(...rows.map(event => {
      const item = node("article", "event");
      const indicator = node("span", "event-icon");
      indicator.innerHTML = icon("motion");
      const details = node("div", "event-info");
      details.append(node("strong", "", event.title), node("p", "", event.detail));
      item.append(indicator, details, node("time", "", formatTime(event.time, showDate)));
      return item;
    }));
  }
}

async function loadAlerts() {
  alertRequest?.abort();
  const active = session;
  if (!active) {
    const now = Date.now() * 90;
    displayAlerts([
      { title: "检测到移动", detail: "前门 · 演示事件", time: now - ticks * 60 * 8 },
      { title: "检测到移动", detail: "车道 · 演示事件", time: now - ticks * 60 * 26 },
      { title: "检测到移动", detail: "后院 · 演示事件", time: now - ticks * 60 * 47 },
    ]);
    return;
  }

  const signals = snapshot.signals ?? [];
  if (!signals.length) {
    displayAlerts([], "Moonfire 尚未配置告警信号。");
    return;
  }
  const run = new AbortController();
  alertRequest = run;
  try {
    const now = Date.now() * 90;
    const start = now - 24 * hour;
    const payload = await data(active, "/api/signals" + query({ startTime90k: start, endTime90k: now }), run.signal);
    if (run.signal.aborted) return;
    const states = new Map();
    const byId = new Map(signals.map(signal => [signal.id, signal]));
    const types = new Map((snapshot.signalTypes ?? []).map(type => [type.uuid, type]));
    const events = [];
    for (const [index, time] of payload.times90k.entries()) {
      const id = payload.signalIds[index];
      const signal = byId.get(id);
      if (!signal) continue;
      const state = types.get(signal.type)?.states.find(item => item.value === payload.states[index]);
      const wasMotion = states.get(id);
      states.set(id, state?.motion === true);
      if (time < start || time >= now || !state?.motion || wasMotion) continue;
      const labels = cameras().filter(camera => signal.cameras?.[camera.uuid]).map(camera => camera.shortName);
      events.push({ title: signal.shortName, detail: [...labels, state.name].join(" · "), time });
    }
    displayAlerts(events.sort((a, b) => b.time - a.time));
  } catch (error) {
    if (!run.signal.aborted) displayAlerts([], error.message);
  }
}

function changeView() {
  const next = location.hash.slice(1);
  page = Object.hasOwn(knownViews, next) ? next : "live";
  $("breadcrumb").textContent = knownViews[page];
  document.querySelectorAll("[data-view]").forEach(link => {
    if (link.dataset.view === page) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  for (const id of Object.keys(knownViews)) $(id + "-page").hidden = id !== page;
  if (page === "live") renderCameras();
  else closeLive();
  if (page === "recordings") void loadRecordings();
  else recordRequest?.abort();
  if (page === "live" || page === "alerts") void loadAlerts();
  if (page === "devices") updateDevices();
  else clearDeviceSetup();
}

function clearDeviceSetup() {
  scanRequest?.abort();
  profileRequest?.abort();
  $("onvif-form").reset();
  $("scan-status").textContent = "";
  $("onvif-status").textContent = "";
  $("device-results").replaceChildren();
  $("profile-results").replaceChildren();
  $("profile-panel").hidden = true;
  cameraWriteRequest?.abort();
  $("camera-write-form").reset();
  $("camera-write-status").textContent = "";
  $("setup-preview").hidden = true;
  $("setup-storage").replaceChildren(new Option("先读取目录", ""));
  $("setup-storage").disabled = true;
  $("setup-folders").disabled = false;
  $("setup-save").disabled = true;
}

function updateDevices() {
  const paired = localDashboard && session?.base === location.origin;
  $("devices-remote").hidden = localDashboard;
  $("devices-unpaired").hidden = !localDashboard || paired;
  $("devices-local").hidden = !paired;
  $("camera-write-panel").hidden = !paired || !session.setupEnabled;
  $("devices-local-link").href = (session?.base || "http://127.0.0.1:8787") + "/#devices";
}

async function scanDevices(event) {
  const active = session;
  if (!localDashboard || active?.base !== location.origin) return;
  const control = event.currentTarget;
  const run = new AbortController();
  scanRequest = run;
  const status = $("scan-status");
  const list = $("device-results");
  status.textContent = "正在搜索局域网…";
  list.replaceChildren();
  control.disabled = true;
  try {
    const response = await request(active, "/api/onvif/discover", { signal: run.signal, payload: {} });
    const cameras = await response.json();
    if (run.signal.aborted) return;
    status.textContent = cameras.length ? "找到 " + cameras.length + " 台设备。选择一台以填写地址。" :
      "这次没有收到设备响应。可以直接填写设备地址。";
    for (const camera of cameras) {
      const row = node("div", "discovered-item");
      const name = camera.name ? camera.name + " · " + camera.endpoint : camera.endpoint;
      row.append(button(name, "camera", () => {
        $("onvif-endpoint").value = camera.endpoint;
        $("onvif-password").value = "";
        $("onvif-username").focus();
      }, true));
      list.append(row);
    }
  } catch (error) {
    if (!run.signal.aborted) status.textContent = error.message;
  } finally {
    control.disabled = false;
  }
}

async function loadProfiles(event) {
  event.preventDefault();
  const active = session;
  if (!localDashboard || active?.base !== location.origin) return;
  const run = new AbortController();
  profileRequest = run;
  const status = $("onvif-status");
  const control = event.submitter;
  const password = $("onvif-password");
  const payload = { endpoint: $("onvif-endpoint").value, username: $("onvif-username").value, password: password.value };
  password.value = "";
  $("profile-panel").hidden = true;
  $("profile-results").replaceChildren();
  status.textContent = "正在读取设备码流…";
  control.disabled = true;
  try {
    const response = await request(active, "/api/onvif/profiles", { signal: run.signal, payload });
    const inventory = await response.json();
    if (run.signal.aborted) return;
    status.textContent = "";
    $("onvif-model").textContent = [inventory.manufacturer, inventory.model].filter(Boolean).join(" ");
    inventory.profiles.forEach((profile, index) => {
      const label = node("label");
      const resolution = profile.width && profile.height ? profile.width + " × " + profile.height : "";
      label.textContent = [profile.name || ("码流 " + (index + 1)), profile.encoding, resolution].filter(Boolean).join(" · ");
      const row = node("div", "profile-item");
      if (!profile.rtsp) {
        label.append(node("span", "", "设备未返回 RTSP 地址"));
        row.append(label);
      } else {
        const input = node("input");
        input.readOnly = true;
        input.value = profile.rtsp;
        input.spellcheck = false;
        input.addEventListener("focus", () => input.select());
        label.append(input);
        const copy = button("复制地址", "copy", async () => {
          try {
            await navigator.clipboard.writeText(input.value);
            toast("已复制 RTSP 地址");
          } catch {
            input.focus();
            toast("地址已选中，可手动复制");
          }
        }, true);
        row.append(label);
        if (active.setupEnabled) {
          const actions = node("div", "profile-actions");
          actions.append(copy);
          for (const [label, field] of [["设为主码流", "setup-main"], ["设为子码流", "setup-sub"]]) {
            actions.append(button(label, "add", () => {
              $(field).value = profile.rtsp;
              $(field).focus();
            }, true));
          }
          row.append(actions);
        } else {
          row.append(copy);
        }
      }
      $("profile-results").append(row);
    });
    $("profile-panel").hidden = false;
  } catch (error) {
    if (!run.signal.aborted) status.textContent = error.message;
  } finally {
    control.disabled = false;
  }
}

async function writeCameraRequest(path, payload, handle) {
  const active = session;
  if (!localDashboard || active?.base !== location.origin || !active.setupEnabled) return;
  cameraWriteRequest?.abort();
  const run = new AbortController();
  cameraWriteRequest = run;
  const status = $("camera-write-status");
  $("setup-folders").disabled = true;
  $("setup-save").disabled = true;
  status.textContent = "正在处理…";
  try {
    const response = await request(active, "/api/setup/" + path, { signal: run.signal, payload });
    const value = await response.json();
    if (!run.signal.aborted) handle(value, status);
  } catch (error) {
    if (!run.signal.aborted) status.textContent = error.message;
  } finally {
    if (run === cameraWriteRequest && !run.signal.aborted) {
      $("setup-folders").disabled = false;
      $("setup-save").disabled = $("setup-storage").disabled;
    }
  }
}

function loadSetupFolders() {
  const select = $("setup-storage");
  $("setup-save").disabled = true;
  select.disabled = true;
  select.replaceChildren(new Option("请选择录像目录", ""));
  void writeCameraRequest("storage", {}, (items, status) => {
    if (!items.length) {
      status.textContent = "Moonfire 还没有录像目录。请先在 Moonfire 配置工具中添加。";
      return;
    }
    select.append(...items.map(item => new Option(item.name + "（" + item.id + "）", item.id)));
    if (items.length === 1) select.value = items[0].id;
    select.disabled = false;
    $("setup-save").disabled = false;
    status.textContent = "";
  });
}

function saveSetupCamera(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const password = $("setup-password");
  const payload = {
    name: $("setup-camera-name").value, main: $("setup-main").value, sub: $("setup-sub").value,
    username: $("setup-user").value, password: password.value,
    storageId: Number($("setup-storage").value), retainGiB: Number($("setup-retention").value),
  };
  password.value = "";
  void writeCameraRequest("cameras", payload, (_, status) => {
    const storage = $("setup-storage").value;
    form.reset();
    $("setup-storage").value = storage;
    status.textContent = "已保存并启用录像。启动 Moonfire 后即可预览。";
    $("setup-preview").hidden = false;
  });
}

function setSession(active, inventory) {
  recordRequest?.abort();
  alertRequest?.abort();
  if (session !== active) clearDeviceSetup();
  session = active;
  snapshot = inventory;
  $("record-date").value = today();
  fillRecordingFilters();
  updateConnection();
  changeView();
}

async function refreshInventory() {
  const active = session;
  if (!active) return;
  const reload = $("reload-cameras");
  reload.disabled = true;
  try {
    const current = await data(active, "/api/", AbortSignal.timeout(15000));
    if (active !== session) return;
    snapshot = current;
    fillRecordingFilters();
    updateConnection();
    if (page === "live") renderCameras();
    void loadAlerts();
    return true;
  } catch (error) {
    toast(error.message);
  } finally {
    reload.disabled = false;
  }
}

document.querySelectorAll("[data-layout]").forEach(control => control.addEventListener("click", () => {
  document.querySelectorAll("[data-layout]").forEach(option => option.setAttribute("aria-pressed", String(option === control)));
  $("camera-grid").classList.toggle("single", control.dataset.layout === "single");
}));
document.querySelectorAll("[data-connect]").forEach(control => control.addEventListener("click", () => {
  $("connect-error").textContent = "";
  $("connect-token").value = "";
  $("setup-dialog").showModal();
}));
document.querySelectorAll("[data-close]").forEach(control => control.addEventListener("click", () => control.closest("dialog").close()));
$("setup-dialog").addEventListener("close", () => {
  connectRequest?.abort();
  $("connect-token").value = "";
});
$("player-dialog").addEventListener("close", () => {
  $("record-player").pause();
  $("record-player").removeAttribute("src");
  $("record-player").load();
});
$("record-player").addEventListener("error", () => {
  if ($("player-dialog").open) $("player-message").textContent = "无法播放此片段。请重新打开录像，或下载后使用本地播放器。";
});
$("connect-form").addEventListener("submit", async event => {
  event.preventDefault();
  const submit = event.submitter;
  const active = { token: $("connect-token").value.trim(), base: new URL("http://127.0.0.1:" + Number($("connect-port").value)).origin };
  const run = new AbortController();
  connectRequest = run;
  $("connect-error").textContent = "正在连接…";
  submit.disabled = true;
  try {
    const signal = AbortSignal.any([run.signal, AbortSignal.timeout(20000)]);
    if (localDashboard && active.base === location.origin) {
      const setup = await request(active, "/api/setup/info", { signal, payload: {} })
        .then(response => response.json()).catch(() => undefined);
      active.setupEnabled = setup?.enabled === true;
    }
    const inventory = await data(active, "/api/", signal).catch(error => {
      if (page !== "devices" || !active.setupEnabled || signal.aborted) throw error;
      return { cameras: [], signals: [], setupOffline: true };
    });
    if (!run.signal.aborted) {
      setSession(active, inventory);
      $("setup-dialog").close();
      toast(inventory.setupOffline ? "本机桥接已配对" : "本地设备已连接");
    }
  } catch (error) {
    if (!run.signal.aborted) $("connect-error").textContent = error.name === "TimeoutError" ? "连接超时，请检查桥接和本地网络权限。" : error.message;
  } finally {
    submit.disabled = false;
  }
});
$("disconnect").addEventListener("click", () => {
  setSession(undefined, undefined);
  $("setup-dialog").close();
  toast("已断开本地连接");
});
$("reload-cameras").addEventListener("click", () => void refreshInventory());
$("reload-alerts").addEventListener("click", () => void loadAlerts());
$("scan-devices").addEventListener("click", scanDevices);
$("onvif-form").addEventListener("submit", loadProfiles);
$("setup-folders").addEventListener("click", loadSetupFolders);
$("camera-write-form").addEventListener("submit", saveSetupCamera);
$("setup-preview").addEventListener("click", async event => {
  const control = event.currentTarget;
  control.disabled = true;
  try {
    if (await refreshInventory()) location.hash = "#live";
  } finally {
    control.disabled = false;
  }
});
$("record-camera").addEventListener("change", () => fillStreams(false));
$("record-form").addEventListener("submit", event => {
  event.preventDefault();
  void loadRecordings();
});
window.addEventListener("hashchange", changeView);
window.setInterval(() => {
  if (session && (page === "live" || page === "alerts") && !document.hidden) void loadAlerts();
}, 30000);
if (location.hostname === "127.0.0.1" && location.port) $("connect-port").value = location.port;
decorate(document);
setSession(undefined, undefined);
