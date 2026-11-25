const decoder = new TextDecoder();
const maxFrame = 16 * 1024 * 1024;

function readPart(frame) {
  let boundary = -1;
  for (let offset = 0; offset < Math.min(frame.length - 3, 4096); offset++) {
    if (frame[offset] === 13 && frame[offset + 1] === 10 &&
        frame[offset + 2] === 13 && frame[offset + 3] === 10) {
      boundary = offset;
      break;
    }
  }
  if (boundary < 0) throw new Error("视频分片格式无效");
  const headers = new Map(decoder.decode(frame.subarray(0, boundary)).split("\r\n").map(line => {
    const separator = line.indexOf(":");
    return [line.slice(0, separator).toLowerCase(), line.slice(separator + 1).trim()];
  }));
  const type = headers.get("content-type");
  const entry = headers.get("x-video-sample-entry-id");
  if (!type || !/^\d+$/.test(entry)) throw new Error("视频分片缺少编解码信息");
  return { type, entry, body: frame.subarray(boundary + 4) };
}

async function* readFrames(body) {
  const reader = body.getReader();
  let chunk = new Uint8Array(0);
  let offset = 0;
  async function read(size) {
    const output = new Uint8Array(size);
    for (let written = 0; written < size;) {
      if (offset === chunk.length) {
        const result = await reader.read();
        if (result.done) throw new Error("实时视频已中断");
        chunk = result.value;
        offset = 0;
      }
      const length = Math.min(size - written, chunk.length - offset);
      output.set(chunk.subarray(offset, offset + length), written);
      written += length;
      offset += length;
    }
    return output;
  }

  try {
    while (true) {
      const length = new DataView((await read(4)).buffer).getUint32(0);
      if (!length || length > maxFrame) throw new Error("视频分片大小无效");
      yield readPart(await read(length));
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

function waitFor(target, success, failures, signal, start = () => {}) {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    function finish(error) {
      target.removeEventListener(success, onSuccess);
      failures.forEach(failure => target.removeEventListener(failure, onFailure));
      signal.removeEventListener("abort", onAbort);
      if (error) reject(error);
      else resolve();
    }
    function onSuccess() { finish(); }
    function onFailure() { finish(new Error("浏览器无法解码此视频流")); }
    function onAbort() { finish(signal.reason); }
    target.addEventListener(success, onSuccess);
    failures.forEach(failure => target.addEventListener(failure, onFailure));
    signal.addEventListener("abort", onAbort);
    try {
      start();
    } catch (error) {
      finish(error);
    }
  });
}

export function startLive(video, request, path, report) {
  const controller = new AbortController();
  const { signal } = controller;
  let mediaURL;
  let deadline;
  let message = "等待摄像头数据超时";
  function armDeadline() {
    clearTimeout(deadline);
    deadline = setTimeout(() => controller.abort(new Error(message)), 22000);
  }

  async function play() {
    const MediaApi = window.ManagedMediaSource ?? window.MediaSource;
    if (!MediaApi) throw new Error("当前浏览器不支持实时预览，请使用新版 Chrome 或 Edge");
    const media = new MediaApi();
    video.disableRemotePlayback = true;
    mediaURL = URL.createObjectURL(media);
    armDeadline();
    await waitFor(media, "sourceopen", ["sourceclose"], signal, () => { video.src = mediaURL; });

    const response = await request(path, { signal });
    let buffer;
    let activeType;
    let activeEntry;
    let ready = false;
    function update(method) {
      return waitFor(buffer, "updateend", ["error", "abort"], signal, method);
    }

    for await (const part of readFrames(response.body)) {
      armDeadline();
      if (video.error) throw new Error("浏览器无法解码此视频流");
      if (!MediaApi.isTypeSupported(part.type)) throw new Error("浏览器不支持此编码，请将该码流设为 H.264");
      if (!buffer) buffer = media.addSourceBuffer(part.type);
      if (activeType && activeType !== part.type) buffer.changeType(part.type);
      if (activeType !== part.type || activeEntry !== part.entry) {
        const init = await request("/api/init/" + part.entry + ".mp4", { signal });
        const bytes = await init.arrayBuffer();
        await update(() => buffer.appendBuffer(bytes));
        activeType = part.type;
        activeEntry = part.entry;
      }
      const current = video.currentTime;
      if (buffer.buffered.length && buffer.buffered.start(0) < current - 30) {
        await update(() => buffer.remove(0, current - 20));
      }
      const previous = buffer.buffered;
      buffer.timestampOffset = previous.length ? previous.end(previous.length - 1) : 0;
      await update(() => buffer.appendBuffer(part.body));
      const ranges = buffer.buffered;
      if (!ranges.length) continue;
      const latest = ranges.length - 1;
      if (video.currentTime < ranges.start(latest) || ranges.end(latest) - video.currentTime > 8) {
        video.currentTime = Math.max(ranges.start(latest), ranges.end(latest) - 1.5);
      }
      if (!ready) {
        ready = true;
        message = "实时视频已中断";
        report("live");
        video.play().catch(() => {});
      }
    }
  }

  play().catch(error => {
    if (signal.aborted && signal.reason.name === "AbortError") return;
    report("error", error.message || "无法播放实时视频");
  }).finally(() => {
    clearTimeout(deadline);
    if (mediaURL) URL.revokeObjectURL(mediaURL);
  });
  return () => controller.abort();
}
