
package main

import (
    "bytes"
    "crypto/tls"
    "embed"
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

// -------------------------- 内嵌 ffmpeg.exe --------------------------

// 在构建时嵌入 src/assets/ffmpeg.exe
// 你无需将 exe 提交到仓库；稍后我给你 GitHub Actions 步骤在构建阶段自动下载到 src/assets 再编译。
//
//go:embed assets/ffmpeg.exe
var ffmpegExe []byte

// 确保 ffmpeg 可用：优先找 PATH，其次把内嵌的 ffmpeg.exe 落盘到临时目录使用
func ensureFFmpegAvailable() (string, error) {
    // 1) 尝试从 PATH 查找
    if p, err := exec.LookPath("ffmpeg"); err == nil {
        return p, nil
    }
    // 2) 使用内嵌的 ffmpeg.exe
    if len(ffmpegExe) == 0 {
        return "", fmt.Errorf("未找到 ffmpeg：PATH 中无，且内嵌资源缺失")
    }
    dir := filepath.Join(os.TempDir(), "ffmpeg_embed")
    _ = os.MkdirAll(dir, 0755)
    path := filepath.Join(dir, "ffmpeg.exe")
    if _, err := os.Stat(path); os.IsNotExist(err) {
        if err := os.WriteFile(path, ffmpegExe, 0755); err != nil {
            return "", fmt.Errorf("写入内嵌 ffmpeg.exe 失败：%v", err)
        }
    }
    return path, nil
}

// -------------------------- 入口：开始录制 --------------------------

func GoRecording(config *Config, video *Video) {
    // 设备目录
    tempPath := config.Path + "/" + video.Name

    // 断线自动重连
    for {
        // 分流抓取一段（视频 + 音频）
        vBytes, aBytes := linkServerDemux(video)
        if len(vBytes) == 0 && len(aBytes) == 0 {
            FmtPrint(video.Name + " 设备连接/抓取失败，稍后自动重试(" + strconv.Itoa(config.Sleep) + "s)")
            time.Sleep(time.Duration(config.Sleep) * time.Second)
            continue
        }

        // 基础文件名（YYYYMMDD/HHmmss）
        base := getFileName(tempPath)

        // 保存原始
        vFile := base + ".hevc"  // Annex‑B HEVC 裸流（仅视频）
        aFile := base + ".alaw"  // G.711 A‑law 原始样本（8kHz 单声道）
        saveFile(vFile, &vBytes)
        saveFile(aFile, &aBytes)
        FmtPrint("录制完成（原始）：", vFile, " + ", aFile)

        // 自动合成 MP4（视频不转码 copy；音频转 AAC）
        mp4 := base + ".mp4"
        if err := MergeToMP4(vFile, aFile, mp4, 25); err != nil {
            FmtPrint("合成 MP4 失败：", err)
        } else {
            FmtPrint("合成完成：", mp4)
        }

        // 清理旧文件夹
        DeleteOldFiles(config, video)
    }
}

// -------------------------- WebSocket 连接 + 分流抓取 --------------------------

func linkServerDemux(video *Video) (vBytes, aBytes []byte) {
    var vbuf, abuf bytes.Buffer

    host := video.WsHost // 例："vd-file-...:50443"
    u := url.URL{Scheme: "wss", Host: host, Path: "/h5player/live"}

    hostWithoutPort := host
    if i := strings.Index(host, ":"); i > 0 {
        hostWithoutPort = host[:i]
    }

    dialer := websocket.Dialer{
        HandshakeTimeout:  15 * time.Second,
        EnableCompression: true,
        TLSClientConfig: &tls.Config{
            InsecureSkipVerify: true,
            ServerName:         hostWithoutPort,
            MinVersion:         tls.VersionTLS12,
        },
    }
    header := http.Header{}
    header.Set("Origin", "https://"+hostWithoutPort)
    header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
    header.Set("Accept-Language", "zh-CN,zh;q=0.9")

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

    // 1) 发送业务参数
    paramMsg := "_paramStr_=" + video.ParamMsg
    if err := conn.WriteMessage(websocket.TextMessage, []byte(paramMsg)); err != nil {
        FmtPrint("发送参数失败：", err)
        return nil, nil
    }

    // 2) 接收 2 条欢迎/状态文本
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

    // 3) 启流指令
    startCmd := fmt.Sprintf(`{"time":%d,"cmd":3}`, time.Now().Unix())
    if err := conn.WriteMessage(websocket.TextMessage, []byte(startCmd)); err != nil {
        FmtPrint("发送启动指令失败：", err)
        return nil, nil
    }

    // 4) 分流
    for {
        mt, resp, err := conn.ReadMessage()
        if err != nil {
            FmtPrint("接收消息失败：", err)
            break
        }
        if mt != websocket.BinaryMessage || len(resp) < 16 {
            continue
        }
        fType := resp[1] // 0x63=视频；0x62=音频

        switch fType {
        case 0x63: // 视频（Annex‑B HEVC）
            skip := dynamicSkipAfterPercentJSON(resp) // 找 "%7D"/'}'，再跳 8 字节
            if skip <= 0 || skip >= len(resp) {
                continue
            }
            payload := resp[skip:]
            if align := indexAnnexB(payload); align >= 0 {
                vbuf.Write(payload[align:])
            }

        case 0x62: // 音频（G.711 A‑law）
            // 结构：00 62 | 00 01 | 4字节序列/时间戳 | 00 00 00 00 | A‑law负载 | 00 00 01 4B
            if len(resp) >= 2+2+4+4+4 {
                if resp[len(resp)-4] == 0x00 && resp[len(resp)-3] == 0x00 && resp[len(resp)-2] == 0x01 && resp[len(resp)-1] == 0x4B {
                    ap := resp[12 : len(resp)-4]
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                } else {
                    ap := resp[12:]
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                }
            }
        default:
            // 其他类型暂忽略
        }

        // 达到设定大小（MB）返回
        if vbuf.Len() > 1024*1024*video.Size || abuf.Len() > 1024*1024*video.Size {
            break
        }
    }

    return vbuf.Bytes(), abuf.Bytes()
}

// -------------------------- 负载定位 --------------------------

func dynamicSkipAfterPercentJSON(resp []byte) int {
    idx := bytes.Index(resp, []byte("%7D"))
    if idx < 0 {
        idx = bytes.IndexByte(resp, '}')
    }
    if idx < 0 {
        return 0
    }
    return idx + len("%7D") + 8 // 经验：JSON 后还有 8 字节长度/对齐
}

func indexAnnexB(p []byte) int {
    for i := 0; i+4 <= len(p); i++ {
        if p[i] == 0 && p[i+1] == 0 && p[i+2] == 0 && p[i+3] == 1 {
            return i
        }
        if i+3 <= len(p) && p[i] == 0 && p[i+1] == 0 && p[i+2] == 1 {
            return i
        }
    }
    return -1
}

// -------------------------- 合成 MP4（自动找/落地 ffmpeg） --------------------------

func MergeToMP4(hevcPath, alawPath, mp4Path string, fps int) error {
    if !fileExists(hevcPath) || !fileExists(alawPath) {
        return fmt.Errorf("源文件不存在：%s 或 %s", hevcPath, alawPath)
    }
    ffmpegPath, err := ensureFFmpegAvailable()
    if err != nil {
        return err
    }
    cmd := exec.Command(
        ffmpegPath,
        "-hide_banner", "-y",
        "-f", "alaw", "-ar", "8000", "-ac", "1", "-i", alawPath,
        "-r", strconv.Itoa(fps), "-fflags", "+genpts", "-i", hevcPath,
        "-c:v", "copy", "-c:a", "aac", "-shortest", mp4Path,
    )
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

// -------------------------- 文件名/落盘/清理 --------------------------

func getFileName(dirPath string) string {
    dateFolder := time.Now().Format("20060102")
    fullPath := dirPath + "/" + dateFolder
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        if err := os.MkdirAll(fullPath, 0755); err != nil {
            FmtPrint("创建文件夹失败：", err)
            os.Exit(0)
        }
    }
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

func DeleteOldFiles(config *Config, video *Video) {
    dirPath := config.Path + "/" + video.Name
    foldersToKeep := video.Count
    var folders []fs.FileInfo
    entries, _ := os.ReadDir(dirPath)
    for _, entry := range entries {
        if entry.IsDir() {
            info, _ := os.Stat(filepath.Join(dirPath, entry.Name()))
            folders = append(folders, info)
        }
    }
    if len(folders) <= foldersToKeep {
        return
    }
    sort.Slice(folders, func(i, j int) bool {
        return folders[i].ModTime().After(folders[j].ModTime())
    })
    for i := foldersToKeep; i < len(folders); i++ {
        old := filepath.Join(dirPath, folders[i].Name())
        _ = os.RemoveAll(old)
    }
}
