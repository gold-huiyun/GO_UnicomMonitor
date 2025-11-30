
package main

import (
    "bytes"
    "crypto/tls"
    "fmt"
    "io/fs"
    "net/http"
    "net/url"
    "os"
    "os/exec"
    "path/filepath"
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/gorilla/websocket"
)

// ========================== 入口：开始录制（分流 + 有声落盘 + 合成） ==========================

func GoRecording(config *Config, video *Video) {
    // 临时变量：按设备名建立目录
    tempPath := config.Path + "/" + video.Name

    // 断开后自动重连
    for {
        // 连接服务器并分流抓取一段（视频 + 音频）
        vBytes, aBytes := linkServerDemux(video)

        // 若两路都为空，判定本次连接失败，休眠后重试
        if len(vBytes) == 0 && len(aBytes) == 0 {
            FmtPrint(video.Name + " 设备连接/抓取失败，稍后自动重试(" + strconv.Itoa(config.Sleep) + "s)")
            timeout := time.Duration(config.Sleep)
            time.Sleep(timeout * time.Second)
            continue
        }

        // 生成基础文件名（YYYYMMDD/HHmmss）
        base := getFileName(tempPath)

        // 分别保存视频与音频原始数据
        vFile := base + ".hevc"  // Annex‑B HEVC 裸流
        aFile := base + ".alaw"  // G.711 A‑law 原始样本
        saveFile(vFile, &vBytes)
        saveFile(aFile, &aBytes)
        FmtPrint("录制完成（原始）：", vFile, " + ", aFile)

        // 自动合成 MP4：视频不转码（copy），音频转 AAC（兼容性最佳）
        // 若系统已安装 ffmpeg 且 PATH 可用，则生成 base.mp4
        mp4 := base + ".mp4"
        if err := MergeToMP4(vFile, aFile, mp4, 25); err != nil {
            FmtPrint("合成 MP4 失败（请确认 ffmpeg 可用）：", err)
        } else {
            FmtPrint("合成完成：", mp4)
        }

        // 按策略删除旧文件夹
        DeleteOldFiles(config, video)
    }
}

// ========================== WebSocket 连接 + 分流抓取 ==========================

// 分流抓取一段：返回视频与音频字节（达到设定大小后返回）
func linkServerDemux(video *Video) (vBytes []byte, aBytes []byte) {
    var vbuf, abuf bytes.Buffer

    // 构建 WS URL（确保 Host 含端口）
    host := video.WsHost // 例如 "vd-file-...wcloud.wojiazongguan.cn:50443"
    u := url.URL{Scheme: "wss", Host: host, Path: "/h5player/live"}

    // 提取域名（去端口）用于 SNI 与 Origin 头
    hostWithoutPort := host
    if i := strings.Index(host, ":"); i > 0 {
        hostWithoutPort = host[:i]
    }

    // Dialer（握手修复：SNI、压缩、TLS >= 1.2、忽略证书）
    dialer := websocket.Dialer{
        HandshakeTimeout:  15 * time.Second,
        EnableCompression: true,
        TLSClientConfig: &tls.Config{
            InsecureSkipVerify: true,
            ServerName:         hostWithoutPort,
            MinVersion:         tls.VersionTLS12,
        },
    }

    // 补常见握手头（尤其 Origin/UA）
    header := http.Header{}
    header.Set("Origin", "https://"+hostWithoutPort)
    header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
    header.Set("Accept-Language", "zh-CN,zh;q=0.9")
    // 如后端要求 Referer，可按需开启：
    // header.Set("Referer", "https://"+hostWithoutPort+"/")

    // 发起连接，并保留 resp 便于诊断
    conn, resp, err := dialer.Dial(u.String(), header)
    if err != nil {
        status := ""
        if resp != nil {
            status = resp.Status
        }
        FmtPrint(fmt.Sprintf("无法连接到服务器: %v; HTTP状态: %s", err, status))
        return nil, nil
    }
    defer conn.Close()

    // 1) 发送业务参数（H5 同步）
    paramMsg := "_paramStr_=" + video.ParamMsg
    if err := conn.WriteMessage(websocket.TextMessage, []byte(paramMsg)); err != nil {
        FmtPrint("发送参数失败：", err)
        return nil, nil
    }

    // 2) 接收若干欢迎/状态文本（通常 2 条）
    for i := 0; i < 2; i++ {
        mt, resp, err := conn.ReadMessage()
        if err != nil {
            FmtPrint("接收欢迎消息失败：", err)
            return nil, nil
        }
        if mt == websocket.TextMessage {
            // 可选：FmtPrint("欢迎：", string(resp))
        }
    }

    // 3) 发送启动指令（与前端行为一致）
    startCmd := fmt.Sprintf(`{"time":%d,"cmd":3}`, time.Now().Unix())
    if err := conn.WriteMessage(websocket.TextMessage, []byte(startCmd)); err != nil {
        FmtPrint("发送启动指令失败：", err)
        return nil, nil
    }

    // 4) 接收二进制帧并分流
    for {
        mt, resp, err := conn.ReadMessage()
        if err != nil {
            FmtPrint("接收消息失败：", err)
            break
        }
        if mt != websocket.BinaryMessage {
            continue // 只处理二进制
        }
        if len(resp) < 16 {
            continue
        }

        // 第二字节为类型：0x63=视频（HEVC Annex‑B）；0x62=音频（G.711 A‑law）
        fType := resp[1]

        switch fType {
        case 0x63: // 视频
            skip := dynamicSkipAfterPercentJSON(resp) // 找 "%7D"/'}'，再跳 8 字节
            if skip <= 0 || skip >= len(resp) {
                continue
            }
            payload := resp[skip:]
            // 对齐到 Annex‑B 起始码（00 00 00 01 / 00 00 01）
            align := indexAnnexB(payload)
            if align >= 0 {
                vbuf.Write(payload[align:])
            }

        case 0x62: // 音频（A‑law）
            // 固定结构：00 62 | 00 01 | 4字节序列/时间戳 | 00 00 00 00 | <A‑law负载> | 00 00 01 4B
            if len(resp) >= 2+2+4+4+4 {
                // 验证尾标（若不存在，也可按长度留用）
                if resp[len(resp)-4] == 0x00 && resp[len(resp)-3] == 0x00 && resp[len(resp)-2] == 0x01 && resp[len(resp)-1] == 0x4B {
                    ap := resp[12 : len(resp)-4] // 剔 12 字节头与 4 字节尾标
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                } else {
                    // 无尾标时的兜底：尝试剔掉 12 字节头直接写入
                    ap := resp[12:]
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                }
            }

        default:
            // 其他类型：留空或打印，便于后续扩展
            // FmtPrint(fmt.Sprintf("未知类型=0x%02x, head=% x", fType, resp[:16]))
        }

        // 达到设定大小（MB）后返回
        if vbuf.Len() > 1024*1024*video.Size || abuf.Len() > 1024*1024*video.Size {
            break
        }
    }

    return vbuf.Bytes(), abuf.Bytes()
}

