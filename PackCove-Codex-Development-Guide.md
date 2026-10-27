# PackCove 开发设计指南

> 面向 Codex 的项目设计、实现和验收指南  
> 版本：v0.1｜日期：2026-10-27｜状态：需求基线与实现建议

## 1. 项目定位

PackCove 是一个独立部署在内网的**开发依赖缓存代理服务**。它提供与 Maven、npm、PyPI 兼容的仓库访问地址，用户在包管理器或 CI 构建环境中配置这些地址后，按原来的方式安装依赖。PackCove 自动读取本地缓存；未命中时从对应上游获取、缓存并返回，供其他项目和构建环境复用。

核心目标是减少重复的外部依赖下载，改善开发环境、临时构建容器和 CI Runner 的依赖获取时间及网络稳定性。

### 1.1 已确认的产品决策

1. PackCove 是一个独立服务，部署在内网，不依赖现有开发平台。
2. 使用自己的持久化存储缓存依赖。
3. 第一版目标格式：Maven、npm、PyPI。
4. 使用者配置 PackCove 地址后，匿名读取，不需要登录或选择缓存。
5. 有有效缓存时使用本地数据；缺失或按策略需要更新时访问上游。
6. 使用配置文件管理服务和仓库；v0.1 的配置变更通过重启生效。
7. 通过 HTTP API、日志和指标观察配置及运行情况。
8. 不提供 Web 页面。

### 1.2 产品边界

第一版实现代理缓存及其运行所需的配置、监控和部署能力。以下内容暂不列为 v0.1 验收范围：

- 上传或发布自有制品、完整私有制品仓库、仓库聚合与跨仓库搜索。
- Docker/OCI 镜像代理、Git 仓库、操作系统包仓库。
- Web UI、用户账号、租户和用户级权限系统。
- 配置热更新、通过 API 修改配置、分布式集群或多实例共享缓存。
- 自动清理、离线导入导出、主动预热、依赖漏洞扫描。
- 缓存编译结果、`node_modules`、虚拟环境或 CI 构建产物。

上述均是版本范围选择，不表示项目永远不支持。不要为了未来扩展过度设计。

### 1.3 对用户的实际价值和限制

- 包管理器自身仍会先按自己的规则使用本机缓存；PackCove 负责它真正发出的仓库请求。
- 首次冷缓存需要访问上游，耗时不保证比直连更短；缓存主要改善后续跨环境重复下载。
- 如果主要耗时在编译、测试或镜像构建，依赖缓存对总耗时的改善可能有限。
- 外网不可达时，能否完成构建取决于依赖文件及必要元数据是否完整存在、是否仍有效，以及是否启用允许使用过期元数据的策略。
- Git 依赖、直接 URL、其他 registry 地址可能绕过 PackCove；不承诺对所有外部下载自动生效。

## 2. 推荐技术方案

以下是便于 Codex 开工的**建议**，不是用户已指定的技术栈。若目标仓库已有成熟技术栈和约定，应优先兼容现有仓库并在交付时说明。

- 语言：Go，选用实施时受支持的稳定版本；不要硬编码未经核实的版本号。
- 服务形态：单个 HTTP 服务进程、一个可执行文件；可打包为容器。
- HTTP：优先标准库 `net/http`，并正确处理超时、上下文和流式传输。
- 配置：YAML 文件，启动时校验；配置错误时输出清晰错误并退出。
- 存储：本地文件系统保存内容；SQLite 保存缓存索引、元数据和下载地址映射。
- 指标：Prometheus 文本格式；尽量采用现有成熟客户端库，固定依赖版本。
- 日志：标准输出的结构化日志，例如 JSON。

SQLite 可采用与目标环境兼容的成熟 Go 驱动；优先选择易于容器部署的方案。第一版假定**单实例独占数据目录**，不支持多进程或多实例同时写入同一目录。目标环境以 Linux 服务器或 Kubernetes 单副本配持久卷为主。

Go module 路径在仓库地址确定后填写；未知时使用明确的临时 module 路径，并在 README 说明，不能假装已经创建远程仓库。

## 3. 关键概念与请求流程

