export const meta = {
  apiVersion: 1,
  key: "async-image",
  name: "异步图片",
  icon: "text:异步图",
  version: "1.0.1",
  author: { name: "QuantumNous" },
  description: { en: "Image tasks with configurable submission and polling", zh: "可配置提交和查询接口的图片任务" },
  models: [],
  fetchMode: "per_task",
  upstreams: ["vendor"],
  protocols: ["openai_image"],
  usageSchema: {
    image_count: { type: "number", unit: "count", description: { en: "Image generation unit price", zh: "图片生成单价" } },
  },
  channelConfig: {
    type: "object",
    properties: {
      image_async_enabled: { type: "boolean", title: { en: "Enable asynchronous image responses", zh: "开启图片异步返回" }, default: false },
      submit_path: {
        type: "string",
        title: { en: "Submission path", zh: "任务提交路径" },
        default: "/v1/images/generations",
        examples: ["/v1/images/generations"],
        pattern: "^/[^/].*",
        maxLength: 1024,
      },
      edit_path: {
        type: "string",
        title: { en: "Image editing path", zh: "图片编辑路径" },
        default: "/v1/images/edits",
        examples: ["/v1/images/edits"],
        pattern: "^/[^/].*",
        maxLength: 1024,
      },
      submit_parameters: { type: "object", format: "json", title: { en: "Additional submission parameters", zh: "附加提交参数" } },
      query_path: {
        type: "string",
        title: { en: "Task query path", zh: "任务查询路径" },
        default: "/v1/tasks/{task_id}",
        examples: ["/v1/tasks/{task_id}"],
        pattern: "^/[^/].*\\{task_id\\}.*",
        maxLength: 1024,
      },
      task_id_field: { type: "string", title: { en: "Task ID field", zh: "任务编号字段" }, default: "task_no", maxLength: 128 },
      status_field: { type: "string", title: { en: "Task status field", zh: "任务状态字段" }, default: "status", maxLength: 128 },
      progress_field: { type: "string", title: { en: "Progress field", zh: "进度字段" }, default: "progress", maxLength: 128 },
      result_field: { type: "string", title: { en: "Image result field", zh: "图片结果字段" }, default: "result.data", maxLength: 128 },
      error_field: { type: "string", title: { en: "Error field", zh: "错误字段" }, default: "error", maxLength: 128 },
      success_status: {
        type: "string",
        title: { en: "Success status values (comma-separated)", zh: "成功状态值（逗号分隔）" },
        default: "completed,succeeded,success",
        maxLength: 256,
      },
      failure_status: {
        type: "string",
        title: { en: "Failure status values (comma-separated)", zh: "失败状态值（逗号分隔）" },
        default: "failed,failure,error,canceled,cancelled",
        maxLength: 256,
      },
    },
  },
};

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function config(ctx) {
  return ctx.state && ctx.state.config ? ctx.state.config : ctx.config || {};
}

function field(value, path) {
  if (!path) return value;
  for (const key of path.split(".")) {
    if (!value || typeof value !== "object" || !Object.prototype.hasOwnProperty.call(value, key)) return undefined;
    value = value[key];
  }
  return value;
}

