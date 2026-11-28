# 工具仓库接入指南

一个工具要出现在「安装工作流」的下拉框里，需要发布一个声明 `install` 的签名包，并由管理员把这个版本注册到 Center。安装命令写在工具仓库的适配器里。工作流保存工具版本、执行顺序和要下发的配置字段，管理员在网页上选好这些内容就能使用。

## 受管服务发包准则

每个由 Center 安装或升级的服务，都在自己的仓库独立发版。建议在仓库的 `pto-center/` 目录维护清单、适配器和可选的配置 Schema。工具维护者负责发布可安装的版本，Center 管理员决定哪些机器在何时使用它。

| 发生什么变化 | 应该怎么发布 |
|---|---|
| 程序、适配器、动作清单或配置 Schema 有变更 | 为该服务创建新版本。建议用 Git 标签标记发布提交，例如 `v1.2.0`。CI 构建并测试该提交，测试通过后用同一版本号打签名包。同一服务的一个版本号只对应一个内容摘要。 |
| 已有配置字段的值有变更 | 在 Center 编辑配置并下发，或把值保存到安装工作流。例如把健康轮询间隔从 10 秒改为 30 秒，只需创建配置修订。 |
| 同一版本从 CI 组推广到 User 组，或用于新机工作流 | 选择已经注册的同一份包。Center 为任务和工作流步骤固定包摘要，各组部署相同的程序，各机仍可使用不同的配置。 |

