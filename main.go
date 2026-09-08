package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 局域网场景，不限制来源
}

// serverZip 保存启动时打包好的可独立运行软件包（供手机下载后拿到其它电脑单独运行）
var serverZip []byte

// baseDir 运行资源(static/photos/recordings)的根目录 = 可执行文件所在目录。
// 保证把整套程序(含静态页/ffmpeg)复制到任意位置双击即可用，不依赖当前工作目录。
var baseDir string

// mDir 返回 photos/recordings 等资源子目录在 baseDir 下的绝对路径。
func mDir(sub string) string { return filepath.Join(baseDir, sub) }

// thumbDir 照片缩略图缓存目录(baseDir/data/thumbs)，浏览页用缩略图避免整图加载。
var thumbDir string

// ---------- ffmpeg 可用性缓存 ----------
// 一旦确认本机 ffmpeg “起不来”(创建进程失败，而非运行后退出码非0)，本次运行期内就不再生硬重试，
// 缩略图直接走纯 Go、重封装直接放弃，省掉每次多余的进程 spawn 开销。

// ffmpegBroken 在本进程内是否已确认 ffmpeg 无法启动。
var ffmpegBroken = new(atomic.Bool)

// execLaunchError 判断 ffmpeg 失败原因是“根本没跑起来”(缺 DLL/版本不兼容/路径无效等)。
// 若是这样，说明这台机器用不了这个 ffmpeg，后续别再试了。
func execLaunchError(err error) bool {
	var ee *exec.ExitError
	return err != nil && !errors.As(err, &ee)
}

func ffmpegUsable() bool {
	if ffmpegBroken.Load() {
		return false
	}
	if findFFmpeg() == "" {
		return false
	}
	return true
}

func markFFmpegBroken() {
	if ffmpegBroken.CompareAndSwap(false, true) {
		log.Printf("[ffmpeg] 确认本机 ffmpeg 无法启动，本次运行不再尝试，改用纯 Go 生成缩略图")
	}
}

// ensurePhotoThumb 若缓存缩略图不存在则生成一个，带并发保护避免重复生成。
func ensurePhotoThumb(src, dst string) bool {
	if _, err := os.Stat(dst); err == nil {
		return true
	}
	thumbs.Lock()
	defer thumbs.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return true // 别人已生成
	}
	return genPhotoThumb(src, dst)
}

// thumbs 防止并发生成同一张缩略图时重复跑 ffmpeg。
var thumbs sync.Mutex

// genPhotoThumb 优先用 ffmpeg 压图(质量好、自动纠EXIF方向)；ffmpeg 缺失或(老旧系统上)跑不起来时，
// 退化为纯 Go 生成，保证所有系统都能出缩略图。已确认 ffmpeg 无法启动时，直接走纯 Go，不再重试。
func genPhotoThumb(src, dst string) bool {
	ff := findFFmpeg()
	if ff != "" && !ffmpegBroken.Load() {
		args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", src,
			"-vf", "scale=400:400:force_original_aspect_ratio=decrease",
			"-frames:v", "1", "-q:v", "6", dst}
		cmd := exec.Command(ff, args...)
		if runtime.GOOS == "windows" {
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		}
		if err := cmd.Run(); err == nil {
			return true
		} else if execLaunchError(err) {
			markFFmpegBroken() // 起不来 => 本次运行期内不再尝试
			_ = os.Remove(dst)
			log.Printf("[thumb] ffmpeg 无法启动(%v)，改用纯 Go 生成", err)
		} else {
			_ = os.Remove(dst) // 跑起来但失败 => 仅本次换 Go，下次仍给 ffmpeg 机会
			log.Printf("[thumb] ffmpeg 处理失败(%v)，本次改用纯 Go", err)
		}
	}
	return genThumbGo(src, dst)
}

// genThumbGo 纯 Go 生成缩略图(不依赖 ffmpeg，可运行在 Win7/Server2012 等老旧系统)。
// 自动读取 JPEG EXIF 方向并纠正，输出最长边约 maxEdge 的 JPEG。
func genThumbGo(src, dst string) bool {
	f, err := os.Open(src)
	if err != nil {
		return false
	}
	var img image.Image
	img, _, err = image.Decode(f)
	f.Close()
	if err != nil {
		return false
	}
	img = fixOrientation(img, readEXIFOrientation(src))
	thumb := scaleImageToMax(img, 400)
	out, err := os.Create(dst)
	if err != nil {
		return false
	}
	defer out.Close()
	if err := jpeg.Encode(out, thumb, &jpeg.Options{Quality: 80}); err != nil {
		_ = os.Remove(dst)
		return false
	}
	return true
}

// scaleImageToMax 把图像限制在最长边不超过 maxEdge，双线性插值缩放。
func scaleImageToMax(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return src
	}
	f := float64(maxEdge) / math.Max(float64(w), float64(h))
	nw, nh := int(float64(w)*f+0.5), int(float64(h)*f+0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		syR := float64(h-1) * float64(y) / float64(maxInt(nh-1, 1))
		for x := 0; x < nw; x++ {
			sxR := float64(w-1) * float64(x) / float64(maxInt(nw-1, 1))
			dst.Set(x, y, bilinearAt(src, b, sxR, syR))
		}
	}
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func bilinearAt(src image.Image, b image.Rectangle, sx, sy float64) color.Color {
	x0, y0 := int(sx), int(sy)
	x1, y1 := x0+1, y0+1
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > b.Max.X-1 {
		x1 = b.Max.X - 1
	}
	if y1 > b.Max.Y-1 {
		y1 = b.Max.Y - 1
	}
	r00, g00, bb00, a00 := src.At(x0+b.Min.X, y0+b.Min.Y).RGBA()
	r10, g10, bb10, a10 := src.At(x1+b.Min.X, y0+b.Min.Y).RGBA()
	r01, g01, bb01, a01 := src.At(x0+b.Min.X, y1+b.Min.Y).RGBA()
	r11, g11, bb11, a11 := src.At(x1+b.Min.X, y1+b.Min.Y).RGBA()
	dx := sx - float64(x0)
	dy := sy - float64(y0)
	to8 := func(v uint32) uint8 { return uint8(v >> 8) }
	c00 := []float64{float64(r00), float64(g00), float64(bb00)}
	c10 := []float64{float64(r10), float64(g10), float64(bb10)}
	c01 := []float64{float64(r01), float64(g01), float64(bb01)}
	c11 := []float64{float64(r11), float64(g11), float64(bb11)}
	var rr, gg, bbb uint8
	for i := 0; i < 3; i++ {
		top := c00[i] + (c10[i]-c00[i])*dx
		bot := c01[i] + (c11[i]-c01[i])*dx
		v := top + (bot-top)*dy
		switch i {
		case 0:
			rr = to8(uint32(v))
		case 1:
			gg = to8(uint32(v))
		default:
			bbb = to8(uint32(v))
		}
	}
	_ = bb00
	_ = bb10
	_ = a00
	_ = a10
	alpha := uint8((a00 + a10 + a01 + a11) / 4 >> 8)
	return color.RGBA{rr, gg, bbb, alpha}
}

