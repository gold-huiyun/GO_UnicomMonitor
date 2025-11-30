
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
            FmtPrint("请在系统中安装/配置 FFmpeg 后重试（Windows：下载预编译包，解压后将其 bin 目录加入 PATH；命令行执行 ffmpeg -version 验证）。")
        } else {
            FmtPrint("合成完成：", mp4)
        }

        // 清理旧文件夹
        DeleteOldFiles(config, video)
    }
}

// ========================== WebSocket 连接 + 分流抓取 ==========================

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
    // 如后端要求 Referer/Cookie，可在此补充

    // 修复：丢弃 resp，避免“未使用变量”编译错误
    conn, _, err := dialer.Dial(u.String(), header)
    if err != nil {
        FmtPrint(fmt.Sprintf("无法连接到服务器: %v", err))
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

    // 3) 启流指令（与前端类似）
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
                    ap := resp[12 : len(resp)-4] // 去掉 12 字节头 + 4 字节尾标
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                } else {
                    // 无尾标时兜底：仍剔除 12 字节头
                    ap := resp[12:]
                    if len(ap) > 0 {
                        abuf.Write(ap)
                    }
                }
            }

        default:
            // 其他类型：暂忽略或打印调试
            // FmtPrint(fmt.Sprintf("未知类型=0x%02x, head=% x", fType, resp[:16]))
        }

        // 达到设定大小（MB）返回
        if vbuf.Len() > 1024*1024*video.Size || abuf.Len() > 1024*1024*video.Size {
            break
        }
    }

    return vbuf.Bytes(), abuf.Bytes()
}

// ========================== 负载定位工具 ==========================

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
        if p[i] == 0 && p[i+1] == 0 && p[i+2] == 0 && p[i+3] == 1 { // 00 00 00 01
            return i
        }
        if i+3 <= len(p) && p[i] == 0 && p[i+1] == 0 && p[i+2] == 1 { // 00 00 01
            return i
        }
    }
    return -1
}

// ========================== 合成 MP4（调用本机 FFmpeg） ==========================

func MergeToMP4(hevcPath, alawPath, mp4Path string, fps int) error {
    if !fileExists(hevcPath) || !fileExists(alawPath) {
        return fmt.Errorf("源文件不存在：%s 或 %s", hevcPath, alawPath)
    }
    ffmpegPath, err := exec.LookPath("ffmpeg")
    if err != nil {
        return fmt.Errorf("未在 PATH 中找到 ffmpeg：请安装 FFmpeg 并加入 PATH 后重试（cmd 执行 `ffmpeg -version` 验证）。")
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

// ========================== 文件名/落盘/清理 ==========================

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

// 删除文件夹下的旧文件夹（保留最近 Count 个）
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