| 概念 | 定义 |
| --- | --- |
| 代理仓库 Repository | 一个具名配置：格式、一个上游地址、缓存策略和本地入口。 |
| 元数据 Metadata | 包版本、依赖、索引或 Maven metadata 等解析信息，可能经常变化。 |
| 制品文件 Artifact | JAR、POM、npm tarball、Python wheel、sdist 等具体文件；其中部分文件也可能变化。 |
| 有效缓存 | 已完整写入、索引可用，并符合对应过期与更新策略的数据。 |
| 上游 Upstream | 代理仓库配置的来源，以及经管理员允许的文件下载或重定向主机。 |

### 3.1 基本流程

1. 客户端访问 `/repository/{仓库名}/...`。
2. 根据仓库配置选择 Maven、npm 或 PyPI 适配器，识别请求类型与上游目标。
3. 按仓库、资源标识、必要查询参数及响应表示形式查索引。
4. 有有效缓存时，从本地返回；记录命中。
5. 缺失或需要重新验证时请求上游；成功获取完整内容并检查完整性后发布缓存，再返回客户端。
6. 上游失败时遵守错误和过期数据策略；不能把失败或不完整内容登记为有效缓存。

同一资源的多个并发请求应合并上游获取：可以等待同一个下载完成后分别返回。第一版优先保证正确性，不要求边回源边向所有客户端广播数据。

### 3.2 状态边界

- PackCove 运行正常、缓存可读，不等于每个上游都可达。
- 仓库状态应区分：`unknown`（尚无有效观察）、`available`、`unavailable`；附最后检查或请求时间。
- 上游状态基于实际请求结果或明确启用的检查，不要在从未检测时声称可用。
- 上游失联不应直接使整个进程存活检查失败，也不应阻止读取有效本地缓存。
- 一次请求从本地返回的内容必须是其所请求的仓库和变体，不能串仓库或串元数据格式。

## 4. 缓存与一致性设计

### 4.1 建议默认策略

这些是实现默认值，可按配置调整；它们是为 MVP 做的建议，并非用户已指定的精确数值。

| 类别 | 建议默认行为 |
| --- | --- |
| Maven/npm/PyPI 元数据 | 缓存 5 分钟；过期后重新验证或拉取。 |
| Maven SNAPSHOT 等可变文件 | 缓存 5 分钟后重新验证；相关 metadata 同样按短 TTL 更新。 |
| 约定为不可变的固定版本制品 | 完整保存后长期复用；前提是对应仓库确实采用不可变发布策略。 |
| 明确 404、410 | 第一版不做负缓存，直接返回；避免新发布版本持续不可见。 |
| 超时、5xx、下载失败 | 不缓存为成功结果。 |
| 上游失败时使用过期元数据 | 默认关闭；可配置开启 `stale_if_error`，并在日志中标记。 |
| 自动删除 | 默认不自动清理；v0.1 不实现自动清理。 |

对于会覆盖同名版本或文件的自定义上游，不可无条件假设不可变：仓库需要配置有限的制品 TTL，或由未来的策略能力明确支持。元数据与文件分开缓存；不能把所有内容简单设置成同一个 TTL。

有 `ETag` 或 `Last-Modified` 时，可以对上游做条件请求。上游返回 `304` 时更新缓存的验证时间并使用已保存内容；如果对 npm/PyPI 元数据进行了 URL 改写，不能把**原始上游响应的 ETag 或 Content-Length**原样用于改写后的响应。应为改写内容生成正确的响应头，或不暴露不适用的字段。

`stale_if_error` 开启后需要说明：可能暂时看不到新版本，也可能延迟反映撤回或其他上游变化。不能声称启用后所有构建都能离线成功。

### 4.2 完整性与原子发布