工具维护者把清单、适配器、可选配置 Schema 和待安装程序一起签名。Shell 或 Python 工具可以把多个运行文件归档为 `tar.gz`，通过 `pto-center pack --payload` 放进签名 JSON。仅用于盘点或配置已有安装的适配器可以单独发包。安装、升级、激活和回退的具体操作由各工具实现，只在清单中声明已支持的动作。[打包命令示例](#打包并注册示例版本)

各工具可以把签名 JSON 放在自己仓库的 GitHub Release，由 Center 定期验签并登记。同一份包也可以手动上传，或从已登记的仓库文件路径同步。[配置发布密钥和工作流](#从-github-actions-独立发版)

注册成功后，新版本出现在工具目录。管理员在网页上选定版本和目标机器并下发任务，或运行固定了该版本的工作流。Agent 优先使用本机已验证的包，缺包时通过 NetBird 按摘要从 Center 取包并验签，再调用适配器。验收时核对已安装版本和实际运行版本。首批试点计划用 `pto-system-doctor` 验证安装、升级和回退，`npu-taskqueue` 接入升级时要保留原生的更新预约和队列空闲检查。[试点范围与进度](running.md#试点验证)

### 从 GitHub Actions 独立发版

1. 管理员在 Center 的「工具与配置 → 工具发布密钥 → 接入工具」填写清单中的工具 ID 和 GitHub 仓库（如 `pypto-tools/pto-system-doctor`）。点「生成工具密钥」。浏览器使用 Web Crypto 在本机生成 Ed25519 密钥对。已有一套签名流水线时，可直接选择「登记已有公钥」，用于让多个 Center 信任同一工具。
2. 打开该工具仓库的 `Settings → Environments`，创建或选择名为 `release` 的环境，将刚生成的私钥保存为环境 Secret `PTO_SIGNING_KEY`。回到 Center，勾选「我已保存私钥」，点「登记公钥」。此时状态为「待验证发布包」。Center 只接收公钥，关闭窗口后私钥无法从 Center 取回。在不支持 Ed25519 Web Crypto 的浏览器上，可用 CLI 的 `pto-center keygen --dir <新目录>` 生成同一格式的密钥，再登记 `release.pub`。[GitHub 环境 Secret 设置](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets#creating-secrets-for-an-environment)
3. 下载同一面板的「发布工作流」，放入工具仓库的 `.github/workflows/publish-tool.yml`。模板默认从 `pto-center/pto-tool.json` 读取清单。按需修改 `TOOL_DIR`，把 `PTO_PAYLOAD` 设为该目录内的程序或归档相对路径。如果安装包需要编译，在模板标注处加入该仓库的构建步骤。仅分发适配器时可留空。[下载工作流源文件](../internal/center/tool-release.yml)
4. 从任意开发机推送版本标签，例如 `v1.2.0`。Actions 读取该仓库的 Secret，用兼容的 `pto-center pack` 签出 `<工具ID>-v1.2.0.json`，作为该标签的 GitHub Release 附件。已有正式 Release 时会补充附件；已有草稿时会将附件加入草稿，待仓库发布者将其公开。重试遇到同名附件会比较内容。已用过的版本号应继续对应同一份包。
5. 在 Center 点击该工具的「验证 GitHub 发布」，或等待下一次自动检查（每 15 分钟）。Center 读取最新正式 Release，核对标签、附件、签名、清单工具 ID 和仓库后登记版本。页面显示「签名已验证」、版本和检查时间；失败会显示原因，并保留最近验签通过的版本。Center 使用 Release 附件证明流水线能产出受信包，GitHub Secret 的值仅保存在仓库发布环境。登记后可在「工具与配置」或「安装工作流」选择目标主机，运行结果见「发布记录」。

先部署支持工具公钥的 Center 和 Agent。原 `v0.1.0` Agent 需要先升级，旧平台签名的已登记版本仍可使用。工具密钥丢失时，管理员在「密钥详情 → 更换发布密钥」生成新密钥，更新 GitHub Secret 后登记。请使用新标签发布；历史已登记版本按原签名继续可用。平台 Center 和 Agent 自身升级始终使用部署时的受信平台公钥。

### 自动化安装的三个阶段

每个可安装的服务都应在工具仓库里定义三个阶段，并用同一份签名包在目标机执行。三个阶段可以共用一个适配器。

| 阶段 | 工具负责什么 | 在 pto-center v1 中怎样接入 |
|---|---|---|
| 环境检查 | 以 Agent 的实际身份，只读检查该版本需要的架构、运行时、目录权限等条件。条件不足时，在写入前结束，并在回执里写明缺少什么。 | Agent 先用 `probe` 盘点现有安装。需要安装时，适配器在 `install` 开头执行环境检查。 |
| 执行安装 | 从已验签的 `PTO_RELEASE_FILE` 取出目标版本，调用工具自己的安装入口。重试时保留本机配置和业务数据。 | 清单声明 `install`，由 Agent 在目标机调用适配器。安装所需的脚本随包分发。 |
| 验收安装结果 | CLI 可实际执行 `--version`。常驻服务应检查就绪状态，并从正在服务的进程读出运行版本。确知仍待重启时回报 `pending-activation`，无法核实时回报 `unknown`。 | Agent 在安装后再次调用 `probe`。工作流按 `installedVersion` 和 `runningVersion` 核对目标版本，验收通过才继续该主机的下一步。 |

在隔离环境中分别验证依赖缺失、新机安装成功、安装命令返回 0 但服务尚未就绪三种情况。最后一种应让该主机的工作流停在当前步骤。

Agent 在 `probe` 判定工具尚未安装时会跳过自定义动作。如果要在网页上给这类主机单独下发「仅检查环境」，需要给协议和派发逻辑增加对应动作。

### 上报安装阶段

`install` 适配器通过 stdout 输出逐行 JSON，每行立即刷新。用顶层 `status: "progress"` 报告进度，用 `phases` 中的一个条目报告当前阶段。新接入的工具应依次上报 `preflight`（环境检查）和 `install`（执行安装），阶段开始用 `running`，结束用 `succeeded` 或 `failed`。整个动作最后只输出一条顶层终态，并以退出码 0 表示回执已完整交付。适配器可在最后一条回执里同时给出最后一个阶段的结果。

例如，新机缺少服务依赖的 Python 3 时，安装入口输出下面两行后结束。网页会显示环境检查失败、缺少的依赖和原因码，后两阶段显示「未执行」。

```jsonl
{"status":"progress","summary":"检查运行时和目录权限","phases":{"preflight":{"state":"running"}}}
{"status":"failed","summary":"缺少 Python 3","phases":{"preflight":{"state":"failed","code":"missing_dependency"}}}
```

环境检查通过时，先上报 `preflight.succeeded`，再上报 `install.running`，最后报告安装入口的结果。每个阶段可附独立的 `summary`（最多 1024 字节），省略时沿用本行的顶层摘要。`code` 是可供自动化识别的原因码，使用小写字母开头，后接小写字母、数字、下划线或连字符，最长 64 个字符。原生命令退出码可以放进顶层 `data` 作为诊断信息。[可运行的适配器示例](../examples/demo-info/adapter.py)

`verify`（安装验收）由 Agent 在独立的 `probe` 中核对目标版本。安装入口成功、旧进程仍在服务时，安装阶段可成功，验收显示「待激活」。Center 按主机和任务保存各阶段最近一次回执。在「安装工作流 → 运行记录」和「发布记录 → 逐机任务」都可以回看。已有安装可跳过前两阶段，用盘点结果验收。旧包可继续只返回最终结果，缺少证据的阶段显示「未上报」。升级 Center 和 Agent 后即可收集新回执。

## 从一个可安装的工具开始

仓库里的 `demo-info` 是一个能运行 `--version` 的命令行工具。它把下面的 `pto-tool.json` 和 `adapter.py` 放在同一目录。打包命令的 `--dir` 指向这个目录，清单内的文件路径都相对于它。[完整示例目录](../examples/demo-info)

```json
{
  "apiVersion": "pto-center/v1",
  "id": "demo-info",
  "source": {"type": "git", "repository": "pypto-tools/pto-center"},
  "adapter": "adapter.py",
  "capabilities": ["probe", "install"]
}
```

接入自己的工具时，将 `id` 换成稳定的工具名，`source.repository` 填承载发布包的 `owner/repository`。`adapter` 指向能在目标 Linux 主机上直接执行的入口。若依赖 Python 或工具自己的更新程序，目标机需要具备相应的解释器或入口。适配器以 Agent 服务账号的权限运行。

在示例中，`probe` 查找已安装的程序，运行它的 `--version` 取得实际版本。`install` 从 `PTO_RELEASE_FILE` 读取随包分发的程序，在临时目录校验版本，再替换本机入口。已有原生安装程序的工具，可以在自己的 `install` 分支调用它，更新和回滚也各有对应分支。常驻服务的 `probe` 应分别读取已安装版本和正在服务的进程版本。[示例安装实现](../examples/demo-info/adapter.py)

| 想在网页上做什么 | 工具仓库需要实现什么 |
|---|---|
| 查看主机上有没有这个工具 | 所有工具都实现 `probe`，报告 `present`、`absent` 或 `unknown`。 |
| 在新机上安装，加入工作流 | 声明并实现 `install`。安装后再盘点，已安装版本和运行版本都匹配，工作流才继续。 |
| 更新、激活、回退 | 按实际支持情况声明 `update`、`activate`、`rollback`。在相应动作里执行本工具的空闲检查、锁定、重启或版本切换。 |
| 编辑配置，在工作流中预填字段 | 同时提供 `configSchema`、`configSchemaVersion` 和 `read-config`、`validate-config`、`apply-config` 三个动作。要用于安装工作流，还需支持 `install`。 |
| 增加自己的按钮 | 在 `actions` 中声明 `custom.*` 的 `id` 和按钮标题 `title`，并在适配器中处理同一个 ID。`effect: read` 允许只读用户查询，`effect: change` 仅允许管理员发布。`supportsBatch` 决定是否允许多选主机，可选的 `inputSchema` 决定表单参数。 |

`demo-service` 展示配置与自定义动作的完整接法。配置 Schema 中 `maxConcurrent` 的标题是「最大并发数」，范围是 1 到 64。动作 `custom.sample` 的标题是「查看采样窗口」，勾选两台机器、填入 3 天，两台 Agent 会分别收到 `params.days = 3`。[配置 Schema](../examples/demo/config.schema.json) · [动作清单](../examples/demo/pto-tool.json) · [对应实现](../examples/demo/adapter.py)

## 适配器接口

每次调用时，适配器从 stdin 读取 Agent 传入的一个 JSON 对象。`taskId`、`tool`、`action`、`targetVersion` 和 `bundle` 标识任务和发布版本。`PTO_TOOL_DIR` 是工具在 Agent 下的私有持久目录，也是适配器的工作目录。`--payload` 接收一个文件，Agent 验签后将其路径放在 `PTO_RELEASE_FILE`。多文件程序可以先打成一个归档，再由适配器解包。

配置的 `read-config` 在 `data` 返回托管字段。`validate-config` 收到的是合并后的完整 `values`，`apply-config` 收到的是本次选中的 `values` 及预览基线 `expected`。一个键未出现在 `expected` 中表示预览时该字段没有值。适配器在本机写锁内复核基线，只改选中的字段，保留原生配置中的其他设置及文件权限。

适配器在 stdout 逐行输出 JSON，可先输出 `progress`，最后输出一条结果并以退出码 0 结束。网页会展示 `summary` 和 `data`。例如，下面是安装后 Agent 再次调用 `probe` 时可读到的结果。

```json
{"status":"succeeded","summary":"盘点完成","data":{"presence":"present","installedVersion":"v1","runningVersion":"v1"}}
```

`probe` 还可以报告本机的 `capabilities`、`writtenRevision` 和 `effectiveRevision`。写操作可返回 `succeeded`、`pending-activation`、`failed`、`conflict` 或 `unknown`。配置已落盘但还要等服务重新加载时，返回 `pending-activation`。要让中心确认配置生效，Agent 还会读回所写字段，并从盘点中核实生效修订或 `"activation": "effective"` 的运行证据。

配置和动作输入的根 Schema 使用 `additionalProperties: false` 的对象。自定义动作的整个参数对象可用顶层 `$ref` 指向本地定义，将 `properties`、`required` 和 `additionalProperties` 放在同一个被引用的对象里。中心和 Agent 均按声明校验参数。[注册清单 Schema](../protocol/v1/tool.schema.json) · [带本地引用的动作示例](../examples/demo/pto-tool.json) · [JSON Schema 的封闭对象规则](https://json-schema.org/understanding-json-schema/reference/object#extending-closed-schemas)

## 打包并注册示例版本

在仓库根目录执行下面的命令，可以用独立的临时目录验证从清单到中心目录的全过程。`--payload demo-info` 指向 `examples/demo-info` 内的待安装程序。`validate`、`pack` 和 `register` 都应输出 `demo-info v1` 和相同的摘要。

```sh
make build
umask 077
trial_dir=$(mktemp -d)
bin/pto-center keygen --dir "$trial_dir/publisher"
bin/pto-center validate --dir examples/demo-info --version v1 --payload demo-info
bin/pto-center pack --dir examples/demo-info --version v1 --payload demo-info \
  --signing-key "$trial_dir/publisher/signing.key" --out "$trial_dir/demo-info-v1.json"
bin/pto-center init --state "$trial_dir/center" --trust "$trial_dir/publisher/release.pub"
bin/pto-center register --state "$trial_dir/center" --file "$trial_dir/demo-info-v1.json"
```

示例使用临时签名密钥。正式发布时由发布者保管私钥，Center 和 Agent 使用对应公钥验签。

注册后，在「工具与配置」选择该版本可看到「首次安装」。在「安装工作流」创建模板并选择此版本，选一台已接入的新机运行，逐机回执应显示「已安装」。再对同一台机器运行，版本已满足时会显示「原已安装」。带步骤配置的演练可用 `make demo` 启动，演示中心已注册 `demo-info` 和 `demo-service`。[安装并下发配置](running.md#新主机与安装工作流)
