# 运行和接入

## 本地双节点演示

在 Linux 上准备 Go 1.21 或更新版本、C 编译器、Make 和 Python 3。中心通过 CGO 使用 SQLite，Agent 可用 `CGO_ENABLED=0` 构建。以普通账号运行以下命令。

```sh
make demo
```

打开终端显示的环回地址，用 `admin` 和输出中指向文件的密码登录。演示启动一个中心和 `demo-a`、`demo-b` 两个独立 Agent，默认监听本机 8080 和 8081 端口。可以用 `PTO_DEMO_ADMIN_PORT` 和 `PTO_DEMO_NODE_PORT` 指定空闲端口。按 Ctrl-C 退出会清理演示进程及其独立临时目录。

1. 在「工具与配置」选择 `demo-service · v1` 和「首次安装」，勾选两台机器，预览并发布。在「发布记录」核对两条结果。
2. 选择「下发配置」和两台机器，只勾选「最大并发数」，填 8，预览两行 `4 → 8` 再发布。配置生效后，总览可查看写入与生效修订号。
3. 点击「查看采样窗口」，填 3 天并发布。展开逐机结果可查看返回的数据。
4. 在一台机器上先发布 v2，再对另一台发布。选择 v1 的「回退版本」可演练回滚。示例只操作 Agent 状态目录中的演示服务。

增减目标机器时，页面保留已编辑的字段并刷新各机器的当前值。发布时中心再次核对这些字段，已观察到的变化会要求重新预览。同一节点有连续写入时，中心依据前一笔的结果和回读值决定后一笔的预期值。前一笔结果不明时，重新盘点并预览。「发布记录」每页显示 200 条，使用「较早记录」可以查看大批次中其余机器的结果。

## 生成和注册工具包

工具接入协议要求可信的工具仓库明确声明入口、能力与参数。打包时解析相对路径，校验清单和配置 Schema。发布者用独立的 Ed25519 私钥签署整个发布内容，包括适配器、程序版本和可选负载。中心与节点均只需发布公钥。私钥由发布者保管。

```sh
make build
umask 077
bin/pto-center keygen --dir ./publisher
bin/pto-center validate --dir examples/demo --version v1
bin/pto-center pack --dir examples/demo --version v1 \
  --signing-key ./publisher/signing.key --out ./demo-v1.json
bin/pto-center init --state ./center-state --trust ./publisher/release.pub
bin/pto-center register --state ./center-state --file ./demo-v1.json
```

网页「注册发布包」也支持上传该 JSON，或填写 GitHub 仓库、完整提交和**已签名包**的相对路径。通过 GitHub 导入后，中心每 15 分钟检查该包在默认分支的最新提交。也可以点「同步仓库版本」。新候选可选后仍由管理员决定安装或更新。私有仓库可在启动中心时用 `--github-token-file` 指定具有必要内容读取权限的凭据文件。

每个工具的版本号唯一绑定到已签名内容。适配器或负载有变化时应使用新的版本号。首版的单个已签名 JSON 包上限为 8 MiB，适合脚本和小型发布包。大型工具链的独立制品缓存、分块下载以及多架构制品选择仍待实现。

### 适配器接口

[注册清单 Schema](../protocol/v1/tool.schema.json) 是可执行的协议定义。[演示适配器](../examples/demo/adapter.py) 包含从安装、配置到回滚的例子。配置和动作参数的根 Schema 应是 `additionalProperties: false` 的对象。使用本地定义和引用，Agent 进行校验时无需联网。