- 下载写入数据目录同一文件系统内的临时文件。
- 对已知 `Content-Length`、来自可信元数据的校验信息进行适当检查；若上游没有可信摘要，只计算本地 SHA-256 不等于验证上游真实性。
- 完整下载且检查通过后，以原子重命名发布内容，再事务性登记索引。
- 可按内容 SHA-256 命名文件以便去重，但索引仍须隔离不同仓库、资源和请求变体。
- 若进程在文件发布和索引提交之间崩溃，允许出现无引用文件；提供启动或维护时的安全回收方案。索引存在而文件缺失时按未命中处理。
- 下载中断、长度或摘要错误、磁盘写满时，不能发布半成品为缓存命中。
- 存储满或写入失败时，已有有效缓存仍应可读；新内容无法缓存时返回明确错误并记录。第一版无需实现无缓存透传降级。
- GET 已缓存文件应支持正确的 `Content-Type`、`Content-Length`，以及满足客户端的 `HEAD`、字节范围请求；仅因 HEAD 或部分 Range 回源而拿到的局部数据不得登记为完整文件。

### 4.3 并发与安全边界

- 以仓库、规范化资源标识及必要响应变体为键合并同一进程中的并发回源；不同仓库不可合并。
- 任一客户端中断不得让其他等待同一回源的客户端拿到部分文件；错误要传播给相关等待者。
- 每个仓库只访问其配置的上游，及通过 `allowed_download_hosts` 明确允许的制品或重定向主机。每次重定向都要检查目标，不能提供可传入任意 URL 的开放代理。
- 安全处理路径遍历、编码路径和查询参数；不能允许请求写入缓存目录之外。
- 请求超时和上游错误可观察；日志、配置 API、指标中不输出凭据或带敏感参数的原始 URL。
- v0.1 主要覆盖公共上游；若未来加入上游认证，凭据与权限共享范围需要单独设计。仓库下载当前按照已确认需求匿名开放。

## 5. 协议适配要求

**不能把整个项目实现为只按路径缓存普通 HTTP 文件的通用反向代理。** npm 和 PyPI 的索引可能给出其他主机上的真实下载 URL，需要保证依赖文件实际经过 PackCove，同时保留协议所需的信息。

### 5.1 Maven

入口示例：`/repository/maven-central/`。

- 按 Maven 仓库布局代理目录和文件，包括 `.pom`、`.jar`、其他扩展名、classifier 文件、插件及校验文件。
- 正确处理 `maven-metadata.xml` 及 SNAPSHOT 更新；不要只缓存 JAR。
- 保留客户端需要的响应码与内容类型；路径经安全校验后转发至所配置的上游根路径。
- 仓库地址可在 `settings.xml` 中配置为 `central` 的 mirror；不能默认将 `mirrorOf` 设为 `*`，以免覆盖用户另有用途的私有仓库。
- 第一版以 Maven 读取协议为目标；不实现 `PUT` 发布。

### 5.2 npm

入口示例：`/repository/npm/`。

- 支持普通包、带 scope 的包名及其合法编码形式。
- 支持 npm 的完整及精简 package metadata 表示；按 `Accept` 区分变体，不能把两种响应混用。
- 处理 metadata 中每个版本的 `dist.tarball`：返回 PackCove 可访问的地址，保证 tarball 请求进入缓存服务；保留 `dist.integrity`、`shasum`、版本和依赖字段。
- 上游实际 tarball URL 可使用内部映射保存；对客户端只暴露固定的 PackCove 下载地址，不能设计成传入任意外部 URL 的接口。
- 处理上游允许主机的重定向。不能把其他主机的敏感认证头无条件转发。
- `package-lock.json` 如果指向不同的自定义 registry、Git 或直接 URL，可能绕过本服务。README 说明此边界，不应静默重写用户 lockfile。
- 支持实际 `npm install`、`npm ci` 所需的读取行为；不实现 `npm publish`。

### 5.3 PyPI

入口示例：`/repository/pypi/simple/`。

- 实现 Python Simple Repository API 的必要读取行为：项目名称规范化、项目索引、文件下载，以及 HTML / JSON 表示的内容协商；实现根索引的正确代理行为。
- 索引中的文件可能位于 `files.pythonhosted.org` 等不同域名，需将下载入口改写到 PackCove，再按允许主机规则回源。
- 保留 filename、相对/绝对链接语义、哈希片段、`data-requires-python`、`data-yanked`、相关 metadata 字段及 JSON 对应字段；不可因为改写丢失客户端所需信息。
- 对 wheel、sdist 及声明的关联 metadata 文件保持可访问；同名但不同 Python 版本、平台或架构的文件不能互相覆盖。
- 正确处理内容协商、`Content-Type`、缓存变体及响应头。
- 使用标准 `pip install` / `pip download` 验证；不实现发布上传。

