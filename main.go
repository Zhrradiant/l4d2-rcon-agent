package main

//
// L4D2 RCON Agent
//
// 轻量 HTTP 服务，暴露本机所有 L4D2 房间的在线玩家 steamid。
// 内置节流：玩家列表没变化时不重复 RCON，直接返回缓存。
//
// 带交互式命令行面板，支持初始化、自检、测试、运行、配置。
//

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ============================================================
// 配置结构
// ============================================================

type RoomConfig struct {
	Port     int    `json:"port"`
	Password string `json:"password"`
}

type Config struct {
	Listen string       `json:"listen"`
	Token  string       `json:"token"`
	Host   string       `json:"host"`   // 游戏服务器公网 IP（便于站点识别，RCON/UDP 仍连本机）
	Rooms  []RoomConfig `json:"rooms"`
}

const configFileName = "config.json"

// ============================================================
// RCON 协议实现（Source RCON Protocol）
// ============================================================

const (
	rconAuth        = 3
	rconAuthResp    = 2
	rconExecCommand = 2
	rconRespValue   = 0
)

type RCONConn struct {
	conn    net.Conn
	reqID   int32
	timeout time.Duration
}

func rconDial(host string, port int, password string, timeout time.Duration) (*RCONConn, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tcp connect failed: %w", err)
	}
	r := &RCONConn{conn: conn, reqID: 1, timeout: timeout}
	if err := r.authenticate(password); err != nil {
		conn.Close()
		return nil, err
	}
	return r, nil
}

func (r *RCONConn) authenticate(password string) error {
	if err := r.sendPacket(rconAuth, password); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	for attempts := 0; attempts < 5; attempts++ {
		pkt, err := r.readPacket()
		if err != nil {
			return fmt.Errorf("read auth resp: %w", err)
		}
		if pkt.id == -1 {
			return fmt.Errorf("auth rejected (wrong password)")
		}
		if pkt.typ == rconAuthResp {
			return nil
		}
	}
	return fmt.Errorf("auth timeout")
}

func (r *RCONConn) command(cmd string) (string, error) {
	if err := r.sendPacket(rconExecCommand, cmd); err != nil {
		return "", fmt.Errorf("send cmd: %w", err)
	}
	var result strings.Builder
	for {
		pkt, err := r.readPacket()
		if err != nil {
			break
		}
		if pkt.typ == rconRespValue {
			if pkt.body == "" {
				break
			}
			result.WriteString(pkt.body)
			if result.Len() > 65535 {
				break
			}
		}
	}
	return result.String(), nil
}

func (r *RCONConn) sendPacket(typ int32, body string) error {
	r.reqID++
	id := r.reqID
	inner := make([]byte, 0, 10+len(body))
	inner = append(inner, byte(id), byte(id>>8), byte(id>>16), byte(id>>24))
	inner = append(inner, byte(typ), byte(typ>>8), byte(typ>>16), byte(typ>>24))
	inner = append(inner, []byte(body)...)
	inner = append(inner, 0, 0)
	size := len(inner)
	packet := make([]byte, 0, 4+size)
	packet = append(packet, byte(size), byte(size>>8), byte(size>>16), byte(size>>24))
	packet = append(packet, inner...)
	_, err := r.conn.Write(packet)
	return err
}

type rconPacket struct {
	id   int32
	typ  int32
	body string
}

func (r *RCONConn) readPacket() (rconPacket, error) {
	r.conn.SetReadDeadline(time.Now().Add(r.timeout))
	sizeBuf := make([]byte, 4)
	if _, err := io.ReadFull(r.conn, sizeBuf); err != nil {
		return rconPacket{}, err
	}
	size := int(sizeBuf[0]) | int(sizeBuf[1])<<8 | int(sizeBuf[2])<<16 | int(sizeBuf[3])<<24
	if size <= 0 || size > 4194304 {
		return rconPacket{}, fmt.Errorf("invalid packet size %d", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.conn, data); err != nil {
		return rconPacket{}, err
	}
	id := int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16 | int32(data[3])<<24
	typ := int32(data[4]) | int32(data[5])<<8 | int32(data[6])<<16 | int32(data[7])<<24
	body := ""
	if size > 10 {
		body = string(data[8 : size-2])
	}
	return rconPacket{id, typ, body}, nil
}

func (r *RCONConn) close() {
	r.conn.Close()
}

// ============================================================
// status 输出解析
// ============================================================

type PlayerInfo struct {
	Name    string `json:"name"`
	SteamID string `json:"steamid"`
}

var playerLineRegex = regexp.MustCompile(
	`^#\s*(\d+)\s+(\d+)\s+"([^"]+)"\s+([A-Z_:0-9]+)\s+(\d+(?::\d+)+)`,
)

func parseStatus(text string) []PlayerInfo {
	var players []PlayerInfo
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") || strings.Contains(line, "userid name") {
			continue
		}
		if m := playerLineRegex.FindStringSubmatch(line); len(m) >= 5 {
			players = append(players, PlayerInfo{Name: m[3], SteamID: m[4]})
		}
	}
	return players
}

