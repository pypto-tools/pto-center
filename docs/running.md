# 运行和接入

## 本地双节点演示

在 Linux 上准备 Go 1.21 或更新版本、C 编译器、Make 和 Python 3。中心通过 CGO 使用 SQLite，Agent 可用 `CGO_ENABLED=0` 构建。以普通账号运行以下命令。

```sh
make demo
```

打开终端显示的环回地址，用 `admin` 和输出中指向文件的密码登录。演示启动一个中心和 `demo-a`、`demo-b` 两个独立 Agent，默认监听本机 8080 和 8081 端口。可以用 `PTO_DEMO_ADMIN_PORT` 和 `PTO_DEMO_NODE_PORT` 指定空闲端口。按 Ctrl-C 退出会清理演示进程及其独立临时目录。

Control Center 使用整页点阵画布。实线表示当前在线的管理连接，虚线表示已登记、当前离线的 Agent，未接入的主机没有连线。通过顶部按连接状态、主机名或分组筛选，拖动画布平移，使用滚轮、双指或右下角按钮缩放，也可以点击「适应画布」回到全图。左上角可收起侧栏。

点击中心或任一 Agent 卡片上的「详情」展开该机，点击「简约」收起。每台机器的选择会在当前浏览器中保留。详细模式显示当前登录用户数和 systemd 服务状态，点击「查看 … 项」可搜索这台机器的服务列表。「该机工具」链接会打开这台主机的工具盘点。

左上角切到「工具版本」，选择工具和目标版本。节点显示最近盘点的已安装版本和运行版本，两者都匹配才计入已运行目标版本的主机数。展开卡片可看盘点时间和工具已上报的配置写入、生效修订。离线机器标记为快照。

「分批发布」用于为同一个工具版本分两次执行更新。管理员点击「新建轮次」，选择发布包、第一批和第二批分组。预览固定本轮的机器名单，同时属于两组的机器只进入第一批。未接入、尚未盘点、未安装或不支持更新的机器会列出原因。确认第一批后，中心开始派发，离线 Agent 的任务进入队列。

点击「第二批剩余」查看共用机器和第二批独有机器。第一批的回执需要带有目标安装版本和运行版本的证据，中心在发布第二批前还会核对最近盘点。若第一批有待激活或失败的主机，可处理该机后在「工具与配置」选择同一发布包并「刷新盘点」。卡片保留原回执，并列出后续核对任务。第一批全部核实后，管理员可预览并确认第二批。轮次、目标名单和任务关联保存在中心 SQLite 中，只读用户可查看全程。

中心机直接采集自身状态。Agent 在管理连接建立后立即采样，之后每 30 秒通过独立的认证请求上报。在线人数按 Linux 的 `who /run/utmp` 登录记录去重，同一账号多个终端只计一人，仅上报总数。系统服务来自 `systemctl list-units --type=service --all`，覆盖 systemd 当前载入的服务，每项保留 unit、active 和 sub 状态。卡片优先显示失败和过渡状态，最多上报 256 项，超过时在页面标明截断。旧 Agent 或无法读取的指标显示「未知」，成功采集的零值显示为 0。中心按收到报告的时间标记快照，离线后仍可查看上次数据，在线超过 90 秒未更新会标明采样过期。

1. 在「工具与配置」选择 `demo-service · v1` 和「首次安装」，勾选两台机器，预览并发布。在「发布记录」核对两条结果。
2. 选择「下发配置」和两台机器，只勾选「最大并发数」，填 8，预览两行 `4 → 8` 再发布。配置生效后，「主机清单」可查看写入与生效修订号。
3. 点击「查看采样窗口」，填 3 天并发布。展开逐机结果可查看返回的数据。
4. 在一台机器上先发布 v2，再对另一台发布。选择 v1 的「回退版本」可演练回滚。示例只操作 Agent 状态目录中的演示服务。

增减目标机器时，页面保留已编辑的字段并刷新各机器的当前值。发布时中心再次核对这些字段，已观察到的变化会要求重新预览。同一节点有连续写入时，中心依据前一笔的结果和回读值决定后一笔的预期值。前一笔结果不明时，重新盘点并预览。「发布记录」每页显示 200 条，使用「较早记录」可以查看大批次中其余机器的结果。

