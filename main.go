package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 局域网场景，不限制来源
}

// connInfo 记录每个 WebSocket 连接的信息
type connInfo struct {
	conn *websocket.Conn
	role string // "phone" 或 "display"
	id   string // 仅 phone 使用：唯一客户端编号，用于多手机分流
}

// hub 负责消息转发：phone 的消息转发给所有 display，视频片段经服务器中转。
type hub struct {
	mu        chan struct{} // 简单的互斥通道，避免加锁代码
	conns     []*connInfo
	openCount map[string]int
	nextID    uint64 // 给 phone 分配自增编号
}

func newHub() *hub {
	return &hub{mu: make(chan struct{}, 1), openCount: map[string]int{"phone": 0, "display": 0}}
}

func (h *hub) lock()   { h.mu <- struct{}{} }
func (h *hub) unlock() { <-h.mu }

type signalMsg struct {
	From   string          `json:"from"`
	Type   string          `json:"type"` // "hello" | "offer" | "answer" | "candidate"
	Data   json.RawMessage `json:"data"`
}

func (h *hub) handleWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade error:", err)
		return
	}
	info := &connInfo{conn: conn}
	h.lock()
	h.conns = append(h.conns, info)
	h.unlock()
	defer func() {
		h.lock()
		h.conns = removeConn(h.conns, conn)
		if info.role != "" {
			h.openCount[info.role]--
		}
		h.unlock()
		if info.role == "phone" && info.id != "" {
			// 手机断开/退出：广播“已停止”，让电脑端清除冻结画面并提示
			if note, err := json.Marshal(map[string]string{"type": "stopped", "id": info.id}); err == nil {
				h.relay("display", note, nil)
			}
		}
		conn.Close()
	}()

	// 视频片段可能较大，放宽读上限
	conn.SetReadLimit(64 * 1024 * 1024)

	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			// 手机发来的视频二进制片段 → 加上来源 id 头后转发给所有电脑端(display)，
			// 头部格式：[1 字节 id 长度] + [id UTF-8 字节] + [视频片段]
			env := make([]byte, 0, 1+len(info.id)+len(payload))
			env = append(env, byte(len(info.id)))
			env = append(env, info.id...)
			env = append(env, payload...)
			h.relayBinary("display", env, conn)
			continue
		}
		var msg signalMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "hello":
			info.role = msg.From
			if msg.From == "phone" {
				info.id = fmt.Sprintf("p%d", atomic.AddUint64(&h.nextID, 1))
			}
			h.lock()
			h.openCount[msg.From]++
			h.unlock()
		case "photo":
			// 手机拍照：data 为 base64 编码的 JPEG，落盘到 photos/ 并通知电脑端
			var m struct{ Data string `json:"data"` }
			if err := json.Unmarshal(payload, &m); err != nil {
				log.Println("[photo] 解析失败:", err)
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				log.Println("[photo] base64 解码失败:", err)
				continue
			}
			if len(raw) == 0 {
				continue
			}
			_ = os.MkdirAll("photos", 0o755)
			name := time.Now().Format("20060102_150405") + ".jpg"
			if err := os.WriteFile(filepath.Join("photos", name), raw, 0o644); err != nil {
				log.Println("[photo] 写入失败:", err)
				continue
			}
			log.Printf("[photo] %s 保存 photos/%s (%d 字节)", info.id, name, len(raw))
			note, _ := json.Marshal(map[string]string{"type": "photo_saved", "file": name})
			h.relay("display", note, nil) // 广播给所有电脑端
		default:
			// 其余文本指令(streaminfo/streamend 等)按连接登记的角色转发，
			// 并在消息里注入来源 id，方便电脑端多手机分流。
			if info.role == "phone" {
				var m map[string]interface{}
				if json.Unmarshal(payload, &m) == nil {
					m["id"] = info.id
					if b, err := json.Marshal(m); err == nil {
						payload = b
					}
				}
			}
			target := "display"
			if info.role == "display" {
				target = "phone"
			}
			h.relay(target, payload, conn)
		}
	}
}