// ============================================================
// 房间状态缓存（节流核心）
// ============================================================

type RoomState struct {
	mu          sync.Mutex
	players     []PlayerInfo
	rawNames    string // RCON 玩家名指纹（权威数据）
	udpRawNames string // UDP 玩家名指纹（轻量快照）
	challenge   []byte // 复用 A2S_PLAYER 的 challenge，避免每次握手
}

type Agent struct {
	config      *Config
	rooms       map[int]*RoomState
	rconTimeout time.Duration
	udpTimeout  time.Duration
}

func newAgent(cfg *Config) *Agent {
	a := &Agent{config: cfg, rooms: make(map[int]*RoomState), rconTimeout: 3 * time.Second, udpTimeout: 2 * time.Second}
	for _, room := range cfg.Rooms {
		a.rooms[room.Port] = &RoomState{}
	}
	return a
}

// queryPlayersUDP 用 A2S_PLAYER 查询玩家列表（轻量 UDP，避免 TCP RCON）。
// 返回玩家名列表。内部处理 challenge 握手；若发生握手则返回新 challenge 供调用方缓存复用。
// 协议响应结构（剥离 4 字节包头后）：
//   0x44 [playerCount:u8] 然后循环：index(u8) name(NULL结尾) score(int32 LE) duration(float32 LE)
func (a *Agent) queryPlayersUDP(room RoomConfig) (names []string, newChallenge []byte, err error) {
	addr := fmt.Sprintf("127.0.0.1:%d", room.Port)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("udp dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(a.udpTimeout))

	state := a.rooms[room.Port]
	// 复用缓存的 challenge；没有就用 0xFFFFFFFF 触发握手
	ch := state.challenge
	if len(ch) != 4 {
		ch = []byte{0xFF, 0xFF, 0xFF, 0xFF}
	}

	// request: FF FF FF FF 55 <challenge 4B>
	req := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x55, ch[0], ch[1], ch[2], ch[3]}

	for attempt := 0; attempt < 2; attempt++ {
		if _, err = conn.Write(req); err != nil {
			return nil, nil, fmt.Errorf("udp write: %w", err)
		}
		buf := make([]byte, 1400)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, nil, fmt.Errorf("udp read: %w", err)
		}
		if n < 5 {
			return nil, nil, fmt.Errorf("udp response too short")
		}
		// 跳过 4 字节包头 FF FF FF FF
		resp := buf[4:n]
		switch resp[0] {
		case 0x41: // S2C_CHALLENGE：服务器返回有效 challenge，重试
			if len(resp) < 5 {
				return nil, nil, fmt.Errorf("challenge response too short")
			}
			newChallenge = resp[1:5]
			// 用新 challenge 重建请求再发一次
			req = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x55, newChallenge[0], newChallenge[1], newChallenge[2], newChallenge[3]}
			continue
		case 0x44: // S2A_PLAYER：成功，解析玩家列表
			return parseA2SPlayers(resp), newChallenge, nil
		default:
			return nil, nil, fmt.Errorf("unexpected udp response type: 0x%02x", resp[0])
		}
	}
	return nil, nil, fmt.Errorf("udp challenge retry exhausted")
}