// readEXIFOrientation 解析 JPEG 的 EXIF 方向标签(1-8)，返回 0 表示无/未知。
func readEXIFOrientation(path string) int {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			break
		}
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 {
			i += 2
			continue
		}
		if i+4 > len(b) {
			break
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		dataStart := i + 4
		if marker == 0xE1 && segLen >= 2+6 && dataStart+8 <= len(b) &&
			string(b[dataStart:dataStart+6]) == "Exif\x00\x00" {
			return parseEXIFOrientation(b[dataStart+6 : dataStart+segLen])
		}
		i += 4 + segLen
	}
	return 0
}

func parseEXIFOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if t[0] == 'M' && t[1] == 'M' {
		bo = binary.BigEndian
	} else if !(t[0] == 'I' && t[1] == 'I') {
		return 0
	}
	if bo.Uint16(t[2:4]) != 42 {
		return 0
	}
	ifd := int(bo.Uint32(t[4:8]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[ifd : ifd+2]))
	p := ifd + 2
	for k := 0; k < count && p+12 <= len(t); k++ {
		if bo.Uint16(t[p:p+2]) == 0x0112 {
			return int(bo.Uint32(t[p+8 : p+12]))
		}
		p += 12
	}
	return 0
}

// fixOrientation 根据 EXIF 方向把图像转正。
func fixOrientation(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	flipH := func() *image.RGBA {
		out := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(w-1-x, y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	}
	rotate := func(cw int) *image.RGBA { // cw 次顺时针90度
		cur := img
		for n := 0; n < cw; n++ {
			cb := cur.Bounds()
			cw0, ch0 := cb.Dx(), cb.Dy()
			out := image.NewRGBA(image.Rect(0, 0, ch0, cw0))
			for y := 0; y < ch0; y++ {
				for x := 0; x < cw0; x++ {
					out.Set(ch0-1-y, x, cur.At(cb.Min.X+x, cb.Min.Y+y))
				}
			}
			cur = out
		}
		return cur.(*image.RGBA)
	}
	switch o {
	case 2:
		return flipH()
	case 3:
		return rotate(2)
	case 4: // 垂直翻转
		out := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(x, h-1-y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 5: // 沿主对角线翻转 + 旋转 -> transpose
		return rotate(1)
	case 6:
		return rotate(1)
	case 7: // transverse
		return rotate(1)
	default: // 8: 逆时针90
		return rotate(3)
	}
}

// buildPackageZip 在启动时把 phonecam.exe + 说明一起打进 zip，方便在“手机连不上本机网络”时，
// 下载整个已编译好的工具到另一台能被手机访问的电脑上独立运行（手机开热点共享网络即可）。
func buildPackageZip() error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) error {
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	// 1) 供其它电脑独立运行的已编译程序
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(exe); err == nil {
			if err := add("phonecam.exe", b); err != nil {
				return err
			}
		}
	}
	// 1b) Win7/8 专用版（用 Go 1.20 交叉编译，兼容老系统）
	if b, err := os.ReadFile(filepath.Join(baseDir, "phonecam-win7.exe")); err == nil {
		if err := add("phonecam-win7.exe", b); err != nil {
			return err
		}
	}
	// 1c) ffmpeg(纯封装重封装视频用，找不到也能运行，仅视频第二次封装不可用)
	if b, err := os.ReadFile(filepath.Join(baseDir, "ffmpeg.exe")); err == nil {
		if err := add("ffmpeg.exe", b); err != nil {
			return err
		}
	}
	// 1d) 静态页面(电脑端/发送端/浏览页)，缺了页面也能起服务但无可操作界面
	if entries, err := os.ReadDir(filepath.Join(baseDir, "static")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(baseDir, "static", e.Name())); err == nil {
				if err := add("static/"+e.Name(), b); err != nil {
					return err
				}
			}
		}
	}
	// 2) 一键启动脚本（先切到脚本所在目录，保证复制到任意位置都能找到 static/ffmpeg/data）
	bat := "\xef\xbb\xbf@echo off\r\nrem 手机摄像头投屏工具 - 双击即启动(自动打开电脑端页面)\r\ncd /d \"%~dp0\"\r\nstart \"\" \"%~dp0phonecam.exe\"\r\n"
	if err := add("启动服务.bat", []byte(bat)); err != nil {
		return err
	}
	// 3) 使用说明
	readme := []string{
		"手机摄像头投屏工具 - 使用说明",
		"==============================",
		"",
		"这是一套可独立运行的软件包，用于“手机连不上服务器同一网络”时的替代方案。",
		"",
		"使用方法：",
		"1. 在一台可被手机访问的电脑上下载并解压本包；",
		"2. 双击「启动服务.bat」或「phonecam.exe」启动；",
		"3. 用手机开热点给这台电脑共享网络（或两者连同一网络/热点）；",
		"4. 用手机浏览器扫描电脑端页面显示的二维码即可投摄像头。",
		"",
		"Windows 7 / 8 请使用「phonecam-win7.exe」（本包 exe 用 go1.20 编译，兼容老系统）。",
		"",
		"提示：首次访问证书不被信任时，点「高级 → 继续访问」并在手机端授权摄像头。",
		"",
		"其它参数：",
		"  phonecam.exe -port 9000    换成 9000 端口(并自动保存，下次沿用)",
		"  phonecam.exe -no-open      不自动打开浏览器",
		"",
	}
	var body string
	for _, l := range readme {
		body += l + "\r\n"
	}
	if err := add("使用说明.txt", []byte(body)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	serverZip = buf.Bytes()
	return nil
}

// connInfo 记录每个 WebSocket 连接的信息
type connInfo struct {
	conn  *websocket.Conn
	role  string // "phone" 或 "display"
	id    string // 仅 phone 使用：唯一客户端编号，用于多手机分流
	name  string // 仅 phone 使用：手机自定义昵称
	title string // 仅 phone 使用：本次录像的视频名称(可选)
}

// hub 负责消息转发：phone 的消息转发给所有 display，视频片段经服务器中转。
type hub struct {
	mu        chan struct{} // 简单的互斥通道，避免加锁代码
	conns     []*connInfo
	openCount map[string]int
	nextID    uint64                // 给 phone 分配自增编号
	rec       map[string]*recording // 正在录制的手机录音对象 (key: phone id)
	recOn     bool                  // 是否启用自动录制
}

// recording 记录某台手机当前这一路投屏的落盘状态
type recording struct {
	id    string
	file  *os.File
	path  string
	size  int64
	ext   string // 容器后缀(webm/mp4)
	start time.Time
}

func newHub() *hub {
	return &hub{mu: make(chan struct{}, 1), openCount: map[string]int{"phone": 0, "display": 0}, rec: map[string]*recording{}, recOn: true}
}

