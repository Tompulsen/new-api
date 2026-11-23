export const meta = {
  apiVersion: 1,
  key: "c-compatible",
  name: "C兼容",
  icon: "text:C",
  description: { en: "Asynchronous video generation with reference media", zh: "异步视频生成，支持参考素材" },
  version: "1.0.0",
  author: { name: "QuantumNous" },
  models: [],
  fetchMode: "per_task",
  upstreams: ["vendor"],
  protocols: ["openai_video"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
  },
  usageExamples: [{ label: "6s", facts: { seconds: 6 } }],
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

function rejectClientPlaceholders(value) {
  if (!value || typeof value !== "object") return;
  for (const key of Object.keys(value)) {
    if (key === "__fileRef" || key === "__videoUpload") throw new Error("attach reference media instead of supplying host file placeholders");
    rejectClientPlaceholders(value[key]);
  }
}

function reference(value, mediaType) {
  if (object(value)) value = value.url === undefined ? value.image_url : value.url;
  if (object(value)) value = value.url;
  if (typeof value !== "string" || !value.toLowerCase().startsWith("data:" + mediaType + "/"))
    throw new Error("attach reference media or use a base64 data URL; remote URLs and upstream media IDs are not supported");
  return { media_id: { __videoUpload: { dataUrl: value, mediaType } } };
}

function videoRequest(req, files, model) {
  if (!object(req)) throw new Error("request body must be an object");
  if (req.metadata !== undefined && !object(req.metadata)) throw new Error("metadata must be an object");
  rejectClientPlaceholders(req);
  if (!trimmed(req.prompt)) throw new Error("prompt is required");
  // Freeze an explicit duration for both reservation and submission, without guessing model defaults.
  const duration = selected(req, ["seconds", "duration_seconds", "duration"]);
  if (typeof duration !== "string" && typeof duration !== "number") throw new Error("seconds is required");
  const seconds = Number(duration);
  if (!Number.isSafeInteger(seconds) || seconds <= 0 || seconds > 3600)
    throw new Error("seconds must be a positive integer no greater than 3600");
  const body = { model, prompt: req.prompt, seconds };
  for (const [key, aliases] of [
    ["size", ["size"]],
    ["resolution", ["resolution", "resolution_name"]],
    ["aspect_ratio", ["aspect_ratio", "aspectRatio", "ratio"]],
    ["video_reference_frame_mode", ["video_reference_frame_mode"]],
  ]) {
    const value = selected(req, aliases);
    if (value !== undefined) {
      if (typeof value !== "string" || !value.trim() || value.length > 128) throw new Error(key + " must be a non-empty string");
      body[key] = value.trim();
    }
  }
  if (body.size && !/^[1-9]\d{0,4}x[1-9]\d{0,4}$/.test(body.size)) throw new Error("size must be WIDTHxHEIGHT");
  const frameRate = selected(req, ["frame_rate"]);
  if (frameRate !== undefined) {
    const value = Number(frameRate);
    if ((typeof frameRate !== "number" && typeof frameRate !== "string") || !Number.isSafeInteger(value) || value < 0 || value > 240)
      throw new Error("frame_rate must be an integer between 0 and 240");
    body.frame_rate = value;
  }
  const audio = selected(req, ["generate_audio"]);
  if (audio !== undefined) {
    if (audio !== false && audio !== "false") throw new Error("generate_audio supports false only");
    body.generate_audio = false;
  }
  if (selected(req, ["generation_options"]) !== undefined)
    throw new Error("generation_options is not supported by this video plugin");

  const references = { image: [], video: [], audio: [] };
  const first = [], last = [];
  for (const key of ["first_frame", "first_frame_image", "image", "input_reference"]) {
    const value = selected(req, [key]);
    if (value !== undefined) first.push(reference(value, "image"));
  }
  for (const key of ["last_frame", "last_frame_image"]) {
    const value = selected(req, [key]);
    if (value !== undefined) last.push(reference(value, "image"));
  }
  for (const [mediaType, keys] of [
    ["image", ["images", "reference_images", "input_images"]],
    ["video", ["reference_videos", "input_videos"]],
    ["audio", ["reference_audios", "input_audios"]],
  ]) {
    for (const key of keys) {
      const values = selected(req, [key]);
      if (values === undefined) continue;
      if (!Array.isArray(values)) throw new Error(key + " must be an array");
      for (const value of values) references[mediaType].push(reference(value, mediaType));
    }
  }
  const content = selected(req, ["content"]);
  if (content !== undefined) {
    if (!Array.isArray(content)) throw new Error("content must be an array");
    for (const item of content) {
      if (!object(item) || item.type !== "image_url") throw new Error("content supports image_url references only");
      const value = reference(item.image_url, "image");
      if (item.role === "first_frame") first.push(value);
      else if (item.role === "last_frame") last.push(value);
      else if (!item.role || item.role === "reference_image" || item.role === "middle_frame") references.image.push(value);
      else throw new Error("unsupported reference image role");
    }
  }
  for (const file of files || []) {
    const field = file.field.replace(/\[\]$/, "");
    const types = {
      input_reference: "image", image: "image", images: "image", input_image_files: "image",
      input_images: "image", reference_images: "image", first_frame: "image", last_frame: "image",
      input_videos: "video", reference_videos: "video", input_audios: "audio", reference_audios: "audio",
    };
    const mediaType = types[field];
    if (!mediaType) throw new Error("unexpected reference file field");
    const limit = mediaType === "image" ? 20971520 : 67108864;
    if (file.size <= 0 || file.size > limit) throw new Error("reference file exceeds the upload size limit");
    const value = { media_id: { __videoUpload: { fileRef: file.ref, mediaType } } };
    if (field === "last_frame") last.push(value);
    else if (field === "first_frame" || field === "input_reference") first.push(value);
    else references[mediaType].push(value);
  }
  const endings = new Set(last.map((value) => JSON.stringify(value)));
  references.image = first.concat(references.image).filter((value) => !endings.has(JSON.stringify(value))).concat(last);
  for (const mediaType of Object.keys(references)) {
    const seen = new Set();
    const values = references[mediaType].filter((value) => {
      const key = JSON.stringify(value);
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
    if (values.length > 128) throw new Error("too many reference media items");
    if (values.length) body["input_" + mediaType + "s"] = values;
  }
  return body;
}

function decode(ctx) {
  if (!ctx.body || !["json", "multipart"].includes(ctx.body.kind)) throw new Error("JSON or multipart body required");
  let req = ctx.body.value;
  if (ctx.body.kind === "multipart") {
    req = {};
    for (const key of Object.keys(ctx.body.fields || {})) {
      const values = ctx.body.fields[key];
      if (values.length !== 1) throw new Error("request fields must be provided once");
      req[key] = values[0];
    }
    for (const key of ["metadata", "images", "reference_images", "reference_videos", "reference_audios", "input_images", "input_videos", "input_audios", "content"]) {
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
  return { kind: "submit", model, action: body.input_images || body.input_videos || body.input_audios ? "reference_to_video" : "text_to_video", requestBody: req };
}

export function buildSubmitRequest(ctx) {
  const key = trimmed(ctx.publicTaskId);
  if (key.length < 8 || key.length > 200) throw new Error("a host task ID is required for generation idempotency");
  return {
    url: apiRoot(ctx) + "/videos",
    method: "POST",
    headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json", "Idempotency-Key": key },
    body: videoRequest(ctx.requestBody, ctx.files, ctx.upstreamModel || ctx.model),
  };
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  const id = object(body) && trimmed(body.id);
  if (!id || id.length > 256) throw new Error("video submission response is missing a valid task ID");
  return { taskId: id };
}

export function extractUsage(ctx) {
  return { seconds: videoRequest(ctx.requestBody, ctx.files, ctx.upstreamModel || ctx.model).seconds };
}

export function extractUsageOnComplete(task, result, body) {
  if (!object(body)) return {};
  const value = body.seconds === undefined ? body.duration_seconds : body.seconds;
  const seconds = typeof value === "number" || typeof value === "string" ? Number(value) : NaN;
  return Number.isFinite(seconds) && seconds > 0 && seconds <= 3600 ? { seconds } : {};
}

export function buildQueryRequest(ctx) {
  return { url: apiRoot(ctx) + "/tasks/" + encodeURIComponent(ctx.taskId), method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(ctx, body) {
  const statuses = { created: "QUEUED", queued: "QUEUED", pending: "QUEUED", processing: "IN_PROGRESS", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE", canceled: "FAILURE", cancelled: "FAILURE" };
  const status = statuses[body && body.status] || "UNKNOWN";
  return status === "FAILURE" ? { status, reason: "Task failed" } : { status };
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return {
    url: apiRoot(ctx) + "/tasks/" + encodeURIComponent(ctx.upstreamTaskId) + "/content?delivery=stream",
    method: ctx.clientRequest.method,
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_video: {
    decodeRequest: decode,
    render: function () {
      // The host supplies public IDs and gateway artifact URLs; provider data stays private.
      return {};
    },
  },
};
