# CLIProxyAPI Cursor 插件

将 Cursor 接入 CLIProxyAPI，支持流式回复、图片、工具调用和额度展示，沿用 CPA 的全局代理与请求日志。
Go 原生插件，运行时无需 Node.js。

[English](README.md)

## 商城安装

Release 发布后，可添加本仓库的第三方商城源：

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  store-sources:
    - "https://raw.githubusercontent.com/spancerxing/cliproxyapi-cursor-plugin/main/registry.json"
```

在 CPA 商城安装 **Cursor**。发布构建覆盖 Linux AMD64/ARM64、macOS AMD64/ARM64 和 Windows AMD64，
由 CPA 按运行平台下载；Docker 按容器的 Linux 架构选择。运行时不需要构建工具。
Docker 部署请挂载可写的插件目录。官方索引收录后可省略第三方源配置。

## 从源码构建

需要 Go 1.23+、C 编译器和 Make。在与 CPA 相同的系统、CPU 架构下执行：

```sh
git clone https://github.com/spancerxing/cliproxyapi-cursor-plugin.git
cd cliproxyapi-cursor-plugin
make build
```

产物位于 `dist/`：Linux 为 `spx-cursor.so`，Windows 为 `spx-cursor.dll`，macOS 为 `spx-cursor.dylib`。
Windows 构建需要匹配架构的 MinGW 工具链。

## 安装到 CPA

CPA 需要支持 C ABI v1 插件。将动态库放入插件目录，并在 CPA 配置中启用：

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  configs:
    spx-cursor:
      enabled: true
      priority: 1
```

将 `dir` 改为实际路径，重启 CPA 后检查加载日志。Docker 部署需挂载插件目录，配置填写容器内路径。
手动安装需放入编译后的动态库；仅放入源码或填写 GitHub 仓库地址不会加载插件。

## 登录

在 CPA 中选择 Cursor 登录。若界面没有入口，可使用 CPA v8 管理接口：

```sh
curl 'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=cursor' \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

打开返回的 `url` 完成授权，再用返回的 `state` 查询，直到 `status` 为 `ok`：

```sh
curl "http://127.0.0.1:8317/v8/management/oauth/status?state=$STATE" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### API Key 登录

安装插件后，在 CPA 的插件资源菜单打开 **Cursor**，
填写 CPA 管理密钥、Cursor API Key 和可选的账号名称，点击“验证并保存”。
插件先验证 API Key，成功后由 CPA 保存账号；刷新账号和模型列表即可使用。
页面不保存浏览器中的密钥。浏览器授权入口继续保留。

默认资源地址为 `http://你的CPA地址/v0/resource/plugins/spx-cursor/login`；
如果 CPA 使用其他资源前缀，以管理面板中的资源链接为准。

也可在 CPA 导入 Cursor API Key 凭据：

```json
{
  "type": "cursor",
  "cursor_auth": {"type": "api", "key": "YOUR_CURSOR_API_KEY"}
}
```

## 使用

从 CPA 的 `/v1/models` 获取模型 ID，在客户端选择对应的 `cursor/…` 模型。
浏览器登录的访问令牌会在过期前约一天通过 CPA 的凭据刷新自动续期，刷新保留账号的启用/禁用设置。
如果 Cursor 返回无效的新令牌或拒绝续期，插件会保留当前令牌并停止续期，重启后也不会重试。
当前令牌可继续使用至到期（除非 Cursor 提前撤销），重新登录后恢复自动续期。
临时网络或服务端故障仍可在后续刷新时重试。暂不支持单账号代理覆盖。

## 开发

```sh
make test
```

验证范围与限制见 [VALIDATION.md](VALIDATION.md)。真实 Cursor 账号和完整 CPA 联调尚未验证。
参考 [magpie Cursor 插件](https://github.com/magpie-community/plugins/tree/main/packages/cursor) 移植，采用 MIT 许可证，来源见 [NOTICE.md](NOTICE.md)。

发布与官方索引提交流程见 [docs/publishing.md](docs/publishing.md)。