// recExt 由编解码标识推断录像文件后缀（便于按容器播放）
func recExt(mime string) string {
	m := strings.ToLower(mime)
	if strings.Contains(m, "mp4") || strings.Contains(m, "avc") || strings.Contains(m, "h264") {
		return "mp4"
	}
	if strings.Contains(m, "webm") || strings.Contains(m, "vp8") || strings.Contains(m, "vp9") {
		return "webm"
	}
	return "bin"
}

// sanNick 把手机昵称清理成可安全放进文件名的片段（去空格/非法字符/截长）
func sanNick(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 16 {
		runes = runes[:16]
	}
	var b strings.Builder
	for _, r := range runes {
		switch r {
		case ' ', '\t', '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// startRecording 在某台手机开始投屏(收到 streaminfo)时，为它新建一路录像文件。
func (h *hub) startRecording(id, mime, nick, title string) {
	h.lock()
	defer h.unlock()
	if !h.recOn {
		return
	}
	if _, ok := h.rec[id]; ok {
		return // 该手机已在录制(如码率/分辨率切换再发 streaminfo)，继续沿用当前文件
	}
	_ = os.MkdirAll(mDir("recordings"), 0o755)
	// 录像文件命名：视频名称放前面，手机名放中间，时间日期放后面，最后带 id 防止重名
	var parts []string
	if t := sanNick(title); t != "" {
		parts = append(parts, t)
	}
	if n := sanNick(nick); n != "" {
		parts = append(parts, n)
	}
	parts = append(parts, time.Now().Format("20060102_150405"), id)
	name := strings.Join(parts, "_") + "." + recExt(mime)
	path := filepath.Join(mDir("recordings"), name)
	f, err := os.Create(path)
	if err != nil {
		log.Println("[record] 创建失败:", err)
		return
	}
	h.rec[id] = &recording{id: id, file: f, path: path, ext: recExt(mime), start: time.Now()}
	log.Printf("[record] 开始录制 %s -> %s (视频名=%q 手机名=%q)", id, name, title, nick)
}

// appendRecording 追加一段视频字节到该手机的当前录像文件。
func (h *hub) appendRecording(id string, data []byte) {
	h.lock()
	r := h.rec[id]
	if r != nil && r.file != nil {
		n, _ := r.file.Write(data)
		r.size += int64(n)
	}
	h.unlock()
}

// finalizeRecording 结束该手机的投屏录制(收到 streamend 或连接断开)，关闭文件并广播给电脑端。
func (h *hub) finalizeRecording(id string) {
	h.lock()
	r := h.rec[id]
	if r != nil {
		delete(h.rec, id)
		if r.file != nil {
			r.file.Close()
		}
	}
	h.unlock()
	if r == nil {
		return
	}
	log.Printf("[record] 结束录制 %s (%d 字节)", id, r.size)
	// 录像完成后，优先用 ffmpeg 做“纯封装重封装”(-c copy，绝不转码)：把索引表/时长写到文件头部，
	// 让电脑端首次点击即可拖动进度条(比内置的纯 Go Cues 前移更稳妥，贴近播放器规范)。
	// 找不到 ffmpeg 或封装失败时，webm 退回内置的纯 Go 索引前移，mp4 保持原文件。
	go func() {
		if !remuxByFFmpeg(r.path, id, r.ext) && r.ext == "webm" {
			remuxWebMForSeek(r.path, id)
		}
	}()
	name := filepath.Base(r.path)
	if note, err := json.Marshal(map[string]string{"type": "rec_final", "file": name}); err == nil {
		h.relay("display", note, nil)
	}
}

// findFFmpeg 定位 ffmpeg 可执行文件：依次查 环境变量、工具同目录、本机已知路径、PATH。
// 找不到返回 ""，调用方会回退到内置方案，绝不因此拖累核心功能。
func findFFmpeg() string {
	if p := os.Getenv("PCAM_FFMPEG"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, n := range []string{"ffmpeg.exe", "phonecam-ffmpeg.exe"} {
			cand := filepath.Join(dir, n)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand
			}
		}
	}
	// 本机已知路径(用户机器上自带 ffmpeg 的转码工具)
	for _, p := range []string{
		`E:\开发\videoconvert\VideoConvertTool\dist\ffmpeg.exe`,
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// remuxByFFmpeg 用 ffmpeg 把录像文件按原样重新封装到临时文件再原子替换回来(-c copy 不转码)。
// 返回是否成功。WebM 加书写正确索引；MP4 加 +faststart(把 moov 移到文件头)便于快速拖动。
func remuxByFFmpeg(src, id, ext string) bool {
	if ffmpegBroken.Load() { // 已确认 ffmpeg 起不来，本次运行直接放弃，保留原文件
		return false
	}
	ff := findFFmpeg()
	if ff == "" {
		return false
	}
	suffix := ".tmp.webm"
	extra := []string{}
	if ext == "mp4" {
		suffix = ".tmp.mp4"
		extra = append(extra, "-movflags", "+faststart")
	}
	tmp := src + suffix
	args := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-i", src, "-map", "0", "-c", "copy"}
	args = append(args, extra...)
	args = append(args, tmp)
	cmd := exec.Command(ff, args...)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} // 后台执行，不弹窗
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if execLaunchError(err) {
			markFFmpegBroken() // 起不来 => 本次运行期内不再尝试，录完也直接保留原文件
		}
		log.Printf("[remux] %s ffmpeg 封装失败, 保留原文件: %v %s", id, err, out)
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, src); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[remux] %s 替换失败: %v", id, err)
		return false
	}
	log.Printf("[remux] %s ffmpeg 已重新封装(%s→%s, 不转码, 可拖动)", id, ext, ext)
	return true
}

// ---- WebM(Cues-in-front) 索引重排：解决首次拖动进度条无效的问题 ----
// MediaRecorder 生成的 WebM 把索引表(Cues)放在文件末尾，浏览器必须先把整段加载完才能解析索引。
// 本函数用纯 Go 做最小的 EBML 解析：把 Cues 重建并移到所有 Cluster 之前，集群字节原样保留，
// 只更新 Segment 的 size 与 CueClusterPosition/CueRelativePosition。失败时原样保留文件（不影响已有录像）。

const (
	ebmlHeaderID = "\x1A\x45\xDF\xA3"
	segmentID    = "\x18\x53\x80\x67"
	cuesID       = "\x1C\x53\xBB\x6B"
	clusterID    = "\x1F\x43\xB6\x75"
	trackInfoID  = "\x16\x54\xAE\x6B"
	seekHeadID   = "\x11\x4D\x9B\x74"
	// 集群子元素
	timecodeID    = "\xE7"
	simpleBlockID = "\xA3"
	blockGroupID  = "\xA0"
	// Cues 子元素
	cuePointID      = "\xBB"
	cueTimeID       = "\xB3"
	cueTrackPosID   = "\xB7"
	cueTrackID      = "\xF7"
	cueClusterPosID = "\xF1"
	cueRelPosID     = "\xF0"
)

// vintLen 返回给定首字节的 EBML VINT/元素ID 长度(字节数)。
func vintLen(first byte) int {
	switch {
	case first <= 0x01:
		return 8
	case first <= 0x03:
		return 7
	case first <= 0x07:
		return 6
	case first <= 0x0F:
		return 5
	case first <= 0x1F:
		return 4
	case first <= 0x3F:
		return 3
	case first <= 0x7F:
		return 2
	default:
		return 1
	}
}

// readVint 读取一个 VINT 值，返回其数值与头部长度。ok=false 表示空间不足或解析失败。
func readVint(b []byte, pos int) (val uint64, headerLen int, ok bool) {
	if pos >= len(b) {
		return 0, 0, false
	}
	headerLen = vintLen(b[pos])
	if pos+headerLen > len(b) {
		return 0, headerLen, false
	}
	val = uint64(b[pos] & (0xFF >> uint(headerLen)))
	for _, x := range b[pos+1 : pos+headerLen] {
		val = val<<8 | uint64(x)
	}
	// 未知大小 = 值位全 1
	if headerLen < 8 {
		if val >= uint64(1)<<uint(7*headerLen)-1 {
			return 0, headerLen, false
		}
	}
	return val, headerLen, true
}

// readVintSize 读取元素大小 VINT，额外返回 isUnknown(是否为“未知大小”，0x01 FF FF …)。
// 未知大小元素(如 MediaRecorder 写的 Cluster/Segment)内容一直延伸到下一个同级元素。
func readVintSize(b []byte, pos int) (val uint64, headerLen int, isUnknown, ok bool) {
	if pos >= len(b) {
		return 0, 0, false, false
	}
	headerLen = vintLen(b[pos])
	if pos+headerLen > len(b) {
		return 0, headerLen, false, false
	}
	val = uint64(b[pos] & (0xFF >> uint(headerLen)))
	for _, x := range b[pos+1 : pos+headerLen] {
		val = val<<8 | uint64(x)
	}
	if headerLen < 8 {
		isUnknown = val >= uint64(1)<<uint(7*headerLen)-1
	} else {
		// 8 字节：当值位(后 7 字节)全为 0xFF 时为未知大小
		isUnknown = true
		for _, x := range b[pos+1 : pos+headerLen] {
			if x != 0xFF {
				isUnknown = false
				break
			}
		}
	}
	return val, headerLen, isUnknown, true
}

// walkVariable 给定一个“未知大小”元素的数据起始，依次遍历它内部定长子元素，
// 返回其结束位置(即下一个同级元素的起始)。用于确定 cluster 的真实边界。
func walkVariable(data []byte, start, limit int) int {
	i := start
	for i < limit {
		hl1 := vintLen(data[i])
		if i+hl1 > limit {
			break
		}
		p := i + hl1
		if p >= limit {
			break
		}
		sz, hl, unk, ok := readVintSize(data, p)
		if !ok || unk {
			break // 碰到下一个未知大小同级元素(如下一个 cluster)，即当前元素终点
		}
		de := p + hl + int(sz)
		if de < p || de > limit {
			break
		}
		i = de
	}
	return i
}

// elemBounds 解析一个元素，返回 头部长度 与 数据区起始/结束下标。
func elemBounds(b []byte, pos int) (hdrLen, ds, de int, ok bool) {
	if pos >= len(b) {
		return 0, 0, 0, false
	}
	idLen := vintLen(b[pos])
	p := pos + idLen
	if p >= len(b) {
		return 0, 0, 0, false
	}
	sz, szLen, ok2 := readVint(b, p)
	if !ok2 {
		return 0, 0, 0, false
	}
	ds = p + szLen
	if ds > len(b) {
		return 0, 0, 0, false
	}
	de = ds + int(sz)
	if de > len(b) {
		return 0, 0, 0, false
	}
	return pos + idLen + szLen, ds, de, true
}

func elemID(b []byte, pos int) []byte {
	n := vintLen(b[pos])
	if pos+n > len(b) {
		return nil
	}
	return b[pos : pos+n]
}

// encVint 编码一个非未知大小的 VINT。
func encVint(val uint64) []byte {
	n := 1
	for ; n <= 8; n++ {
		bits := 7 * n
		if uint(bits) >= 64 {
			break
		}
		if val <= uint64(1)<<uint(bits)-2 {
			return encVintN(n, val)
		}
	}
	return encVintN(8, val)
}

func encVintN(n int, val uint64) []byte {
	out := make([]byte, n)
	if n < 8 {
		out[0] = byte(1 << uint(8-n)) // 标记位
	}
	shift := 8 * (n - 1)
	out[0] |= byte(val >> uint(shift))
	for i := 1; i < n; i++ {
		out[i] = byte(val >> uint(8*(n-1-i)))
	}
	return out
}

// encUint 把 Matroska 的“无符号整数”值编码为最小大端字节序列(不带 VINT 标记位)。
// 用于 CueTime/CueClusterPosition/CueRelativePosition 等 uint 类型元素的值。
func encUint(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	n := 1
	for q := v; q >= 0x100; q >>= 8 {
		n++
	}
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

// readUint 读取 Matroska uint 元素的值(大端)，b 即该元素的数据区。
func readUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// ebmlElem 组装一个 EBML 元素：[ID][大小VINT][数据]。
func ebmlElem(id string, data []byte) []byte {
	out := make([]byte, 0, len(id)+1+len(data))
	out = append(out, id...)
	out = append(out, encVint(uint64(len(data)))...)
	out = append(out, data...)
	return out
}

// remuxWebMForSeek 把 WebM 的索引表前移，使首次加载即可拖动进度条。
func remuxWebMForSeek(path, id string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[remux] %s 读取失败: %v", id, err)
		return
	}
	if len(b) < 64 || len(b) > 512<<20 { // 太小的手尾、过大的文件都不处理，原样保留
		return
	}
	out, err := remuxWebMBytes(b)
	if err != nil {
		log.Printf("[remux] %s 跳过(索引前移失败，保留原文件): %v", id, err)
		return
	}
	if len(out) == 0 || bytes.Equal(out, b) {
		return
	}
	// 原子替换：先写临时文件再改名，避免进程中断留下半截文件
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		log.Printf("[remux] %s 写临时文件失败: %v", id, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[remux] %s 替换失败: %v", id, err)
		return
	}
	log.Printf("[remux] %s WebM 索引已前移 (%d → %d 字节)", id, len(b), len(out))
}

// remuxWebMBytes 在内存中把 Cues 前移到所有 Cluster 之前。
func remuxWebMBytes(b []byte) ([]byte, error) {
	// 步骤1：定位 EBML 头部 与 Segment
	if len(b) < 4 || string(b[0:4]) != ebmlHeaderID {
		return nil, fmt.Errorf("不是 EBML 文件")
	}
	_, _, ebmlEnd, ok := elemBounds(b, 0)
	if !ok || ebmlEnd+4 > len(b) {
		return nil, fmt.Errorf("解析 EBML 头部失败")
	}
	// Segment 元素。MediaRecorder 常把 Segment 写成“未知大小”(0x01 FF FF FF…)，
	// 此时内容一直延伸到文件尾，需按到 EOF 处理，而不是按 size 截断。
	if string(b[ebmlEnd:ebmlEnd+4]) != segmentID {
		return nil, fmt.Errorf("未找到 Segment")
	}
	szHlen := vintLen(b[ebmlEnd+4])
	segDS := ebmlEnd + 4 + szHlen
	segDE := len(b) // 默认到文件尾
	if segDS > len(b) {
		return nil, fmt.Errorf("解析 Segment 失败")
	}
	if v, hl, okp := readVint(b, ebmlEnd+4); okp {
		segDE = segDS + int(v) // 有限大小才截断
		if segDE > len(b) {
			segDE = len(b)
		}
		_ = hl
	}
	segContent := b[segDS:segDE]

	// 步骤2：遍历 Segment 顶层子元素，分拣 元数据(前置部分)/Cluster；丢弃原始 Cues。
	// 注意：MediaRecorder 的 Cluster 也常写成“未知大小”，需遍历子元素确定其真实边界。
	var front []byte       // 非 Cluster 也非 Cues 的字节(原样拼接)
	var clusters [][]byte  // 各 Cluster 原始字节(含元素头)
	var clusterTC []uint64 // 各 Cluster 的 Timecode
	var blockRel []int     // 各 Cluster 内首个关键块 相对 Cluster 起始的偏移
	buf := make([]byte, 0, len(segContent))
	pos := 0
	for pos < len(segContent) {
		id := elemID(segContent, pos)
		if len(id) == 0 {
			break
		}
		p := pos + len(id)
		if p >= len(segContent) {
			break
		}
		val, hl, unk, ok := readVintSize(segContent, p)
		if !ok {
			break
		}
		ds := p + hl
		de := 0
		if unk {
			de = walkVariable(segContent, ds, len(segContent)) // 未知大小 → 遍历子元素定尾
		} else if int(val) <= len(segContent)-ds {
			de = ds + int(val)
		} else {
			break
		}
		if de < ds {
			break
		}
		if bytes.Equal(id, []byte(clusterID)) {
			clusters = append(clusters, segContent[pos:de])
			tc, br := scanCluster(segContent[pos:de])
			clusterTC = append(clusterTC, tc)
			blockRel = append(blockRel, br)
			pos = de
			continue
		}
		if bytes.Equal(id, []byte(cuesID)) {
			pos = de
			continue // 丢弃原始 Cues，稍后重建
		}
		// 其余(Info/Tracks/SeekHead/Void 等)原样保留
		buf = append(buf, segContent[pos:de]...)
		pos = de
	}
	front = buf
	if len(clusters) == 0 {
		return nil, fmt.Errorf("没有 Cluster")
	}

	// 步骤3：以“元数据 + 新Cues + Clusters”的顺序计算新布局，构造 Cues 及每个 CuePoint。
	newCuesStart := len(front)
	var cues bytes.Buffer
	// 先按“元数据+占位Cues+Clusters”精算每个 cluster 的新相对偏移，再据以生成 Cues。
	// Cues 前置到所有 Cluster 之前，因此 CueClusterPosition = len(front)+len(cues 中的 2水平偏移量)
	//（相对 Segment 数据起始，front 之后即 Cues 起始）。
	newRel := make([]int64, len(clusters))
	cur := int64(newCuesStart)
	for i := range clusters {
		newRel[i] = cur
		cur += int64(len(clusters[i]))
	}
	buildCues(&cues, clusterTC, newRel, blockRel)
	cuesBytes := cues.Bytes()
	// Cues 实际长度可能和预估不同，重新精算偏移(再迭代一次)
	newRel2 := make([]int64, len(clusters))
	cur2 := int64(newCuesStart) + int64(len(cuesBytes))
	for i := range clusters {
		newRel2[i] = cur2
		cur2 += int64(len(clusters[i]))
	}
	cues.Reset()
	buildCues(&cues, clusterTC, newRel2, blockRel)
	cuesBytes = cues.Bytes()
	// 用最终偏移再重算一次Cues(长度不变，仅内部数值)，收敛
	newRel3 := make([]int64, len(clusters))
	cur3 := int64(newCuesStart) + int64(len(cuesBytes))
	for i := range clusters {
		newRel3[i] = cur3
		cur3 += int64(len(clusters[i]))
	}
	cues.Reset()
	buildCues(&cues, clusterTC, newRel3, blockRel)
	cuesBytes = cues.Bytes()

	// 步骤4：组装新 Segment 内容（cur3 恰为新段总长）
	newSegContent := make([]byte, 0, int(cur3))
	newSegContent = append(newSegContent, front...)
	newSegContent = append(newSegContent, cuesBytes...)
	for i := range clusters {
		newSegContent = append(newSegContent, clusters[i]...)
	}

	// 步骤5：重写 Segment 头并拼出完整文件。
	// 注意：不能直接复用 b[0:segDS](其含源 Segment 的“未知大小”size 字节)，只取 EBML 头，再拼新的 Segment 头。
	// Segment 沿用原文件的“未知大小”(0x01 FF×7)风格——内部 Cluster 也是未知大小元素，
	// 若把父 Segment 写成有限大小，会触发 “Unknown-sized element inside parent with finite size” 警告。
	// CueClusterPosition/CueRelativePosition 本就是以 Segment 数据区起始为基准，故不受 Segment 头写法影响。
	out := make([]byte, 0, len(newSegContent)+ebmlEnd+32)
	out = append(out, b[0:ebmlEnd]...) // EBML 头
	out = append(out, segmentID...)
	out = append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // Segment 未知大小
	out = append(out, newSegContent...)
	return out, nil
}

// scanCluster 扫描一个 Cluster(含元素头)，返回其 Timecode 值与首个关键块
// (SimpleBlock/BlockGroup) 相对 Cluster 元素起始的偏移。兼容“未知大小”Cluster。
func scanCluster(c []byte) (tc uint64, blockRel int) {
	pos := 0
	blockRel = -1
	// 跳过 cluster 根元素头(通常为“未知大小”)，从数据区开始遍历子元素
	if len(c) > 0 {
		p := vintLen(c[0])
		if p < len(c) {
			if _, hl, _, ok := readVintSize(c, p); ok {
				pos = p + hl
			}
		}
	}
	for pos < len(c) {
		id := elemID(c, pos)
		_, ds, de, ok := elemBounds(c, pos)
		if !ok {
			break
		}
		if bytes.Equal(id, []byte(timecodeID)) {
			if ds < de {
				tc = readUint(c[ds:de]) // Timecode 是 uint，值=数据区大端整数
			}
		} else if bytes.Equal(id, []byte(simpleBlockID)) || bytes.Equal(id, []byte(blockGroupID)) {
			if blockRel < 0 {
				blockRel = pos // 关键块元素相对簇起始的偏移(与 CueClusterPosition 同基准)
			}
			return tc, blockRel // 找到首个关键块即可停止
		}
		pos = de
	}
	return tc, blockRel
}

// buildCues 依据各 Cluster 的偏移与 Timecode 生成 Cues 元素字节。
func buildCues(w *bytes.Buffer, tc []uint64, rel []int64, blockRel []int) {
	var body []byte
	for i := range tc {
		if blockRel[i] < 0 {
			continue // 该簇无关键块，跳过(不影响其余)
		}
		var cp []byte
		cp = append(cp, ebmlElem(cueTimeID, encUint(tc[i]))...)
		var tp []byte
		tp = append(tp, ebmlElem(cueTrackID, encUint(1))...) // 通常视频轨号=1
		tp = append(tp, ebmlElem(cueClusterPosID, encUint(uint64(rel[i])))...)
		if blockRel[i] > 0 {
			tp = append(tp, ebmlElem(cueRelPosID, encUint(uint64(blockRel[i])))...)
		}
		cp = append(cp, ebmlElem(cueTrackPosID, tp)...)
		body = append(body, ebmlElem(cuePointID, cp)...)
	}
	w.Write(ebmlElem(cuesID, body))
}

func (h *hub) lock()   { h.mu <- struct{}{} }
func (h *hub) unlock() { <-h.mu }

type signalMsg struct {
	From string          `json:"from"`
	Type string          `json:"type"` // "hello" | "offer" | "answer" | "candidate"
	Name string          `json:"name"` // 仅 hello(手机) 用：自定义昵称
	Data json.RawMessage `json:"data"`
}

func (h *hub) handleWs(w http.ResponseWriter, r *http.Request) {
	// 捕捉本连接处理循环中的 panic，防止进程闪退并输出堆栈便于定位
	defer func() {
		if pr := recover(); pr != nil {
			log.Printf("[PANIC] ws 处理异常:\n%v\n%s", pr, debug.Stack())
		}
	}()

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade error:", err)
		return
	}
	info := &connInfo{conn: conn}
	h.lock()
	h.conns = append(h.conns, info)
	h.unlock()
	log.Printf("[ws] 新连接来自 %s(%s)", conn.RemoteAddr(), r.UserAgent())
	defer func() {
		h.lock()
		h.conns = removeConn(h.conns, conn)
		if info.role != "" {
			h.openCount[info.role]--
		}
		h.unlock()
		if info.role == "phone" && info.id != "" {
			// 手机断开/退出：广播“已停止”，让电脑端清除冻结画面并提示；并结束该路录制
			h.finalizeRecording(info.id)
			if note, err := json.Marshal(map[string]string{"type": "stopped", "id": info.id}); err == nil {
				h.relay("display", note, nil)
			}
		}
		if info.role != "" {
			log.Printf("[ws] %s(%s) 断开, 当前在线 phone=%d display=%d", info.role, info.id, h.openCount["phone"], h.openCount["display"])
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
			// 若该手机正在录制，把实际视频字节追加到录像文件
			if info.id != "" {
				h.appendRecording(info.id, payload)
			}
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
				info.name = msg.Name
			}
			h.lock()
			h.openCount[msg.From]++
			h.unlock()
		case "photo":
			// 手机拍照：data 为 base64 编码的 JPEG，落盘到 photos/ 并通知电脑端
			var m struct {
				Data string `json:"data"`
			}
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
			_ = os.MkdirAll(mDir("photos"), 0o755)
			// 照片命名与录像一致：视频名称_手机名_时间_id
			var parts []string
			if t := sanNick(info.title); t != "" {
				parts = append(parts, t)
			}
			if n := sanNick(info.name); n != "" {
				parts = append(parts, n)
			}
			parts = append(parts, time.Now().Format("20060102_150405"), info.id)
			name := strings.Join(parts, "_") + ".jpg"
			if err := os.WriteFile(filepath.Join(mDir("photos"), name), raw, 0o644); err != nil {
				log.Println("[photo] 写入失败:", err)
				continue
			}
			log.Printf("[photo] %s 保存 photos/%s (%d 字节)", info.id, name, len(raw))
			go func() {
				_ = os.MkdirAll(thumbDir, 0o755)
				ensurePhotoThumb(filepath.Join(mDir("photos"), name), filepath.Join(thumbDir, name+".jpg"))
			}() // 后台压一张预览图，浏览页缩略图直接用，不用整图加载
			note, _ := json.Marshal(map[string]string{"type": "photo_saved", "file": name})
			h.relay("display", note, nil) // 广播给所有电脑端
		case "videoTitle":
			// 手机设置本次录像的视频名称(可选)，发给电脑端显示，并在下次开始录制时用作文件前缀
			var m struct {
				Title string `json:"title"`
			}
			if json.Unmarshal(payload, &m) == nil {
				info.title = m.Title
				note, _ := json.Marshal(map[string]string{"type": "videoTitle", "id": info.id, "title": info.title})
				h.relay("display", note, conn)
			}
		default:
			// 其余文本指令(streaminfo/streamend 等)按连接登记的角色转发，
			// 并在消息里注入来源 id，方便电脑端多手机分流。
			if info.role == "phone" {
				if msg.Type == "streaminfo" {
					var m map[string]interface{}
					codec := ""
					if json.Unmarshal(payload, &m) == nil {
						if v, ok := m["codec"].(string); ok {
							codec = v
						} else if v, ok := m["mime"].(string); ok {
							codec = v
						}
						if v, ok := m["title"].(string); ok {
							info.title = v
						}
					}
					h.startRecording(info.id, codec, info.name, info.title) // 开始这一路投屏的自动录制
				} else if msg.Type == "streamend" {
					h.finalizeRecording(info.id) // 本次投屏结束，落盘并广播
				}
				var m map[string]interface{}
				if json.Unmarshal(payload, &m) == nil {
					m["id"] = info.id
					if n, ok := m["name"].(string); ok && n != "" {
						info.name = n // 记录最新昵称
					} else {
						m["name"] = info.name
					}
					if t, ok := m["title"].(string); ok && t != "" {
						info.title = t
					} else {
						m["title"] = info.title
					}
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

// loadConfigPort 从 config.json 读取：端口与是否自动录制。返回 (端口, 端口是否有效, 是否录制)
func loadConfigPort(path string) (int, bool, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, true
	}
	var cfg struct {
		Port      int   `json:"port"`
		Recording *bool `json:"recording"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return 0, false, true
	}
	rec := true
	if cfg.Recording != nil {
		rec = *cfg.Recording
	}
	return cfg.Port, cfg.Port >= 1 && cfg.Port <= 65535, rec
}

// saveConfig 把端口与录制开关写入 config.json（下次启动生效）
func saveConfig(path string, port int, recording bool) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.Marshal(map[string]interface{}{"port": port, "recording": recording})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// safeFileName 校验文件名为纯文件名(不含路径分隔符/点号)，防止穿越到目录外。
func safeFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
		return false
	}
	return true
}

// mediaDelete 删除 photos/ 或 recordings/ 下某个文件，返回提示信息与是否成功。
func mediaDelete(dir, name string) (string, bool) {
	if !safeFileName(name) {
		return "非法文件名", false
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		if os.IsNotExist(err) {
			return "文件不存在", false
		}
		return "删除失败: " + err.Error(), false
	}
	return "", true
}

// mediaRename 在 photos/ 或 recordings/ 下重命名文件，返回提示信息与是否成功。
func mediaRename(dir, old, new string) (string, bool) {
	if !safeFileName(old) || !safeFileName(new) {
		return "非法文件名", false
	}
	if err := os.Rename(filepath.Join(dir, old), filepath.Join(dir, new)); err != nil {
		if os.IsNotExist(err) {
			return "文件不存在", false
		}
		return "重命名失败: " + err.Error(), false
	}
	return "", true
}

// mediaOpHandler 生成删除/重命名照片或录像的 HTTP 处理函数。
func mediaOpHandler(dir, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "仅支持POST"})
			return
		}
		var in struct {
			Name string `json:"name"`
			Old  string `json:"old"`
			New  string `json:"new"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "参数错误"})
			return
		}
		var msg string
		var ok bool
		if op == "delete" {
			msg, ok = mediaDelete(dir, in.Name)
		} else {
			msg, ok = mediaRename(dir, in.Old, in.New)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok, "msg": msg})
	}
}

