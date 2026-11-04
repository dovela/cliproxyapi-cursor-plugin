# 发布

插件 ID 为 `spx-cursor`，登录 provider 和模型前缀为 `cursor`。
官方索引已存在其他作者的 `cursor` 插件，因此使用独立 ID。

## Release

每次推送和 PR 会构建 Linux AMD64/ARM64、macOS AMD64/ARM64、Windows AMD64 五个平台包。
macOS 分别使用 Intel 和 Apple Silicon runner 构建，并在对应架构上执行 C ABI 加载测试。
Linux 使用 manylinux2014 工具链，目标为 glibc 2.17+，适用于官方 Debian Bookworm Docker 镜像。
不提供 Alpine/musl 安装包。

正式发布前同步修改 `runtime.go` 和 `registry.json` 中的版本，再提交并推送：

```sh
git push origin main
git tag v0.1.2
git push origin v0.1.2
```

GitHub Actions 会执行测试、各平台 C ABI 加载、ZIP 格式检查，并发布：

```text
spx-cursor_0.1.2_linux_amd64.zip
spx-cursor_0.1.2_linux_arm64.zip
spx-cursor_0.1.2_darwin_amd64.zip
spx-cursor_0.1.2_darwin_arm64.zip
spx-cursor_0.1.2_windows_amd64.zip
checksums.txt
```

版本标签必须匹配代码和索引。仅推送代码、手动运行工作流不会创建 Release。
工作流使用仓库自带的 `GITHUB_TOKEN`，无需将个人令牌写入文件。
标签构建全部通过后，工作流创建 Release；若同名 Release 已存在，则替换安装包和校验文件。
本次浏览器自动续期及账号状态保护发布为 `v0.1.2`，保留已有版本的标签和 Release。
后续发布继续递增版本，便于已安装的 CPA 识别更新。

## 官方商城索引

`spx-cursor` 已收录官方商城。后续更新只需发布新 Release，CPA 会从插件仓库的
最新正式 Release 获取版本和安装包，无需再向商城提交版本更新 PR。

以下脚本仅用于首次收录新插件，已有插件会被重复检查拒绝。
Release 成功发布后，用有权创建 fork 和 PR 的 GitHub 令牌设置环境变量 `GH_TOKEN`。
先预览，再提交：

```sh
python3 scripts/submit-store-pr.py
python3 scripts/submit-store-pr.py --submit
```

脚本会检查最新正式 Release、全部五个平台包及校验清单，fork 官方商城仓库，
创建独立分支，只修改 `registry.json` 并提交 PR。若 ID 或仓库已经收录则停止。
官方审核合并后，默认商城才能展示。网络或认证失败不会视为提交成功。

历史 PR #277 曾因缺少 macOS 两个平台包被关闭；插件已在补齐平台包后完成收录。

官方格式说明：<https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#adding-a-plugin>
