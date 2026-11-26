# pto-center

`pto-center` 是为 [pypto-tools](https://github.com/pypto-tools) 设计的服务器工具管理中心。管理员通过 NetBird 打开网页，选择一个工具和一批机器，即可下发配置、发布指定版本，或运行该工具在页面上注册的自定义动作。

中心、Agent 和 HTML 管理面板已提供首个可运行版本。Linux 上准备 Go、C 编译器、Python 3 和 Make 后，以普通用户运行本地双节点演示：

```sh
make demo
```

- [运行、部署与接入](docs/running.md)包含登录方式、发布包签名、NetBird 监听、systemd 示例和两个试点适配器的能力范围。
- [阅读完整方案](docs/design.md)，包括配置批量编辑语义、插件清单示例和断线后如何确认结果。
- [查看实施进度与试点验收（#1）](https://github.com/pypto-tools/pto-center/issues/1)，按阶段查阅验收条件及验证记录。