// peekConn 在读取时先返回已预读的缓冲，供嗅探 TLS 用（不影响 Write/Deadline 等）
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// originSniff 在单个端口上同时承载 https 与纯 http：
// 首字节为 0x16(ClientHello) 则按 TLS 握手，否则原样交给 http 处理。
type originSniff struct {
	net.Listener
	tlsCfg *tls.Config
}

func (o *originSniff) Accept() (net.Conn, error) {
	for {
		raw, err := o.Listener.Accept()
		if err != nil {
			// 只有监听器真正关闭才返回错误；单个连接的问题绝不能拖垮整个服务
			return nil, err
		}
		br := bufio.NewReader(raw)
		// 给“首字节嗅探”限时，避免某个只连接不发数据、或半开的连接把 Accept 卡死导致前台转圈
		raw.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, err := br.Peek(1)
		raw.SetReadDeadline(time.Time{}) // 清除，之后的读写交给上层自行调度
		if err != nil {
			// 客户端连上但没发数据就断开/超时(如端口探测、防病毒扫描、空连接)。
			// 这里只关闭该连接并继续下一个，绝不能让 http.Server 把这种 EOF 当成致命错误退出。
			raw.Close()
			continue
		}
		conn := &peekConn{Conn: raw, r: br}
		if b[0] == 0x16 { // TLS ClientHello
			return tls.Server(conn, o.tlsCfg), nil
		}
		return conn, nil
	}
}