function endpoint(base, path) {
  if (typeof path !== "string" || !/^\/[^/\\]/.test(path) || /[\\#\r\n]/.test(path)) throw new Error("use a relative API path on the channel address");
  return base.replace(/\/+$/, "").replace(/\/v1$/, "") + path;
}

function count(req) {
  if (req.n !== undefined && ((typeof req.n !== "number" && typeof req.n !== "string") || req.n === "")) throw new Error("n must be a number");
  const n = req.n === undefined ? 1 : Number(req.n);
  if (!Number.isInteger(n) || n < 1 || n > 128) throw new Error("n must be an integer between 1 and 128");
  return n;
}

function request(ctx) {
  const req = ctx.requestBody;
  if (!object(req) || typeof req.prompt !== "string" || !req.prompt.trim()) throw new Error("prompt is required");
  const extra = config(ctx).submit_parameters || {};
  // Billing and routing are derived from the same final body sent upstream.
  const body = Object.assign({}, req, extra, { model: ctx.upstreamModel || ctx.model });
  body.n = count(body);
  body.response_format = extra.response_format === undefined ? "url" : extra.response_format;
  delete body.stream;
  delete body.partial_images;
  delete body.metadata;
  return body;
}

function decode(ctx) {
  let req;
  if (ctx.body && ctx.body.kind === "json") req = ctx.body.value;
  else if (ctx.body && ctx.body.kind === "multipart") {
    req = {};
    for (const key of Object.keys(ctx.body.fields || {})) {
      if (ctx.body.fields[key].length !== 1) throw new Error(key + " must be provided once");
      req[key] = ctx.body.fields[key][0];
    }
  } else throw new Error("JSON or multipart body required");
  if (!object(req)) throw new Error("request body must be an object");
  if (req.stream === true || req.stream === "true") throw new Error("image task responses do not support stream=true");
  count(req);
  const body = {};
  for (const key of [
    "prompt",
    "n",
    "size",
    "quality",
    "aspect_ratio",
    "response_format",
    "seed",
    "style",
    "background",
    "output_format",
    "output_compression",
    "image",
    "images",
    "mask",
  ]) {
    if (req[key] !== undefined) body[key] = req[key];
  }
  return {
    kind: "submit",
    model: ctx.model,
    action: (ctx.body.files || []).length || req.image || req.images ? "image_to_image" : "text_to_image",
    requestBody: body,
  };
}

export function buildSubmitRequest(ctx) {
  const cfg = config(ctx),
    body = request(ctx),
    files = ctx.files || [];
  endpoint(ctx.baseUrl, cfg.query_path);
  if (!cfg.query_path.includes("{task_id}")) throw new Error("query path must contain {task_id}");
  const descriptor = {
    url: endpoint(ctx.baseUrl, files.length ? cfg.edit_path : cfg.submit_path),
    method: "POST",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
  if (files.length) {
    descriptor.bodyType = "multipart";
    descriptor.parts = Object.keys(body)
      .filter(function (key) {
        return !["image", "images", "mask"].includes(key);
      })
      .map(function (key) {
        return { name: key, value: typeof body[key] === "object" ? JSON.stringify(body[key]) : String(body[key]) };
      });
    for (const file of files) {
      if (!["image", "image[]", "mask"].includes(file.field)) throw new Error("unexpected image upload field");
      descriptor.parts.push({ name: file.field, fileRef: file.ref, filename: file.filename });
    }
  } else {
    descriptor.headers["Content-Type"] = "application/json";
    descriptor.body = body;
  }
  return descriptor;
}

function images(body, cfg) {
  let values = field(body, cfg.result_field);
  if (values === undefined && object(body) && body.data !== undefined) values = body.data;
  if (typeof values === "string" || object(values)) values = [values];
  if (!Array.isArray(values)) return [];
  if (values.length > 128) throw new Error("too many result images");
  const result = [];
  for (const value of values) {
    const item = typeof value === "string" ? { url: value } : value;
    if (!object(item)) continue;
    const image = {};
    if (typeof item.url === "string" && item.url) {
      if (!/^https?:\/\//i.test(item.url)) throw new Error("invalid result image URL");
      image.url = item.url;
    }
    if (typeof item.revised_prompt === "string") image.revised_prompt = item.revised_prompt;
    if (image.url) result.push(image);
  }
  return result;
}

function status(ctx, body) {
  const cfg = config(ctx),
    value = String(field(body, cfg.status_field) || "").toLowerCase();
  const result = { status: "IN_PROGRESS" };
  if (
    cfg.success_status.split(",").some(function (item) {
      return item.trim().toLowerCase() === value;
    })
  ) {
    const output = images(body, cfg);
    if (!output.length) throw new Error("completed image task has no image result; check the result field mapping");
    result.status = "SUCCESS";
    if (output[0].url) result.url = output[0].url;
  } else if (
    cfg.failure_status.split(",").some(function (item) {
      return item.trim().toLowerCase() === value;
    })
  ) {
    result.status = "FAILURE";
    const error = field(body, cfg.error_field);
    result.reason = typeof error === "string" ? error : object(error) && typeof error.message === "string" ? error.message : "upstream image task failed";
  } else if (["queued", "pending", "submitted"].includes(value)) result.status = "QUEUED";
  else if (!["processing", "running", "in_progress"].includes(value)) throw new Error("unrecognized image task status; check the status field mapping");
  const progress = Number(String(field(body, cfg.progress_field) || "").replace("%", ""));
  if (Number.isFinite(progress) && progress >= 0 && progress <= 100) result.progress = progress + "%";
  return result;
}

export function parseSubmitResponse(ctx, response) {
  const cfg = config(ctx),
    body = response.body;
  const state = { config: cfg, baseUrl: ctx.baseUrl };
  const id = field(body, cfg.task_id_field);
  if (typeof id === "string" && id.trim() && id.length <= 200) return { taskId: id, taskData: {}, state: state };
  const output = images(body, cfg);
  if (output.length) return { taskId: ctx.publicTaskId, taskData: { data: output }, state: state, immediate: { status: "SUCCESS", progress: "100%" } };
  throw new Error("submission response is missing the upstream task ID");
}

export function buildQueryRequest(ctx) {
  const cfg = config(ctx);
  if (!cfg.query_path.includes("{task_id}")) throw new Error("query path must contain {task_id}");
  return {
    url: endpoint((ctx.state && ctx.state.baseUrl) || ctx.baseUrl, cfg.query_path.replaceAll("{task_id}", encodeURIComponent(ctx.taskId))),
    method: "GET",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  return status(ctx, body);
}

export function extractUsage(ctx) {
  return { image_count: request(ctx).n };
}

export function extractUsageOnComplete(ctx, _task, body) {
  const output = images(body, config(ctx));
  const urls = output.filter(function (item) {
    return item.url;
  }).length;
  return output.length ? { image_count: urls } : {};
}

export function listArtifacts(ctx) {
  if (ctx.status !== "SUCCESS") return [];
  return images(ctx.data, config(ctx)).map(function (_item, index) {
    return { key: "image-" + (index + 1), type: "image" };
  });
}

export function buildContentRequest(ctx) {
  const output = images(ctx.data, config(ctx));
  const index = output.findIndex(function (_item, position) {
    return ctx.artifactKey === "image-" + (position + 1);
  });
  if (index < 0) throw new Error("artifact_not_found");
  return { url: output[index].url, method: ctx.clientRequest.method, credentialless: true };
}

export const protocols = {
  openai_image: {
    decodeRequest: decode,
    render: function (ctx, task) {
      return { data: images(task.data, config(ctx)) };
    },
  },
};