> 对 Codex 的要求：依据本文末尾的官方协议文档实现上述范围；不应宣称已完整兼容所有可选扩展，超出首版支持范围的行为必须在 README 中说明。

## 6. 配置设计

建议配置文件：`packcove.yaml`。以下是计划中的配置示例，不代表服务已经实现。

```yaml
server:
  listen: "0.0.0.0:8080"
  # 用于生成 npm / PyPI 元数据中的外部下载地址。
  # 对构建环境而言必须可访问；示例仅为占位地址。
  public_base_url: "https://packcove.example.internal"

storage:
  data_dir: "/var/lib/packcove"

cache:
  metadata_ttl: "5m"
  mutable_artifact_ttl: "5m"
  # 固定版本制品在对应上游采用不可变策略的前提下长期缓存。
  immutable_artifact_ttl: "never"
  stale_if_error: false

upstream:
  connect_timeout: "10s"
  request_timeout: "5m"
  max_redirects: 5

repositories:
  - name: maven-central
    type: maven
    upstream: "https://repo.maven.apache.org/maven2/"
    allowed_download_hosts:
      - "repo.maven.apache.org"

  - name: npm
    type: npm
    upstream: "https://registry.npmjs.org/"
    allowed_download_hosts:
      - "registry.npmjs.org"

  - name: pypi
    type: pypi
    upstream: "https://pypi.org/simple/"
    allowed_download_hosts:
      - "pypi.org"
      - "files.pythonhosted.org"
```

配置字段和默认值最终以实际实现为准，但必须保证：

- 仓库名唯一、格式受支持、路径和 URL 合法；非法配置启动失败并提示具体字段。
- 默认允许 GET、HEAD；写入类请求不被误当作代理发布，返回明确的不支持状态。
- `public_base_url` 显式配置并用于生成绝对 URL，不能无条件信任客户端传入的 `Host` 或转发头。
- 上游地址和允许的下载主机由管理员配置；重定向也要遵守允许范围。更换镜像源时同步检查它所使用的文件主机。
- 生产环境建议让反向代理或 Kubernetes Ingress 提供 HTTPS，并配置客户端信任的证书。若使用非 localhost 的纯 HTTP，pip 等客户端可能需要额外的信任配置，不能假设开箱即用。
- 默认预置上述三个公共来源；网络或组织实际环境不通时由管理员更换为可信上游。
- 服务重启后继续使用同一数据目录和索引，缓存仍可复用。

## 7. 外部接口设计

### 7.1 用户仓库接口

以下是示例接口，所有下载不要求用户登录：

```text
GET/HEAD /repository/maven-central/...
GET/HEAD /repository/npm/...
GET/HEAD /repository/pypi/simple/...
```

对 npm/PyPI 中改写出的文件地址，可设计服务内部的固定路径与不透明 ID；必须映射到该仓库已验证的上游资源，且不暴露未过滤的外部 URL 作为可任意调用的代理参数。

针对受支持的资源方法，保留协议需要的响应语义。对不支持的写入方法返回 `405 Method Not Allowed` 和适当的 `Allow`；不存在的资源可返回上游的 `404`；网关上游错误或超时用清晰可区分的 5xx，并在日志记录原因。不要把网络超时伪装成包不存在。

### 7.2 只读运维接口

```text
GET /_packcove/health
GET /_packcove/repositories
GET /_packcove/stats
GET /metrics
```

- `health`：服务和本地存储状态；必要时区分存活与就绪。不能因为一个上游不可达就让存活检查失败。
- `repositories`：返回名称、类型、公开的本地地址、已脱敏的上游地址、最近观察到的上游状态和时间。
- `stats`：返回自进程启动后的聚合统计；服务重启时进程内计数可以重置。
- `metrics`：Prometheus 文本格式，不是 JSON。允许接入用户已有的监控系统；不要求 PackCove 自带历史数据库。