// httpToHTTPS 处理纯 http 请求：302 跳转到 https（保证从 http 页点进 https 能正常到达）
func httpToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	w.Header().Set("Location", target)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound) // 302
	fmt.Fprintf(w, `<a href="%s">请点击前往安全连接(HTTPS)</a><br>提示：如遇证书提示，请点「高级 → 继续前往」`, target)
}

// installUserRootCert 把自签证书安装到当前用户的受信任根证书库，桌面浏览器免告警。
// 失败不影响主服务（手机端仍需手动信任）。
// 注意：certutil 有时会弹权限/确认框导致阻塞，因此放到子协程并在 5 秒内未完成就放弃，
// 绝不因为证书安装让服务起不来（这是之前“启动后无日志卡住/闪退”的根因之一）。
func installUserRootCert(certFile, cn string) {
	if _, err := os.Stat(certFile); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = exec.Command("certutil", "-user", "-delstore", "Root", cn).Run()
		if b, err := exec.Command("certutil", "-user", "-addstore", "Root", certFile).CombinedOutput(); err != nil {
			log.Println("[cert] 装入用户根证书失败:", string(b))
			return
		}
		log.Print("[cert] 已将自签证书装入当前用户根证书库，本机访问免告警")
	}()
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		log.Println("[cert] 根证书安装超时(可能弹确认框)，跳过，不影响服务启动")
	}
}