// parseA2SPlayers 解析 A2S_PLAYER 响应负载（已剥离 4 字节包头）。
// 结构：0x44 [count:u8] { index:u8 name:NULL结尾 score:i32LE duration:f32LE }
// 注意 count 不可信，以剩余字节为准。
func parseA2SPlayers(resp []byte) []string {
	if len(resp) < 2 || resp[0] != 0x44 {
		return nil
	}
	pos := 2 // 跳过 0x44 和 count
	var names []string
	for pos < len(resp) {
		pos++ // 跳过 index (u8)
		if pos > len(resp) {
			break
		}
		// name：NULL 结尾字符串
		end := pos
		for end < len(resp) && resp[end] != 0 {
			end++
		}
		if end >= len(resp) {
			break
		}
		name := string(resp[pos:end])
		if name != "" {
			names = append(names, name)
		}
		pos = end + 1 // 跳过 name 和 NULL
		// score (int32 LE) + duration (float32 LE) = 8 字节
		if pos+8 > len(resp) {
			break
		}
		pos += 8
	}
	return names
}

func (a *Agent) fetchRoomPlayers(room RoomConfig) ([]PlayerInfo, bool, error) {
	state := a.rooms[room.Port]
	if state == nil {
		state = &RoomState{}
		a.rooms[room.Port] = state
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	// 1. 先用 UDP 轻量查玩家列表，对比玩家名指纹
	var udpKey string
	udpNames, newCh, udpErr := a.queryPlayersUDP(room)
	if udpErr == nil {
		// 缓存 challenge 供下次复用
		if len(newCh) == 4 {
			state.challenge = newCh
		}
		// 计算 UDP 玩家名指纹（排序后拼接）
		sort.Strings(udpNames)
		udpKey = strings.Join(udpNames, "\x00")
		// 玩家名指纹没变 → 直接走缓存，跳过 RCON
		if udpKey == state.udpRawNames {
			return state.players, false, nil
		}
		// 注意：udpRawNames 延迟到 RCON 成功后才更新，避免 RCON 失败时节流误判
	}
	// UDP 失败不致命，继续走 RCON 兜底

	// 2. UDP 检测到变化（或 UDP 失败）→ 发 RCON 拿详细列表
	rconn, err := rconDial("127.0.0.1", room.Port, room.Password, a.rconTimeout)
	if err != nil {
		return state.players, false, fmt.Errorf("rcon: %w", err)
	}
	defer rconn.close()

	statusText, err := rconn.command("status")
	if err != nil {
		return state.players, false, fmt.Errorf("status: %w", err)
	}

	newPlayers := parseStatus(statusText)
	newNames := make([]string, 0, len(newPlayers))
	for _, p := range newPlayers {
		if p.Name != "" {
			newNames = append(newNames, p.Name)
		}
	}
	sort.Strings(newNames)
	newKey := strings.Join(newNames, "\x00")

	// 空房间（无玩家）→ 清空缓存，返回空列表（避免旧数据固化）
	if len(newPlayers) == 0 {
		state.players = nil
		state.rawNames = ""
		if udpErr == nil {
			state.udpRawNames = udpKey
		}
		return nil, false, nil
	}

	// 玩家名没变 → 走缓存
	if newKey == state.rawNames {
		return state.players, false, nil
	}

	state.players = newPlayers
	state.rawNames = newKey
	// UDP 成功且指纹变化 → 更新 UDP 指纹（此时 RCON 已成功，可安全更新）
	if udpErr == nil {
		state.udpRawNames = udpKey
	}
	return newPlayers, true, nil
}

// ============================================================
// HTTP 接口
// ============================================================

type RoomResponse struct {
	Addr    string       `json:"addr"`   // ip:port（方便下游识别）
	Online  bool         `json:"online"`
	Updated bool         `json:"updated"`
	Players []PlayerInfo `json:"players"`
	Error   string       `json:"error,omitempty"`
}

func (a *Agent) handlePlayers(w http.ResponseWriter, r *http.Request) {
	if a.config.Token != "" {
		if r.URL.Query().Get("token") != a.config.Token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	n := len(a.config.Rooms)
	rooms := make([]RoomResponse, n)
	var wg sync.WaitGroup
	startTime := time.Now()

	for i, room := range a.config.Rooms {
		wg.Add(1)
		go func(idx int, rc RoomConfig) {
			defer wg.Done()
			players, updated, err := a.fetchRoomPlayers(rc)
			addr := ""
			if a.config.Host != "" {
				addr = fmt.Sprintf("%s:%d", a.config.Host, rc.Port)
			} else {
				addr = fmt.Sprintf(":%d", rc.Port)
			}
			resp := RoomResponse{Addr: addr, Online: err == nil, Updated: updated, Players: players}
			if err != nil {
				resp.Error = err.Error()
			}
			rooms[idx] = resp
		}(i, room)
	}

	wg.Wait()

	// 统计
	var updated, skipped, failed int
	for _, resp := range rooms {
		if resp.Error != "" {
			failed++
		} else if resp.Updated {
			updated++
		} else {
			skipped++
		}
	}
	log.Printf("[rcon] 请求完成 | 总房间:%d 更新:%d 跳过:%d 失败:%d | 耗时:%v",
		n, updated, skipped, failed, time.Since(startTime).Truncate(time.Millisecond))

	json.NewEncoder(w).Encode(map[string]interface{}{"rooms": rooms, "time": time.Now().Format("2006-01-02 15:04:05")})
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "rooms": len(a.config.Rooms), "time": time.Now().Format("2006-01-02 15:04:05")})
}

