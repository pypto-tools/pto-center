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

推荐将签名 JSON 作为 GitHub Release 附件，与标签和变更说明一起保留。目前管理员可下载附件，在 Center 的「工具与配置 → 注册发布包」上传文件。要让 Center 自动同步后续版本，发布者将同一份签名 JSON 提交到工具仓库默认分支的固定路径，由管理员首次登记该来源。直接发现 GitHub Release 附件属于后续功能。[注册与同步操作](running.md#生成和注册工具包)

注册成功后，新版本出现在工具目录。管理员在网页上选定版本和目标机器并下发任务，或运行固定了该版本的工作流。Agent 优先使用本机已验证的包，缺包时通过 NetBird 按摘要从 Center 取包并验签，再调用适配器。验收时核对已安装版本和实际运行版本。首批试点计划用 `pto-system-doctor` 验证安装、升级和回退，`npu-taskqueue` 接入升级时要保留原生的更新预约和队列空闲检查。[试点范围与进度](running.md#试点验证)

### 自动化安装的三个阶段

每个可安装的服务都应在工具仓库里定义三个阶段，并用同一份签名包在目标机执行。三个阶段可以共用一个适配器。

| 阶段 | 工具负责什么 | 在 pto-center v1 中怎样接入 |
|---|---|---|
| 环境检查 | 以 Agent 的实际身份，只读检查该版本需要的架构、运行时、目录权限等条件。条件不足时，在写入前结束，并在回执里写明缺少什么。 | Agent 先用 `probe` 盘点现有安装。需要安装时，适配器在 `install` 开头执行环境检查。 |
| 执行安装 | 从已验签的 `PTO_RELEASE_FILE` 取出目标版本，调用工具自己的安装入口。重试时保留本机配置和业务数据。 | 清单声明 `install`，由 Agent 在目标机调用适配器。安装所需的脚本随包分发。 |
| 验收安装结果 | CLI 可实际执行 `--version`。常驻服务应检查就绪状态，并从正在服务的进程读出运行版本。确知仍待重启时回报 `pending-activation`，无法核实时回报 `unknown`。 | Agent 在安装后再次调用 `probe`。工作流按 `installedVersion` 和 `runningVersion` 核对目标版本，验收通过才继续该主机的下一步。 |

在隔离环境中分别验证依赖缺失、新机安装成功、安装命令返回 0 但服务尚未就绪三种情况。最后一种应让该主机的工作流停在当前步骤。

Agent 在 `probe` 判定工具尚未安装时会跳过自定义动作。如果要在网页上给这类主机单独下发「仅检查环境」，需要给协议和派发逻辑增加对应动作。

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
