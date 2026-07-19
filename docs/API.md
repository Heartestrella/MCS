# HTTP API

面板前端的全部功能都走这套 API,可以直接用脚本调用。Base URL 默认 `http://127.0.0.1:8145`。

## 约定

- 请求体和响应都是 JSON(`Content-Type: application/json`),文件上传除外(multipart)
- 出错时返回非 2xx 状态码,响应体为 `{"error": "人话描述"}`
- 简单操作成功返回 `{"ok": true}`
- `{id}` 是实例 ID(12 位 hex),从实例列表接口获取

## 认证

面板未设密码时所有接口直接可用。设了密码后,除 `/api/auth/*` 和 `/api/public/*` 外的 `/api/*` 接口都需要登录会话(Cookie)。

```
POST /api/auth/login
{"username": "admin", "password": "xxxxxx"}
```

成功后返回 `Set-Cookie: mcs_session=<token>`(有效期 7 天),后续请求带上即可:

```bash
curl -c cookie.txt -X POST http://127.0.0.1:8145/api/auth/login \
  -d '{"username":"admin","password":"xxxxxx"}'
curl -b cookie.txt http://127.0.0.1:8145/api/instances
```

同 IP 10 分钟内密码错 5 次会被暂时封禁(429)。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/auth/status` | 是否启用密码 / 当前会话是否有效 |
| POST | `/api/auth/login` | 登录,`{username, password}` |
| POST | `/api/auth/logout` | 注销当前会话 |
| POST | `/api/auth/set` | 开关 / 修改密码,`{enabled, username, password}` |

## 实例

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances` | 实例列表 |
| GET | `/api/instances/{id}` | 单个实例 |
| POST | `/api/instances` | 创建 Minecraft 服务器 |
| POST | `/api/instances/import` | 导入已有服务器目录,`{name, path, port, memoryMB}` |
| POST | `/api/instances/generic` | 创建通用服务器(SteamCMD / 自定义命令) |
| PATCH | `/api/instances/{id}` | 修改设置 |
| DELETE | `/api/instances/{id}` | 删除(需先停止;移入回收站保留 7 天) |

创建:

```
POST /api/instances
{
  "name": "生存服",
  "type": "paper",          // paper(默认) / purpur / fabric / forge / neoforge
  "version": "1.21.4",
  "loaderVersion": "",      // fabric/forge/neoforge 可指定,空=最新稳定版
  "port": 25565,            // 默认 25565
  "memoryMB": 2048,         // 默认 2048
  "dir": ""                 // 自定义部署路径(可选,绝对路径空目录)
}
```

返回 201 和实例对象,此时 `status` 为 `downloading`,下载安装在后台进行,轮询 `GET /api/instances/{id}` 直到 `status` 变为 `stopped`(就绪)或 `error`。

实例对象主要字段:

```
{
  "id": "a1b2c3d4e5f6",
  "name": "生存服",
  "type": "paper",
  "version": "1.21.4",
  "port": 25565,
  "memoryMB": 2048,
  "status": "running",      // stopped / starting / running / stopping / downloading / sleeping / error
  "players": 2,
  "playerList": ["Alice", "Bob"],
  "uptimeSec": 3600,
  "cpuPct": 12.5,
  "memUsedMB": 1500,
  "tps": 20.0,              // Paper 系运行中才有
  "autoRestart": true, "autoSleep": false, "autoStart": false,
  "error": "",              // status=error 时的原因
  "dlLabel": "", "dlDone": 0, "dlTotal": 0   // downloading 时的进度
}
```

`PATCH /api/instances/{id}` 可改字段(只传要改的):`name`、`memoryMB`(重启后生效)、`autoRestart`、`autoSleep`、`autoStart`、`javaPath`(空字符串=恢复自动匹配)。

## 启停与控制台

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/instances/{id}/start` | 启动 |
| POST | `/api/instances/{id}/stop` | 停止 |
| POST | `/api/instances/{id}/restart` | 重启(未运行则直接启动) |
| POST | `/api/instances/{id}/sleep` | 手动进入待机(停服但监听端口,玩家连入自动唤醒) |
| POST | `/api/instances/{id}/command` | 发控制台命令,`{"command": "say hello"}` |
| GET | `/api/instances/{id}/console` | WebSocket:连上先回放最近 500 行,之后实时推送;发文本帧=执行命令 |

```bash
# 例:向服务器广播
curl -X POST http://127.0.0.1:8145/api/instances/a1b2c3d4e5f6/command \
  -d '{"command":"say 5 分钟后重启"}'
