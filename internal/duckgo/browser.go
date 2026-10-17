package duckgo

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	browserManagerLock sync.Mutex
	activeChromium     *ChromiumInstance
)

// ChromiumInstance 管理运行中的 Headless Chromium 进程及 CDP 通信
type ChromiumInstance struct {
	cmd     *exec.Cmd
	port    int
	tempDir string
	wsURL   string
}

// ExecuteVQDHashInChromium 优先在真正的 Chromium 浏览器实例中求解 JSA 挑战
func ExecuteVQDHashInChromium(jsCode string) (string, error) {
	if os.Getenv("DISABLE_BROWSER") == "true" || os.Getenv("DISABLE_BROWSER") == "1" {
		return "", errors.New("browser execution disabled by env")
	}

	// 1. 优先尝试外部 Playwright / 伴生浏览器服务（例如容器内运行的伴生服务）
	if solverURL := getBrowserSolverURL(); solverURL != "" {
		if payload, err := callBrowserSolverService(solverURL, jsCode); err == nil && payload != "" {
			return payload, nil
		}
	}

	// 2. 尝试本地真实 Chromium / Edge 进程（通过 CDP 通信）
	inst, err := getOrStartChromiumInstance()
	if err != nil {
		return "", err
	}

	return inst.EvaluateChallenge(jsCode)
}

func getBrowserSolverURL() string {
	if u := os.Getenv("PLAYWRIGHT_SERVICE_URL"); u != "" {
		return u
	}
	if u := os.Getenv("BROWSER_SERVICE_URL"); u != "" {
		return u
	}
	return ""
}

