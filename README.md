# Emby302Gateway

通过 Deno 运行，将 Emby STRM 播放请求重定向到 OpenList 返回的 CDN 直链地址。

## 安装：

克隆 [Emby302Gateway](https://github.com/qvshuo/Emby302Gateway) 到本地：

```
git clone https://github.com/qvshuo/Emby302Gateway.git --depth=1 && cd Emby302Gateway
```

从 Deno 的 [Releases 页面](https://github.com/denoland/deno/releases) 直接下载单一可执行文件，或者：

```
curl -fsSL https://deno.land/install.sh | sh
```

## 配置：

```
cp .env.example .env
```

编辑 `.env` 文件，根据实际情况填写：

```
EMBY_HOST=http://localhost:8096         # Emby 地址
EMBY_API_KEY=your_emby_api_key          # Emby API Key
OPENLIST_ADDR=http://localhost:5244     # OpenList 地址
OPENLIST_TOKEN=your_openlist_token      # OpenList 令牌，获取路径：管理 → 设置 → 其他 → 令牌
PORT=18096                              # 网关端口，默认 18096
CACHE_TTL=180                           # 缓存有效期，默认 3 分钟
```

## 启动：

```
deno run --allow-net --allow-read=.env main.ts
```

后台运行：

```
nohup deno run --allow-net --allow-read=.env main.ts >> /tmp/emby302gateway.log 2>&1 &
```

## Emby 客户端：

将 Emby 客户端的服务器地址配置为 Deno 网关地址：`http://你的服务器IP:18096`；

播放 STRM 视频时，实际请求将通过 302 重定向解析为 OpenList 返回的 CDN 直链，从而不再受 Emby 所在设备的网络带宽限制；本地视频及其他无法解析为 OpenList 直链的资源，仍由 Emby 按原方式处理。

## 详细教程：

[OpenList + STRM + rclone + Emby + 302 重定向网关（Deno）的家庭影院部署方案

](https://anjing.art/posts/2026-01-16-OpenList%20+%20STRM%20+%20rclone%20+%20Emby%20+%20302%20重定向网关（Deno）的家庭影院部署方案)

## License

MIT
