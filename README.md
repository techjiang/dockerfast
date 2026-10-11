# DockerFast

DockerFast 是一个用 Go 编写的 Docker 镜像拉取加速代理（Registry V2 pull-through cache）。

它对外提供与 Docker Registry V2 完全兼容的接口，Docker 客户端只需把它配置为 `registry-mirrors`
即可无感使用：首次拉取时 DockerFast 回源上游并把内容落盘缓存，之后的拉取直接从本地缓存返回，
从而显著减少跨国/跨地域拉取的耗时与带宽消耗。

## 特性

- 透明代理：实现 Registry V2 协议（`/v2/`），无需修改任何拉取命令
- 多上游：内置 Docker Hub（默认）、GHCR、GCR、`k8s.gcr.io`、`registry.k8s.io`、Quay、MCR、
  ECR Public、NVCR、GitLab、Elastic 等，并支持任意自定义私有仓库
- 自动认证：自动解析上游 `WWW-Authenticate` 挑战，获取并缓存 Bearer token；支持 Basic 认证与私有仓库凭据
- 本地磁盘缓存：内容寻址存储，manifest 与 blob 分别缓存，支持 tag 到 digest 的引用映射
- 容量治理：可配置最大容量，超出后按 LRU 淘汰；支持 TTL 过期清理
- 并发去重：相同的层并发拉取只会回源一次，其余请求等待后直接读缓存
- 流式下发：未命中时一边回源一边写缓存并同时下发给客户端，首字节延迟低
- 弹性重试：上游出现网络错误或 429/5xx 时按指数退避自动重试（可配置次数与退避区间）
- 可观测：访问日志、`/healthz` 健康检查、Prometheus 格式的 `/metrics`、内置 `/status` 与状态面板
- 缓存标识：manifest/blob 响应带 `X-DockerFast-Cache: HIT|MISS|BYPASS` 头，便于排查
- 可选鉴权：支持 HTTP Basic 认证保护代理，避免缓存被滥用
- 零第三方依赖：仅使用 Go 标准库

## 快速开始

### 方式一：直接运行二进制

```bash
# 构建
make build

# 使用示例配置启动
./bin/dockerfast -config config.example.json
```

### 方式二：Docker

```bash
docker build -t dockerfast:latest .
docker run -d --name dockerfast \
  -p 5000:5000 \
  -v "$PWD/config.example.json:/etc/dockerfast/config.json:ro" \
  -v dockerfast-data:/data \
  dockerfast:latest
```

### 方式三：Docker Compose

```bash
docker compose up -d
```

服务默认监听 `:5000`，数据目录 `/data`。

## 配置 Docker 客户端

### 1. 作为 Docker Hub 的镜像加速器

编辑 `/etc/docker/daemon.json`：

```json
{
  "registry-mirrors": ["http://<dockerfast-host>:5000"]
}
```

重启 Docker：

```bash
sudo systemctl restart docker
docker pull nginx
```

配置 `registry-mirrors` 后，所有 Docker Hub 的拉取都会经过 DockerFast，
请求路径形如 `/v2/library/nginx/...`，DockerFast 会自动路由到默认上游 `docker.io`。

### 2. 代理其他 Registry（GHCR / GCR / Quay 等）

`registry-mirrors` 只对 Docker Hub 生效。要加速其他仓库，使用「上游主机名作为路径前缀」的方式：

```bash
# 拉取 ghcr.io/owner/repo:tag
docker pull <dockerfast-host>:5000/ghcr.io/owner/repo:tag

# 拉取 quay.io/org/app:tag
docker pull <dockerfast-host>:5000/quay.io/org/app:tag
```

DockerFast 会识别路径第一段中的主机名（`ghcr.io`、`gcr.io`、`registry.k8s.io`、`quay.io` 等）
并路由到对应上游，同时把该前缀从仓库名中剥离。

也可以把 DockerFast 部署在某个 Registry 主机名后面（例如通过反向代理），
此时它会根据请求的 `Host` 头自动选择上游。

## 配置文件

参考 `config.example.json`。字段说明：

| 字段 | 说明 | 默认值 |
| --- | --- | --- |
| `listen` | 监听地址 | `:5000` |
| `dataDir` | 缓存数据目录 | `./data` |
| `logLevel` | 日志级别 `debug/info/warn/error` | `info` |
| `accessLog` | 是否输出访问日志 | `true` |
| `allowPush` | 是否允许推送透传到上游 | `false` |
| `maxManifestSize` | 单个 manifest 最大字节数 | `8388608`（8 MiB） |
| `upstreamTimeout` | 上游请求超时 | `5m` |
| `cache.maxSizeBytes` | 缓存最大容量（字节） | `21474836480`（20 GiB） |
| `cache.blobTTL` | blob 过期时间，`0` 表示不过期 | `720h` |
| `cache.manifestTTL` | manifest 过期时间，`0` 表示不过期 | `24h` |
| `cache.cleanupInterval` | 清理任务执行间隔 | `30m` |
| `retry.attempts` | 上游失败重试次数（1 表示不重试） | `3` |
| `retry.initialBackoff` | 首次重试退避 | `200ms` |
| `retry.maxBackoff` | 退避上限 | `2s` |
| `auth.enabled` | 是否开启代理访问 Basic 认证 | `false` |
| `auth.username` / `password` | 代理访问凭据 | 空 |
| `upstreams[].name` | 上游名称，同时作为缓存命名空间 | - |
| `upstreams[].hosts` | 可识别的主机别名（用于路径前缀 / Host 路由） | `[name]` |
| `upstreams[].endpoint` | 上游 Registry 地址 | `https://<name>` |
| `upstreams[].default` | 是否为默认上游，只能有一个 | 第一个 |
| `upstreams[].username` / `password` | 私有仓库凭据（可选） | 空 |
| `upstreams[].insecureSkipVerify` | 跳过 TLS 证书校验（不建议） | `false` |