// ============================================================
// 配置文件读写
// ============================================================

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configFileName)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Listen: ":27051"}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if len(cfg.Rooms) == 0 {
		return nil, fmt.Errorf("no rooms configured")
	}
	return cfg, nil
}

func saveConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFileName, data, 0644)
}

func configExists() bool {
	_, err := os.Stat(configFileName)
	return err == nil
}

// ============================================================
// 端口段解析（支持 "27015" 和 "27015-27020"）
// ============================================================

func parsePorts(input string) ([]int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("输入为空")
	}
	var ports []int
	parts := strings.Split(input, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			rangeParts := strings.SplitN(part, "-", 2)
			start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err1 != nil || err2 != nil || start > end {
				return nil, fmt.Errorf("无效的端口段: %s", part)
			}
			for p := start; p <= end; p++ {
				if p >= 1 && p <= 65535 {
					ports = append(ports, p)
				}
			}
		} else {
			p, err := strconv.Atoi(part)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("无效的端口: %s", part)
			}
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("没有有效的端口")
	}
	return ports, nil
}

// ============================================================
// 交互式命令行面板
// ============================================================

var reader *bufio.Reader

func init() {
	reader = bufio.NewReader(os.Stdin)
}

func readLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func printHeader() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║       L4D2 RCON Agent  交互式面板        ║")
	fmt.Println("╚══════════════════════════════════════════╝")
}

func printMenu() {
	fmt.Println()
	fmt.Println("┌──────────────────────────────────────────┐")
	fmt.Println("│  1. 初始化配置（向导式创建 config.json） │")
	fmt.Println("│  2. 自检（检查配置文件和连通性）         │")
	fmt.Println("│  3. 运行测试（查询一次并显示结果）       │")
	fmt.Println("│  4. 启动服务（后台 HTTP 服务）           │")
	fmt.Println("│  5. 查看当前配置                         │")
	fmt.Println("│  6. 清空配置（删除 config.json）         │")
	fmt.Println("│  0. 退出                                 │")
	fmt.Println("└──────────────────────────────────────────┘")
}

