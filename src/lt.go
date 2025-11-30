
package main

import (
    "crypto/tls"
    "fmt"
    "io/fs"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/gorilla/websocket"
)

// 开始录制
func GoRecording(config *Config, video *Video) {
    // 临时变量
    tempPath := config.Path + "/" + video.Name

    // 断开后重连
    for {
        // 连接服务器传输数据
        bytes := linkServer(video)

        // 检查数据
        if len(bytes) == 0 {
            FmtPrint(video.Name + "设备连接失败，稍后自动重连(" + strconv.Itoa(config.Sleep) + ")")
            timeout := time.Duration(config.Sleep)
            time.Sleep(timeout * time.Second)
            continue
        }

        // 文件名称（yyyyMMdd/HHmmss.hevc）
        fileName := getFileName(tempPath) + ".hevc"

        // 保存文件
        saveFile(fileName, &bytes)

        // 录制完成
        FmtPrint("录制完成：" + fileName)

        // 按策略删除旧文件夹
        DeleteOldFiles(config, video)
    }
}

// 连接服务器（修复：补充握手头 + TLS SNI + 打印 HTTP 状态）
func linkServer(video *Video) []byte {
    bytes := []byte{}

    // 确保 Host 含端口，如 "vd-file-hnzz2-wcloud.wojiazongguan.cn:50443"
    host := video.WsHost
    u := url.URL{
        Scheme: "wss",
        Host:   host,
        Path:   "/h5player/live",
    }

    // 提取域名（去掉端口）用于 SNI 和 Origin
    hostWithoutPort := host
    if i := strings.Index(host, ":"); i > 0 {
        hostWithoutPort = host[:i]
    }

    // 构造 Dialer
    dialer := websocket.Dialer{
        HandshakeTimeout:  15 * time.Second,
        EnableCompression: true,
        TLSClientConfig: &tls.Config{
            InsecureSkipVerify: true,            // 保持你当前策略
            ServerName:         hostWithoutPort, // 显式 SNI 更稳
            MinVersion:         tls.VersionTLS12,
        },
        Subprotocols: []string{}, // 如服务端要求可设置为 []string{"binary"}
    }

    // 关键：补充浏览器常见请求头，尤其 Origin
    header := http.Header{}
    header.Set("Origin", "https://"+hostWithoutPort)
    header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
    header.Set("Accept-Language", "zh-CN,zh;q=0.9")
    // 某些风控可能依赖 Referer，可视情况打开：
    // header.Set("Referer", "https://"+hostWithoutPort+"/")

    // 发起握手，并保留 resp 便于诊断
    conn, resp, err := dialer.Dial(u.String(), header)
    if err != nil {
        status := ""
        if resp != nil {
            status = resp.Status
        }
        FmtPrint(fmt.Sprintf("无法连接到服务器: %v; HTTP状态: %s", err, status))
        return bytes
    }
    defer conn.Close()

    // 发送业务参数（沿用你的逻辑）
    message := "_paramStr_=" + video.ParamMsg
    if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
        FmtPrint("发送消息失败：", err)
        return bytes
    }

    // 接收消息（保持原先协议处理思路：只要有数据就拼接，达到 Size(MB) 即返回）
    for {
        msgType, response, err := conn.ReadMessage()
        if err != nil {
            FmtPrint("接收消息失败：", err)
            return bytes
        }

        // 如果服务端返回文本帧（常见欢迎/状态），打印以便观察
        if msgType == websocket.TextMessage {
            // 可选：打印文本首条若需要调试
            // FmtPrint("文本消息：", string(response))
            continue
        }

        // 二进制帧：拼接数据
        if len(response) > 1 {
            bytes = append(bytes, response[:]...)
            // 结束条件：达到设定的大小（MB）
            if len(bytes) > 1024*1024*video.Size {
                return bytes
            }
        }
    }
}

// 获取文件名称（目录/文件名）
func getFileName(dirPath string) string {
    // 添加日期文件夹
    dateFolder := time.Now().Format("20060102")
    fullPath := dirPath + "/" + dateFolder

    // 检查文件夹是否存在
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        // 文件夹不存在，创建它
        if err := os.MkdirAll(fullPath, 0755); err != nil {
            FmtPrint("创建文件夹失败：", err)
            os.Exit(0)
        }
    }

    // 文件名称（HHmmss）
    fileName := time.Now().Format("150405")
    tempPath := fullPath + "/" + fileName
    return tempPath
}

// 保存文件
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
    // 临时变量
    dirPath := config.Path + "/" + video.Name
    foldersToKeep := video.Count

    // 读取文件夹
    var folders []fs.FileInfo
    entries, _ := os.ReadDir(dirPath)
    for _, entry := range entries {
        if entry.IsDir() {
            info, _ := os.Stat(filepath.Join(dirPath, entry.Name()))
            folders = append(folders, info)
        }
    }

    // 检查文件夹数量
    if len(folders) <= foldersToKeep {
        return
    }

    // 按时间排序（新到旧）
    sort.Slice(folders, func(i, j int) bool {
        return folders[i].ModTime().After(folders[j].ModTime())
    })

    // 删除最旧的文件夹（保留最近 Count 个）
    for i := foldersToKeep; i < len(folders); i++ {
        oldFolder := filepath.Join(dirPath, folders[i].Name())
        _ = os.RemoveAll(oldFolder)
    }
}
