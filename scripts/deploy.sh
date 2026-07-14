#!/usr/bin/env bash
#
# L4D2 RCON Agent - Linux 一键部署脚本
#
# 功能：
#   1. 从 GitHub Release 下载最新 linux_amd64 压缩包并解压
#   2. 交互式询问生成 config.json
#   3. 提示启动方式（不注册 systemd 服务）
#
# 一行安装（推荐，无需先下载脚本）：
#   bash <(curl -fsSL https://raw.githubusercontent.com/Zhrradiant/l4d2-rcon-agent/main/scripts/deploy.sh)
#
# 本地运行（脚本位于仓库 scripts/ 目录）：
#   bash scripts/deploy.sh              # 默认装到 ~/l4d2-rcon-agent
#   bash scripts/deploy.sh /opt/agent   # 指定安装目录
#
# 卸载：删除安装目录（默认 ~/l4d2-rcon-agent）即可，脚本不注册系统服务。
#

set -euo pipefail

# ==================== 配置区 ====================
REPO="Zhrradiant/l4d2-rcon-agent"
INSTALL_DIR="${1:-$HOME/l4d2-rcon-agent}"
ASSET_PATTERN="_linux_amd64.tar.gz"   # Release 资产匹配规则
BINARY_NAME="l4d2-rcon-agent"

# 管道安装（curl ... | bash）时 stdin 被脚本内容占用，
# 需要把交互输入重定向到 /dev/tty，否则 read 读不到用户键入。
if ! [[ -t 0 ]]; then
    if [[ -r /dev/tty ]]; then
        exec 0</dev/tty
    else
        echo "[✗] 检测到非交互式环境且无法打开 /dev/tty，无法继续交互式安装。" >&2
        echo "    请改用本地运行：先下载 deploy.sh，再 bash deploy.sh（位于仓库 scripts/ 目录）" >&2
        exit 1
    fi
fi

# ==================== 颜色与日志 ====================
if [[ -t 1 ]]; then
    RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
    BLUE=$'\033[0;34m'; CYAN=$'\033[0;36m'; BOLD=$'\033[1m'; NC=$'\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; BOLD=''; NC=''
fi

log()  { echo -e "${GREEN}[✓]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[✗]${NC} $*" >&2; }
info() { echo -e "${BLUE}[i]${NC} $*"; }
step() { echo -e "\n${BOLD}${CYAN}━━ $* ━━${NC}"; }

die() { err "$*"; exit 1; }

# ==================== 前置检查 ====================
step "环境检查"

command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || die "需要 curl 或 wget，请先安装"
command -v tar    >/dev/null 2>&1 || die "需要 tar，请先安装"