func menuInit() {
	fmt.Println("\n【初始化配置】")

	if configExists() {
		overwrite := readLine("配置文件已存在，是否覆盖？(y/n): ")
		if overwrite != "y" && overwrite != "Y" {
			fmt.Println("已取消")
			return
		}
	}

	listen := readLine("HTTP 监听端口 [27051]: ")
	if listen == "" {
		listen = "27051"
	}
	if !strings.HasPrefix(listen, ":") {
		listen = ":" + listen
	}

	token := readLine("鉴权 Token（可留空）: ")

	host := readLine("游戏服务器公网 IP（便于站点识别，可留空）: ")

	fmt.Println()
	fmt.Println("配置房间端口")
	fmt.Println("  支持单端口(27015) 或 端口段(27015-27020)，逗号分隔")
	portInput := readLine("房间端口: ")
	ports, err := parsePorts(portInput)
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		return
	}
	fmt.Printf("  解析到 %d 个端口: %v\n", len(ports), ports)

	fmt.Println()
	fmt.Println("配置 RCON 密码")

	var passwordMode string
	if len(ports) > 1 {
		fmt.Println("  1. 所有端口使用同一个密码")
		fmt.Println("  2. 逐个端口输入密码")
		fmt.Println("  3. 混合模式（端口段,密码,端口段,密码 格式）")
		passwordMode = readLine("选择 [1]: ")
		if passwordMode == "" {
			passwordMode = "1"
		}
	} else {
		passwordMode = "1"
	}

	var rooms []RoomConfig
	switch passwordMode {
	case "2":
		for _, port := range ports {
			pwd := readLine(fmt.Sprintf("  端口 %d 的 RCON 密码: ", port))
			rooms = append(rooms, RoomConfig{Port: port, Password: pwd})
		}
	case "3":
		input := readLine("  请输入（格式: 端口段,密码,端口段,密码，密码不能含逗号）: ")
		parts := strings.Split(input, ",")
		if len(parts) < 2 || len(parts)%2 != 0 {
			fmt.Println("  错误: 输入必须成对出现（端口段,密码）")
			return
		}
		for i := 0; i < len(parts); i += 2 {
			portStr := strings.TrimSpace(parts[i])
			pwd := strings.TrimSpace(parts[i+1])
			subPorts, err := parsePorts(portStr)
			if err != nil {
				fmt.Printf("  错误: 端口段 '%s' 无效: %v\n", portStr, err)
				return
			}
			for _, port := range subPorts {
				rooms = append(rooms, RoomConfig{Port: port, Password: pwd})
			}
		}
		fmt.Printf("  解析到 %d 个端口\n", len(rooms))
	default:
		pwd := readLine("  统一 RCON 密码: ")
		for _, port := range ports {
			rooms = append(rooms, RoomConfig{Port: port, Password: pwd})
		}
	}

	cfg := &Config{
		Listen: listen,
		Token:  token,
		Host:   host,
		Rooms:  rooms,
	}

	if err := saveConfig(cfg); err != nil {
		fmt.Printf("保存失败: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("✓ 配置已保存到 " + configFileName)
	fmt.Printf("  监听: %s\n", cfg.Listen)
	if cfg.Host != "" {
		fmt.Printf("  服务器IP: %s\n", cfg.Host)
	}
	fmt.Printf("  房间: %d 个\n", len(cfg.Rooms))
	for _, r := range cfg.Rooms {
		fmt.Printf("    :%d  密码: %s\n", r.Port, maskPassword(r.Password))
	}
}

func menuCheck() {
	fmt.Println("\n【自检】")

	// 1. 检查配置文件
	fmt.Print("1. 配置文件... ")
	if !configExists() {
		fmt.Println("✗ 未找到 " + configFileName)
		fmt.Println("   请先执行「初始化配置」")
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("✗ 读取失败: %v\n", err)
		return
	}
	fmt.Println("✓ 正常")

	// 2. 显示配置摘要
	fmt.Printf("2. 配置内容:\n")
	fmt.Printf("   监听: %s\n", cfg.Listen)
	if cfg.Host != "" {
		fmt.Printf("   服务器IP: %s\n", cfg.Host)
	} else {
		fmt.Printf("   服务器IP: ⚠️ 未配置\n")
		hostInput := readLine("   是否现在补充？输入服务器公网 IP（留空跳过）: ")
		if hostInput != "" {
			cfg.Host = hostInput
			if err := saveConfig(cfg); err != nil {
				fmt.Printf("   ✗ 保存失败: %v\n", err)
			} else {
				fmt.Printf("   ✓ 已写入服务器IP: %s\n", cfg.Host)
			}
		}
	}
	fmt.Printf("   Token: %s\n", maskPassword(cfg.Token))
	fmt.Printf("   房间: %d 个\n", len(cfg.Rooms))

	fmt.Println("\n自检完成")
}

func menuTest() {
	fmt.Println("\n【运行测试】")

	if !configExists() {
		fmt.Println("✗ 未找到配置文件，请先初始化")
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		return
	}

	agent := newAgent(cfg)

	fmt.Println("查询所有房间...\n")
	for _, room := range cfg.Rooms {
		fmt.Printf("━━━ 房间 :%d ━━━\n", room.Port)
		players, updated, err := agent.fetchRoomPlayers(room)
		if err != nil {
			fmt.Printf("  ✗ 错误: %v\n", err)
			fmt.Println()
			continue
		}
		if updated {
			fmt.Println("  (本次重新 RCON)")
		} else {
			fmt.Println("  (走缓存)")
		}
		if len(players) == 0 {
			fmt.Println("  无在线玩家")
		} else {
			for _, p := range players {
				fmt.Printf("  %s  %s\n", p.SteamID, p.Name)
			}
		}
		fmt.Println()
	}
}

func menuStart() {
	fmt.Println("\n【启动服务】")

	if !configExists() {
		fmt.Println("✗ 未找到配置文件，请先初始化")
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		return
	}

	agent := newAgent(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/players", agent.handlePlayers)
	mux.HandleFunc("/health", agent.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "L4D2 RCON Agent\n\n接口:\n  GET /players?token=xxx  - 获取所有房间玩家列表\n  GET /health             - 健康检查\n")
	})

	server := &http.Server{Addr: cfg.Listen, Handler: mux}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Println("\n正在关闭...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	fmt.Printf("服务已启动: http://localhost%s\n", cfg.Listen)
	fmt.Printf("房间数量: %d\n", len(cfg.Rooms))
	fmt.Println("按 Ctrl+C 停止")
	fmt.Println()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("服务失败: %v\n", err)
	}
}