时长字段支持 Go duration 字符串（`30m`、`720h`）或纯数字（单位：秒）。

### 环境变量覆盖

| 环境变量 | 对应字段 |
| --- | --- |
| `DOCKERFAST_LISTEN` | `listen` |
| `DOCKERFAST_DATA_DIR` | `dataDir` |
| `DOCKERFAST_LOG_LEVEL` | `logLevel` |
| `DOCKERFAST_ACCESS_LOG` | `accessLog` |
| `DOCKERFAST_ALLOW_PUSH` | `allowPush` |
| `DOCKERFAST_MAX_CACHE_BYTES` | `cache.maxSizeBytes` |
| `DOCKERFAST_BLOB_TTL` | `cache.blobTTL` |
| `DOCKERFAST_MANIFEST_TTL` | `cache.manifestTTL` |
| `DOCKERFAST_RETRY_ATTEMPTS` | `retry.attempts` |
| `DOCKERFAST_AUTH_ENABLED` | `auth.enabled` |
| `DOCKERFAST_AUTH_USERNAME` | `auth.username` |
| `DOCKERFAST_AUTH_PASSWORD` | `auth.password` |

### 使用国内镜像作为上游

若希望 Docker Hub 走某个国内加速源，把默认上游的 `endpoint` 换成该源即可：

```json
{
  "name": "docker.io",
  "hosts": ["docker.io", "registry-1.docker.io", "index.docker.io"],
  "endpoint": "https://<your-mirror-endpoint>",
  "default": true
}
```

### 私有仓库示例

```json
{
  "name": "registry.example.com",
  "hosts": ["registry.example.com"],
  "endpoint": "https://registry.example.com",
  "username": "your-user",
  "password": "your-token-or-password"
}
```

凭据会用于向上游的 token 服务发起 Basic 认证，或在注册表返回 Basic 挑战时直接使用。

## 运维端点

| 端点 | 说明 |
| --- | --- |
| `GET /v2/` | Registry V2 版本探测，返回 `{}` |
| `GET /healthz` | 健康检查，返回 `{"status":"ok"}`（开启鉴权时仍保持开放） |
| `GET /metrics` | Prometheus 指标（请求数、缓存命中/未命中、重试、字节数、缓存容量、运行时间等） |
| `GET /status` | JSON 状态：版本、运行时长、缓存用量、上游列表与全部计数 |
| `GET /` | 内置状态面板（无外部依赖的 HTML 页面，每 3 秒刷新） |

## 缓存目录结构

```
<dataDir>/
  blobs/<algo>/<hex>           镜像层内容
  manifests/<algo>/<hex>       manifest 内容
  manifests/<algo>/<hex>.media manifest 媒体类型
  refs/<upstream>/<repo>/<tag> tag -> digest 引用
  tmp/                         原子写入临时文件
```

缓存写入全部采用「临时文件 + rename」的原子方式，进程异常退出不会留下损坏的半成品文件。

## 命令行参数

```
dockerfast [-config PATH] [-listen ADDR] [-data-dir DIR] [-log-level LEVEL] [-version]
```

命令行参数优先级高于配置文件，环境变量次之，配置文件最低。

## 开发

```bash
make build   # 构建到 bin/dockerfast
make test    # 运行全部测试（含 -race）
make vet     # go vet
make fmt     # gofmt
make docker  # 构建 Docker 镜像
```

测试覆盖：配置加载与环境覆盖、摘要解析、缓存读写/TTL/LRU、路径解析、上游路由、
认证挑战解析、Bearer token 协商、上游重试退避、代理 Basic 鉴权、并发下载去重，
以及基于 `httptest` 的 manifest/blob 端到端缓存与缓存标识验证。

## 安全说明

- 默认关闭推送（`allowPush: false`），DockerFast 只提供拉取加速
- 缓存按内容摘要寻址，写入后校验 sha256，摘要不匹配的内容不会进入缓存
- 引用路径做了防目录穿越处理
- 私有仓库凭据仅保存在本地配置文件中，不会写入缓存
- 如将代理暴露在不可信网络，建议开启 `auth`（HTTP Basic）以避免缓存被他人盗用
- 上游仅在网络错误或 429/5xx 等瞬时故障时重试，且不会重试带不可重放请求体的调用

## License

MIT