// syncWriter 是同时写控制台与日志文件、且每次写后立即 fsync 的 writer，
// 保证进程异常退出/闪退时日志已经落盘，便于事后排查。
type syncWriter struct{ f *os.File }

func (w syncWriter) Write(p []byte) (int, error) {
	os.Stdout.Write(p) // 控制台
	n, err := w.f.Write(p)
	_ = w.f.Sync() // 立即落盘
	return n, err
}

// initLogfile 把日志同时写到控制台和 data/phonecam.log，便于闪退/定位问题。
func initLogfile(dir string) {
	_ = os.MkdirAll(dir, 0700)
	f, err := os.OpenFile(filepath.Join(dir, "phonecam.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(syncWriter{f: f})
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("===== 服务启动，日志写入 data/phonecam.log =====")
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[FATAL] 进程捕获到未处理异常，即将退出:\n%v\n%s", r, debug.Stack())
		}
	}()
	port := flag.Int("port", 8443, "监听端口")
	dataDir := flag.String("data", "", "证书/数据存放目录(默认:程序所在目录/data)")
	noOpen := flag.Bool("no-open", false, "启动后不自动打开浏览器")
	flag.Parse()

	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			*dataDir = "./data"
		} else {
			baseDir = filepath.Dir(exe)
			*dataDir = filepath.Join(baseDir, "data")
		}
	}
	// 即便用 -data 指定了数据目录，也把资源目录定为程序所在目录，照常“复制即用”
	if baseDir == "" {
		if exe, err := os.Executable(); err == nil {
			baseDir = filepath.Dir(exe)
		} else {
			baseDir = "."
		}
	}
	thumbDir = filepath.Join(*dataDir, "thumbs")
	_ = os.MkdirAll(thumbDir, 0o755)
	// 端口：仅在未显式指定 -port 时，才读取网页保存的 config.json（下次启动生效）
	cfgFile := filepath.Join(*dataDir, "config.json")
	initLogfile(*dataDir) // 日志同时写控制台与 data/phonecam.log
	flagSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })
	if !flagSet["port"] {
		if p, ok, _ := loadConfigPort(cfgFile); ok {
			*port = p
		}
	}
	certFile := filepath.Join(*dataDir, "cert.pem")
	keyFile := filepath.Join(*dataDir, "key.pem")

	ips := lanIPv4s()
	log.Printf("[boot] 本机局域网 IP: %v", ips)
	if err := ensureCert(certFile, keyFile, ips); err != nil {
		log.Fatalln("生成证书失败:", err)
	}
	log.Printf("[boot] 证书就绪: %s", certFile)
	installUserRootCert(certFile, "phonecameratoweb") // 本机信任自签证书，桌面端免告警
	log.Print("[boot] 根证书处理完成")
	if err := buildPackageZip(); err != nil {
		log.Println("软件包打包失败:", err)
	}
	log.Print("[boot] 独立安装包构建完成")

	h := newHub()
	_, _, recOn := loadConfigPort(cfgFile)
	h.recOn = recOn
	// 命令行用 -port 显式指定端口时，一并写入 config.json，
	// 这样即使下次不写 -port，也会自动沿用本次指定的端口。
	if flagSet["port"] {
		_ = saveConfig(cfgFile, *port, h.recOn)
		log.Printf("[boot] 命令行指定端口 %d，已保存到 %s(下次启动自动沿用)", *port, cfgFile)
	}

	// 静态资源
	_ = os.MkdirAll(mDir("photos"), 0o755)
	_ = os.MkdirAll(mDir("recordings"), 0o755)
	fs := http.FileServer(http.Dir(filepath.Join(baseDir, "static")))
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	// 照片图集：保存到 photos/ 目录，可通过 /photos/xxx.jpg 访问回看
	mux.Handle("/photos/", http.StripPrefix("/photos/", http.FileServer(http.Dir(mDir("photos")))))
	// 录像回放：recordings/ 目录
	mux.Handle("/recordings/", http.StripPrefix("/recordings/", http.FileServer(http.Dir(mDir("recordings")))))
	// /sender 无扩展名，手动映射到 sender.html
	mux.HandleFunc("/sender", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(baseDir, "static", "sender.html"))
	})
	mux.HandleFunc("/ws", h.handleWs)

	// 可独立运行的软件包下载（手机连不上本机网络时，到别的电脑单独运行）
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		if len(serverZip) == 0 {
			http.Error(w, "软件包未生成", 500)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="phonecam-server.zip"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(serverZip)))
		w.Header().Set("Cache-Control", "no-store")
		w.Write(serverZip)
	})

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
			var in struct {
				Port int `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Port < 1 || in.Port > 65535 {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "端口需在 1-65535 之间"})
				return
			}
			if err := saveConfig(cfgFile, in.Port, h.recOn); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "port": in.Port, "apply": "重启后生效"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"port": *port, "recording": h.recOn, "configFile": cfgFile})
	})

	// 自动录制开关：GET 查询，POST {recording:true/false} 立即生效并保存
	mux.HandleFunc("/api/recording", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var in struct {
				Recording bool `json:"recording"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "参数错误"})
				return
			}
			h.lock()
			h.recOn = in.Recording
			h.unlock()
			_ = saveConfig(cfgFile, *port, in.Recording)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "recording": in.Recording})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"recording": h.recOn})
	})

	// 照片列表：返回 photos/ 目录下按时间倒序的照片文件（文件名校验安全，仅匹配 .jpg）
	mux.HandleFunc("/api/photos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entries, err := os.ReadDir(mDir("photos"))
		if err != nil {
			json.NewEncoder(w).Encode([]interface{}{})
			return
		}
		type photoItem struct {
			Name string
			Size int64
			Time string
			Ts   int64 // 修改时间(Unix 毫秒)，用于可靠排序
		}
		var items = make([]photoItem, 0)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			items = append(items, photoItem{Name: e.Name(), Size: fi.Size(), Time: fi.ModTime().Format("01-02 15:04"), Ts: fi.ModTime().Unix()})
		}
		// 按修改时间倒序：新照片靠前(文件名里时间戳位置不固定，不能按名字排)
		sort.Slice(items, func(i, j int) bool { return items[i].Ts > items[j].Ts })
		json.NewEncoder(w).Encode(items)
	})

	// 录像列表：返回 recordings/ 目录下按时间倒序的录像文件
	mux.HandleFunc("/api/recordings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entries, err := os.ReadDir(mDir("recordings"))
		if err != nil {
			json.NewEncoder(w).Encode([]interface{}{})
			return
		}
		type recItem struct {
			Name string
			Size int64
			Time string
			Ext  string
			Ts   int64 // 修改时间(Unix 秒)，用于可靠排序
		}
		var items = make([]recItem, 0)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Name()), "."))
			items = append(items, recItem{Name: e.Name(), Size: fi.Size(), Time: fi.ModTime().Format("01-02 15:04"), Ext: ext, Ts: fi.ModTime().Unix()})
		}
		// 按修改时间倒序：新录像靠前
		sort.Slice(items, func(i, j int) bool { return items[i].Ts > items[j].Ts })
		json.NewEncoder(w).Encode(items)
	})

	// 照片/录像管理：删除、重命名文件(仅限 photos/ 与 recordings/ 内)
	mux.HandleFunc("/api/photo/delete", mediaOpHandler(filepath.Join(baseDir, "photos"), "delete"))
	mux.HandleFunc("/api/photo/rename", mediaOpHandler(filepath.Join(baseDir, "photos"), "rename"))
	mux.HandleFunc("/api/recording/delete", mediaOpHandler(filepath.Join(baseDir, "recordings"), "delete"))
	mux.HandleFunc("/api/recording/rename", mediaOpHandler(filepath.Join(baseDir, "recordings"), "rename"))

	// 照片缩略图：先出缓存，没有就用 ffmpeg 现场生成(400px预览)；ffmpeg 缺失时退化为原图。
	mux.HandleFunc("/api/photo/thumb", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if !safeFileName(name) {
			http.NotFound(w, r)
			return
		}
		src := filepath.Join(mDir("photos"), name)
		if _, err := os.Stat(src); err != nil {
			http.NotFound(w, r)
			return
		}
		dst := filepath.Join(thumbDir, name+".jpg")
		_ = os.MkdirAll(thumbDir, 0o755)
		if !ensurePhotoThumb(src, dst) {
			http.ServeFile(w, r, src) // 无法压图时给原图，保证不白屏
			return
		}
		w.Header().Set("Cache-Control", "max-age=86400")
		http.ServeFile(w, r, dst)
	})

	// 独立浏览页：在浏览器新窗口查看照片与录像的缩略格子，并可删除/重命名
	mux.HandleFunc("/gallery", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(baseDir, "static", "gallery.html"))
	})
	// 兼容带 .html 后缀的访问（方便手动输入 chrome 自动补全时也能进）
	mux.HandleFunc("/gallery.html", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(baseDir, "static", "gallery.html"))
	})

	// 同一端口上，https 正常访问；纯 http 请求自动跳转到 https
	redirectMW := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				httpToHTTPS(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}

	cer, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalln("读取证书失败:", err)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cer}}

	srv := &http.Server{
		Handler:           redirectMW(mux),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalln("监听端口失败:", err)
	}
	log.Printf("[boot] 已监听端口 %d", *port)

	lanURL := fmt.Sprintf("https://localhost:%d", *port)
	fmt.Println("========================================")
	fmt.Println("  手机摄像头投屏工具 服务已启动")
	fmt.Printf("  电脑端(显示) 页面: %s\n", lanURL)
	fmt.Printf("  局域网监听端口 : %d\n", *port)
	fmt.Printf("  局域网 IP     : %s\n", strings.Join(ips, ", "))
	fmt.Println("  直接敲 http://IP:端口 会自动跳转到 https")
	fmt.Println("  手机请扫描电脑页显示的二维码打开摄像头传输页")
	fmt.Println("========================================")

	if !*noOpen {
		time.AfterFunc(500*time.Millisecond, func() { openBrowser(lanURL) })
	}

	if err := srv.Serve(&originSniff{Listener: ln, tlsCfg: tlsCfg}); err != nil {
		log.Fatalln("HTTP 服务启动失败:", err)
	}
}