`health` 示例：

```json
{
  "status": "ok",
  "storage": {"status": "ok", "path": "/var/lib/packcove"},
  "started_at": "2026-10-27T08:00:00Z"
}
```

`repositories` 示例：

```json
{
  "repositories": [
    {
      "name": "pypi",
      "type": "pypi",
      "url": "https://packcove.example.internal/repository/pypi/simple/",
      "upstream": "https://pypi.org/simple/",
      "upstream_status": "unknown",
      "last_observed_at": null
    }
  ]
}
```

示例值仅表示接口形状；不要在实现中写死状态、仓库内容或数字。只读运维接口默认与服务同处内网；不提供配置修改、删除缓存或发布制品的管理 API。若将来暴露到其他网络，应另外设计访问控制。

### 7.3 指标口径

第一版至少记录：

- `packcove_artifact_requests_total{repository,result}`：成功的包文件 GET，`result` 为 `hit`、`miss` 或 `revalidated`；每次请求只计一个结果。
- `packcove_request_errors_total{repository,reason}`：错误次数，`reason` 使用有限枚举。
- `packcove_upstream_requests_total{repository,result}`：实际发起的上游请求数，可观察并发合并效果。
- `packcove_bytes_served_total{repository,source}`：提供给客户端的文件字节数，来源按本地缓存或上游获取结果区分，口径在 README 中定义。
- `packcove_cache_bytes`：已登记缓存文件占用量；与底层文件系统可用空间分别呈现。
- 请求耗时直方图和上游耗时直方图，避免在指标标签中放包名、版本、路径或完整 URL。

包文件命中率可定义为 `hit / (hit + miss + revalidated)`：只统计成功的包文件 GET，不混入 metadata、HEAD 和失败请求；分母为 0 时返回 `null` 或不展示百分比。并发等待同一次上游下载的客户端请求仍可能各自记一次 miss，而实际回源请求只记一次。指标口径应在 README 明确。

缓存命中流量不是用户实际节省的构建时间。构建总耗时需要用户的 CI 系统或额外采集；第一版不捏造“节省时间”。

## 8. 数据目录与内部模块建议

```text
/data/packcove/
  index.sqlite       # 仓库资源索引、元数据与受控下载映射
  blobs/             # 完整制品与缓存响应内容
  tmp/               # 正在下载的临时文件
```

可按下列职责拆分，具体目录以仓库约定为准：

```text
cmd/packcove/        启动入口
internal/config/     加载与校验配置
internal/server/     HTTP 路由、健康检查、只读 API
internal/repository/ 仓库配置及协议适配接口
internal/maven/      Maven 读取协议
internal/npm/        npm 元数据及文件代理
internal/pypi/       Simple API 及文件代理
internal/cache/      缓存决策、并发合并、过期处理
internal/storage/    文件存储、索引及原子发布
internal/upstream/   上游请求、重定向和超时
internal/metrics/    指标和聚合统计
```

不要为了目录结构拆出大量空接口；以实际行为和测试为准。数据索引需记录：仓库、规范化资源键、响应变体、文件引用、大小、内容类型、来源验证信息、缓存时间和必要的受控 URL 映射。映射应在服务重启后可用；索引引用丢失的文件时必须能够回源恢复。

## 9. 用户接入示例

假设域名和 HTTPS 已在用户环境中部署，以下地址仅作示意。

### Maven

在 `~/.m2/settings.xml` 的 `<mirrors>` 中添加：

```xml
<mirror>
  <id>packcove-central</id>
  <mirrorOf>central</mirrorOf>
  <url>https://packcove.example.internal/repository/maven-central/</url>
</mirror>
```

这样仅替换 ID 为 `central` 的仓库；项目使用的其他仓库需要根据实际情况单独配置。

### npm

```bash
npm config set registry https://packcove.example.internal/repository/npm/
npm install
```

已有的项目或 scope 可能另行指定 registry，lockfile 中自定义地址也可能继续生效。

### pip