自定义动作的整个参数对象可以通过顶层 `$ref` 指向本地定义。把 `properties`、`required` 和 `additionalProperties: false` 一起放在被引用的对象里。中心在注册时检查字段与封闭对象的定义是否兼容，页面根据该定义生成表单，中心和 Agent 都按原始 Schema 校验参数。[示例动作清单](../examples/demo/pto-tool.json) · [JSON Schema 的封闭对象规则](https://json-schema.org/understanding-json-schema/reference/object#extending-closed-schemas)

每次调用从 stdin 读取一个 JSON 对象。`taskId`、`tool`、`action`、`targetVersion` 和 `bundle` 标识已固定的任务与发布版本。自定义参数在 `params`，配置写入在 `values`，预览时已有的字段在 `expected`，缺失的键表示当时尚未设置。`validate-config` 接收完整候选配置，`apply-config` 只接收所选字段。适配器应在自己的本机写锁内再次核对基线，保存未托管字段及文件权限。

适配器的当前目录和环境变量 `PTO_TOOL_DIR` 指向该工具在 Agent 下的私有状态目录。`PTO_RELEASE_FILE` 指向用 `pack --payload` 打包并校验的负载。动作来自注册清单，动态参数只作为 JSON 数据传递。stdout 每行一个 JSON 事件，可以先输出 `progress`，最后输出一个结果。`summary` 和 `data` 都会出现在网页和执行记录中，只返回适合管理员查看的数据。

```json
{"status":"succeeded","summary":"盘点完成","data":{"presence":"present","installedVersion":"v1","runningVersion":"v1"}}
```

单个任务按最终 JSON 编码限 1 MiB，回执限 512 KiB，两者均包含消息末尾换行。过大的任务会在发布时提示错误。过大的回执保留任务 ID 和执行状态，并在摘要中说明省略的详情。盘点数据也被省略时，可以重新盘点。配置基线按工具和 Schema 版本分别保存，节点报告工具已移除时会清除相应基线。

`probe` 的安装状态为 `present`、`absent` 或 `unknown`。可额外报告本机支持的 `capabilities`、`writtenRevision` 和 `effectiveRevision`。`read-config` 在 `data` 返回托管字段的对象。写操作应区分 `succeeded`、`pending-activation`、`failed`、`conflict` 和 `unknown`。声明配置已生效时，Agent 还会读回所写字段并核对运行修订或显式的 `activation: effective` 运行证据。

## NetBird 部署

中心宿主机也需要加入 NetBird。准备一张可验证其内网名称或地址的 TLS 证书。选用该机 NetBird 接口的实际地址与两个独立端口。生产模式会核对接口地址并将监听 socket 绑定到指定接口。`--local` 只允许环回地址。NetBird 放行规则、主机防火墙与公网代理路由按[设计方案](design.md)配置。

建议使用组织约定的 `/home/pypto-tools/<名称>/{app,config,state,logs,tmp}`。将二进制放在 `app/`，由对应服务账号拥有私有 `state/`。只给中心安装发布公钥和 TLS 服务端凭据。初始化产生 `admin.token` 与 `viewer.token`，分别对应管理员和只读账号。只读账号可运行工具声明的只读查询。

- [中心 systemd 单元](../deploy/pto-center.service) 读取 `config/center.env` 中的 `ADMIN_LISTEN`、`NODE_LISTEN`、`NETBIRD_INTERFACE`、`TLS_CERT`、`TLS_KEY`。单元使用 `CAP_NET_RAW` 将监听限定到接口。
- 通过网页「注册节点」或 `pto-center invite --state <中心状态目录> --node <节点名> --group <组名>` 生成一次性注册码。将注册码、发布公钥和中心 TLS CA 文件交给目标 Agent，避免把凭据写入命令历史或 Git。
- [Agent systemd 单元](../deploy/pto-agent.service) 读取 `config/agent.env` 中的 `NODE_NAME` 和 `CENTER_URL`。初次注册由节点先持久化自己的凭据，断线重试可恢复。注册完成后，重启可省略 `--invite-file`。需要回收某台身份时使用 `pto-center revoke --state <中心状态目录> --node <节点名>`。
- Agent 使用普通账号。发布包由可信发布者审核，适配器继承 Agent 的系统权限。样例单元只允许写 Agent 状态目录，生产工具的特定写入按该工具的本机权限单独配置。

## 首批工具

| 工具 | 现有能力 | 验收边界 |
|---|---|---|
| [pto-system-doctor](../integrations/pto-system-doctor/pto-tool.json) | 识别标准安装，读取并按字段更新 `resource-guard.conf` 中的健康轮询间隔与告警冷却时间。 | 写入保留其他字段、注释及权限，并报告待激活。默认服务单元需按站点策略赋予相应权限后才能写入。 |
| [npu-taskqueue](../integrations/npu-taskqueue/pto-tool.json) | 识别标准安装和发布标记，通过原生 `task-submit` 提供队列摘要、使用统计按钮。 | 现有更新器要求 root 管理的路径、更新预约和空闲队列。生产发布入口的接管需在指定试点验证。 |

两个适配器目前随中心仓库提供，可用 `validate`、`pack` 按上文流程接入。其固定原生入口与上游标准安装布局对应，临时目录用例见 [试点测试](../tests/test_pilot_adapters.py)。真实 NetBird 策略验证、生产队列等待与既有 timer 切换按[试点 issue](https://github.com/pypto-tools/pto-center/issues/1)逐机记录。
