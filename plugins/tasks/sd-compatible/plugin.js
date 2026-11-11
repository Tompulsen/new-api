export const meta = {
  apiVersion: 1,
  key: "sd-compatible",
  name: "SD兼容",
  icon: "text:SD",
  description: {
    en: "SD video generation with public reference image URLs",
    zh: "SD 视频生成，支持公网图片链接作为参考图",
  },
  version: "1.0.0",
  author: { name: "QuantumNous" },
  baseUrl: "https://api.kunrouai.com",
  models: ["sd2.0-fast-480p", "sd2.0-fast-720p", "sd2.0-mini-480p", "sd2.0-mini-720p"],
  fetchMode: "per_task",
  upstreams: ["vendor"],
  protocols: ["openai_video"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
    resolution: { enum: ["480p", "720p", "1080p"], description: { en: "Output video resolution", zh: "输出视频分辨率" } },
  },
  usageExamples: [
    { label: "480p 5s", facts: { seconds: 5, resolution: "480p" } },
    { label: "720p 10s", facts: { seconds: 10, resolution: "720p" } },
  ],
};

function object(value) {
  return value && typeof value === "object" && !Array.isArray(value);
}

function trimmed(value) {
  return typeof value === "string" ? value.trim() : "";
}

function apiRoot(ctx) {
  return ctx.baseUrl.replace(/\/+$/, "").replace(/\/v1$/, "") + "/v1";
}

function selected(req, keys) {
  for (const source of [req, req.metadata || {}]) {
    for (const key of keys) {
      if (source[key] !== undefined && source[key] !== null) return source[key];
    }
  }
}

