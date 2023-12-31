package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"

	// "net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	crdp "github.com/chromedp/chromedp"
)

var urls = []string{
	"https://jiang.netlify.app/", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/freefq/free/master/v2",                               //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/mfuu/v2ray/master/v2ray",                             //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/peasoft/NoMoreWalls/master/list_raw.txt",             //raw
	"https://ghproxy.com/https://raw.githubusercontent.com/ermaozi/get_subscribe/main/subscribe/v2ray.txt",      //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/aiboboxx/v2rayfree/main/v2",                          //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/Pawdroid/Free-servers/main/sub",                      //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription1", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription2", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription3", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription4", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription5", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription6", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription7", //b64
	"https://ghproxy.com/https://raw.githubusercontent.com/w1770946466/Auto_proxy/main/Long_term_subscription8", //b64
}

var concurrent = -1
var startPort = 3080

// var wTestStartPort = 10180
var gTestStartPort = 10280

// var wTestPorts SafeStack
var gTestPorts SafeStack
var verbose = false

var headlessAddrs = []string{
	"ws://10.10.1.127:9222",
	"ws://10.10.1.127:9223",
	"ws://10.10.1.127:9224",
	"ws://10.10.1.127:9225",
	"ws://10.10.1.127:9226",
	"ws://10.10.1.127:9227",
	"ws://10.10.1.127:9228",
	"ws://10.10.1.127:9229",
	"ws://10.10.1.127:9230",
	"ws://10.10.1.127:9231",
	"ws://10.10.1.127:9232",
	"ws://10.10.1.127:9233",
}

type HeadlessContextItem struct {
	BaseCtx   context.Context
	Ctx       context.Context
	RawCancel context.CancelFunc
}

func (item *HeadlessContextItem) cancel() {
	item.Ctx = nil
	item.RawCancel()
	item.RawCancel = nil
}

func (item *HeadlessContextItem) current() (context.Context, context.CancelFunc) {
	if item.Ctx == nil {
		// fmt.Println("creating new target context")
		pCtx, pCancel := crdp.NewContext(item.BaseCtx)
		err := crdp.Run(pCtx, crdp.Navigate("about:blank"))
		if err != nil {
			fmt.Println("unexpect error:", err)
			pCancel()
			os.Exit(-1)
		}
		tis, err := crdp.Targets(pCtx)
		if err != nil {
			fmt.Println("unexpect error:", err)
			os.Exit(-1)
		}
		ctx, tCancel := crdp.NewContext(
			item.BaseCtx,
			crdp.WithTargetID(tis[0].TargetID),
		)
		item.Ctx = ctx
		item.RawCancel = func() {
			tCancel()
			pCancel()
		}
		// fmt.Println("new target context created")
	}
	return item.Ctx, item.RawCancel
}

var headlessContexts = []*HeadlessContextItem{}

var headlessPorxyPorts = []int{20001, 20002, 20003, 20004, 20005, 20006, 20007, 20008, 20009, 20010, 20011, 20012}

type Config struct {
	Outbounds []Outbound `json:"outbounds"`
}

type Outbound struct {
	Settings       Settings        `json:"settings"`
	StreamSettings *StreamSettings `json:"streamSettings,omitempty"`
}

type Settings struct {
	Servers []Server `json:"servers,omitempty"`
	VNexts  []VNext  `json:"vnext,omitempty"`
}

type Server struct {
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

type VNext struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Users   []User `json:"users"`
}

type User struct {
	ID      string `json:"id,omitempty"`
	AlterID int    `json:"alterId,omitempty"`
}

type StreamSettings struct {
	Network     string       `json:"network"`
	WSSettings  *WSSettings  `json:"wsSettings,omitempty"`
	TLSSettings *TLSSettings `json:"tlsSettings,omitempty"`
}