func menuView() {
	fmt.Println("\n【当前配置】")

	if !configExists() {
		fmt.Println("✗ 未找到配置文件")
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		return
	}

	fmt.Printf("监听: %s\n", cfg.Listen)
	if cfg.Host != "" {
		fmt.Printf("服务器IP: %s\n", cfg.Host)
	} else {
		fmt.Printf("服务器IP: ⚠️ 未配置（输出将缺少 host 内容）\n")
	}
	fmt.Printf("Token: %s\n", maskPassword(cfg.Token))
	fmt.Printf("房间列表 (%d 个):\n", len(cfg.Rooms))
	for _, r := range cfg.Rooms {
		fmt.Printf("  :%d  密码: %s\n", r.Port, maskPassword(r.Password))
	}
}

func menuClear() {
	fmt.Println("\n【清空配置】")
	if !configExists() {
		fmt.Println("配置文件不存在，无需清空")
		return
	}
	confirm := readLine("确认删除 config.json？(y/n): ")
	if confirm != "y" && confirm != "Y" {
		fmt.Println("已取消")
		return
	}
	if err := os.Remove(configFileName); err != nil {
		fmt.Printf("删除失败: %v\n", err)
		return
	}
	fmt.Println("✓ 已删除 config.json")
}

func maskPassword(s string) string {
	if s == "" {
		return "(空)"
	}
	if len(s) <= 2 {
		return strings.Repeat("*", len(s))
	}
	return s[:1] + strings.Repeat("*", len(s)-2) + s[len(s)-1:]
}

// ============================================================
// 主函数
// ============================================================

func main() {
	// 如果带参数 -serve 直接启动服务（用于开机自启等场景）
	if len(os.Args) > 1 && (os.Args[1] == "-serve" || os.Args[1] == "--serve") {
		quickServe()
		return
	}

	log.SetFlags(0)
	printHeader()

	for {
		printMenu()
		choice := readLine("请选择 [0-6]: ")

		switch choice {
		case "1":
			menuInit()
		case "2":
			menuCheck()
		case "3":
			menuTest()
		case "4":
			menuStart()
		case "5":
			menuView()
		case "6":
			menuClear()
		case "0", "q", "quit", "exit":
			fmt.Println("再见！")
			return
		case "":
			continue
		default:
			fmt.Println("无效选择")
		}
	}
}

// quickServe 跳过交互面板，直接启动服务
func quickServe() {
	log.SetFlags(log.LstdFlags)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	agent := newAgent(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/players", agent.handlePlayers)
	mux.HandleFunc("/health", agent.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "L4D2 RCON Agent\n\n接口:\n  GET /players?token=xxx  - 获取所有房间玩家列表\n  GET /health             - 健康检查\n")
	})
	server := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()
	log.Printf("L4D2 RCON Agent 启动，监听 %s，房间 %d 个", cfg.Listen, len(cfg.Rooms))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务失败: %v", err)
	}
}
