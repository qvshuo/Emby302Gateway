# Emby302Gateway 与 embyExternalUrl 对比报告

评估日期：2026-10-11。

> 此报告描述优化前的 Go 迁移版本；后续实现已按“仅 OpenList STRM 302，其余透传”调整。当前行为以 README.md 和 main.go 为准。

## 1. 结论与评估范围

**针对目前“Emby + OpenList + HTTP STRM + 115 CDN”的需求，建议继续使用当前 Go 网关，按实际需要补充少量兼容性和可靠性措施。** 当需求扩展到 rclone/CD2 挂载路径转直链、Jellyfin/Plex、音频下载或复杂转码策略时，embyExternalUrl 的现成能力更有价值。

embyExternalUrl 是多个工具的集合，包含 `emby2Alist`、`plex2Alist`、服务端外部播放器链接及浏览器脚本。与当前网关直接对应的是 **emby2Alist**；其他子项目是扩展能力，不应都计入一个基础 302 服务的部署要求。参见[仓库总览](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/README.zh-Hans.md)。

本报告基于当前工作区 Go 版本及对方 `main` 分支快照 `91798cc2ce5e14646c6b939b9f5d1d8d137541e9`，该快照提交时间为 2025-10-27。当前 Go 改动尚未提交，因此没有对应的新 Git 提交号。对方进行了源码与配置审查，**没有部署或压测**；本项目的线上验证结果来自本次迁移。功能存在、默认开关及性能实测分别说明。

## 2. 架构与维护规模

| 项目 | 当前 Emby302Gateway | emby2Alist |
| --- | --- | --- |
| 运行架构 | Go `net/http` 单进程，全部逻辑在 `main.go` | Nginx 反向代理 + njs JavaScript 模块 |
| 核心依赖 | Go 标准库；编译后独立可执行文件 | Nginx、njs，以及配套 JS/配置文件 |
| 配置 | `.env`：4 个必填项、2 个可选项 | `constant.js` 及 mount/pro/strm/transcode/ext 等分拆配置 |
| 代码规模 | 运行代码 386 行；测试 185 行 | `emby2Alist` 目录有 28 个 JS 文件、4,924 行；7 个 `.conf` 文件、528 行 |
| 缓存 | 进程内 URL map + mutex + TTL | Nginx worker 共享字典；路由缓存、图片和字幕磁盘缓存 |
| TLS | 当前入口只监听 HTTP，可在外层加 TLS 代理 | 提供 HTTPS 配置模板，默认主配置未启用该 include |
| 部署 | 单个网关容器、只读 `.env`、host 网络 | 单个 Nginx 容器即可连接现有 Emby/Alist；挂载多份配置及缓存/日志目录 |

行数统计包含注释、空行和对方示例配置，反映维护范围，不代表运行成本或代码质量。本项目无需数据库；对方基本直链服务也不要求数据库。不能把 Nginx+njs 等同于 Node.js，或认为对方必须额外部署 rclone、Alist 才能使用已有服务。

依据：[Compose](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/docker/docker-compose.yml)、[Nginx 主配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/nginx.conf)、[服务配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.conf)、本地 `main.go`、`go.mod`、`Dockerfile`、`compose.yml`。

## 3. 功能对照

“透传”表示请求仍由 Emby 处理，不表示相关功能被网关增强或重定向。

| 能力 | 当前 Go 网关 | emby2Alist |
| --- | --- | --- |
| OpenList/Alist `/api/fs/get` → 302 | 支持，携带客户端 UA 与固定 Token | 支持挂载路径解析；HTTP STRM 也可走链接重定向解析 |
| HTTP `/d/...` STRM | 提取路径，调用配置中的 OpenList | 可按规则探测原链接的 Location，或做路径映射 |
| 相对路径 STRM，如 `/d/115/a.mkv` | 不识别为 OpenList HTTP URL，透传 | 提供相对路径映射及更广的 STRM 判定，需正确配置 |
| rclone/CD2 路径转 CDN | 不支持，透传 | 支持挂载根目录剥离、路径映射和软链接处理 |
| 任意远程 HTTP STRM | PlaybackInfo 会强制直放；视频 URL 非 `/d/...` 则透传 | 支持直接重定向，及按规则获取一跳 Location |
| 本地媒体 PlaybackInfo | 保持原样，包括原有转码能力 | 遍历媒体源改写直放标记；转码默认配置会禁用 `SupportsTranscoding` |
| 指定媒体源不存在 | 透传，避免错误选择第一源 | 查找失败进入错误处理；默认回源 |
| 客户端视频 HEAD | 直接透传 Emby，不解析 OpenList | 视频、音频、下载等路由配置了 HEAD 透传 |
| 视频直链路由 | `/videos/.../stream` 或 `/original` | 视频串流、直播/master，以及其他专用路由 |
| 音频、网页下载、客户端同步下载 | 透传 | 专门支持 Audio、Items/Download、Sync/JobItems/File |
| WebSocket | 无隧道；Upgrade/Connection 被移除 | 专门代理 `/socket`、`/embywebsocket` 并保留 Upgrade |
| 网页 CORS 补丁 | Emby `basehtmlplayer.js` | 相同 Emby 表达式补丁，另有 Jellyfin 前端补丁 |
| System/Info | 改写两个端口字段 | 另改写 Local/RemoteAddresses 与旧版单地址字段 |
| 按客户端/路径选择代理或直链 | 无配置规则 | 支持 UA、用户、设备、路径、媒体属性等分组规则 |
| 转码策略与多服务器 | 远程非直播禁用转码；本地/直播由 Emby 处理 | 可开启转码策略；另有实验性的分布式媒体服务方案 |
| 多版本排序、虚拟 HLS 源 | 不支持 | 有对应模块与配置；115 HLS 默认关闭 |
| 图片、字幕缓存 | 无，直接透传 | Nginx 文件缓存及字幕适配路由 |
| 外部播放器按钮 | 不支持 | 仓库其他子项目支持，不是基础 emby2Alist 自动附带的能力 |
| Jellyfin/Plex | 未针对适配、未验证 | emby2Alist 明确适配 Jellyfin；Plex 有独立子项目 |

