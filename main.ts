/**
 * Emby 302 Gateway
 * 核心：STRM → OpenList API → raw_url → 302 CDN
 * 运行：deno run --allow-net --allow-read=.env main.ts
 */

// ==================== CONFIG ====================

const env = loadEnv();
const EMBY_HOST = mustEnv("EMBY_HOST");
const EMBY_API_KEY = mustEnv("EMBY_API_KEY");
const OPENLIST_ADDR = mustEnv("OPENLIST_ADDR");
const OPENLIST_TOKEN = mustEnv("OPENLIST_TOKEN");
const PORT = mustPositiveInt("PORT", env.PORT, 18096);
const CACHE_TTL = mustPositiveInt("CACHE_TTL", env.CACHE_TTL, 180);

interface MediaSource {
  Id?: string;
  Name?: string;
  Path?: string;
  IsRemote?: boolean;
  IsInfiniteStream?: boolean;
  SupportsDirectPlay?: boolean;
  SupportsDirectStream?: boolean;
  SupportsTranscoding?: boolean;
  TranscodingUrl?: string;
  TranscodingSubProtocol?: string;
  TranscodingContainer?: string;
  DirectStreamUrl?: string;
}

interface EmbyItem {
  MediaSources?: MediaSource[];
}

interface ItemsResponse {
  Items?: EmbyItem[];
}

interface PlaybackInfoResponse {
  MediaSources?: MediaSource[];
  [key: string]: unknown;
}

interface OpenListFsGetResponse {
  code?: number;
  message?: string;
  data?: { raw_url?: string };
}

