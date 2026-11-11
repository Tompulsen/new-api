export const meta = {
  apiVersion: 1,
  key: "async-compatible",
  name: "异步兼容",
  icon: "text:异步",
  description: {
    en: "Asynchronous video generation through /v1/generations with uploaded reference media",
    zh: "通过 /v1/generations 异步生成视频，支持上传参考素材",
  },
  version: "1.0.0",
  author: { name: "QuantumNous" },
  models: [
    "seedance-2.0-official",
    "seedance-2.0-fast-official",
    "seedance-2.5-official",
    "sd-2.5-fullblood-line3",
    "sd-2.5-fullblood",
    "sd-2.5-fullblood-line2",
    "seedance-2.5-dedicated-line1",
    "wan3.0",
  ],
  fetchMode: "per_task",
  upstreams: ["vendor"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
    resolution: { enum: ["480p", "720p", "1080p"], description: { en: "Output video resolution", zh: "输出视频分辨率" } },
  },
  usageExamples: [
    { label: "720p 5s", facts: { seconds: 5, resolution: "720p" } },
    { label: "1080p 10s", facts: { seconds: 10, resolution: "1080p" } },
  ],
  routes: [
    { method: "POST", path: "/v1/generations", type: "submit", decode: "createGeneration", render: "generation" },
    { method: "GET", path: "/v1/generations/:task_id", type: "query", render: "generation" },
  ],
  protocols: ["openai_video"],
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

// Host placeholders are created here, never accepted from a client or metadata.
function rejectClientPlaceholders(value) {
  if (!value || typeof value !== "object") return;
  for (const key of Object.keys(value)) {
    if (key === "__fileRef" || key === "__videoUpload") throw new Error("attach reference media instead of supplying host file placeholders");
    rejectClientPlaceholders(value[key]);
  }
}

function selected(root, metadata, keys) {
  for (const source of [root, metadata]) {
    for (const key of keys) {
      if (source[key] !== undefined && source[key] !== null) return source[key];
    }
  }
}

function reference(value, type) {
  if (object(value) && object(value.image_url)) value = value.image_url;
  if (object(value)) value = value.url === undefined ? value.image_url : value.url;
  if (typeof value !== "string") throw new Error("reference media must be an uploaded attachment or base64 data URL");
  if (/^file_/i.test(value)) throw new Error("upstream file IDs cannot be shared between gateway users; attach the reference media instead");
  if (!value.toLowerCase().startsWith("data:" + type + "/"))
    throw new Error("remote reference URLs are not supported; attach the reference media or use a base64 data URL");
  return { __videoUpload: { dataUrl: value, mediaType: type } };
}

function videoRequest(req, files, model) {
  if (!object(req)) throw new Error("request body must be an object");
  if (req.metadata !== undefined && !object(req.metadata)) throw new Error("metadata must be an object");
  rejectClientPlaceholders(req);
  const metadata = req.metadata || {};
  const values = Object.assign({}, metadata, req);
  values.model = model;
  if (!trimmed(values.prompt)) throw new Error("prompt is required");
  // One explicit duration keeps reservation independent of per-model defaults.
  const duration = selected(req, metadata, ["seconds", "duration"]);
  if (duration !== undefined && typeof duration !== "number" && typeof duration !== "string") throw new Error("duration must be a number");
  values.duration = duration === undefined ? 5 : Number(duration);
  if (!Number.isFinite(values.duration) || values.duration <= 0 || values.duration > 3600)
    throw new Error("duration must be greater than 0 and at most 3600 seconds");
  const dimensions = /^(\d+)[x:](\d+)$/.exec(trimmed(values.size));
  const width = dimensions ? Number(dimensions[1]) : 0;
  const height = dimensions ? Number(dimensions[2]) : 0;
  let resolution = trimmed(selected(req, metadata, ["resolution", "resolution_name"])).toLowerCase();
  if (!resolution || resolution === "auto") resolution = width > 0 && height > 0 ? Math.min(width, height) + "p" : "720p";
  values.resolution = resolution;
  let ratio = trimmed(selected(req, metadata, ["aspect_ratio", "aspectRatio", "ratio"]));
  if (!ratio || ratio === "auto") {
    ratio = "16:9";
    if (width > 0 && height > 0 && Number.isSafeInteger(width) && Number.isSafeInteger(height)) {
      let divisor = width,
        rest = height;
      while (rest) {
        const next = divisor % rest;
        divisor = rest;
        rest = next;
      }
      ratio = width / divisor + ":" + height / divisor;
    }
  }
  values.aspect_ratio = ratio;
  // Optional paid image editing is a separate service, outside video pricing.
  if (values.reference_image_processing && values.reference_image_processing !== "direct")
    throw new Error("paid reference image processing is not supported by this video plugin");
  for (const type of ["image", "video", "audio"]) {
    const key = "reference_" + type + "s";
    if (values[key] !== undefined && !Array.isArray(values[key])) throw new Error(key + " must be an array");
    values[key] = (values[key] || []).map(function (item) {
      return reference(item, type);
    });
  }
  const firstFrames = [],
    images = [],
    lastFrames = [];
  for (const key of ["first_frame", "first_frame_image", "image", "input_reference"]) {
    if (values[key] !== undefined && values[key] !== null) firstFrames.push(values[key]);
  }
  if (values.images !== undefined && !Array.isArray(values.images)) throw new Error("images must be an array");
  images.push.apply(images, values.images || []);
  for (const key of ["last_frame", "last_frame_image"]) {
    if (values[key] !== undefined && values[key] !== null) lastFrames.push(values[key]);
  }
  if (values.content !== undefined) {
    if (!Array.isArray(values.content)) throw new Error("content must be an array");
    for (const item of values.content) {
      if (!object(item) || item.type !== "image_url")
        throw new Error("content supports image_url references; put video/audio references in reference_videos/reference_audios");
      if (item.role === "first_frame") firstFrames.push(item.image_url);
      else if (item.role === "last_frame") lastFrames.push(item.image_url);
      else if (!item.role || item.role === "reference_image" || item.role === "middle_frame") images.push(item.image_url);
      else throw new Error("unsupported reference image role");
    }
  }
  for (const file of files || []) {
    const field = file.field.replace(/\[\]$/, "");
    const types = { input_reference: "image", images: "image", reference_images: "image", reference_videos: "video", reference_audios: "audio" };
    const type = types[field];
    if (!type) throw new Error("unexpected reference file field: " + file.field);
    values["reference_" + type + "s"].push({ __videoUpload: { fileRef: file.ref, mediaType: type } });
  }
  const endings = lastFrames.map(function (item) {
    return reference(item, "image");
  });
  const endingSources = new Set(
    endings.map(function (item) {
      return JSON.stringify(item);
    })
  );
  values.reference_images = firstFrames
    .concat(images)
    .map(function (item) {
      return reference(item, "image");
    })
    .concat(values.reference_images)
    .filter(function (item) {
      return !endingSources.has(JSON.stringify(item));
    })
    .concat(endings);
  for (const key of [
    "seconds",
    "size",
    "metadata",
    "resolution_name",
    "aspectRatio",
    "ratio",
    "images",
    "image",
    "input_reference",
    "first_frame",
    "last_frame",
    "first_frame_image",
    "last_frame_image",
    "vendorBody",
    "content",
  ])
    delete values[key];
  for (const key of ["reference_images", "reference_videos", "reference_audios"]) {
    const seen = new Set();
    values[key] = values[key].filter(function (item) {
      const source = JSON.stringify(item);
      if (seen.has(source)) return false;
      seen.add(source);
      return true;
    });
    if (values[key].length > 128) throw new Error("too many reference media items");
    if (!values[key].length) delete values[key];
  }
  return values;
}

function decode(ctx) {
  if (!ctx.body || !["json", "multipart"].includes(ctx.body.kind)) throw new Error("JSON or multipart body required");
  let req;
  if (ctx.body.kind === "json") req = ctx.body.value;
  else {
    req = {};
    for (const name of Object.keys(ctx.body.fields || {})) {
      const values = ctx.body.fields[name];
      if (values.length !== 1) throw new Error(name + " must be provided once");
      req[name] = values[0];
    }
    for (const name of ["metadata", "images", "reference_images", "reference_videos", "reference_audios"]) {
      if (req[name] !== undefined) {
        try {
          req[name] = JSON.parse(req[name]);
        } catch (e) {
          throw new Error(name + " must contain valid JSON", { cause: e });
        }
      }
    }
  }
  const model = ctx.model || (object(req) && trimmed(req.model));
  if (!model) throw new Error("model is required");
  const values = videoRequest(req, ctx.body.files || [], model);
  return {
    kind: "submit",
    model: model,
    action: values.reference_images || values.reference_videos || values.reference_audios ? "reference_to_video" : "text_to_video",
    requestBody: req,
  };
}

export function buildSubmitRequest(ctx) {
  const key = trimmed(ctx.publicTaskId);
  if (key.length < 8 || key.length > 200) throw new Error("a host task ID is required for generation idempotency");
  return {
    url: apiRoot(ctx) + "/generations",
    method: "POST",
    headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json", "Idempotency-Key": key },
    body: videoRequest(ctx.requestBody, ctx.files || [], ctx.upstreamModel || ctx.model),
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp.body;
  if (!object(body) || !trimmed(body.id)) throw new Error("generation response is missing a task ID");
  return { taskId: body.id, taskData: body };
}

export function extractUsage(ctx) {
  const body = videoRequest(ctx.requestBody, ctx.files || [], ctx.upstreamModel || ctx.model);
  const facts = { seconds: body.duration };
  const match = /^(\d+)p$/.exec(body.resolution);
  if (match && Number(match[1]) > 0) {
    const edge = Number(match[1]);
    facts.resolution = edge <= 480 ? "480p" : edge < 1080 ? "720p" : "1080p";
  }
  return facts;
}

export function extractUsageOnComplete(task, result, body) {
  const facts = {};
  if (!object(body)) return facts;
  const seconds = Number(body.duration);
  if (Number.isFinite(seconds) && seconds > 0 && seconds <= 3600) facts.seconds = seconds;
  const dimensions = /^(\d+)[x:](\d+)$/.exec(trimmed(body.size));
  const width = Number(body.width === undefined ? dimensions && dimensions[1] : body.width);
  const height = Number(body.height === undefined ? dimensions && dimensions[2] : body.height);
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
  return { url: apiRoot(ctx) + "/generations/" + encodeURIComponent(ctx.taskId), method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(ctx, body) {
  const statuses = {
    created: "QUEUED",
    queued: "QUEUED",
    processing: "IN_PROGRESS",
    review: "IN_PROGRESS",
    succeeded: "SUCCESS",
    failed: "FAILURE",
    canceled: "FAILURE",
  };
  const result = { status: statuses[body && body.status] || "UNKNOWN" };
  if (result.status === "UNKNOWN") result.reason = "unrecognized generation status";
  if (result.status === "FAILURE") {
    const error = body.error;
    result.reason = trimmed(object(error) ? error.message : error) || trimmed(body.fail_reason) || "generation failed";
  }
  return result;
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" && task.data && trimmed(task.data.result_url) ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video" || !ctx.data || !trimmed(ctx.data.result_url)) throw new Error("artifact_not_found");
  return { url: ctx.data.result_url, method: ctx.clientRequest.method, credentialless: true };
}

export const native = {
  createGeneration: decode,
  generation: function (ctx, task) {
    const states = { NOT_START: "created", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "processing", SUCCESS: "succeeded", FAILURE: "failed" };
    const body = Object.assign({}, task.data || {}, { id: task.task_id, object: "video" });
    if (!body.status) body.status = states[task.status] || "unknown";
    if (task.status === "FAILURE") body.status = "failed";
    return body;
  },
};

export const protocols = {
  openai_video: {
    decodeRequest: decode,
    render: function (ctx, task) {
      return Object.assign({}, task.data || {});
    },
  },
};