```bash
python -m pip config set global.index-url https://packcove.example.internal/repository/pypi/simple/
python -m pip install requests
```

`pip config` 写入的配置位置取决于调用方式和当前环境；CI 中也可用 `PIP_INDEX_URL` 配置。实际部署的 HTTPS 证书必须被客户端信任。

### 运维查看

```bash
curl -sS https://packcove.example.internal/_packcove/health | jq
curl -sS https://packcove.example.internal/_packcove/repositories | jq
curl -sS https://packcove.example.internal/_packcove/stats | jq
curl -sS https://packcove.example.internal/metrics
```

## 10. 交付物

Codex 完成首版时应交付：

1. 可编译、可运行的服务代码和固定版本依赖。
2. 配置示例及三个协议的接入说明。
3. 本地可运行方式、持久化目录说明、容器镜像构建文件。
4. 只读 HTTP API、健康检查、日志和 Prometheus 指标说明。
5. 真实客户端兼容性测试、自动化集成测试和执行方法。
6. README：安装、配置、运行、缓存策略、运维、已知边界和故障排查。
7. 一份适合单实例 + 持久卷的 Kubernetes 示例，明确 ingress/public_base_url/TLS 由部署环境提供；不做多副本共享同一数据目录。

部署样例不应要求真实生产域名、真实密钥或访问公网才能完成基础测试。

## 11. 实施顺序

按顺序交付可以运行和验证的增量；最终验收必须覆盖三种格式，不能在 Maven 完成后宣称 v0.1 已完成。

1. 初始化工程、配置校验、HTTP 服务器、数据目录、日志及只读健康接口。
2. 实现本地存储、SQLite 索引、原子写入、并发合并、回源超时和指标。
3. 完成 Maven 适配及基于真实 Maven 客户端的验证。
4. 完成 npm metadata / tarball 适配及 `npm install`、`npm ci` 验证。
5. 完成 PyPI Simple API / 文件适配及 pip 验证。
6. 完成重启恢复、失效策略、存储失败、监控接口、容器与部署文档。
7. 执行全量测试，汇总已验证格式、已知限制和没有验证的内容。

## 12. 验收标准

### 12.1 透明缓存与共享

- 使用模拟上游和两个互相隔离、初始为空的包管理器本地缓存。
- 环境 A 首次获取固定依赖：请求经过 PackCove、上游被访问、文件完整保存。
- 环境 B 再次获取相同依赖：成功安装，PackCove 返回本地文件；模拟上游对应文件的请求计数不增加。相关 metadata 是否需要验证按配置和 TTL 独立判断。
- 重启 PackCove 并保留数据目录后，环境 C 仍可复用缓存。
- 使用有隔离本地缓存的真实 `npm`、`pip`、`mvn` 验证，而不是仅用 curl 假装兼容包管理器。

### 12.2 协议正确性

- Maven：POM、JAR、校验文件、metadata 和 SNAPSHOT 场景；验证 mirror 不覆盖不相关仓库。真实 Maven 的测试夹具应预先准备所需插件或可用的独立插件源，不能把插件缺失误判成代理成功。
- npm：普通包和 scoped 包；完整 / 精简 metadata；tarball URL 改写；`npm ci` 使用 lockfile；验证 integrity、转发及自定义 registry 例外。
- PyPI：规范化包名、HTML/JSON Simple API、不同平台 wheel、sdist、哈希片段、`requires-python`、yanked 及声明的关联 metadata；确认文件请求确实经过 PackCove。
- 针对重定向、`HEAD`、Range 和安全路径编码编写真实 HTTP 测试；必要时同时验证允许和拒绝的主机。

### 12.3 故障与并发

- 多个客户端同时请求同一尚未缓存的文件，只发生一次对应上游获取；所有成功客户端收到完整一致内容。
- 上游超时、404、500、断流和错误长度：错误分类准确，不产生有效半成品缓存。
- 缓存有效且上游断开时，所需文件和有效 metadata 可以继续提供；未缓存内容返回明确错误。
- 元数据已过期且上游断开时，分别验证 `stale_if_error: false` 和 `true` 的约定行为。
- 存储写入失败或空间不足时，已存在的有效缓存仍能读取；不能把失败写入标记为有效。
- 进程重启、索引或文件缺失后，不返回损坏内容，并能按需要重新获取。