```

## 玩家

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/players` | 白名单 / OP / 封禁列表,`whitelistOn` |
| POST | `/api/instances/{id}/players` | `{"action": "...", "name": "玩家名"}` |
| GET | `/api/instances/{id}/stats` | 在线人数曲线、玩家时长排行 |
| GET | `/api/instances/{id}/chat` | 游戏内聊天记录 |

action 取值:`whitelist-add` / `whitelist-remove` / `op` / `deop` / `ban` / `pardon` / `kick`。运行中走控制台命令即时生效;停服状态直接改 JSON 文件(`kick` 除外)。

## 文件

路径参数 `path` 是相对实例目录的路径,有穿越防护。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/files?path=` | 目录列表 `[{name, isDir, size}]` |
| GET | `/api/instances/{id}/files/download?path=` | 下载文件 |
| GET | `/api/instances/{id}/files/zipdir?path=` | 目录打包成 zip 下载 |
| DELETE | `/api/instances/{id}/files?path=` | 删除(server.properties / eula.txt 受保护) |
| POST | `/api/instances/{id}/files/upload` | multipart 上传:`file` + `path`(目标目录),上限 1GB |
| GET | `/api/instances/{id}/files/content?path=` | 读文本文件 `{content}`(限 2MB,常见文本扩展名) |
| POST | `/api/instances/{id}/files/content` | 写文本文件,`{path, content}` |
| POST | `/api/instances/{id}/files/rename` | `{path, newName}` |
| POST | `/api/instances/{id}/files/mkdir` | `{path}` |
| POST | `/api/instances/{id}/files/unzip` | 解压 zip/mrpack 到同名文件夹,`{path}` |

## 日志

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/logs` | logs/ 下的日志文件列表(含 .gz) |
| GET | `/api/instances/{id}/logs/{file}` | 日志内容(纯文本,最多尾部 512KB,.gz 自动解压) |
| GET | `/api/instances/{id}/logs/search?q=` | 跨文件搜索 |

## 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/props` | server.properties,`{props: {k:v}, meta}` |
| POST | `/api/instances/{id}/props` | 合并写入,body 为 `{"k": "v"}`;改 `server-port` 会同步实例元数据 |
| GET | `/api/instances/{id}/timedcmds` | 定时指令列表 |
| POST | `/api/instances/{id}/timedcmds` | 整组覆盖,`[{intervalMin, command, enabled}]` |
| GET | `/api/instances/{id}/java` | 当前实例的 Java 信息 |
| POST | `/api/instances/{id}/fixjava` | 自动安装匹配的 Java 并重启 |

## 联机

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/addresses` | 地址中心:局域网 / frp / UPnP / 公网,带推荐与警告 |
| GET | `/api/instances/{id}/netdoctor` | 联机诊断(7 项检查) |
| POST | `/api/instances/{id}/netfix` | 一键修复诊断出的问题 |
| POST | `/api/instances/{id}/upnp` | 开 UPnP 端口映射 |
| DELETE | `/api/instances/{id}/upnp` | 关 UPnP 映射 |
| GET/POST | `/api/instances/{id}/frp` | frp 配置(custom 自建 / sakura 樱花) |
| POST | `/api/instances/{id}/frp/start` | 启动 frpc |
| POST | `/api/instances/{id}/frp/stop` | 停止 frpc |
| GET/POST | `/api/instances/{id}/frp/toml` | 直接读写 frpc.toml(高级) |
| GET | `/api/frp/sakura/tunnels` | 列出樱花账号下的隧道 |
| GET | `/api/instances/{id}/geyser` | Geyser 基岩互通状态 |
| POST | `/api/instances/{id}/geyser` | 安装 Geyser + Floodgate(仅 Paper/Purpur) |
| DELETE | `/api/instances/{id}/geyser` | 卸载 |
| GET/POST | `/api/instances/{id}/statuspage` | 公开状态页开关,`{enabled}` |
| GET | `/api/public/status/{slug}` | 公开状态数据(无需登录) |

## 模组

Modrinth 代理,面板做了版本 / 加载器兼容过滤。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/mods/search` | 搜索。参数:`q`、`type`(mod/plugin/datapack)、`version`、`loader`、`category`、`sort`、`offset`(每页 20) |
| GET | `/api/mods/{slug}/versions` | 某项目的可用版本 |
| POST | `/api/mods/install` | 安装,`{instId, url, filename, kind, slug, title, versionId}` |
| GET | `/api/mods/updates` | 检查已装模组更新 |
| POST | `/api/mods/update` | 更新单个模组 |
| GET | `/api/instances/{id}/installed` | 已装模组 / 插件列表 |
| POST | `/api/instances/{id}/installed` | 停用 / 启用 / 删除,`{action, dir, name}` |
| POST | `/api/instances/{id}/installed/upload` | 手动上传 jar |
| POST | `/api/modpack/install` | 从 URL 装整合包建服,`{name, url, filename, port, memoryMB}` |
| POST | `/api/modpack/upload` | 上传整合包建服(multipart:`name` + `file`,上限 2GB) |
| GET | `/api/instances/{id}/export` | 导出客户端整合包(仅模组服) |