type WSSettings struct {
	Path    *string           `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type TLSSettings struct {
	ServerName *string `json:"serverName,omitempty"`
}

type ProxyInfo struct {
	Id       uint32 `json:"id"`
	Port     int    `json:"port"`
	IsOnDuty bool   `json:"isOnDuty"`
	Cfg      string
}

func (p ProxyInfo) String() string {
	return fmt.Sprintf("{Id: %d, Port: %d, IsOnDuty: %t}", p.Id, p.Port, p.IsOnDuty)
}

type SafeStack struct {
	mu    sync.Mutex
	stack []interface{}
}

func (s *SafeStack) Push(item interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stack = append(s.stack, item)
}

func (s *SafeStack) Pop() (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.stack) == 0 {
		return nil, errors.New("stack is empty")
	}

	item := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]

	return item, nil
}

func main() {
	// 定义命令行参数
	wFlag := flag.Bool("w", false, "Execute w function")
	gFlag := flag.Bool("g", false, "Execute g function")
	tFlag := flag.Bool("t", false, "Execute test")
	vFlag := flag.Bool("v", false, "verbose")
	// 解析命令行参数
	flag.Parse()

	ensureDirs()

	concurrent = len(headlessAddrs)

	if *vFlag {
		verbose = true
	}

	if *tFlag {
		test()
		return
	}

	// 根据启动参数执行相应的函数
	if *wFlag {
		w()
	}

	if !*wFlag && *gFlag {
		g()
	}

	if !*wFlag && !*gFlag {
		w()
	}

}

func getBodyHtmlWithProxy(headlessId int) string {
	contextItem := headlessContexts[headlessId]
	ctx, _ := contextItem.current()
	isOk := false
	//設置一個超時
	timer := time.AfterFunc(8*time.Second, func() {
		// fmt.Println("cancelled by timer")
		contextItem.cancel()
	})
	// fmt.Println("trying to listen target")
	crdp.ListenTarget(ctx, func(ev interface{}) {
		if res, ok := ev.(*network.EventResponseReceived); ok {
			// fmt.Println("status->", res.Response.URL, res.Response.Status)
			// 提取HTTP状态码
			if res.Response.Status == 403 {
				timer.Stop()
				contextItem.cancel()
			} else if res.Response.URL == "https://chat.openai.com/auth/login" && res.Response.Status == 200 {
				timer.Stop()
				isOk = true
				contextItem.cancel()
			}
		}
	})

	// fmt.Println("navi to chatgpt")
	err := crdp.Run(ctx, crdp.Navigate("https://chat.openai.com"))
	timer.Stop()
	if err != nil {
		if isOk {
			return "ok"
		}
		return ""
	}

	// time.Sleep(2 * time.Second)
	// var html string
	// err = crdp.Run(ctx, crdp.Evaluate("document?document.documentElement?document.documentElement.innerHTML:\"elem not ready\":\"document not ready\"", &html))
	// if err != nil {
	// 	fmt.Println(err)
	// 	return ""
	// }
	return "ok"
}

func ensureDirs() {
	directories := []string{
		"./cfgs",
		"./chatgpt",
		"./tmp",
	}

	for _, dir := range directories {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			// 文件夹不存在，创建它
			err := os.MkdirAll(dir, 0755)
			if err != nil {
				fmt.Println("Error creating directory:", err)
			} else {
				fmt.Println("Directory", dir, "created successfully.")
			}
		}
	}
}

func test() {
	initHeadlessContexts()
	fmt.Println("initHeadlessContexts done.")
	html := getBodyHtmlWithProxy(0)
	fmt.Println(html)
	html = getBodyHtmlWithProxy(0)
	fmt.Println(html)
}

//	func initWTestPorts() {
//		wTestPorts = SafeStack{}
//		emptyPortInUse := make(map[int]bool)
//		for i := 0; i <= concurrent; i++ {
//			port, _ := findAvailablePort(wTestStartPort+i, emptyPortInUse)
//			wTestPorts.Push(port)
//		}
//	}
func initGTestPorts() {
	gTestPorts = SafeStack{}
	emptyPortInUse := make(map[int]bool)
	for i := 0; i <= concurrent; i++ {
		port, _ := findAvailablePort(gTestStartPort+i, emptyPortInUse)
		gTestPorts.Push(port)
	}
}

func initHeadlessContexts() {
	if len(headlessContexts) > 0 {
		return
	}
	for _, addr := range headlessAddrs {
		rootCtx, _ := crdp.NewRemoteAllocator(
			context.Background(),
			addr,
		)
		bCtx, _ := crdp.NewContext(
			rootCtx,
		)
		tis, err := crdp.Targets(bCtx)
		if err != nil {
			fmt.Println("unexpect error:", err)
			os.Exit(-1)
		}
		if len(tis) == 0 {
			err := crdp.Run(bCtx, crdp.Navigate("about:blank"))
			if err != nil {
				fmt.Println("unexpect error:", err)
				os.Exit(-1)
			}
			tis, err = crdp.Targets(bCtx)
			if err != nil {
				fmt.Println("unexpect error:", err)
				os.Exit(-1)
			}
		}
		headlessCtx := HeadlessContextItem{BaseCtx: rootCtx}
		headlessCtx.Ctx, headlessCtx.RawCancel = crdp.NewContext(
			bCtx,
			crdp.WithTargetID(tis[0].TargetID),
		)
		headlessContexts = append(headlessContexts, &headlessCtx)
	}
}

func w() {
	//初始化testPorts
	initHeadlessContexts()
	allInfos, chatGPTInfos := currentProxyInfo()
	doFetch(allInfos, chatGPTInfos)
}

func doFetch(allInfos []ProxyInfo, chatGPTInfos []ProxyInfo) {
	count := 0
	vmessCount := 0
	trojanCount := 0
	vlessCount := 0

	existAllInfos := make(map[uint32]ProxyInfo)
	//用来记录增量的proxyInfo
	fetchedAllInfos := make(map[uint32]ProxyInfo)

	//用来保存全部server的info
	allServerInfos := make(map[uint32]ProxyInfo)

	//用来测试tcp连通性
	serverINetAddrs := make(map[uint32]string) //serverId, addr:port
	//用来记录tcp连通性测试的结果
	serverTcpFlags := sync.Map{}

	//用来存放已经被占用的本地Port
	portsInUse := make(map[int]bool)

	//把已存在的proxies预先加入测试集合
	for _, proxy := range allInfos {
		existAllInfos[proxy.Id] = proxy
		allServerInfos[proxy.Id] = proxy
		//serverLocalPorts[proxy.Id] = proxy.Port
		portsInUse[proxy.Port] = true
		addr, port, _, _, _, _, _, _, _ := extractServerInfo(proxy.Cfg)
		inetAddr := fmt.Sprintf("%s:%d", addr, port)
		serverINetAddrs[proxy.Id] = inetAddr
		serverTcpFlags.Store(inetAddr, true)
	}

	// 遍历urls

	for _, nodeSourceUrl := range urls {
		fmt.Println("Fetching", nodeSourceUrl)
		nodes := fetchNodeSource(nodeSourceUrl)
		// 逐行读取到数组
		lines := strings.Split(nodes, "\n")
		// id := 0
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if strings.HasPrefix(line, "ss://") || strings.HasPrefix(line, "ssr://") {
				//什么都不做，我们不要ss的节点。
				continue
			}
			//fmt.Println(line)
			//调用fq2json.py,转换成json配置。
			jsonCfg, errout := cvt2json(line)
			//从stdout中提取信息，host，port，uid(password), altId, ws(host,path), tls
			if errout != "" || jsonCfg == "" {
				// fmt.Println(line, jsonCfg, errout)
				continue
			}
			addr, port, uid, aid, sni, wsh, wsp, nw, _ := extractServerInfo(jsonCfg)
			if port == 0 {
				continue
			}
			serverId := hashString(fmt.Sprintf("addr->%sport->%duid->%said->%dsni->%snw->%swsh->%swsp->%s", addr, port, uid, aid, sni, nw, wsh, wsp))
			_, ok := serverINetAddrs[serverId]
			if ok {
				continue
			}

			//执行到这里说明当前处理的proxy cfg是新的
			pInfo := ProxyInfo{Id: serverId, IsOnDuty: false, Cfg: jsonCfg}
			allServerInfos[serverId] = pInfo
			fetchedAllInfos[serverId] = pInfo
			inetAddr := fmt.Sprintf("%s:%d", addr, port)
			serverINetAddrs[serverId] = inetAddr
			serverTcpFlags.Store(inetAddr, true)

			if strings.HasPrefix(line, "vmess://") {
				vmessCount = vmessCount + 1
			} else if strings.HasPrefix(line, "vless://") {
				vlessCount = vlessCount + 1
			} else if strings.HasPrefix(line, "trojan://") {
				trojanCount = trojanCount + 1
			}
			count = count + 1
		}
		fmt.Println("total->", count, " vmess->", vmessCount, " trojan->", trojanCount, " vless->", vlessCount)
	}

	//测试服务器的连通性
	testServiceConnections(&serverTcpFlags)

	// serverTcpFlags.Range(func(server, reachable interface{}) bool {
	// 	fmt.Println("server:", server, " reachable is :", reachable)
	// 	return true
	// })

	//清理./chatgpt目录
	removeAllContents("./chatgpt")

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrent)
	headlessIds := make(chan int, concurrent)
	for id := range headlessAddrs {
		headlessIds <- id
	}

	for _, proxyInfo := range allServerInfos {
		serverINetAddr, ok := serverINetAddrs[proxyInfo.Id]
		if !ok {
			fmt.Printf("Unknown server: id = %d, port = %d\n", proxyInfo.Id, proxyInfo.Port)
			continue
		}
		reachable, ok := serverTcpFlags.Load(serverINetAddr)
		if !ok || !reachable.(bool) {
			continue
		}
		fileName := ""
		//如果是新的，则本地Port为0，需要为其分配一个localPort
		if proxyInfo.Port == 0 {
			localPort, _ := findAvailablePort(startPort, portsInUse)
			proxyInfo.Port = localPort
			//serverLocalPorts[proxyInfo.Id] = localPort
			portsInUse[localPort] = true
			//将sCfg中的555555替换成localPort
			proxyInfo.Cfg = strings.Replace(proxyInfo.Cfg, "555555", fmt.Sprintf("%d", localPort), -1)
			//将新抓取的这个proxy config写入配置文件
			fileName = fmt.Sprintf("./cfgs/%d_%d.json", proxyInfo.Id, localPort)
			file, err := os.Create(fileName)
			if err != nil {
				fmt.Println("Error creating file:", err)
				continue
			}
			defer file.Close()
			// 写入文件内容
			_, err = io.WriteString(file, proxyInfo.Cfg)
			if err != nil {
				fmt.Println("Error writing to file:", err)
				continue
			}
		} else {
			fileName = fmt.Sprintf("./cfgs/%d_%d.json", proxyInfo.Id, proxyInfo.Port)
		}
		//测试chatgpt连通性
		sem <- struct{}{} // 通过信号量控制并行度
		wg.Add(1)
		// tp, _ := wTestPorts.Pop()
		// testPort := tp.(int)

		hlId := <-headlessIds
		go func(cfgFile string, cfg string, serverId uint32, originPort int, headlessId int) {
			defer func() {
				// wTestPorts.Push(testPort)
				<-sem // 释放信号量
				headlessIds <- headlessId
				wg.Done()
			}()
			isReachable := testChatGptConnect(cfg, serverId, originPort, headlessId, false) > 0
			if isReachable {
				//copy cfg file to ./chatgpt
				copyFile(cfgFile, "./chatgpt/")
			}
		}(fileName, proxyInfo.Cfg, proxyInfo.Id, proxyInfo.Port, hlId)
	}
	wg.Wait()
}

func testChatGptConnect(cfg string, serverId uint32, port int, headlessId int, shouldTestSpeed bool) float64 {
	testPort := headlessPorxyPorts[headlessId]
	ok, cmd := startTmpXray(cfg, port, testPort)
	defer func() {
		if cmd != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	if ok {
		//开始测试chatgpt连接
		return testChatGptConnectWithProxy(headlessId, shouldTestSpeed, serverId, port)
	}
	return -1
}

func startTmpXray(cfg string, port int, testPort int) (bool, *exec.Cmd) {
	//将cfg中的" {port}," 替换成 " {testPort},"， 并写入到./tmp/{testPort}.json中，用新的这个cfg文件进行测试。
	testCfg := strings.Replace(cfg, fmt.Sprintf(" %d,", port), fmt.Sprintf(" %d,", testPort), -1)
	testCfgFileName := fmt.Sprintf("./tmp/%d.json", testPort)
	file, err := os.Create(testCfgFileName)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return false, nil
	}
	defer file.Close()
	// 写入文件内容
	_, err = io.WriteString(file, testCfg)
	if err != nil {
		fmt.Println("Error writing to file:", err)
		return false, nil
	}

	cmd := exec.Command("xray", "-c", testCfgFileName)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Println("Error creating StdoutPipe:", err)
		return false, cmd
	}

	err = cmd.Start()
	if err != nil {
		fmt.Println("Error starting xray:", err)
		return false, cmd
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.Contains(text, "Reading config:") && strings.Contains(text, fmt.Sprintf("%d.json", testPort)) {
				//fmt.Println("xray local service started with testPorted cfg:", testCfgFileName, " testPort = ", testPort)
				break
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Println("Error reading from pipe:", err)
		}
	}()
	wg.Wait()
	time.Sleep(1 * time.Second)
	return true, cmd
}

func testChatGptConnectWithProxy(headlessId int, shouldTestSpeed bool, serverId uint32, port int) float64 {
	if shouldTestSpeed {
		if verbose {
			fmt.Println("testing proxy port:", headlessPorxyPorts[headlessId])
		}
	}
	// targetURL := "https://chat.openai.com"
	// timeout := 8000 * time.Millisecond

	sBody := getBodyHtmlWithProxy(headlessId)
	if sBody == "" {
		return -1
	}

	if shouldTestSpeed {
		//fmt.Println("ChatGpt supported. begin to test speed with proxy:", proxyUrlStr)
		return testSpeed(fmt.Sprintf("socks5://127.0.0.1:%d", headlessPorxyPorts[headlessId]))
	}
	return 1

}

func testGoogle(proxyUrlStr string) bool {
	targetURL := "https://www.google.com"
	timeout := 5000 * time.Millisecond

	// 设置代理服务器地址
	proxyURL, err := url.Parse(proxyUrlStr)
	if err != nil {
		fmt.Println("Error parsing proxy URL:", err)
		return false
	}

	// 创建一个通过代理进行连接的Transport
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		Dial: (&net.Dialer{
			Timeout:   timeout,
			DualStack: false, // 禁用IPv6，只使用IPv4
		}).Dial,
		TLSHandshakeTimeout: 5 * time.Second,
	}

	// 创建一个客户端并使用自定义Transport
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		fmt.Println("Failed to create request:", err)
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Failed to send request to Google:", proxyUrlStr, err)
		return false
	} else if resp.StatusCode != 200 {
		fmt.Println("Failed to load Google with http status:", resp.StatusCode)
	}
	return resp.StatusCode == 200
}

func testSpeed(proxyUrlStr string) float64 {
	targetURL := "http://cachefly.cachefly.net/10mb.test"
	timeout := 5000 * time.Millisecond

	// 设置代理服务器地址
	proxyURL, err := url.Parse(proxyUrlStr)
	if err != nil {
		//fmt.Println("Error parsing proxy URL:", err)
		return -1
	}

	// 创建一个通过代理进行连接的Transport
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		Dial: (&net.Dialer{
			Timeout:   timeout,
			DualStack: false, // 禁用IPv6，只使用IPv4
		}).Dial,
		TLSHandshakeTimeout: 5 * time.Second,
	}

	// 创建一个客户端并使用自定义Transport
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		// fmt.Println("Failed to create request:", err)
		return -1
	}
	dataStart := time.Now().Add(-500 * time.Microsecond)
	resp, err := client.Do(req)
	if err != nil {
		// fmt.Println("Failed to send request:", err)
		return -1
	}
	defer resp.Body.Close()
	// dataStart := times["ondata"]
	dataElapsed := time.Since(dataStart).Seconds()
	if resp.StatusCode != 200 {
		return -1
	}
	speed := float64(10) / dataElapsed

	return speed
}

func cvt2json(cfgStr string) (string, string) {
	cmd := exec.Command("python", "fq2json.py", cfgStr)

	// 创建管道连接到命令的标准输出和标准错误输出
	stdout, err := cmd.StdoutPipe()
	if err != nil {

		fmt.Println("Failed to create pipe for stdout:", err)

		return "", ""
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {

		fmt.Println("Failed to create pipe for stderr:", err)

		return "", ""
	}

	err = cmd.Start()
	if err != nil {

		fmt.Println("Failed to start the command:", err)

		return "", ""
	}

	// 读取命令标准输出到字符串变量
	stdoutOutput, err := io.ReadAll(stdout)
	if err != nil {
		fmt.Println("Failed to read command stdout:", err)

		return "", "Failed to read command stdout"
	}

	// 读取命令标准错误输出到字符串变量
	stderrOutput, err := io.ReadAll(stderr)
	if err != nil {

		fmt.Println("Failed to read command stderr:", err)

		return "", "Failed to read command stderr"
	}
	cmd.Wait()
	return string(stdoutOutput), string(stderrOutput)
}

func fetchNodeSource(url string) string {
	resp, err := http.Get(url)
	if err != nil {

		fmt.Println("Error fetching content from url", url, err)

		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {

		fmt.Println("Error reading response body from url", url, err)

		return ""
	}
	bodyStr := strings.TrimSpace(string(body))
	// Base64解码
	decoded, err := base64.StdEncoding.DecodeString(bodyStr)
	if err != nil {
		return bodyStr
	}
	return string(decoded)
}

func extractServerInfo(jsonStr string) (string, int, string, int, string, string, string, string, error) { //return address, port, password/userId, alterId, tlsServerName, wsHost, wsPath, error
	addr, port, uid, aid, sni, wsHost, wsPath, nw := "", 0, "", 0, "", "", "/", "tcp"
	var config Config
	err := json.Unmarshal([]byte(jsonStr), &config)
	if err != nil {
		return addr, port, uid, aid, sni, wsHost, wsPath, nw, err
	}

	if len(config.Outbounds) > 0 {
		outbound := config.Outbounds[0]
		settings := outbound.Settings

		// Check servers
		if len(settings.Servers) > 0 {
			server := settings.Servers[0]
			addr, port, uid = server.Address, server.Port, server.Password
		}

		// Check vnext
		if len(settings.VNexts) > 0 {
			vnext := settings.VNexts[0]
			addr, port = vnext.Address, vnext.Port
			users := vnext.Users
			if len(users) > 0 {
				user := users[0]
				uid, aid = user.ID, user.AlterID
			}
		}
		if outbound.StreamSettings != nil {
			nw = outbound.StreamSettings.Network
			if outbound.StreamSettings.WSSettings != nil {
				if outbound.StreamSettings.WSSettings.Path != nil {
					wsPath = strings.TrimSpace(*outbound.StreamSettings.WSSettings.Path)
				}
				wsHost = strings.TrimSpace(outbound.StreamSettings.WSSettings.Headers["Host"])
			}
			if outbound.StreamSettings.TLSSettings != nil {
				if outbound.StreamSettings.TLSSettings.ServerName != nil {
					sni = strings.TrimSpace(*outbound.StreamSettings.TLSSettings.ServerName)
				}
			}
		}
	}
	return addr, port, uid, aid, sni, wsHost, wsPath, nw, nil
}

func hashString(str string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(str))
	return h.Sum32()
}

func testServiceConnections(serverTcpFlags *sync.Map) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3*concurrent)
	serverAddrs := make(map[string]bool)
	serverTcpFlags.Range(func(key, value interface{}) bool {
		serverAddrs[key.(string)] = true
		return true
	})

	for server := range serverAddrs {
		sem <- struct{}{} // 通过信号量控制并行度

		wg.Add(1)
		go func(service string) {
			defer func() {
				<-sem // 释放信号量
				wg.Done()
			}()

			if testServiceConnection(service) {
				if verbose {
					fmt.Printf("Service %s is reachable\n", service)
				}
			} else {
				if verbose {
					fmt.Printf("Service %s is unreachable\n", service)
				}
				serverTcpFlags.Store(service, false)
			}
		}(server)
	}
	wg.Wait()
}

func testServiceConnection(service string) bool {
	conn, err := net.DialTimeout("tcp", service, 3*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

func currentProxyInfo() ([]ProxyInfo, []ProxyInfo) {
	all, chatgpt := loadProxiesFromConfigDir("./cfgs/"), loadProxiesFromConfigDir("./chatgpt/")
	return all, chatgpt
}

/*
func getOnDutyProxies() []ProxyInfo {
	ret := []ProxyInfo{}
	resp, err := http.Get("http://127.0.0.1:8010/actives")
	if err != nil {
		//fmt.Println(err)
		return ret
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ret
	}

	var data struct {
		Proxies []ProxyInfo `json:"proxies"`
	}

	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Println(err)
		return ret
	}

	return data.Proxies
}
*/

func findAvailablePort(startPort int, portsInUse map[int]bool) (int, error) {
	for port := startPort; port <= 65535; port++ {
		_, inUse := portsInUse[port]
		if inUse {
			continue
		}
		address := fmt.Sprintf(":%d", port)
		listener, err := net.Listen("tcp", address)
		if err == nil {
			listener.Close()
			return port, nil
		}
	}
	if verbose {
		fmt.Println("no available port found!")
	}
	return 0, fmt.Errorf("no available port found")
}

/*
func getNewline() string {
	if runtime.GOOS == "windows" {
		return "\r\n" // Windows 使用 "\r\n" 作为换行符
	}
	return "\n" // 其他操作系统使用 "\n" 作为换行符
}
*/

func copyFile(sourceFile, destinationDir string) error {
	// Open the source file
	src, err := os.Open(sourceFile)
	if err != nil {
		return err
	}
	defer src.Close()

	// Create the destination file
	destinationFile := filepath.Join(destinationDir, filepath.Base(sourceFile))
	dst, err := os.Create(destinationFile)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Copy the contents from source to destination
	_, err = io.Copy(dst, src)
	if err != nil {
		return err
	}

	return nil
}

// 清空文件夹的内容
func removeAllContents(folderPath string) error {
	files, err := filepath.Glob(filepath.Join(folderPath, "*"))
	if err != nil {
		return err
	}

	for _, file := range files {
		err = os.RemoveAll(file)
		if err != nil {
			return err
		}
	}

	return nil
}

func loadProxiesFromConfigDir(dir string) []ProxyInfo {
	ret := []ProxyInfo{}
	//遍历dir获取配置文件
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		//获取文件名
		fileName := f.Name()
		id, port := extractServerIdNPort(fileName)
		fullPath := filepath.Join(dir, fileName)
		cfg, err := os.ReadFile(fullPath)
		if err != nil {
			fmt.Printf("Failed to read file: %s\n", err.Error())
			continue
		}
		pi := ProxyInfo{Id: id, Port: port, IsOnDuty: false, Cfg: string(cfg)}
		allProxies[pi.Id] = pi
		ret = append(ret, pi)
	}
	return ret
}

func extractServerIdNPort(str string) (uint32, int) {
	startIndex := strings.LastIndex(str, "_") + 1
	endIndex := strings.LastIndex(str, ".")
	if startIndex <= 0 || endIndex <= 0 || startIndex >= endIndex {
		return 0, 0
	}
	idStr := str[:startIndex-1]
	portStr := str[startIndex:endIndex]
	id, _ := strconv.Atoi(idStr)
	port, _ := strconv.Atoi(portStr)
	return uint32(id), port
}