### 12.4 观测与部署

- `curl` 可以读取健康、仓库配置、统计和 Prometheus 指标；状态和数值基于真实行为。
- 日志不包含认证信息或敏感 URL 参数；指标标签不会随包名和版本无限增长。
- 指标中的缓存命中率与定义的分母一致；统计重启后重置的行为有文档说明。
- 只读运维接口不支持修改配置；包管理入口不支持上传发布。
- 单实例容器挂载持久卷后重启，数据和配置行为符合预期。

### 12.5 可复现的性能比较

选取一个依赖版本固定的真实示例项目，分别测试直连、PackCove 冷缓存和 PackCove 热缓存。每轮使用独立、空的包管理器本机缓存，保持机器、网络、版本和项目不变；记录依赖安装耗时、上游请求/字节数与总构建耗时。多次运行并报告测量条件，不用单次结果或命中率虚构固定加速比例。

## 13. 运维与已知限制

- 通过进程日志和指标排查上游超时、404、完整性失败、磁盘写入失败。
- 第一版不自动清理缓存；管理员应监控存储。不能在服务运行时随意删除 SQLite 和 blob，避免索引不一致。
- 上游故障不保证冷缓存请求成功；过期元数据的行为由配置决定。
- `stale_if_error` 可能提供过期索引，影响新版本与撤回状态的及时性。
- 匿名读取意味着能访问该仓库入口的人可读取其公开出来的依赖；第一版不做用户级隔离。
- 代理私有上游、凭据管理及细粒度访问控制不在首版范围。
- npm lockfile、自定义 registry、Git 或直接文件 URL 等可能绕过服务；检查构建日志和上游请求验证实际流量。
- 单实例限制必须在部署文档中说明；不能通过增加副本冒充已经支持共享缓存并发一致性。

## 14. 协议参考

Codex 开发时应阅读并以官方文档为准：

- Maven Repository Layout：<https://maven.apache.org/repository/layout.html>
- Maven Repository Metadata：<https://maven.apache.org/repositories/metadata.html>
- Maven Mirror Settings：<https://maven.apache.org/guides/mini/guide-mirror-settings.html>
- npm Package Metadata：<https://github.com/npm/registry/blob/main/docs/responses/package-metadata.md>
- npm Registry：<https://docs.npmjs.com/cli/v11/using-npm/registry/>
- Python Simple Repository API：<https://packaging.python.org/en/latest/specifications/simple-repository-api/>
- pip 配置：<https://pip.pypa.io/en/stable/topics/configuration/>
- Prometheus 指标设计：<https://prometheus.io/docs/practices/naming/> 和 <https://prometheus.io/docs/instrumenting/writing_exporters/>

## 15. 可直接给 Codex 的启动指令

```text
请先阅读 PackCove-Codex-Development-Guide.md，然后在当前仓库中开发 PackCove v0.1。

要求：
1. 先检查当前仓库及其已有技术约定；已有约定与指南中的建议技术栈冲突时，说明取舍并优先兼容已有项目。指南中已确认的产品边界必须保留。
2. PackCove 是独立的内网依赖缓存代理服务。用户匿名读取，自动命中或回源；不开发 Web 页面、不实现制品上传发布。
3. 完成 Maven、npm、PyPI 三类代理；特别处理 npm tarball 和 PyPI 文件链接、协议变体、缓存一致性、并发回源及数据持久化。
4. 通过配置文件管理，提供只读 API、日志、健康检查和 Prometheus 指标。不要写死示例状态或统计数据。
5. 按指南中的实施顺序推进，但不要在只完成某一种协议后停止并声称 v0.1 已完成。
6. 编写并运行有意义的自动化测试，使用真实包管理器验证冷缓存、热缓存、重启、故障和协议兼容；不依赖生产服务或真实凭据。
7. 提供 README、示例配置、容器构建和单实例 Kubernetes 示例。最终报告实际完成情况、测试结果、明确的兼容边界和未完成事项。
```