function loadEnv(): Record<string, string> {
  const out: Record<string, string> = {};
  try {
    const envFile = new URL(".env", import.meta.url);
    for (const line of Deno.readTextFileSync(envFile).split(/\r?\n/)) {
      const s = line.trim();
      if (!s || s.startsWith("#")) continue;
      const i = s.indexOf("=");
      if (i < 0) continue;
      const key = s.slice(0, i).trim();
      const value = s.slice(i + 1).trim().replace(/^['"]|['"]$/g, "");
      if (key) out[key] = value;
    }
  } catch (e) {
    console.error(`[env] load_failed file=.env error=${e}`);
  }
  return out;
}

function mustEnv(key: string): string {
  const value = env[key];
  if (!value) throw new Error(`Missing env: ${key}`);
  return value;
}

function mustPositiveInt(
  key: string,
  value: string | undefined,
  fallback: number,
): number {
  const n = Number(value ?? fallback);
  if (!Number.isInteger(n) || n <= 0) {
    throw new Error(`Invalid env: ${key} must be a positive integer`);
  }
  return n;
}

function jsonResponse(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function textResponse(
  text: string,
  status: number,
  contentType = "text/plain",
): Response {
  return new Response(text, {
    status,
    headers: { "Content-Type": contentType },
  });
}

// ==================== CACHE ====================
const cache = new Map<string, { url: string; exp: number }>();

function getCache(key: string): string | null {
  const e = cache.get(key);
  if (!e || Date.now() > e.exp) {
    if (e) cache.delete(key);
    return null;
  }
  return e.url;
}

function setCache(key: string, url: string): void {
  cache.set(key, { url, exp: Date.now() + CACHE_TTL * 1000 });
}

// ==================== UTILS ====================

/** 判断是否为 STRM（已扫描入库的远程文件，IsRemote=true 且非直播流） */
function isStrm(s: MediaSource): boolean {
  return s.IsRemote === true && !s.IsInfiniteStream;
}

function extractPath(httpUrl: string): string | null {
  try {
    const match = new URL(httpUrl).pathname.match(/^\/d(\/.+)$/);
    return match?.[1] ? decodeURIComponent(match[1]) : null;
  } catch {
    return null;
  }
}

function parseId(path: string): string | null {
  return path.match(/\/(videos|items)\/([^\/]+)/i)?.[2] ?? null;
}

function sourceLabel(mediaSourceId: string | null): string {
  return mediaSourceId ?? "default";
}

function proxiedHeaders(upstreamHeaders: Headers): Headers {
  const headers = new Headers(upstreamHeaders);
  headers.delete("Content-Encoding");
  headers.delete("Content-Length");
  headers.delete("Transfer-Encoding");
  return headers;
}

async function proxy(req: Request, url: URL): Promise<Response> {
  const headers = new Headers(req.headers);
  headers.set("Accept-Encoding", "identity");
  const res = await fetch(EMBY_HOST + url.pathname + url.search, {
    method: req.method,
    headers,
    body: req.method !== "GET" && req.method !== "HEAD" ? req.body : undefined,
    redirect: "manual",
  });
  return new Response(res.body, {
    status: res.status,
    statusText: res.statusText,
    headers: proxiedHeaders(res.headers),
  });
}

// ==================== API ====================

/** raw_url 与请求 UA 绑定，需传递客户端 UA */
async function fsGet(path: string, ua: string): Promise<string | null> {
  try {
    const res = await fetch(`${OPENLIST_ADDR}/api/fs/get`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "User-Agent": ua,
        Authorization: OPENLIST_TOKEN,
      },
      body: JSON.stringify({ path }),
    });

    if (!res.ok) {
      console.error(`[openlist] http_failed status=${res.status} path=${path}`);
      return null;
    }

    const data = await res.json() as OpenListFsGetResponse;
    if (data.code !== 200 || !data.data?.raw_url) {
      console.error(
        `[openlist] api_failed code=${data.code} message=${
          data.message ?? ""
        } path=${path}`,
      );
      return null;
    }

    return data.data.raw_url;
  } catch (e) {
    console.error(`[openlist] exception path=${path} error=${e}`);
    return null;
  }
}

async function getMediaPath(
  itemId: string,
  mediaSourceId: string | null,
): Promise<{ path: string } | null> {
  const u = new URL("/Items", EMBY_HOST);
  u.searchParams.set("Ids", itemId);
  u.searchParams.set("Fields", "Path,MediaSources");
  u.searchParams.set("api_key", EMBY_API_KEY);

  const r = await fetch(u);
  if (!r.ok) throw new Error(`Emby ${r.status}`);

  const item = ((await r.json()) as ItemsResponse)?.Items?.[0];
  if (!item?.MediaSources?.[0]) throw new Error("No MediaSource");

  const s = mediaSourceId
    ? item.MediaSources.find((m) => m.Id === mediaSourceId)
    : item.MediaSources[0];

  if (!s?.Path) return null;

  return { path: s.Path };
}

// ==================== HANDLERS ====================

/** basehtmlplayer.js 改写：移除远程直链的 CORS 限制 */
async function handleBaseHtmlPlayer(req: Request, url: URL): Promise<Response> {
  const embyRes = await proxy(req, url);
  if (!embyRes.ok) return embyRes;

  const body = await embyRes.text();
  const modified = body.replace(
    /mediaSource\.IsRemote\s*&&\s*"DirectPlay"\s*===\s*playMethod\s*\?\s*null\s*:\s*"anonymous"/g,
    "null",
  );
  console.log(
    `[player] basehtmlplayer patched=${modified !== body} path=${url.pathname}`,
  );
  return textResponse(modified, embyRes.status, "application/javascript");
}

/** System/Info 改写：将端口替换为网关端口，防止客户端绕过网关 */
async function handleSystemInfo(req: Request, url: URL): Promise<Response> {
  const embyRes = await proxy(req, url);
  if (!embyRes.ok) return embyRes;

  const text = await embyRes.text();
  let body: Record<string, unknown>;
  try {
    body = JSON.parse(text) as Record<string, unknown>;
  } catch {
    return textResponse(text, embyRes.status);
  }

  const upstreamWsPort = body.WebSocketPortNumber;
  const upstreamHttpPort = body.HttpServerPortNumber;
  body.WebSocketPortNumber = PORT;
  body.HttpServerPortNumber = PORT;
  console.log(
    `[system] info_port_rewrite ws=${upstreamWsPort}->${PORT} http=${upstreamHttpPort}->${PORT}`,
  );
  return jsonResponse(body, embyRes.status);
}

/** PlaybackInfo 劫持：强制直链、禁用转码、指向网关 */
async function handlePlaybackInfo(req: Request, url: URL): Promise<Response> {
  const embyRes = await proxy(req, url);
  if (!embyRes.ok) return embyRes;

  const text = await embyRes.text();
  let body: PlaybackInfoResponse;
  try {
    body = JSON.parse(text) as PlaybackInfoResponse;
  } catch {
    return textResponse(
      text,
      embyRes.status,
      embyRes.headers.get("Content-Type") ?? "text/plain",
    );
  }

  const itemId = parseId(url.pathname);

  if (body.MediaSources) {
    for (const s of body.MediaSources) {
      if (!s || !isStrm(s)) continue;

      s.SupportsDirectPlay = true;
      s.SupportsDirectStream = true;
      s.SupportsTranscoding = false;
      delete s.TranscodingUrl;
      delete s.TranscodingSubProtocol;
      delete s.TranscodingContainer;

      if (itemId && s.Id) {
        const streamPath = url.pathname
          .replace(/^.*\/Items/i, "/videos")
          .replace(/PlaybackInfo/i, "stream");
        const params = new URLSearchParams(url.search);
        params.set("MediaSourceId", s.Id);
        params.set("Static", "true");
        if (!params.has("api_key") && !params.has("X-Emby-Token")) {
          params.set("api_key", EMBY_API_KEY);
        }
        s.DirectStreamUrl = streamPath + "?" + params.toString();
        console.log(
          `[playback] strm_forced item=${itemId} source=${s.Id} name="${
            s.Name ?? ""
          }" direct_stream_path=${streamPath}`,
        );
      }
    }
  }

  return jsonResponse(body, embyRes.status);
}

async function handleVideo(req: Request, url: URL): Promise<Response> {
  const itemId = parseId(url.pathname);
  const ua = req.headers.get("user-agent") ?? "";
  if (!itemId) {
    console.error(`[video] bad_request path=${url.pathname}`);
    return new Response("Bad Request", { status: 400 });
  }

  const mediaSourceId = url.searchParams.get("MediaSourceId");
  const source = sourceLabel(mediaSourceId);
  if (req.method === "HEAD") {
    console.log(`[video] head_proxy item=${itemId} source=${source}`);
    return proxy(req, url);
  }

  const cacheKey = `${itemId}:${source}:${ua}`;

  const cached = getCache(cacheKey);
  if (cached) {
    console.log(
      `[video] cache_hit item=${itemId} source=${source} url=${cached}`,
    );
    return Response.redirect(cached, 302);
  }
  console.log(`[video] cache_miss item=${itemId} source=${source}`);

  let ms;
  try {
    ms = await getMediaPath(itemId, mediaSourceId);
  } catch (e) {
    console.error(`[emby] item_query_failed item=${itemId} error=${e}`);
    return new Response("Server Error", { status: 500 });
  }
  if (!ms) {
    console.log(
      `[video] media_source_not_found item=${itemId} source=${source} action=proxy`,
    );
    return proxy(req, url);
  }

  const openListPath = extractPath(ms.path);
  if (!openListPath) {
    console.log(
      `[video] non_openlist_path item=${itemId} source=${source} action=proxy path=${ms.path}`,
    );
    return proxy(req, url);
  }

  console.log(
    `[video] openlist_path item=${itemId} source=${source} path=${openListPath}`,
  );

  const rawUrl = await fsGet(openListPath, ua);
  if (!rawUrl) {
    console.error(
      `[video] cdn_resolve_failed item=${itemId} path=${openListPath}`,
    );
    return new Response("API Error", { status: 502 });
  }

  console.log(`[video] redirect item=${itemId} url=${rawUrl}`);
  setCache(cacheKey, rawUrl);
  return Response.redirect(rawUrl, 302);
}

// ==================== MAIN ====================

console.log(
  `[server] start port=${PORT} emby=${EMBY_HOST} openlist=${OPENLIST_ADDR}`,
);

Deno.serve({ port: PORT }, async (req) => {
  const url = new URL(req.url);
  const path = url.pathname;

  try {
    if (path.endsWith("/basehtmlplayer.js")) {
      return await handleBaseHtmlPlayer(req, url);
    }

    const lowerPath = path.toLowerCase();

    if (lowerPath.endsWith("/system/info")) {
      return await handleSystemInfo(req, url);
    }

    if (/\/items\/[^/]+\/playbackinfo$/.test(lowerPath)) {
      return await handlePlaybackInfo(req, url);
    }

    if (
      lowerPath.includes("/videos/") &&
      (lowerPath.includes("/stream") || lowerPath.includes("/original"))
    ) {
      return await handleVideo(req, url);
    }

    return proxy(req, url);
  } catch (e) {
    console.error(
      `[request] failed method=${req.method} path=${path} error=${e}`,
    );
    return new Response("Error", { status: 500 });
  }
});
