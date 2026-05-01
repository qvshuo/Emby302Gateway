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
const OPENLIST_TOKEN = env.OPENLIST_TOKEN ?? "";
const PORT = Number(env.PORT ?? 18096);
const CACHE_TTL = Number(env.CACHE_TTL ?? 180);

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
      const value = s
        .slice(i + 1)
        .trim()
        .replace(/^['"]|['"]$/g, "");
      if (key) out[key] = value;
    }
  } catch (e) {
    console.error(`[ERROR] env=load_failed file=.env error=${e}`);
  }
  return out;
}

function mustEnv(key: string): string {
  const value = env[key];
  if (!value) throw new Error(`Missing env: ${key}`);
  return value;
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

/** 判断是否为 STRM（支持首次播放前/后两种状态） */
function isStrm(s: any): boolean {
  return (
    (!s.IsRemote && !s.MediaStreams?.length) ||
    (s.IsRemote === true && !s.IsInfiniteStream) ||
    s.Container === "strm"
  );
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

async function proxy(req: Request, url: URL): Promise<Response> {
  return fetch(EMBY_HOST + url.pathname + url.search, {
    method: req.method,
    headers: req.headers,
    body: req.method !== "GET" && req.method !== "HEAD" ? req.body : undefined,
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
        ...(OPENLIST_TOKEN && { Authorization: OPENLIST_TOKEN }),
      },
      body: JSON.stringify({ path }),
    });

    if (!res.ok) {
      console.error(
        `[ERROR] fsGet=http_failed status=${res.status} path=${path}`,
      );
      return null;
    }

    const data = await res.json();
    if (data.code !== 200 || !data.data?.raw_url) {
      console.error(
        `[ERROR] fsGet=api_failed code=${data.code} message=${data.message ?? ""} path=${path}`,
      );
      return null;
    }

    return data.data.raw_url;
  } catch (e) {
    console.error(`[ERROR] fsGet=exception path=${path} error=${e}`);
    return null;
  }
}

async function getMediaPath(itemId: string, mediaSourceId: string | null) {
  const u = new URL("/Items", EMBY_HOST);
  u.searchParams.set("Ids", itemId);
  u.searchParams.set("Fields", "Path,MediaSources");
  u.searchParams.set("api_key", EMBY_API_KEY);

  const r = await fetch(u);
  if (!r.ok) throw new Error(`Emby ${r.status}`);

  const item = (await r.json())?.Items?.[0];
  if (!item?.MediaSources?.[0]) throw new Error("No MediaSource");

  const s = mediaSourceId
    ? item.MediaSources.find((m: any) => m.Id === mediaSourceId) ||
      item.MediaSources[0]
    : item.MediaSources[0];

  return { path: s.Path as string };
}

// ==================== HANDLERS ====================

/** PlaybackInfo 劫持：强制直链、禁用转码、指向网关 */
async function handlePlaybackInfo(req: Request, url: URL): Promise<Response> {
  const embyRes = await proxy(req, url);
  if (!embyRes.ok) return embyRes;

  // 先读 text 再 parse，避免 body 被消费后无法返回原响应
  const text = await embyRes.text();
  let body;
  try {
    body = JSON.parse(text);
  } catch {
    return new Response(text, {
      status: embyRes.status,
      headers: {
        "Content-Type": embyRes.headers.get("Content-Type") ?? "text/plain",
      },
    });
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
        const u = new URL(`/videos/${itemId}/stream`, url.href);
        u.searchParams.set("MediaSourceId", s.Id);
        u.searchParams.set("Static", "true");
        s.DirectStreamUrl = u.toString();
      }
    }
  }

  return new Response(JSON.stringify(body), {
    status: embyRes.status,
    headers: { "Content-Type": "application/json" },
  });
}

async function handleVideo(req: Request, url: URL): Promise<Response> {
  const itemId = parseId(url.pathname);
  if (!itemId) return new Response("Bad Request", { status: 400 });

  const mediaSourceId = url.searchParams.get("MediaSourceId");
  const ua = req.headers.get("user-agent") ?? "";

  const cacheKey = `${itemId}:${mediaSourceId ?? "default"}:${ua}`;

  const cached = getCache(cacheKey);
  if (cached) {
    console.log(`[INFO] cache=hit item=${itemId} url=${cached}`);
    return Response.redirect(cached, 302);
  }

  let ms;
  try {
    ms = await getMediaPath(itemId, mediaSourceId);
  } catch (e) {
    console.error(`[ERROR] emby=api_failed item=${itemId} error=${e}`);
    return new Response("Server Error", { status: 500 });
  }

  const openListPath = extractPath(ms.path);
  if (!openListPath) {
    console.error(`[ERROR] extract=failed item=${itemId} path=${ms.path}`);
    return new Response("Invalid URL", { status: 502 });
  }

  const rawUrl = await fsGet(openListPath, ua);
  if (!rawUrl) {
    console.error(`[ERROR] fsGet=failed item=${itemId} path=${openListPath}`);
    return new Response("API Error", { status: 502 });
  }

  console.log(`[INFO] cdn=ok item=${itemId} cached=false url=${rawUrl}`);
  setCache(cacheKey, rawUrl);
  return Response.redirect(rawUrl, 302);
}

// ==================== MAIN ====================

console.log(
  `[INFO] server=start port=${PORT} emby=${EMBY_HOST} openlist=${OPENLIST_ADDR}`,
);

Deno.serve({ port: PORT }, async (req) => {
  const url = new URL(req.url);
  const path = url.pathname;

  try {
    if (/\/Items\/[^/]+\/PlaybackInfo$/.test(path)) {
      return await handlePlaybackInfo(req, url);
    }

    if (
      path.includes("/videos/") &&
      (path.includes("/stream") || path.includes("/original"))
    ) {
      return await handleVideo(req, url);
    }

    return proxy(req, url);
  } catch (e) {
    console.error(
      `[ERROR] request=failed method=${req.method} path=${path} error=${e}`,
    );
    return new Response("Error", { status: 500 });
  }
});