依据：[直链与 PlaybackInfo 实现](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.js)、[STRM 判定与路径工具](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/common/util.js)、[路由配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.conf)、[System/Info 改写](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/modules/emby-system.js)、[转码配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/config/constant-transcode.js)、[扩展配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/config/constant-ext.js)。

### 三个具体场景

1. `http://openlist:5244/d/115/movie.mkv`：Go 网关明确提取 `/115/movie.mkv`，统一走配置的 OpenList API；对方的 HTTP STRM 路径可能直接返回原链接或通过 HEAD 取得 Location，取决于规则。最终都能实现客户端直连，但解析路径并不完全相同。
2. `/mnt/115/movie.mkv`：Go 透传 Emby；对方可以把 `/mnt` 去掉，将 `/115/movie.mkv` 交给 Alist API，从而让已有挂载库获得直链。
3. `/data/movie.mkv`：Go 保留上游 PlaybackInfo。对方默认 `modifyDirectPlayInfo` 会遍历所有媒体源，先强制直放，并在转码关闭时禁用转码；因此直接套用默认配置可能影响本地媒体客户端的播放策略。

需要区分当前 Go 的两层判定：PlaybackInfo 按 `IsRemote=true` 且非直播决定是否改写；视频处理仅根据源路径能否提取 `/d/...` 决定是否解析，并不再次检查 `IsRemote`。另外，任意远程非直播源都会被禁止转码，即使它不是可解析的 OpenList 地址；这是现有行为的边界。

对方所谓“最后直链”目前是一次 HEAD 请求并取 Location，源码注明没有递归追踪任意多跳，不应理解为通用多级重定向解析器。参见[链接解析实现](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/modules/ngx-ext.js)。

## 4. 缓存、故障与兼容性差异

### 缓存

Go 缓存默认开启，TTL 为 180 秒，键是 `(itemId, mediaSourceId/default, UA)`。对所有来源统一隔离 UA，规则简单，也避免依赖 115 域名识别。缓存没有容量上限，过期项仅在再次访问同一个键时删除；同一键并发未命中时可能重复查询上游，mutex 只保证 map 安全。

对方路由缓存默认 `enable=false`、`enableL2=false`。开启后，默认键表达式为 `r.uri:r.args.MediaSourceId`，识别到配置中的 115 域名时追加 UA；支持 L1/L2 与预加载，也缓存回源路由。共享字典配置有容量、15 分钟超时和 `evict`。这比当前 map 的容量管理完整，但需要同步考虑 CDN 链接有效期、UA 域名匹配和设备规则。图片、字幕缓存是另一套机制，不受路由缓存总开关统一控制。

依据：[路由缓存开关](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/config/constant-pro.js)、[缓存读写实现](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.js)、[共享字典与磁盘缓存](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.conf)。

### 故障行为

- Go：Emby 条目查询异常返回 500，OpenList 解析失败返回 502；缺失指定源或非匹配路径才透传。取得 CDN URL 后不预检，CDN 若随后拒绝请求，客户端会直接看到 CDN 的失败。
- 对方：`fallbackUseOriginal=true` 默认允许解析失败回源；部分 Alist 错误会额外尝试 `/api/fs/list` 与挂载目录重试。`redirectCheckEnable=false` 默认不检测直链，开启后可在探测失败时回源。

回源提高可用性，但也可能把本来要绕过的媒体流量重新带回 Emby，增加带宽和读取负担。这里应按你的使用目的选择，而不是一律认为“失败回源”更好。当前 Go 客户端未设置显式上游整体超时；短小 API 查询应单独限制等待，长时间视频透传需要保留流式播放能力。

依据：[回源与预检开关](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/config/constant-mount.js)、[错误分支与重试](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.js)。

### 客户端边界

