# Game Exporter

Game Exporter 将 OTLP logs 中已准备好的事件转换为平台的 HTTP 批量上报请求。
协议依据：[客户端 API 文档](https://analysis-dev.youwo.com:8000/admin/docs/client-api.html)。

## 接入已有 Collector

在已有配置中增加 exporter，并将它加入 logs pipeline。接收器和 processor 继续使用现有配置：

```yaml
exporters:
  game:
    endpoint: https://analysis-dev.youwo.com:8000
    app_id: ${env:GAME_APP_ID}
    app_secret: ${env:GAME_APP_SECRET}
    timeout: 20s

service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [game]
```

示例中的 `otlp` 和 `batch` 应已在现有配置中定义。正式服的 endpoint 为
`https://analysis.youwo.com`，环境凭据分别配置。endpoint 支持基础地址或完整的
`/analytics/v1/events` 地址；强制 HTTPS，并拒绝查询参数、用户信息和其他路径。

此组件已加入本仓库本地 Collector 的 builder manifest。按项目流程生成并编译：

```sh
make genotelcorecol
make otelcorecol
```

其他 Collector 发行版需要在其 builder manifest 中加入
`go.opentelemetry.io/collector/exporter/gameexporter`，重新构建后才能识别 `game`。

## 输入约定

每个 LogRecord 的 body 是一条完整的平台事件，支持 OTLP map 或 JSON 对象字符串。
body 的字段就是平台事件字段；resource、scope、log attributes 不会混入事件 JSON。
例如 body：

```json
{
  "event_id": "business-event-unique-id",
  "event_name": "app_launch",
  "schema_version": 1,
  "event_time": 1791331200000,
  "device_id": "install-1",
  "session_id": "session-1",
  "account_type": "guest",
  "app_version": "1.0.0",
  "channel": "official",
  "platform": "linux",
  "market_region": "cn",
  "properties": {}
}
```

上面的时间仅展示整数毫秒格式；实际事件使用创建时的时间。平台按其文档校验所有业务字段。
客户端或上游负责准备 event_id、event_time、设备、会话及其他字段；适配器在发送、拆批和再次调用时保留原内容。
JSON 字符串中的大整数和小数 token 不经过 float64 转换；map 输入保留 OTLP int64 精度。
已在上游转换为浮点数的精度无法恢复。

适配器先检查对象结构、非空字符串 event_id、UTF-8、所有层级的重复键和单条 64 KiB 上限。
不合格项作为永久失败返回，其他有效事件仍会发送。

## HTTP 与确认结果

- 固定使用 POST /analytics/v1/events，外层为 `{"events":[...]}`。
- 每个请求最多 200 条，按实际 JSON 字节拆分，目标不超过 256 KiB；同时校验解压后 4 MiB、传输 1 MiB 硬上限。
- 单条、批量及重发都先生成完整 gzip 流，再对同一传输字节计算 HMAC-SHA256；secret 使用原始 UTF-8 字符串。
- 使用 Collector 的 confighttp 创建 HTTP client，支持标准 TLS、CA 和代理设置。compression 可配置 gzip，实际压缩始终在签名前完成，防止二次压缩。
- 签名头固定为 X-App-ID、X-Timestamp、X-Signature；仅允许额外配置 Origin，用于 Web 应用凭据。禁止替换签名的 auth 或 middleware。
- 每次发送生成当前时间戳和签名；有效响应 Date 用于校正后续签名时间，不修改 event_time。每个 exporter 实例同时最多一个请求在途。
- 不跟随重定向，避免将签名凭据发往新地址。响应读取有大小上限，日志不记录原始事件或 secret。

只有 HTTP 200 且 ACK 完整性校验通过才确认事件：检查所有计数、results 数组、整数 index、范围、顺序、重复项、event_id 和 status。
没有出现在 results 中的项被确认；rejected 作为永久失败，retryable 以及尚未发送的后续事件作为可重试失败。
ACK 无效时整份当前请求保留为未确认；此前已经确认的请求不再加入失败子集。

可重试失败使用 `consumererror.Logs` 携带需要保留的原始日志子集，调用方应使用 `Data()` 处理该子集。
永久失败返回 `consumererror.NewPermanent`。混合结果中的 rejected 记录受限错误诊断，并从可重试子集中排除。
429 解析秒数和 HTTP-date 两种 Retry-After，以 Collector 标准 throttle error 返回；5xx、网络错误和无效 ACK 返回可重试错误。
其他 HTTP 状态作为永久请求错误返回。

批次触发、失败后的等待、队列持久化和业务开关的应用由 SDK 或上游处理；远程配置可通过下述 Collector 入口转发。
这个 exporter 是同步 HTTP 适配器，不在内部安排重发，也不以通用指数退避替代平台约定。
因此，仅配置 batch processor 并不构成完整的平台客户端投递保证；需要调用方承担失败保存和重试。

## 远程控制开关转发

SDK 只配置 Collector 的接入地址，不需要平台地址、app_id 或 app_secret。
SDK 向 Collector 发起 GET /analytics/v1/config；game_config extension 提供 HTTP 入口，
game exporter 复用上报所用的平台地址、凭据、TLS、静态 Origin 和时钟偏差签名请求平台，再返回配置。

在已有 Collector 配置中增加以下内容，保留现有 receiver、processor 和 pipeline：

```yaml
extensions:
  game_config:
    endpoint: localhost:13134

exporters:
  game:
    endpoint: https://analysis-dev.youwo.com:8000
    app_id: ${env:GAME_APP_ID}
    app_secret: ${env:GAME_APP_SECRET}
    timeout: 20s
    config_extension: game_config

service:
  extensions: [game_config]
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [game]
```

service.extensions 应合并已有 extension 列表。默认只监听本机；远程 SDK 接入时，使用已有 HTTPS
反向代理将 /analytics/v1/config 路由至此入口，或通过 confighttp 标准 TLS/auth 配置开放监听。
若 SDK 要求与 OTLP 接入共用域名，可由现有网关按路径分别转发至 OTLP receiver 和 game_config。
浏览器跨域接入使用标准 cors.allowed_origins、cors.allowed_headers 配置；如需读取响应头，
配置 cors.exposed_headers 为 Date、X-Request-ID、Retry-After。正式页面的入口必须为 HTTPS。
SDK 的 Authorization 仅用于 Collector 的接入鉴权；平台签名由 exporter 生成。
SDK 的 Cookie、Origin、X-App-ID 和 X-Signature 不转发给平台；Web 平台所需 Origin 仍在 exporter.headers 静态配置。

SDK 请求不能携带查询参数或请求体。成功配置校验版本、开关、事件列表及刷新范围后，保持原始 JSON 返回，
包含 config_version、reporting_enabled、disabled_events、refresh_seconds；不根据请求参数选择平台凭据。
一个 game_config 实例只绑定一个 game exporter/application。多应用按 game_config/android、
game_config/ios 等不同实例和监听地址配置，分别绑定对应 exporter；重复绑定或缺失 extension 将启动失败。
config_extension 留空则不注册转发；移除 service.extensions 中未使用的入口即可关闭监听。

平台的 400–599 状态、错误响应体和 Date、X-Request-ID、Retry-After 透传，响应统一 no-store。
网络故障、响应过大、重定向或无效成功配置返回 502，超时返回 504；exporter 未绑定或已关闭时返回 503。
Collector 不伪造 reporting_enabled=true，不缓存配置，也不在内部轮询；SDK 断开会取消相应平台请求。
请求超时沿用 exporter.timeout，配置转发单独限制并发，不占用事件发送的顺序 gate。

SDK 仍需在启动及后续 refresh_seconds 周期拉取，周期加入 ±30% 抖动；只应用更高版本，
将最近有效配置持久化，失败沿用旧配置。reporting_enabled 决定停止或恢复入队和发送，
disabled_events 按 event_name 精确过滤；在途 ACK、队列保留和过期清理由 SDK 按平台文档处理。
这些客户端行为与 Collector 的 HTTP 转发职责分别实现。

## 本地模拟与验证

生产代码面向最小 `httpDoer` 接口。单元测试注入模拟实现，逐项验证实际签名、gzip 字节和 ACK；
另有本地 httptest TLS server 测试覆盖 Collector factory、标准 TLS 配置以及重定向拦截。
远程配置测试覆盖签名固定向量、开关启用/关闭/恢复、错误透传、凭据隔离、超时、取消及组件注销。
均不依赖平台真实凭据或发送业务事件。

```sh
make -C exporter/gameexporter generate
make -C exporter/gameexporter fmt lint test
make -C exporter/gameexporter test GOTEST_OPT="-timeout 240s -race -tags=integration"
make -C extension/gameconfigextension generate fmt lint test
```

生成代码使用项目的 mdatagen 和 builder；test 使用项目 gotestsum 及默认 race 检查。