// ========================== 工具：定位负载/起始码 ==========================

// 找到 "%7D"（或 '}'）作为 JSON 结尾，再跳 8 字节（长度/对齐）进入负载
func dynamicSkipAfterPercentJSON(resp []byte) int {
    idx := bytes.Index(resp, []byte("%7D"))
    if idx < 0 {
        idx = bytes.IndexByte(resp, '}')
    }
    if idx < 0 {
        return 0
    }
    return idx + len("%7D") + 8
}

// 在 payload 内搜索第一个 Annex‑B 起始码（00 00 00 01 / 00 00 01）用于对齐
func indexAnnexB(p []byte) int {
    for i := 0; i+4 <= len(p); i++ {
        // 00 00 00 01
        if p[i] == 0 && p[i+1] == 0 && p[i+2] == 0 && p[i+3] == 1 {
            return i
        }
        // 00 00 01
        if i+3 <= len(p) && p[i] == 0 && p[i+1] == 0 && p[i+2] == 1 {
            return i
        }
    }
    return -1
}

// ========================== 工具：合成 MP4（ffmpeg） ==========================

// MergeToMP4 将 HEVC（不转码）与 A-law（转 AAC）合为 MP4；需要系统 PATH 存在 ffmpeg
func MergeToMP4(hevcPath, alawPath, mp4Path string, fps int) error {
    if !fileExists(hevcPath) || !fileExists(alawPath) {
        return fmt.Errorf("源文件不存在：%s 或 %s", hevcPath, alawPath)
    }
    // ffmpeg -hide_banner -y -f alaw -ar 8000 -ac 1 -i a.alaw -r 25 -fflags +genpts -i v.hevc -c:v copy -c:a aac -shortest out.mp4
    cmd := exec.Command(
        "ffmpeg",
        "-hide_banner", "-y",
        "-f", "alaw", "-ar", "8000", "-ac", "1", "-i", alawPath,
        "-r", strconv.Itoa(fps), "-fflags", "+genpts", "-i", hevcPath,
        "-c:v", "copy", "-c:a", "aac", "-shortest", mp4Path,
    )
    // 合成过程可采集输出以便诊断
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("ffmpeg 执行失败：%v；输出：%s", err, string(out))
    }
    return nil
}

func fileExists(p string) bool {
    _, err := os.Stat(p)
    return err == nil
}

// ========================== 原有：文件名/落盘/清理 ==========================

func getFileName(dirPath string) string {
    // 添加日期文件夹
    dateFolder := time.Now().Format("20060102")
    fullPath := dirPath + "/" + dateFolder
    // 检查文件夹是否存在，不存在则创建
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        if err := os.MkdirAll(fullPath, 0755); err != nil {
            FmtPrint("创建文件夹失败：", err)
            os.Exit(0)
        }
    }
    // 文件名称（HHmmss）
    fileName := time.Now().Format("150405")
    return fullPath + "/" + fileName
}

func saveFile(fileName string, bytes *[]byte) {
    file, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
    if err != nil {
        FmtPrint("保存文件失败: ", err)
        os.Exit(0)
    }
    defer file.Close()
    if _, werr := file.Write(*bytes); werr != nil {
        FmtPrint("写入文件失败: ", werr)
    }
}

// 删除文件夹下的旧文件夹（保留最近 Count 个）
func DeleteOldFiles(config *Config, video *Video) {
    dirPath := config.Path + "/" + video.Name
    foldersToKeep := video.Count

    // 读取子目录
    var folders []fs.FileInfo
    entries, _ := os.ReadDir(dirPath)
    for _, entry := range entries {
        if entry.IsDir() {
            info, _ := os.Stat(filepath.Join(dirPath, entry.Name()))
            folders = append(folders, info)
        }
    }

    // 检查数量
    if len(folders) <= foldersToKeep {
        return
    }

    // 按修改时间倒序排列（新→旧）
    sort.Slice(folders, func(i, j int) bool {
        return folders[i].ModTime().After(folders[j].ModTime())
    })

    // 删除最旧的
    for i := foldersToKeep; i < len(folders); i++ {
        old := filepath.Join(dirPath, folders[i].Name())
        _ = os.RemoveAll(old)
    }
}