func callBrowserSolverService(serviceURL string, jsCode string) (string, error) {
	endpoint := strings.TrimRight(serviceURL, "/") + "/solve"
	reqBody, err := json.Marshal(map[string]string{
		"js_code": jsCode,
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("solver service returned status %d", resp.StatusCode)
	}

	var res struct {
		Payload string `json:"payload"`
		Token   string `json:"token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.Error != "" {
		return "", errors.New(res.Error)
	}
	if res.Payload != "" {
		return res.Payload, nil
	}
	if res.Token != "" {
		decoded, err := base64.StdEncoding.DecodeString(res.Token)
		if err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("empty response from solver service")
}

func findChromiumExecutable() (string, error) {
	if p := os.Getenv("CHROMIUM_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p := os.Getenv("CHROME_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	default: // linux / bsd
		candidates = []string{
			"/usr/bin/chromium-browser",
			"/usr/bin/chromium",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/local/bin/chromium",
		}
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	// 尝试通过 PATH 查找
	for _, name := range []string{"chromium-browser", "chromium", "google-chrome", "chrome", "msedge"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}

	return "", errors.New("no chromium/chrome executable found on system")
}

func getOrStartChromiumInstance() (*ChromiumInstance, error) {
	browserManagerLock.Lock()
	defer browserManagerLock.Unlock()

	// 检查是否已有配置的 CDP_URL
	if cdpURL := os.Getenv("CDP_URL"); cdpURL != "" {
		return &ChromiumInstance{wsURL: cdpURL}, nil
	}

	// 检查当前运行中的实例是否健康
	if activeChromium != nil {
		if err := activeChromium.ping(); err == nil {
			return activeChromium, nil
		}
		activeChromium.close()
		activeChromium = nil
	}

	exePath, err := findChromiumExecutable()
	if err != nil {
		return nil, err
	}

	port := 9222
	// 尝试检查 9222 是否可用
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		l.Close()
	} else {
		// 动态分配一个空闲端口
		if freeL, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
			port = freeL.Addr().(*net.TCPAddr).Port
			freeL.Close()
		}
	}

	tempDir, err := os.MkdirTemp("", "ddg_cdp_profile_*")
	if err != nil {
		tempDir = filepath.Join(os.TempDir(), fmt.Sprintf("ddg_cdp_%d", time.Now().UnixNano()))
		_ = os.MkdirAll(tempDir, 0755)
	}

	args := []string{
		"--headless=new",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", tempDir),
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-blink-features=AutomationControlled",
		"--disable-background-networking",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		fmt.Sprintf("--user-agent=%s", defaultVQDUserAgent),
		"https://duck.ai/",
	}

	cmd := exec.Command(exePath, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start chromium process: %w", err)
	}

	inst := &ChromiumInstance{
		cmd:     cmd,
		port:    port,
		tempDir: tempDir,
	}

	// 等待 CDP 端口就绪并定位页面
	var wsURL string
	for i := 0; i < 20; i++ {
		time.Sleep(200 * time.Millisecond)
		targets, err := inst.getTargets()
		if err == nil && len(targets) > 0 {
			for _, t := range targets {
				if strings.Contains(t.URL, "duck.ai") && t.WebSocketDebuggerURL != "" {
					wsURL = t.WebSocketDebuggerURL
					break
				}
			}
			if wsURL == "" && len(targets) > 0 {
				wsURL = targets[0].WebSocketDebuggerURL
			}
			if wsURL != "" {
				break
			}
		}
	}

	if wsURL == "" {
		inst.close()
		return nil, errors.New("timeout waiting for chromium page target")
	}

	inst.wsURL = wsURL
	activeChromium = inst
	return inst, nil
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func (c *ChromiumInstance) getTargets() ([]cdpTarget, error) {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", c.port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (c *ChromiumInstance) ping() error {
	if c.cmd != nil && c.cmd.Process != nil {
		if c.port > 0 {
			client := &http.Client{Timeout: 500 * time.Millisecond}
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", c.port))
			if err == nil {
				resp.Body.Close()
				return nil
			}
		}
	}
	if c.wsURL != "" {
		return nil
	}
	return errors.New("chromium instance dead")
}

func (c *ChromiumInstance) close() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	if c.tempDir != "" {
		_ = os.RemoveAll(c.tempDir)
	}
}

// EvaluateChallenge 在真实 Chromium 页面内执行挑战 JS，并计算 SHA-256 base64 输出
func (c *ChromiumInstance) EvaluateChallenge(jsCode string) (string, error) {
	startMs := time.Now().UnixMilli()

	wrappedScript := fmt.Sprintf(`
	(async function() {
		const rawEval = await (async function() {
			return %s;
		})();

		if (!rawEval || typeof rawEval !== "object" || !Array.isArray(rawEval.client_hashes)) {
			throw new Error("Challenge did not return client_hashes object");
		}

		const hashed = await Promise.all(rawEval.client_hashes.map(async function(v) {
			const enc = new TextEncoder().encode(String(v));
			const digest = await crypto.subtle.digest("SHA-256", enc);
			const bytes = new Uint8Array(digest);
			let bin = "";
			for (let i = 0; i < bytes.length; i++) {
				bin += String.fromCharCode(bytes[i]);
			}
			return btoa(bin);
		}));

		const duration = String(Date.now() - %d);
		const origin = window.location.origin || "https://duck.ai";
		const stack = "%s";

		const meta = Object.assign({}, rawEval.meta || {}, {
			origin: origin,
			stack: stack,
			duration: duration
		});

		const result = Object.assign({}, rawEval, {
			client_hashes: hashed,
			meta: meta
		});

		return JSON.stringify(result);
	})()
	`, jsCode, startMs, defaultVQDStack)

	return cdpEvaluateRaw(c.wsURL, wrappedScript)
}

func cdpEvaluateRaw(wsURL string, script string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", err
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "80")
	}

	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("connect to cdp websocket: %w", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. 发起 WebSocket 握手
	secKey := make([]byte, 16)
	_, _ = rand.Read(secKey)
	secKeyB64 := base64.StdEncoding.EncodeToString(secKey)

	handshake := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, secKeyB64)

	if _, err := conn.Write([]byte(handshake)); err != nil {
		return "", fmt.Errorf("write cdp handshake: %w", err)
	}

	respBuf := make([]byte, 1024)
	n, err := conn.Read(respBuf)
	if err != nil || !strings.Contains(string(respBuf[:n]), "101 Switching Protocols") {
		return "", fmt.Errorf("cdp handshake failed: %s", string(respBuf[:n]))
	}

	// 2. 发送 Runtime.evaluate 消息
	payloadObj := map[string]interface{}{
		"id":     1,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    script,
			"awaitPromise":  true,
			"returnByValue": true,
		},
	}
	payloadBytes, _ := json.Marshal(payloadObj)
	wsFrame := encodeMaskedWebSocketFrame(payloadBytes)
	if _, err := conn.Write(wsFrame); err != nil {
		return "", fmt.Errorf("send cdp evaluate frame: %w", err)
	}

	// 3. 读取响应帧
	resData, err := readWebSocketTextMessage(conn)
	if err != nil {
		return "", fmt.Errorf("read cdp evaluate response: %w", err)
	}

	var cdpResp struct {
		ID     int `json:"id"`
		Result struct {
			Result struct {
				Type  string      `json:"type"`
				Value interface{} `json:"value"`
			} `json:"result"`
			ExceptionDetails *struct {
				Text      string `json:"text"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(resData, &cdpResp); err != nil {
		return "", fmt.Errorf("decode cdp response: %w", err)
	}

	if cdpResp.Error != nil {
		return "", fmt.Errorf("cdp error: %s", cdpResp.Error.Message)
	}
	if cdpResp.Result.ExceptionDetails != nil {
		desc := cdpResp.Result.ExceptionDetails.Text
		if cdpResp.Result.ExceptionDetails.Exception.Description != "" {
			desc = cdpResp.Result.ExceptionDetails.Exception.Description
		}
		return "", fmt.Errorf("in-browser exception: %s", desc)
	}

	val := cdpResp.Result.Result.Value
	if valStr, ok := val.(string); ok && valStr != "" {
		return valStr, nil
	}

	if val != nil {
		b, _ := json.Marshal(val)
		return string(b), nil
	}

	return "", errors.New("empty result from chromium evaluate")
}

func encodeMaskedWebSocketFrame(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x81)

	l := len(payload)
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)

	if l < 126 {
		buf.WriteByte(byte(0x80 | l))
	} else if l <= 65535 {
		buf.WriteByte(byte(0x80 | 126))
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(l))
		buf.Write(b)
	} else {
		buf.WriteByte(byte(0x80 | 127))
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(l))
		buf.Write(b)
	}

	buf.Write(mask)

	maskedPayload := make([]byte, l)
	for i := 0; i < l; i++ {
		maskedPayload[i] = payload[i] ^ mask[i%4]
	}
	buf.Write(maskedPayload)

	return buf.Bytes()
}

func readWebSocketTextMessage(r io.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	lenByte := header[1] & 0x7F
	var length uint64
	if lenByte < 126 {
		length = uint64(lenByte)
	} else if lenByte == 126 {
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b))
	} else {
		b := make([]byte, 8)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(b)
	}

	hasMask := (header[1] & 0x80) != 0
	var maskKey []byte
	if hasMask {
		maskKey = make([]byte, 4)
		if _, err := io.ReadFull(r, maskKey); err != nil {
			return nil, err
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}

	if hasMask {
		for i := uint64(0); i < length; i++ {
			payload[i] ^= maskKey[i%4]
		}
	}

	return payload, nil
}