function referenceURL(value) {
  if (object(value)) value = value.url === undefined ? value.image_url : value.url;
  if (object(value)) value = value.url;
  const url = trimmed(value);
  if (url.length > 8192 || !/^https?:\/\/[^/?#@\s]+(?:[/?#][^\s]*)?$/i.test(url))
    throw new Error("SD reference images require public HTTP(S) image URLs; local files and base64 uploads are not supported by the upstream API");
  return url;
}

function videoRequest(req, files, model) {
  if (!object(req)) throw new Error("request body must be an object");
  if (req.metadata !== undefined && !object(req.metadata)) throw new Error("metadata must be an object");
  // The documented SD endpoint has no file upload contract.
  if ((files || []).length)
    throw new Error("SD reference images require public HTTP(S) image URLs; local files and base64 uploads are not supported by the upstream API");
  if (!trimmed(req.prompt)) throw new Error("prompt is required");
  const duration = selected(req, ["seconds", "duration"]);
  if (duration !== undefined && typeof duration !== "string" && typeof duration !== "number") throw new Error("seconds must be a positive integer");
  const seconds = duration === undefined ? 5 : Number(duration);
  if (!Number.isSafeInteger(seconds) || seconds <= 0 || seconds > 3600) throw new Error("seconds must be a positive integer no greater than 3600");
  const dimensions = /^(\d+)[x:](\d+)$/.exec(trimmed(selected(req, ["size"])));
  const width = dimensions ? Number(dimensions[1]) : 0;
  const height = dimensions ? Number(dimensions[2]) : 0;
  const validSize = width > 0 && height > 0 && width <= 65535 && height <= 65535;
  let ratio = trimmed(selected(req, ["ratio", "aspect_ratio", "aspectRatio"]));
  if (ratio === "auto") ratio = "adaptive";
  if (!ratio && validSize) {
    let divisor = width,
      remainder = height;
    while (remainder) {
      const next = divisor % remainder;
      divisor = remainder;
      remainder = next;
    }
    ratio = width / divisor + ":" + height / divisor;
  }
  if (!ratio) ratio = "16:9";
  let resolution = trimmed(selected(req, ["resolution", "resolution_name"])).toLowerCase();
  if (!resolution || resolution === "auto") {
    const suffix = /-(\d+p)$/i.exec(model || "");
    resolution = suffix ? suffix[1].toLowerCase() : validSize ? Math.min(width, height) + "p" : "720p";
  }
  if (!/^[1-9]\d{0,4}p$/.test(resolution) || Number(resolution.slice(0, -1)) > 65535) throw new Error("resolution must be a positive pixel tier such as 720p");
  const firstFrames = [],
    images = [],
    lastFrames = [];
  for (const name of ["first_frame", "image", "input_reference"]) {
    const value = selected(req, [name]);
    if (value !== undefined) firstFrames.push(referenceURL(value));
  }
  for (const name of ["images", "reference_images"]) {
    const values = selected(req, [name]);
    if (values !== undefined) {
      if (!Array.isArray(values)) throw new Error(name + " must be an array");
      for (const value of values) images.push(referenceURL(value));
    }
  }
  const content = selected(req, ["content"]);
  if (content !== undefined) {
    if (!Array.isArray(content)) throw new Error("content must be an array");
    for (const item of content) {
      if (!object(item) || item.type !== "image_url") throw new Error("SD content supports image_url references only");
      const url = referenceURL(item.image_url);
      if (item.role === "first_frame") firstFrames.push(url);
      else if (item.role === "last_frame") lastFrames.push(url);
      else if (!item.role || item.role === "reference_image" || item.role === "middle_frame") images.push(url);
      else throw new Error("unsupported reference image role");
    }
  }
  const last = selected(req, ["last_frame"]);
  if (last !== undefined) lastFrames.push(referenceURL(last));
  const endings = new Set(lastFrames);
  const unique = Array.from(
    new Set(
      firstFrames
        .concat(images)
        .filter((url) => !endings.has(url))
        .concat(lastFrames)
    )
  );
  if (unique.length > 128) throw new Error("too many reference images");
  const body = { model, prompt: req.prompt, seconds: String(seconds), ratio, aspect_ratio: ratio, resolution };
  if (unique.length) body.images = unique;
  return body;
}

function decode(ctx) {
  if (!ctx.body || !["json", "multipart"].includes(ctx.body.kind)) throw new Error("JSON or multipart body required");
  let req = ctx.body.value;
  if (ctx.body.kind === "multipart") {
    req = {};
    for (const key of Object.keys(ctx.body.fields || {})) {
      const values = ctx.body.fields[key];
      if (values.length !== 1) throw new Error(key + " must be provided once");
      req[key] = values[0];
    }
    for (const key of ["metadata", "images", "reference_images", "content"]) {
      if (req[key] !== undefined) {
        try {
          req[key] = JSON.parse(req[key]);
        } catch (error) {
          throw new Error(key + " must contain valid JSON", { cause: error });
        }
      }
    }
  }
  const model = ctx.model || (object(req) && trimmed(req.model));
  if (!model) throw new Error("model is required");
  const body = videoRequest(req, ctx.body.files, model);
  return { kind: "submit", model, action: body.images ? "image_to_video" : "text_to_video", requestBody: req };
}

export function buildSubmitRequest(ctx) {
  return {
    url: apiRoot(ctx) + "/tasks/sora",
    method: "POST",
    headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json" },
    body: videoRequest(ctx.requestBody, ctx.files, ctx.upstreamModel || ctx.model),
  };
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  const id = object(body) && (trimmed(body.id) || trimmed(body.task_id));
  if (!id) throw new Error("SD submission response is missing a task ID");
  return { taskId: id, taskData: body };
}

export function extractUsage(ctx) {
  const body = videoRequest(ctx.requestBody, ctx.files, ctx.upstreamModel || ctx.model);
  const edge = Number(body.resolution.slice(0, -1));
  return { seconds: Number(body.seconds), resolution: edge <= 480 ? "480p" : edge < 1080 ? "720p" : "1080p" };
}

export function extractUsageOnComplete(ctx, result, body) {
  if (!object(body)) return {};
  const facts = {};
  const duration = body.seconds === undefined ? body.duration : body.seconds;
  const seconds = typeof duration === "number" || typeof duration === "string" ? Number(duration) : NaN;
  if (Number.isFinite(seconds) && seconds > 0 && seconds <= 3600) facts.seconds = seconds;
  const size = /^(\d+)[x:](\d+)$/.exec(trimmed(body.size));
  const width = Number(body.width === undefined ? size && size[1] : body.width);
  const height = Number(body.height === undefined ? size && size[2] : body.height);
  let edge = 0;
  if (width > 0 && height > 0 && width <= 65535 && height <= 65535) edge = Math.min(width, height);
  else {
    const resolution = /^(\d+)p$/i.exec(trimmed(body.resolution));
    if (resolution) edge = Number(resolution[1]);
  }
  if (edge > 0 && edge <= 65535) facts.resolution = edge <= 480 ? "480p" : edge < 1080 ? "720p" : "1080p";
  return facts;
}

export function buildQueryRequest(ctx) {
  return { url: apiRoot(ctx) + "/videos/" + encodeURIComponent(ctx.taskId), method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(ctx, body) {
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  const result = { status: statuses[body && body.status] || "UNKNOWN" };
  if (result.status === "FAILURE") {
    const error = body.error;
    result.reason = trimmed(object(error) ? error.message : error) || trimmed(body.fail_reason) || trimmed(body.message) || "SD video generation failed";
  }
  return result;
}

function videoURL(data) {
  return object(data) ? trimmed(data.video_url) || (object(data.result) && trimmed(data.result.video_url)) || "" : "";
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" && videoURL(task.data) ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  const url = videoURL(ctx.data);
  if (ctx.artifactKey !== "video" || !url) throw new Error("artifact_not_found");
  return { url, method: ctx.clientRequest.method, credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest: decode,
    render: function (ctx, task) {
      return object(task.data) ? task.data : {};
    },
  },
};