## 新主机与安装工作流

1. 让新服务器加入 NetBird，并安装、注册一次 `pto-agent`。「主机清单」显示已接入后即可作为安装目标，即使各工具还没有盘点记录。[首次接入一台 Agent](#首次接入一台-agent)
2. 管理员在侧栏打开「安装工作流」。新建时点「创建工作流」，命名为「向服务器配置」。修改已有工作流时，在「工作流模板」下拉框选中它，点「编辑模板」。
3. 在本地双节点演示中，选择 `demo-info · v1`，点「添加步骤」，选择 `demo-service · v1`。在这一步的「安装后下发配置」勾选「最大并发数」，填 `8`。只勾选要统一设置的字段，其余字段沿用各机原值。可以用「上移」「下移」连同配置一起调整顺序。点击「保存模板」会生成新的修订。
4. 需要实际执行时，选中一台或多台主机，点击「运行工作流」。也可以从「主机清单」对应机器的「运行工作流」进入，再选模板。编辑和保存时不会向主机下发任务。
5. 在「运行记录」选批次和主机。带配置的软件分别显示安装回执和配置回执。中心固定本次的模板修订、包摘要、字段值与主机名单。后续编辑只影响新运行。

安装回执分为环境检查、执行安装和安装验收。页面列出每个阶段的最近状态，并展示已上报的说明和原因码。在「发布记录 → 逐机任务」展开同一安装任务也能回看。环境检查失败会将后两项标为「未执行」。旧任务或旧适配器缺少的阶段信息标为「未上报」。[阶段回执格式](tool-integration.md#上报安装阶段)

每台机器各自按顺序执行。Agent 在本机探测，缺少软件时调用该工具的 `install` 适配器。中心收到回执并核实安装版本和运行版本均为目标版本后，才给这台主机排入下一步。原已满足的步骤显示「原已安装」。遇到已有其他版本、待激活、失败或无法核实时，该机停止，后续步骤显示「未执行」。其他主机继续。已接入的离线主机在重连后领取待办。排除故障后，可只对受影响主机重新运行。

带配置的步骤先核实安装，再使用该机刚回读的配置作为基线，合并所选字段并按该发布包的配置 Schema 校验。Agent 在写入前再次核对版本和字段。中心收到所选字段的读回值及生效修订或适配器的明确生效证据后，才排入下一个软件。如果工具已经安装，工作流仍会核对配置。重复运行时，值已匹配且有生效证据的配置显示「配置已满足」。

中心保存签名发布包，通过已认证的 NetBird 管理连接下发任务。Agent 优先使用本机已验证的包缓存，需要时从中心的节点接口按摘要取包并验签。发布者可用 `--payload` 将程序随包分发，安装适配器从 `PTO_RELEASE_FILE` 读取。这样目标机安装包内程序时无需访问 GitHub。命令行工具的适配器可以实际运行已安装入口的 `--version` 核实生效版本。双节点演示的 `demo-info` 就采用这种方式。[安装适配器示例](../examples/demo-info/adapter.py)

模板编辑器仅列出声明了 `install` 的签名发布版本。生产工具要先提供适合其主机权限和安装布局的适配器。[首批工具及其支持的操作](#首批工具)

若一台机器已有其他版本，可在「工具与配置」选择目标发布包，按工具支持的「更新版本」或「回退版本」完成切换。遇到「待激活」时，按工具约定使服务加载配置或通过「激活版本」执行，再「刷新盘点」。确认版本与配置后，只选这台机器重新运行模板即可。

## 生成和注册工具包

Center 的工具列表来自**已注册的签名发布包**。每个包固定清单、版本、适配器、可选配置 Schema 和负载。发布者用独立的 Ed25519 私钥签名，Center 和 Agent 使用对应公钥验签。工具维护者可用仓库内的示例实际跑一遍 `validate`、`pack` 和 `register`。[打包并注册示例版本](tool-integration.md#打包并注册示例版本)

管理员在「工具与配置」选择发布来源。

| 来源 | 填写内容 | 后续版本怎样进入中心 |
|---|---|---|
| 工具的 GitHub Release | 在「工具发布密钥」接入工具，将私钥保存在该仓库的 GitHub 发布环境。 | Center 每 15 分钟检查最新正式 Release 的签名附件，也可立即点击「验证 GitHub 发布」。[仓库发版流程](tool-integration.md#从-github-actions-独立发版) |
| 本地文件 | 选择 `pack` 生成的签名 JSON，点击「注册文件」。在中心机也可用 `pto-center register --state <中心状态目录> --file <签名包>`。 | 每次注册一个版本。 |
| GitHub 仓库文件 | 在「注册发布包」填 `owner/repository`、40 位小写十六进制提交 ID，以及该提交中**签名 JSON** 的仓库相对路径，点击「从 GitHub 导入」。清单的 `source.repository` 须与此仓库匹配。 | Center 每 15 分钟检查默认分支中这个文件的最新提交，也可点击「同步仓库版本」。发布者把签好的新版本提交到同一路径即可。 |

GitHub 同步跟踪管理员登记的工具仓库或文件。注册后，支持 `install` 的版本出现在工作流的选项里，实际安装或更新由管理员发起。私有仓库可在启动中心时用 `--github-token-file` 指定具有必要内容读取权限的凭据文件。

每个工具的版本号唯一绑定到已签名内容。适配器或负载有变化时应使用新的版本号。普通工具的单个已签名 JSON 包上限为 8 MiB，适合脚本和小型发布包。Agent 发布包支持多架构，整个包上限为 32 MiB。

### 平台首装和网页升级

官方发行版同时提供 Linux amd64 和 arm64 首装归档，以及可直接在网页登记的 `pto-center-<版本>.json` 和 `pto-agent-<版本>.json`。每个 JSON 都含两个架构。新部署从对应架构的归档取出所需的可执行文件与 systemd 单元，按下文配置 NetBird、TLS 和账号。两个组件可以同时准备。先启动 Center，再让 Agent 接入。[平台发行版](https://github.com/pypto-tools/pto-center/releases) · [NetBird 部署](#netbird-部署)

首次安装时下载同一版的两个压缩包、两个 JSON、`release.pub`、`release.pem`、`SHA256SUMS` 和 `SHA256SUMS.sig` 到独立目录。先用可信渠道取得的指纹核对 OpenSSL 公钥，再验签和校验文件。首个正式发布者的 PEM 文件 SHA-256 如下。[部署用 Ed25519 公钥](../deploy/release.pub)

```sh
echo 'b478b3f94184c63e488b806c57dc589a40b8c125e83c34b9d82df4bacd04dfc6  release.pem' | sha256sum -c -
openssl pkeyutl -verify -pubin -inkey release.pem -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum -c SHA256SUMS
tar -xzf pto-center-v0.1.0-linux-amd64.tar.gz   # arm64 机器选 linux-arm64
```

1. 新部署使用核验过的 `release.pub` 初始化 Center，并将同一文件放入 Agent 的信任配置。该公钥用于平台升级，并兼容尚未单独登记公钥的业务工具。业务仓库可登记各自的发布公钥。沿用其他公钥的已有部署可用自己的发布密钥重新封装平台产物。
2. 新版本发布后，在「工具与配置 → 中心版本」点「同步官方版本」。Center 从最新正式 GitHub Release 取两个签名包装入本地目录，校验组件、标签和签名，两个均通过才登记。无法直连 GitHub 的中心可在同页点「注册发布包」分别上传文件。登记完成后由管理员选择升级时间。
3. 选择本机架构支持的版本并点「更新中心」。网页请求只包含目标包摘要和当前运行版本。中心机上的独立执行器验签，从已有状态目录启动候选进程，待数据库可用、管理和节点监听均已就绪，并核实候选进程上报的版本后确认。切换期间连接会短暂中断，Agent 自动重连。刷新页面可看到结果，也可用有权限的只读账号访问 `GET /api/version` 读取实际运行版本、平台和协议。
4. 候选版本无法启动、上报版本不符、试运行超时（默认 90 秒）或试运行期间断电，执行器使用上次确认的版本恢复。已切换的进程意外退出时，执行器启动前一个已确认版本，成功后将故障版本记为失败。重启时，执行器最多尝试三次启动已确认版本，然后尝试其前一个版本或最初安装的程序。若两者都无法启动，执行器报告错误，由 systemd 按单元策略重启。要主动回退，在同一面板选择已注册的旧签名包，点「回退到所选版本」。数据库、凭据和配置一直使用原目录。发版应确保数据库变更兼容需要回退到的版本。基础执行器的 `--version` 显示其自身的版本。
5. Center 验收后，在「工具与配置」选择 `pto-agent` 更新单台，或在「分批发布」按组选试点和后续主机，核对新版重连后再推广。[Agent 分批升级](#管理-agent-自身的版本)

从早于 v0.1.0 的 Center 接入时，需在中心机上完成一次受信首装包的二进制替换并重启原单元。原有 `pto-center serve` 入口会启动独立执行器。已有中心保留原状态目录和信任配置。

### 发布新的平台版本

将已通过 CI 的提交打上新标签（如 `v0.1.0`）并推送。仓库的 `Build platform release` 工作流分别在原生 amd64、arm64 Linux 上构建可执行文件，核对两个程序的 `--version` 并用实际 Center 初始化 SQLite。构建机只需源码读取权限。本地原生构建的入口为 `bash scripts/build-release.sh <版本> <输出目录>`。

在持有发布密钥的机器上检出标签对应的同一提交，用 GitHub CLI 下载该标签的成功构建。将命令中的工作流运行 ID 和发布者目录换为实际值。发布者目录内需有相配的 `signing.key` 与 `release.pub`。

```sh
version=v0.1.0
gh run download <运行ID> -D "dist/builds-$version"
make build
python3 scripts/package-release.py --version "$version" --artifacts "dist/builds-$version" \
  --publisher <发布者目录> --out "dist/$version"
```

打包脚本核对源码提交、版本、架构和文件摘要，签出两个原生升级包，在临时 Center 注册验签，并为首装归档生成可由 OpenSSL 校验的脱机签名。将输出目录中的所有文件附到相同标签的 GitHub Release，使用草稿核对附件后发布。后续同步只读取正式 Release。

### 管理 Agent 自身的版本

先更新中心，再在每台机器部署一次支持托管升级的 `pto-agent` 基础版。沿用原节点名、凭据和状态目录。systemd 的 `ExecStart` 指向 `app/pto-agent`，它以服务账号启动工作进程。后续下载、试运行和回退使用可写且可执行的 `state/`。支持托管升级的基础版完成认证并上报实际运行版本及 Linux 架构后，管理员即可从中心发起后续升级。旧节点尚未报告版本时，主机清单显示「未上报」。

发布时给二进制嵌入与发行包一致的版本号。下面在仓库根目录构建两种架构，并用已有的发布者私钥打成一个包。将 `--signing-key` 换成发布者私钥的实际路径。

```sh
version=v1.2.0
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/pypto-tools/pto-center/internal/agent.BuildVersion=$version" \
    -o "dist/pto-agent-$arch" ./cmd/pto-agent
done
bin/pto-center pack-agent --version "$version" \
  --binary dist/pto-agent-amd64 --binary dist/pto-agent-arm64 \
  --signing-key ./publisher/signing.key --out "dist/pto-agent-$version.json"
```

在网页「注册发布包」上传签名 JSON。通过「工具与配置 → pto-agent → 更新版本」选择一台或多台机器，或在「分批发布」按组预览、确认。中心在发布和派发时都核对平台和本机升级能力。新版进程用原凭据重连并领取同一任务、报告目标版本后，中心才记为成功。启动失败、版本不符、试运行期间断电，或在 `--upgrade-timeout`（默认 90 秒）内未能确认时，启动器会恢复上次确认的版本。恢复连接后，原任务会记录失败原因。需要主动回退时，选择已注册的旧版本并执行「回退版本」。

在「主机清单」点击 `pto-agent` 列的版本号，可看最近 100 条版本或架构变化及观察时间。相同版本和平台的重连只刷新盘点时间。离线主机保留最后上报值。运行 `pto-agent --version` 会打印所调用文件的构建版本，`app/pto-agent` 是基础启动器，中心显示当前工作进程上报的版本。

### 适配器接口

工具维护者按清单声明的动作实现一个可执行入口，通过 stdin 接收任务，通过 stdout 回报结果。[请求字段、盘点返回值和配置生效约定](tool-integration.md#适配器接口)

单个任务按最终 JSON 编码限 1 MiB，回执限 512 KiB，两者均包含消息末尾换行。过大的任务会在发布时提示错误。过大的回执保留任务 ID 和执行状态，并在摘要中说明省略的详情。盘点数据也被省略时，可以重新盘点。配置基线按工具和 Schema 版本分别保存，节点报告工具已移除时会清除相应基线。

## NetBird 部署

中心宿主机也要加入 NetBird。在该机的 NetBird 接口上选两个端口，管理端口开放给管理员设备，节点端口开放给受管服务器。生产模式会核对 IP 归属，并将监听 socket 绑定到指定接口。`--local` 只用于环回演示。验收时从获准设备、其他 peer 和网外分别检查两个端口。[访问策略与网络验收](design.md#部署和访问)

### 启动中心

从仓库根目录运行 `make build` 可生成本机架构的 `bin/pto-center` 和 `bin/pto-agent`。首次部署时，发布者在自己的私有目录用 `bin/pto-center keygen --dir <密钥目录>` 生成密钥对，将 `release.pub` 分给中心和各 Agent，自己保管 `signing.key`。

准备 `pto-center` 服务账号和 `/home/pypto-tools/pto-center/{app,config,state}`，让该账号拥有模式为 `0700` 的 `state/`。将中心二进制放在 `app/pto-center`，发布公钥放在 `config/release.pub`，TLS 证书与私钥放在 `config/` 并允许服务账号读取。下面的 `config/center.env` 使用示例 IP 和端口，请替换为中心机实际 NetBird 接口和地址。证书须覆盖浏览器和 Agent 连接时使用的名称或 IP。

```ini
ADMIN_LISTEN=100.64.0.10:8080
NODE_LISTEN=100.64.0.10:8081
NETBIRD_INTERFACE=wt0
TLS_CERT=/home/pypto-tools/pto-center/config/center.crt
TLS_KEY=/home/pypto-tools/pto-center/config/center.key
```

在新中心上以服务账号初始化一次，再安装仓库提供的 systemd 单元。已有中心升级时沿用原状态目录。单元用 `CAP_NET_RAW` 将监听限定到接口。[中心 systemd 单元](../deploy/pto-center.service)

```sh
sudo -u pto-center /home/pypto-tools/pto-center/app/pto-center init \
  --state /home/pypto-tools/pto-center/state \
  --trust /home/pypto-tools/pto-center/config/release.pub
sudo install -m 0644 deploy/pto-center.service /etc/systemd/system/pto-center.service
sudo systemctl daemon-reload
sudo systemctl enable --now pto-center.service
```

从获准的 NetBird 设备打开管理端口（示例为 `https://100.64.0.10:8080`）。网页账号 `admin`、`viewer` 的密码分别保存在 `state/admin.token` 和 `state/viewer.token`。只读账号可查看状态和运行工具声明的只读查询。

### 首次接入一台 Agent

1. 在中心网页打开「主机清单 → 注册节点」，填写节点名，例如 `ci-test-01`，按需加入 `CI`、`User` 等组，生成一次性注册码。也可在中心机用 `pto-center invite --state <中心状态目录> --node ci-test-01 --group CI --group User` 生成。将注册码单独交给目标机。
2. 在目标机准备 `pto-agent` 普通服务账号和 `/home/pypto-tools/pto-agent/{app,config,state}`。把匹配该机 Linux 架构的基础程序放入 `app/pto-agent`。将注册码写入 `config/invite`，发布公钥放入 `config/release.pub`，签发中心 TLS 证书的 CA 放入 `config/center-ca.pem`。服务账号需要能读取这三个文件，并拥有可写、允许执行、模式为 `0700` 的 `state/`。
3. 在 `config/agent.env` 填入与注册码一致的节点名和中心的 **HTTPS 节点地址**。下面的配置对应上文示例。

```ini
NODE_NAME=ci-test-01
CENTER_URL=https://100.64.0.10:8081
```

在目标机安装并启动 Agent 单元。首次启动会将节点凭据持久化，之后用原状态目录重连。[Agent systemd 单元](../deploy/pto-agent.service)

```sh
sudo install -m 0644 deploy/pto-agent.service /etc/systemd/system/pto-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now pto-agent.service
```

在「主机清单」确认该机在线并检查上报的 Agent 版本，点击版本号可看变更历史。如果仍显示「未接入」，在目标机用 `journalctl -u pto-agent.service -n 50` 核对节点端口、证书、节点名和注册码。注册后重启可省略 `--invite-file`。回收节点身份可用 `pto-center revoke --state <中心状态目录> --node <节点名>`。

同一台主机可以同时属于 `CI` 和 `User`。管理员在「主机清单 → 编辑分组」中调整成员关系。「工具与配置」的「全选筛选结果」按节点名去重。在 Control Center 创建先 `CI` 后 `User` 的分批发布轮次时，共用机器只进入第一批。

示例单元只允许 Agent 写自己的状态目录。业务工具如果需要修改其原生配置目录，还要为该路径配置服务账号的文件权限及对应的 systemd 写入范围。

## 首批工具

下表列出本仓库可打包的适配器。已部署中心的实际列表取决于管理员注册了哪些版本。

| 工具 | 可加入安装工作流 | 此版本可执行的操作 |
|---|---|---|
| `demo-info`（示例） | 可以 | 安装包内 CLI，并从实际入口读取版本。[清单](../examples/demo-info/pto-tool.json) |
| `demo-service`（示例） | 可以，可预填配置 | 在 Agent 状态目录中演练安装、更新、激活、回退、按字段配置和自定义动作。[清单](../examples/demo/pto-tool.json) |
| `pto-system-doctor` | 尚缺 `install` | 识别标准安装，读取和修改 `resource-guard.conf` 中的健康轮询间隔及告警冷却时间。写入保留其他字段、注释及权限，结果为待激活。[清单](../integrations/pto-system-doctor/pto-tool.json) |
| `npu-taskqueue` | 尚缺 `install` | 识别标准安装和发布标记，通过原生 `task-submit` 查询队列摘要、NPU 使用统计。接管更新还需对接原工具的更新预约、空闲队列和受限路径。[清单](../integrations/npu-taskqueue/pto-tool.json) |

## 试点验证

| 范围 | 已观察到的结果 |
|---|---|
| NetBird 双机安装工作流 | 目标 Agent 离线时任务进入队列。重连后依次安装 `demo-info v1` 和 `demo-service v1`，本机 CLI 和服务均报告 `v1`。原样重跑时两步均跳过。 |
| Agent 自升级 | `linux/amd64` 和 `linux/arm64` 的试点 Agent 均从 `pilot-v1` 切到 `pilot-v2`，重新连接后中心记录了新旧版本。[自升级与失败恢复测试](../tests/agent_update_test.go) |
| 工作流步骤配置 | 自动化测试覆盖安装后读回配置、生效后进入下一步、重跑跳过已满足字段，以及单机失败时其他机器继续执行。[工作流测试](../tests/workflow_test.go) |

上述安装使用示例工具。业务适配器已有临时目录测试，真实服务的安装、升级、激活与回退仍待逐个工具验证。首个业务试点可从 `pto-system-doctor` 开始，在试点机对照网页回执与本机进程版本、实际加载的配置，再验证升级和回退。`npu-taskqueue` 还需验证有作业时等待空闲及既有更新定时器的交接。[业务适配器测试](../tests/test_pilot_adapters.py) · [试点跟踪 issue](https://github.com/pypto-tools/pto-center/issues/1)

在 Linux 上运行 `make test` 可复核 Go、Python 适配器测试和前端脚本语法（需 Node.js）。要实际运行 Chromium 网页测试，用本机已安装的 Chromium 可执行文件设置 `PTO_CHROME`，例如 `PTO_CHROME=/usr/bin/chromium go test -count=1 -run '^TestBrowserBatchConfiguration$' ./tests`。