当前 Go 网关已让 System/Info 宣告网关端口，但没有实现 WebSocket 升级。因此端口改写本身不能确保实时通知、远程控制等功能正常。对方对此有完整 Nginx 升级代理配置。两者直链都要求客户端能访问 CDN，且能解码原始媒体；“强制 Direct Play”不会增加客户端的解码能力。

当前 Go 只改写两个端口。你之前实测地址数组为空，所以满足当时部署；换服务器或 Emby 版本后若地址数组非空，仍有客户端发现上游地址的可能。对方覆盖更多地址字段，值得按需借鉴。

## 5. 安全与验证证据

### 需要准确理解的权限边界

Go 的视频处理使用配置中的 Emby API Key 查询媒体，没有先独立验证来访请求的用户权限，缓存命中也不回查权限。普通代理由 Emby 鉴权，并不等于直链分支也经过同样鉴权。知道条目 ID 的请求可能获得 CDN URL；这应在公网或多用户使用前处理。

对方查询优先采用查询参数中的 `X-Emby-Token` 或 `api_key`，缺少时同样回退到配置的 API Key；路由缓存默认键也不含用户身份。它提供路由屏蔽规则，但不能仅据此认定已经实现完整授权隔离。对方默认关闭路由缓存，因此缓存身份隔离问题主要适用于启用后的配置。以上是静态代码分析，未做未授权访问实测。参见[请求条目信息](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/common/util.js)、[默认 Token 选择](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/common/url-util.js)。

Go 在请求只有 Header Token、URL 没有 Token 时，会把配置的 API Key 加入 `DirectStreamUrl`。对方 URL 工具也有默认 Key 补入逻辑。两者都输出直链日志；对方还输出部分完整查询参数与 PlaybackInfo。日志和客户端 URL 可能含凭据或签名链接，需要按实际暴露范围处理。

Go 使用标准 HTTPS 证书校验。对方示例 `emby.conf` 设置 `js_fetch_verify off`，示例 HTTPS 模板还列出 TLS 1.0/1.1；若采用，应重新审查这些模板值，而不是直接照搬。依据：[Nginx 服务配置](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/emby.conf)、[HTTPS 模板](https://github.com/bpking1/embyExternalUrl/blob/91798cc2ce5e14646c6b939b9f5d1d8d137541e9/emby2Alist/nginx/conf.d/includes/https.conf)。

### 已验证与未验证

| 证据 | 当前 Go 网关 | 对方项目 |
| --- | --- | --- |
| 自动化测试 | `go test`、`go test -race`、`go vet`、编译通过 | 此快照未找到可直接运行的自动化测试套件；有 `docs/test-data.md` 手动样本 |
| 真实上游验证 | 18096 已验证 STRM 重写、302、缓存、CDN 206/1024 字节、本地透传、Web 资源、端口与播放器补丁 | 本次未部署，功能结论来自源码 |
| 重启与局域网访问 | Compose 重启后验证通过；LAN Web HTTP 200 | 本次未验证 |
| 性能 | 未进行并发负载、延迟和内存基准测试 | 未进行同条件基准测试 |

迁移时某个远程源的 HEAD 返回 500，网关与上游一致，证明透传行为一致；**不表示该源的 HEAD 探测已成功**。CDN 的 Range GET 成功也不能代替所有客户端的拖动进度条、字幕和长时间播放验证。

没有证据证明 Go 版比 Nginx+njs 更快或更省内存。当前编译产物约 6.6 MiB 是二进制文件大小，不是容器内存占用。

## 6. 对当前项目的建议

### 优先考虑，保持极简

1. **授权一致性**：让返回直链的请求拥有与 Emby 一致的访问权限，检查缺失/失效 Token、不同用户和缓存命中的情况；避免把配置级 API Key 下发给客户端。
2. **WebSocket 支持**：如果实际客户端使用实时控制或通知，优先借鉴对方的 Upgrade 路由。Go 标准库可实现，无需引入 Nginx 或第三方包；需要明确隧道超时和关闭行为。
3. **API 超时与缓存回收**：为媒体查询/OpenList API 设置短超时，加入简单容量上限或定期清理；不要把短整体超时应用到视频透传。
4. **失败策略**：如你更重视不中断播放，可增加一个默认行为明确的回源开关，并记录回源原因；原有 500/502 模式仍适合要求流量不经过 Emby 的场景。

### 出现实际需求后再添加

- 地址数组出现上游端口时，扩展 System/Info 改写，使用 URL 解析而非宽泛字符串替换。
- STRM 存在相对路径时，优先统一生成规范 HTTP 地址；若必须兼容，再加一条明确映射规则。不要把所有 `/d/...` 本地路径无条件当成远程。
- 需要下载或音频直链时，可以复用解析核心，但必须分别确认路由、条目 ID 与 HEAD 行为，不能只扩大字符串匹配范围。
- 只有当路径映射、多个媒体服务器、客户端策略和转码配置成为主要需求时，再考虑换用 emby2Alist。115 HLS、搜索过滤、外部播放器及图片缓存暂不适合全部搬入本项目。

这些是评估建议，本次仅新增报告，未改动运行代码或线上服务。