## 世界

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/world` | 世界信息 |
| POST | `/api/instances/{id}/world/reset` | 重置,`{target: "all"/"nether"/"end", seed, backup}` |
| POST | `/api/instances/{id}/world/upload` | 上传单机存档替换(需停服) |
| GET/POST | `/api/instances/{id}/icon` | 服务器图标(PNG,POST body 即图片字节) |
| POST | `/api/instances/{id}/clone` | 克隆实例 |

## 备份

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/backups` | 备份列表 `[{file, instId, instName, size, createdAt}]` |
| POST | `/api/instances/{id}/backup` | 立即备份 |
| GET | `/api/backups/{file}/download` | 下载 |
| GET | `/api/backups/{file}/ls` | 浏览 zip 内容 |
| POST | `/api/backups/{file}/extract` | 局部恢复选中的条目 |
| POST | `/api/backups/restore` | 整包恢复,`{file, instId, noSnap}`(默认恢复前先做安全快照) |
| DELETE | `/api/backups/{file}` | 删除备份 |
| GET/POST | `/api/autobackup` | 自动备份配置,`{enabled, intervalMin, keep, onlyRunning, cloudUpload}` |
| GET | `/api/trash` | 回收站列表 |
| POST | `/api/trash/{name}/restore` | 恢复已删实例 |
| DELETE | `/api/trash/{name}` | 彻底删除 |
| GET/POST | `/api/webdav` | WebDAV 云盘配置 |
| POST | `/api/webdav/test` | 测试连接 |
| GET | `/api/cloud` | 云端备份列表 |
| POST | `/api/cloud/upload/{file}` | 上传备份到云盘 |
| POST | `/api/cloud/pull/{file}` | 从云盘拉回 |

## 核心更新

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/core/check` | 检查 Paper 新构建 |
| POST | `/api/instances/{id}/core/update` | 更新核心(同版本换 build) |
| GET | `/api/instances/{id}/upgrade/versions` | 可升级的更高版本(仅 Paper) |
| POST | `/api/instances/{id}/upgrade` | 跨版本升级,`{version, backup}` |

## 监控

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/instances/{id}/health` | TPS、CPU / 内存历史、卡顿事件、评级 |
| GET | `/api/stats` | 主机指标:CPU / 内存 / 磁盘 / 各实例占用 |
| GET | `/api/activity` | 面板动态(创建 / 启停 / 崩溃等事件流) |
| GET | `/api/system` | 系统信息(os / arch / javaPath) |
| GET | `/api/versions` | 可用 Minecraft 版本列表(缓存 1 小时) |
| GET | `/api/loaders?type=&version=` | 加载器版本列表(fabric / forge / neoforge) |

## 面板设置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/api/mail` | 邮件通知配置,`{enabled, host, port, user, authCode, to}`;`authCode` 传空或 `********` 保持不变 |
| POST | `/api/mail/test` | 发测试邮件 |
| GET/POST | `/api/ai` | AI 助手配置,`{enabled, baseURL, apiKey, model}`(OpenAI 兼容);`apiKey` 同上不回显 |
| POST | `/api/ai/test` | 测试 AI 连通 |
| POST | `/api/ai/models` | 列出可用模型 |
| POST | `/api/instances/{id}/ai/analyze` | AI 分析本次启动日志,返回原因和可执行动作 |
| POST | `/api/instances/{id}/ai/apply` | 执行 AI 给出的修复动作 |
| GET/POST | `/api/restart` | 每日定时重启,`{enabled, time: "HH:MM", warn, backup}` |
| GET/POST | `/api/bootstart` | 面板开机自启,`{enabled}` |

## 通用服务器(非 Minecraft)

```
POST /api/instances/generic
{
  "name": "饥荒服",
  "mode": "steamcmd",       // steamcmd / custom
  "steamAppId": 343050,     // steamcmd 模式
  "execCmd": "",            // custom 模式:启动命令行
  "stopCmd": "",            // 停止指令,空=直接结束进程
  "port": 0
}
```

`PATCH /api/instances/{id}/generic` 修改同样字段。通用实例同样支持启停、控制台、文件、备份,不支持待机唤醒等 Minecraft 专属功能。