# 解析安装目录为绝对路径
case "$INSTALL_DIR" in
    /*) : ;;
    *)  INSTALL_DIR="$HOME/$INSTALL_DIR" ;;
esac
info "安装目录: $INSTALL_DIR"

mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

# ==================== 下载 ====================
step "获取最新 Release"

API_URL="https://api.github.com/repos/${REPO}/releases/latest"
info "查询: $API_URL"

# 拉 Release 元数据
if command -v curl >/dev/null 2>&1; then
    RELEASE_JSON="$(curl -fsSL "$API_URL")" || die "无法访问 GitHub API（检查网络或仓库是否公开）"
else
    RELEASE_JSON="$(wget -qO- "$API_URL")" || die "无法访问 GitHub API（检查网络或仓库是否公开）"
fi

# 提取版本 tag（管道中 grep 无匹配时 || true 容错，避免 pipefail 提前退出）
TAG="$(echo "$RELEASE_JSON" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"
[[ -n "$TAG" ]] || die "未能解析版本号，可能是 GitHub API 限流，稍后重试"
info "最新版本: $TAG"

# 匹配 linux_amd64 资产下载地址（末尾 || true 容错）
ASSET_URL="$(echo "$RELEASE_JSON" \
    | grep -oE '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]+"' \
    | sed -E 's/.*"([^"]+)"$/\1/' \
    | grep -E "$ASSET_PATTERN" \
    | head -1 || true)"

[[ -n "$ASSET_URL" ]] || die "未在 Release $TAG 中匹配到 *$ASSET_PATTERN 资产。请检查 Release 是否已发布对应文件。"
info "下载地址: $ASSET_URL"

ARCHIVE="$(basename "$ASSET_URL")"
if [[ -f "$ARCHIVE" ]]; then
    warn "已存在 $ARCHIVE，将重新下载覆盖"
fi

if command -v curl >/dev/null 2>&1; then
    curl -fSL -o "$ARCHIVE" "$ASSET_URL" || die "下载失败"
else
    wget -qO "$ARCHIVE" "$ASSET_URL" || die "下载失败"
fi
log "下载完成: $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1))"

# ==================== 解压 ====================
step "解压"

# 重复执行（升级）时，根目录可能已有上次的旧二进制。
# 先删除它，避免 find 同时匹配到新旧两个二进制而取错。
rm -f -- "./$BINARY_NAME"

tar -xzf "$ARCHIVE"
log "解压完成"

# 定位二进制
BIN_PATH="$(find . -maxdepth 2 -type f -name "$BINARY_NAME" | head -1 || true)"
[[ -n "$BIN_PATH" ]] || die "压缩包中未找到 $BINARY_NAME 二进制"

# 记录二进制所在的子目录（用于稍后清理空目录）
BIN_SUBDIR=""
if [[ "$BIN_PATH" != "./$BINARY_NAME" ]]; then
    BIN_SUBDIR="$(dirname "$BIN_PATH")"   # 如 ./l4d2-rcon-agent_v0.1_linux_amd64
    mv -f "$BIN_PATH" "./$BINARY_NAME"
    BIN_PATH="./$BINARY_NAME"
fi

# 压缩包只产出二进制，二进制归位后删除它原先所在的子目录（已为空）
if [[ -n "$BIN_SUBDIR" && -d "$BIN_SUBDIR" ]]; then
    rm -rf -- "$BIN_SUBDIR"
fi

# 清理下载的压缩包，避免残留垃圾
rm -f -- "$ARCHIVE"

chmod +x "$BINARY_NAME"
log "二进制就绪: $INSTALL_DIR/$BINARY_NAME"

# ==================== 生成 config.json ====================
step "生成配置文件"

CONFIG_FILE="$INSTALL_DIR/config.json"

if [[ -f "$CONFIG_FILE" ]]; then
    warn "已存在 config.json"
    read -rp "覆盖重新生成？[y/N] " overwrite
    if [[ "${overwrite:-N}" != "y" && "${overwrite:-N}" != "Y" ]]; then
        log "保留已有 config.json"
        SKIP_CONFIG=1
    else
        SKIP_CONFIG=0
    fi
else
    SKIP_CONFIG=0
fi

if [[ "$SKIP_CONFIG" -eq 0 ]]; then
    echo
    echo "请按提示输入配置参数："
    echo

    # 监听端口
    read -rp "HTTP 监听端口 [27051]: " listen
    listen="${listen:-27051}"

    # Token
    read -rp "鉴权 Token（可留空）: " token

    # 公网 IP
    read -rp "游戏服务器公网 IP（便于站点识别，可留空）: " host

    # RCON 连接地址
    read -rp "RCON/UDP 连接地址 [127.0.0.1]: " rcon_host
    rcon_host="${rcon_host:-127.0.0.1}"

    # 房间端口
    echo "房间端口支持单端口(27015) 或 端口段(27015-27020)，逗号分隔"
    while true; do
        read -rp "房间端口: " port_input
        ports=()
        IFS=',' read -ra port_parts <<< "$port_input"
        ok=true
        for part in "${port_parts[@]}"; do
            part="$(echo "$part" | tr -d ' ')"
            [[ -z "$part" ]] && continue
            if [[ "$part" == *-* ]]; then
                start="${part%-*}"; end="${part#*-}"
                if ! [[ "$start" =~ ^[0-9]+$ && "$end" =~ ^[0-9]+$ && "$start" -ge 1 && "$end" -le 65535 && "$start" -le "$end" ]]; then
                    err "无效端口段: $part"; ok=false; break
                fi
                for ((p=start; p<=end; p++)); do
                    ports+=("$p")
                done
            else
                if ! [[ "$part" =~ ^[0-9]+$ && "$part" -ge 1 && "$part" -le 65535 ]]; then
                    err "无效端口: $part"; ok=false; break
                fi
                ports+=("$part")
            fi
        done
        $ok && break
    done
    info "解析到 ${#ports[@]} 个端口: ${ports[*]}"

    # RCON 密码模式
    if [[ ${#ports[@]} -gt 1 ]]; then
        echo "  1. 所有端口使用同一个密码"
        echo "  2. 逐个端口输入密码"
        read -rp "选择 [1]: " pwd_mode
        pwd_mode="${pwd_mode:-1}"
    else
        pwd_mode="1"
    fi

    # 构建 rooms JSON 片段
    # JSON 字符串转义：处理密码中可能含有的反斜杠和双引号，避免破坏 config.json
    json_escape() {
        local s="$1"
        s="${s//\\/\\\\}"   # 反斜杠先转义
        s="${s//\"/\\\"}"  # 双引号转义
        printf '%s' "$s"
    }

    rooms_json="["
    first=1
    gen_room() {
        local p="$1" pwd="$2"
        local escaped
        escaped="$(json_escape "$pwd")"
        [[ $first -eq 0 ]] && rooms_json+=","
        rooms_json+="{\"port\":$p,\"password\":\"$escaped\"}"
        first=0
    }

    case "$pwd_mode" in
        2)
            for p in "${ports[@]}"; do
                read -rsp "  端口 $p 的 RCON 密码: " pwd; echo
                gen_room "$p" "$pwd"
            done
            ;;
        *)
            read -rsp "  统一 RCON 密码: " pwd; echo
            for p in "${ports[@]}"; do
                gen_room "$p" "$pwd"
            done
            ;;
    esac
    rooms_json+="]"

    # 写入 config.json（host/token 留空时输出空字符串）
    # 对 token 和 host 也做 JSON 转义，防止特殊字符破坏结构
    token_esc="$(json_escape "$token")"
    host_esc="$(json_escape "$host")"
    cat > "$CONFIG_FILE" <<EOF
{
  "listen": ":${listen}",
  "token": "${token_esc}",
  "host": "${host_esc}",
  "rcon_host": "${rcon_host}",
  "rooms": ${rooms_json}
}
EOF
    chmod 600 "$CONFIG_FILE"
    log "配置已写入: $CONFIG_FILE"
fi

# ==================== 完成提示 ====================
step "部署完成"

cat <<EOF

${GREEN}L4D2 RCON Agent 已部署到：${BOLD}$INSTALL_DIR${NC}

${CYAN}目录结构：${NC}
  $INSTALL_DIR/
  ├── $BINARY_NAME      主程序
  └── config.json         配置文件

${CYAN}启动方式：${NC}
  cd "$INSTALL_DIR"
  ./$BINARY_NAME          # 交互式面板（首次配置/测试）
  ./$BINARY_NAME -serve   # 直接启动 HTTP 服务（后台运行可加 nohup）

${CYAN}提示：${NC}若运行时提示 Permission denied，请执行 chmod +x $BINARY_NAME

${CYAN}验证服务：${NC}
  curl "http://localhost:${listen:-27051}/health"

${CYAN}如需开机自启，${NC}可自行编写 systemd unit 调用：
  ExecStart=$INSTALL_DIR/$BINARY_NAME -serve

EOF
