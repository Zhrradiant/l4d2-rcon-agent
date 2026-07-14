# L4D2 RCON Agent

> Version: v0.1.1

部署在游戏服务器本机的轻量 HTTP 服务，查询所有房间在线玩家（含 Steam ID）。

## 准备

安装 Go 编译器：https://go.dev/dl/

## 编译

```bash
# Windows 产物
build-win.bat

# Linux 产物（在 Windows 上交叉编译，产物传到 Linux 服务器赋权 chmod +x 即可运行）
build-linux.bat
```

## Linux 一键部署

游戏服务器上执行（需能访问 GitHub）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Zhrradiant/l4d2-rcon-agent/main/scripts/deploy.sh)
```

脚本会自动完成：

1. 从 GitHub Release 下载最新 `*_linux_amd64.tar.gz` 并解压
2. 交互式询问（监听端口、Token、公网 IP、房间端口、RCON 密码）生成 `config.json`
3. 输出启动命令（不注册 systemd 服务，按需自行配置自启）

> 依赖：`curl` 或 `wget`、`tar`。卸载：删除安装目录（默认 `~/l4d2-rcon-agent`）即可，脚本不注册任何系统服务。

## 使用

```bash
# 首次运行（交互式配置）
./l4d2-rcon-agent

# 直接启动（跳过面板，用于开机自启）
./l4d2-rcon-agent -serve
```

## config.json

```json
{
  "listen": ":27051",
  "token": "your-token",
  "host": "203.0.113.10",
  "rooms": [
    { "port": 27015, "password": "rcon_pass" }
  ]
}
```

> `host` 填服务器公网 IP，用于站点识别。RCON/UDP 始终连本机。

## 接口

```
GET /players?token=your-token
```

```json
{
  "rooms": [
    {
      "addr": "203.0.113.10:27015",
      "online": true,
      "players": [
        { "name": "Player1", "steamid": "STEAM_1:0:12345678" }
      ]
    }
  ]
}
```

## 原理

UDP A2S_PLAYER 查玩家名指纹 → 没变就跳过 RCON → 变了才打 RCON 拿权威数据。所有房间并发查询。