// relay 把文本消息转发给指定角色的所有连接（排除发送者自身）
func (h *hub) relay(target string, raw []byte, from *websocket.Conn) {
	h.lock()
	list := make([]*connInfo, 0, len(h.conns))
	for _, c := range h.conns {
		if c.role == target {
			list = append(list, c)
		}
	}
	h.unlock()
	for _, c := range list {
		if c.conn == from {
			continue
		}
		if err := c.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			log.Println("relay write error:", err)
		}
	}
}

// relayBinary 转发二进制视频片段（默认只发给 display；排除发送者）
func (h *hub) relayBinary(target string, payload []byte, from *websocket.Conn) {
	h.lock()
	list := make([]*connInfo, 0, len(h.conns))
	for _, c := range h.conns {
		if c.role == target {
			list = append(list, c)
		}
	}
	h.unlock()
	for _, c := range list {
		if c.conn == from {
			continue
		}
		if err := c.conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			log.Println("relayBinary write error:", err)
		}
	}
}

func removeConn(conns []*connInfo, conn *websocket.Conn) []*connInfo {
	out := conns[:0]
	for _, c := range conns {
		if c.conn != conn {
			out = append(out, c)
		}
	}
	return out
}

// lanInfo 报告服务器在本机的局域网地址
type lanInfo struct {
	Port      int      `json:"port"`
	IPs       []string `json:"ips"`
	Preferred string   `json:"preferred"` // 自动选中的推荐 IP
	SenderURL string   `json:"senderUrl"`
}

func lanIPv4s() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				ips = append(ips, ip4.String())
			}
		}
	}
	return ips
}

// pickPrivateIP 从多个网卡地址里选一个最可能是真实局域网的 IP。
// 优先 192.168.x，其次是 10.x，再次是 172.16-31（跳过虚拟网卡/VPN）。
func pickPrivateIP(ips []string) string {
	var cand172 string
	for _, ip := range ips {
		if strings.HasPrefix(ip, "192.168.") {
			return ip
		}
		if strings.HasPrefix(ip, "10.") {
			return ip
		}
		if cand172 == "" && strings.HasPrefix(ip, "172.") {
			if p := net.ParseIP(ip); p != nil && p.IsPrivate() {
				cand172 = ip
			}
		}
	}
	if cand172 != "" {
		return cand172
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return "127.0.0.1"
}

// preferredIP 返回应推荐的 IP：优先“默认路由”那块网卡的地址(通常是真正上网、
// 也最容易被手机访问到的那块)，找不到再用 pickPrivateIP 兜底。
func preferredIP(ips []string) string {
	if ip := defaultRouteIP(); ip != "" {
		for _, i := range ips {
			if i == ip {
				return ip
			}
		}
	}
	return pickPrivateIP(ips)
}

// defaultRouteIP 通过向外网(8.8.8.8)建立一次 UDP 连接来探测本机默认出口 IP。
func defaultRouteIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// loadConfigPort 从 config.json 读取网页保存的端口，返回 (端口, 是否存在有效配置)
func loadConfigPort(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var cfg struct{ Port int `json:"port"` }
	if json.Unmarshal(data, &cfg) != nil || cfg.Port < 1 || cfg.Port > 65535 {
		return 0, false
	}
	return cfg.Port, true
}

// saveConfigPort 把端口写入 config.json（下次启动生效）
func saveConfigPort(path string, port int) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(map[string]int{"port": port})
	return os.WriteFile(path, data, 0o644)
}

// httpToHTTPS 处理 80 端口的纯 http 请求：说明并跳转 HTTPS（独立端口，不影响主服务）
func httpToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	body := fmt.Sprintf(`<!DOCTYPE html><html><meta charset="utf-8">
<meta http-equiv="refresh" content="0;url=%s">
<title>正在跳转</title>
<body style="font-family:sans-serif;background:#f2f5fa;color:#333;text-align:center;padding:60px 20px">
<h2>此服务仅支持 HTTPS(加密)访问</h2>
<p>要用手机投摄像头，需要 https 才能调用摄像头权限。</p>
<p>若未自动跳转，请点击：<a href="%s" style="color:#2f7ddf;font-size:18px">前往安全连接</a></p>
<p style="color:#999;font-size:12px">提示不安全时，请点「高级 → 继续前往」并允许摄像头权限。</p>
</body></html>`, target, target)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, body)
}

