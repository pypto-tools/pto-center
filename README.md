# pto-center

`pto-center` 是为 [pypto-tools](https://github.com/pypto-tools) 设计的服务器工具管理中心。管理员通过 NetBird 打开网页，选择一个工具和一批机器，即可下发配置、发布指定版本，或运行该工具在页面上注册的自定义动作。

当前阶段为 **v1 方案设计**。仓库先确定中心、Agent 和工具适配器的接口，以及首批试点的验收条件。服务端、客户端和工具适配器的实现由进度 issue 跟踪。

- [阅读完整方案](docs/design.md)，包括配置批量编辑语义、插件清单示例和断线后如何确认结果。
- [查看实施进度与试点验收（#1）](https://github.com/pypto-tools/pto-center/issues/1)，按阶段查阅验收条件及验证记录。