func main() {
	port := flag.Int("port", 8443, "监听端口")
	dataDir := flag.String("data", "", "证书/数据存放目录(默认:程序所在目录/data)")
	noOpen := flag.Bool("no-open", false, "启动后不自动打开浏览器")
	flag.Parse()

	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			*dataDir = "./data"
		} else {
			*dataDir = filepath.Join(filepath.Dir(exe), "data")
		}
	}
	// 端口：仅在未显式指定 -port 时，才读取网页保存的 config.json（下次启动生效）
	cfgFile := filepath.Join(*dataDir, "config.json")
	flagSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag){ flagSet[f.Name] = true })
	if !flagSet["port"] {
		if p, ok := loadConfigPort(cfgFile); ok {
			*port = p
		}
	}
	certFile := filepath.Join(*dataDir, "cert.pem")
	keyFile := filepath.Join(*dataDir, "key.pem")

	ips := lanIPv4s()
	if err := ensureCert(certFile, keyFile, ips); err != nil {
		log.Fatalln("生成证书失败:", err)
	}

	h := newHub()

	// 静态资源
	_ = os.MkdirAll("photos", 0o755)
	fs := http.FileServer(http.Dir("static"))
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	// 照片图集：保存到 photos/ 目录，可通过 /photos/xxx.jpg 访问回看
	mux.Handle("/photos/", http.StripPrefix("/photos/", http.FileServer(http.Dir("photos"))))
	// /sender 无扩展名，手动映射到 sender.html
	mux.HandleFunc("/sender", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/sender.html")
	})
	mux.HandleFunc("/ws", h.handleWs)

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		ipe := preferredIP(ips)
		sender := fmt.Sprintf("https://%s:%d/sender", ipe, *port)
		json.NewEncoder(w).Encode(lanInfo{Port: *port, IPs: ips, Preferred: ipe, SenderURL: sender})
	})

	mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		ipe := preferredIP(ips)
		if q := r.URL.Query().Get("ip"); q != "" {
			ipe = q
		}
		sender := fmt.Sprintf("https://%s:%d/sender", ipe, *port)
		png, err := qrcode.Encode(sender, qrcode.Medium, 512)
		if err != nil {
			http.Error(w, "生成二维码失败", 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	// 端口配置：GET 返回当前端口与保存路径；POST {port} 写入 config.json，下次启动生效
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var in struct{ Port int `json:"port"` }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Port < 1 || in.Port > 65535 {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "端口需在 1-65535 之间"})
				return
			}
			if err := saveConfigPort(cfgFile, in.Port); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "port": in.Port, "apply": "重启后生效"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"port": *port, "configFile": cfgFile})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	lanURL := fmt.Sprintf("https://localhost:%d", *port)
	fmt.Println("========================================")
	fmt.Println("  手机摄像头投屏工具 服务已启动")
	fmt.Printf("  电脑端(显示) 页面: %s\n", lanURL)
	fmt.Printf("  局域网监听端口 : %d\n", *port)
	fmt.Printf("  局域网 IP     : %s\n", strings.Join(ips, ", "))
	fmt.Println("  手机请扫描电脑页显示的二维码打开摄像头传输页")
	fmt.Println("========================================")

	if !*noOpen {
		time.AfterFunc(500*time.Millisecond, func() { openBrowser(lanURL) })
	}

	// 可选：在 80 端口提供 http→https 跳转（尽力而为，失败不影响主服务）
	go func() {
		r80 := &http.Server{
			Handler:           http.HandlerFunc(httpToHTTPS),
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := r80.ListenAndServe(); err != nil {
			log.Println("80 端口 http 跳转服务未启用(可能被占用或无权限):", err)
		}
	}()

	if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil {
		log.Fatalln("HTTP 服务启动失败:", err)
	}
}