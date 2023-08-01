package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProxyTask struct {
	Type    int
	Proxies []ProxyInfo
}

type ProxyNode struct {
	Id         uint32
	Score      float64
	Port       int
	ConfigFile string
}

func (p ProxyNode) String() string {
	return fmt.Sprintf("{Id: %d, Score: %.2f, Port: %d, Config File: %s}", p.Id, p.Score, p.Port, p.ConfigFile)
}

type ProxyNodeByScore []ProxyNode

func (p ProxyNodeByScore) Len() int           { return len(p) }
func (p ProxyNodeByScore) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p ProxyNodeByScore) Less(i, j int) bool { return p[i].Score < p[j].Score }

var maxSelectedProxies = 3 //事不过三

var (
	activeXrayCmds       map[uint32]*exec.Cmd
	activeXrayCmdsRWLock sync.RWMutex
)
var (
	activeProxies = []ProxyInfo{}
	activeRWLock  sync.RWMutex
)

var gateGliderCmd *exec.Cmd
var redGliderCmd *exec.Cmd
var greenGliderCmd *exec.Cmd
var redXrayCmd *exec.Cmd
var greenXrayCmd *exec.Cmd

var testingLock sync.Mutex

var allProxies = make(map[uint32]ProxyInfo)

const (
	ProxiesUpdated = 1 << iota // 1 重新对所有的配置项执行连通性和速度的测试，更新正在运行的proxy
	Task10s                    // 2 检查正在运行的proxy的forwarder的健康状态，如果有问题则立刻剔除掉问题节点，并发起一次ProxiesUpdated
	Task10m                    // 4 对正在运行的proxy进行一次测速，如果发生forward优先级变化，则重新启动red/green glider进行相应调整
	Task30m                    // 8 重新从./chatgpt目录装入配置，然后发起一次ProxiesUpdated
	Task2h                     // 16 重新抓取一次节点源，更新./chatgpt目录，并且发起一次ProxiesUpdated
)

var taskChan = make(chan ProxyTask, 16)

var (
	updatingFlag bool
	updateLock   sync.Mutex
)

func g() {
	initGTestPorts()
	//锁定2580端口，如果失败，则表示已经有一个gate启动了，需要退出。
	l, err := net.Listen("tcp", ":2580")
	if err != nil {
		fmt.Println("Gate has already started.")
		os.Exit(-1)
	}
	defer l.Close()

	// lastSuccFailFlipTimes = make(map[uint32]int)
	// failedInARows = make(map[uint32]int)
	activeXrayCmds = make(map[uint32]*exec.Cmd)

	l.Close()
	//启动gate glider
	startGateGlider()
	defer gateGliderCmd.Process.Kill()

	//启动Task Routine
	go doTask()

	//刚启动，从配置文件夹中获取proxies，并且调用onProxiesUpdate
	loadProxiesFromConfigDir("./chatgpt/")
	onProxyCfgsReady()

	defer finalizeCmds()

	startApiServer()

	//启动监控线程
	go monitorProxies()

	// 创建一个信号通道
	signalCh := make(chan os.Signal, 1)

	// 捕获指定的信号（例如，Ctrl+C）
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	// 阻塞主 goroutine，等待信号
	<-signalCh

	fmt.Println("Exiting...")
	// 这里可以执行一些清理操作或其他需要在程序退出时执行的任务

	os.Exit(0)
}

// ProxiesUpdated = 1 << iota // 1 重新对所有的配置项执行连通性和速度的测试，更新正在运行的proxy
// Task10s                    // 2 检查正在运行的proxy的forwarder的健康状态，如果有问题则立刻剔除掉问题节点，并发起一次ProxiesUpdated
// Task10m                    // 4 对正在运行的proxy进行一次测速，如果发生节点失效，则按Task10s处理，else如果发生forward优先级变化，则重新启动red/green glider进行相应调整
// Task30m                    // 8 重新从./chatgpt目录装入配置，然后发起一次ProxiesUpdated
// Task2h                     // 16 重新抓取一次节点源，更新./chatgpt目录，并且发起一次ProxiesUpdated
func doTask() {
	tasks := []ProxyTask{}
	for {
		select {
		case task := <-taskChan:
			tasks = append(tasks, task)
		default:
			{
				//合并同类项，取最后一个，然后按照优先级选取要处理的task，丢弃其他可以丢弃的。
				//优先级(Task2h > Task30m > Task10m), Task10s不可丢弃
				//如果有PorxiesUpdated，同时也有非PorxiesUpdated的task，则把PorxiesUpdated丢到最后再发一次chan
				if len(tasks) == 0 {
					//每20ms检查一次
					time.Sleep(20 * time.Microsecond)
					continue
				}
				taskMap := make(map[int]ProxyTask)
				for _, task := range tasks {
					taskMap[task.Type] = task
				}

				//清空
				tasks = []ProxyTask{}

				proxiesUpdatedTask, hasProxiesUpdatedTask := taskMap[ProxiesUpdated]
				if hasProxiesUpdatedTask && len(taskMap) == 1 {
					onProxyCfgsReady()
					continue
				}
				_, hasT10s := taskMap[Task10s]
				if hasT10s {
					doTask10s()
				}
				_, hasT2h := taskMap[Task2h]
				if hasT2h {
					doTask2h()
					continue
				}
				_, hasT30m := taskMap[Task30m]
				if hasT30m {
					doTask30m()
					continue
				}
				_, hasT10m := taskMap[Task10m]
				if hasT10m {
					doTask10m()
					if hasProxiesUpdatedTask {
						taskChan <- proxiesUpdatedTask
					}
					continue
				}
				if hasProxiesUpdatedTask {
					taskChan <- proxiesUpdatedTask
				}
			}
		}
	}

}

func startApiServer() {
	//启动http服务
	http.HandleFunc("/", handleRequest)
	// 启动服务器
	go func() {
		if err := http.ListenAndServe(":8010", nil); err != nil {
			log.Fatal(err)
		}
	}()
	fmt.Println("Api Server is running on port 8010. Press Ctrl+C to stop.")
}

func finalizeCmds() {
	//退出时杀掉 active 的xray cmd
	defer func() {
		activeXrayCmdsRWLock.RLock()
		defer activeXrayCmdsRWLock.RUnlock()
		for pid := range activeXrayCmds {
			activeXrayCmds[pid].Process.Kill()
		}
	}()
	//退出时杀掉 green red glider 和 xray 的cmd
	defer func() {
		if greenGliderCmd != nil {
			greenGliderCmd.Process.Kill()
		}
	}()
	defer func() {
		if redGliderCmd != nil {
			redGliderCmd.Process.Kill()
		}
	}()
	defer func() {
		if redXrayCmd != nil {
			redXrayCmd.Process.Kill()
		}
	}()
	defer func() {
		if greenXrayCmd != nil {
			greenXrayCmd.Process.Kill()
		}
	}()
}

func startGateGlider() {
	args := []string{
		"-listen", "socks5://:2580", "-strategy", "ha", "-checkinterval", "2", "-forward", "socks5://127.0.0.1:2581", "-forward", "socks5://127.0.0.1:2582",
	}
	gateGliderCmd = exec.Command("glider", args...)
	err := gateGliderCmd.Start()
	if err != nil {
		log.Fatal("Error starting glider:", err)
	}
	fmt.Println("Gate glider started.")
}

func onProxyCfgsReady() {
	//get proxies slice from allProxies map values
	proxies := []ProxyInfo{}
	for _, proxy := range allProxies {
		proxies = append(proxies, proxy)
	}

	validProxies := testByChatGpt(proxies, true)
	//用合格的proxy构建glider的forwarder

	selectedProxies := []ProxyNode{}
	for _, proxy := range validProxies {
		if proxy.Score > 2 {
			selectedProxies = append(selectedProxies, proxy)
		}
	}

	//取最多maxSelectedProxies个selectedProxy
	if len(selectedProxies) > maxSelectedProxies {
		selectedProxies = selectedProxies[len(selectedProxies)-maxSelectedProxies:]
	}

	activeRWLock.Lock()
	activeProxies = []ProxyInfo{}
	for _, proxyNode := range selectedProxies {
		activeProxies = append(activeProxies, ProxyInfo{
			Id:       proxyNode.Id,
			Port:     proxyNode.Port,
			IsOnDuty: true,
		})
	}
	activeRWLock.Unlock()

	//启动xray服务
	cmdMap := startActiveProxyXrays(selectedProxies)
	activeXrayCmdsRWLock.Lock()
	for pid, cmd := range cmdMap {
		activeXrayCmds[pid] = cmd
	}
	activeXrayCmdsRWLock.Unlock()

	//启动red green glider
	startRedGreenGliderNXrays(selectedProxies)
	//杀掉不合格的proxy
	selectProxyIds := make(map[uint32]bool)
	for _, proxy := range selectedProxies {
		selectProxyIds[proxy.Id] = true
	}

	activeXrayCmdsRWLock.Lock()
	for pid := range activeXrayCmds {
		_, ok := selectProxyIds[pid]
		if !ok {
			fmt.Printf("killing proxy xray process with cfg: ./chatgpt/%d_%d.json\n", pid, allProxies[pid].Port)
			activeXrayCmds[pid].Process.Kill()
			delete(activeXrayCmds, pid)
		}
	}
	activeXrayCmdsRWLock.Unlock()
}

func startRedGreenGliderNXrays(selectedProxies []ProxyNode) {
	gliderArgs := []string{"-strategy", "ha", "-check", "https://chat.openai.com/#expect=200"}
	for _, proxyNode := range selectedProxies {
		gliderArgs = append(gliderArgs, "-forward")
		gliderArgs = append(gliderArgs, fmt.Sprintf("socks5://127.0.0.1:%d", proxyNode.Port))
	}
	rArgs := append([]string{"-listen", "ss://AEAD_AES_128_GCM:123456@127.0.0.1:2583"}, gliderArgs...)
	gArgs := append([]string{"-listen", "ss://AEAD_AES_128_GCM:123456@127.0.0.1:2584"}, gliderArgs...)

	if redGliderCmd != nil {
		redGliderCmd.Process.Kill()
		redXrayCmd.Process.Kill()
		fmt.Println("red glider & xray process killed.")
	}
	redGliderCmd = exec.Command("glider", rArgs...)
	redXrayCmd = exec.Command("xray", "-c", "red.json")
	gerr := redGliderCmd.Start()
	xerr := redXrayCmd.Start()
	if gerr != nil || xerr != nil {
		fmt.Println("Error starting red glider and xray:", gerr, xerr)
		redGliderCmd.Process.Kill()
		redXrayCmd.Process.Kill()
		redGliderCmd = nil
		redXrayCmd = nil
	} else {
		fmt.Println("Red glider started.")
	}
	if greenGliderCmd != nil {
		time.Sleep(4 * time.Second)
		greenGliderCmd.Process.Kill()
		greenXrayCmd.Process.Kill()
		fmt.Println("green glider & xray process killed.")
	}
	greenGliderCmd = exec.Command("glider", gArgs...)
	greenXrayCmd = exec.Command("xray", "-c", "green.json")
	gerr = greenGliderCmd.Start()
	xerr = greenXrayCmd.Start()
	if gerr != nil || xerr != nil {
		fmt.Println("Error starting green glider and xray:", gerr, xerr)
		greenGliderCmd.Process.Kill()
		greenXrayCmd.Process.Kill()
		redGliderCmd = nil
		redXrayCmd = nil
	} else {
		fmt.Println("Green glider started.")
	}
}

func startActiveProxyXrays(proxies []ProxyNode) map[uint32]*exec.Cmd {
	ret := make(map[uint32]*exec.Cmd)
	for _, node := range proxies {
		activeXrayCmdsRWLock.RLock()
		_, exist := activeXrayCmds[node.Id]
		activeXrayCmdsRWLock.RUnlock()
		if exist {
			continue
		}
		fmt.Println("starting xray using cfg: ", "./chatgpt/"+node.ConfigFile)
		cmd := exec.Command("xray", "-c", "./chatgpt/"+node.ConfigFile)
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Println("Error creating StdoutPipe:", err)
			continue
		}

		err = cmd.Start()
		if err != nil {
			fmt.Println("Error starting xray:", err)
			continue
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			scanner := bufio.NewScanner(stdoutPipe)
			for scanner.Scan() {
				text := scanner.Text()
				fmt.Println(text)
				if strings.Contains(text, "Reading config") {
					//fmt.Println("Xray local service started with cfg:", node.ConfigFile)
					break
				}
			}

			if err := scanner.Err(); err != nil {
				fmt.Println("Error reading from xray process:", node.ConfigFile, err)
			}
		}()
		wg.Wait()
		fmt.Println("xray using cfg: ", "./chatgpt/"+node.ConfigFile, " started")
		ret[node.Id] = cmd
	}
	return ret
}

func testOnDutyByChatgpt(shouldTestSpeed bool) []ProxyNode {
	proxies := []ProxyInfo{}
	for _, p := range activeProxies {
		proxies = append(proxies, allProxies[p.Id])
	}
	valids := sync.Map{}
	//遍历配置文件，并测试
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrent)
	for _, proxy := range proxies {
		sem <- struct{}{} // 通过信号量控制并行度
		wg.Add(1)
		go func(cfg string, serverId uint32, originPort int) {
			defer func() {
				<-sem // 释放信号量
				wg.Done()
			}()
			speed := testChatGptConnectWithProxy(fmt.Sprintf("socks5://127.0.0.1:%d", originPort), shouldTestSpeed)
			if speed > 0 {
				//获取配置文件名
				fileName := fmt.Sprintf("%d_%d.json", serverId, originPort)
				//chatgpt可用，记录下来
				valids.Store(ProxyNode{Id: serverId, Score: speed, Port: originPort, ConfigFile: fileName}, true)
			}
		}(proxy.Cfg, proxy.Id, proxy.Port)
	}
	wg.Wait()
	//将结果排序
	nodes := []ProxyNode{}
	valids.Range(func(k, v interface{}) bool {
		nodes = append(nodes, k.(ProxyNode))
		return true
	})
	sort.Sort(ProxyNodeByScore(nodes))
	return nodes
}

func testByChatGpt(proxies []ProxyInfo, shouldTestSpeed bool) []ProxyNode {
	testingLock.Lock()
	defer testingLock.Unlock()

	valids := sync.Map{}
	//遍历配置文件，并测试
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrent)
	for _, proxy := range proxies {
		tp, _ := gTestPorts.Pop()
		testPort := tp.(int)

		//测试chatgpt连通性和速度
		sem <- struct{}{} // 通过信号量控制并行度
		wg.Add(1)

		go func(cfg string, serverId uint32, originPort int, testPort int) {
			defer func() {
				gTestPorts.Push(testPort)
				<-sem // 释放信号量
				wg.Done()
			}()
			speed := testChatGptConnect(cfg, serverId, originPort, testPort, shouldTestSpeed)
			// fmt.Println("test chatgpt connect of ", serverId, ":", originPort, ":", testPort, " returns ", speed)
			if speed > 0 {
				//获取配置文件名
				fileName := fmt.Sprintf("%d_%d.json", serverId, originPort)
				//chatgpt可用，记录下来
				valids.Store(ProxyNode{Id: serverId, Score: speed, Port: originPort, ConfigFile: fileName}, true)
				if shouldTestSpeed {
					fmt.Println("Speed of proxy port(", originPort, ") is ", speed)
				}
			}
		}(proxy.Cfg, proxy.Id, proxy.Port, testPort)
	}
	wg.Wait()
	//将结果排序
	ret := []ProxyNode{}
	valids.Range(func(k, v interface{}) bool {
		ret = append(ret, k.(ProxyNode))
		return true
	})
	sort.Sort(ProxyNodeByScore(ret))
	return ret
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/stop" && r.Method == http.MethodGet {
		password := r.URL.Query().Get("password")
		if password == "letstop" {
			fmt.Fprintln(w, "Stopping server...")
			os.Exit(0)
		} else {
			fmt.Fprintln(w, "Invalid password")
		}
	} else if r.URL.Path == "/actives" && r.Method == http.MethodGet {
		activeRWLock.RLock()
		proxies := activeProxies
		activeRWLock.RUnlock()
		// 构造响应数据
		response := struct {
			Proxies []ProxyInfo `json:"proxies"`
		}{
			Proxies: proxies,
		}

		// 将响应数据编码为 JSON 格式
		jsonData, err := json.Marshal(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// 设置响应头并写入响应数据
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	} else if r.URL.Path == "/update" && r.Method == http.MethodPost {
		shouldUpdate := false
		updateLock.Lock()
		if !updatingFlag {
			shouldUpdate = true
			updatingFlag = true
			defer func() {
				updateLock.Lock()
				updatingFlag = false
				updateLock.Unlock()
			}()
		}
		updateLock.Unlock()

		if !shouldUpdate {
			// 返回响应
			fmt.Fprintln(w, "Proxies is updating by another request, please try later")
			return
		}
		// 读取请求体数据
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// 解析请求体 JSON 数据到 []ProxyInfo 结构体
		var request struct {
			Proxies []ProxyInfo `json:"proxies"`
		}
		err = json.Unmarshal(body, &request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// 处理解析后的数据，例如存储到数据库或进行其他操作
		fmt.Println("Received proxies:", request.Proxies)

		// 返回响应
		fmt.Fprintln(w, "Proxies updated successfully")
	} else if r.URL.Path == "/lockActives" && r.Method == http.MethodGet {

	} else if r.URL.Path == "/unlockActives" && r.Method == http.MethodGet {

	} else {
		fmt.Fprintln(w, "Hello, World!")
	}
}

func monitorProxies() {
	// 创建一个 10 秒钟的定时器
	ticker10s := time.NewTicker(10 * time.Second)
	ticker10m := time.NewTicker(10 * time.Minute)
	tick10mCount := 0
	// 使用 for 循环接收定时器的时间通知
	for {
		select {
		case <-ticker10s.C:
			taskChan <- ProxyTask{Type: Task10s}
		case <-ticker10m.C:
			tick10mCount = tick10mCount + 1
			if tick10mCount%12 == 0 {
				taskChan <- ProxyTask{Type: Task2h}
			} else if tick10mCount%3 == 0 {
				taskChan <- ProxyTask{Type: Task30m}
			} else {
				taskChan <- ProxyTask{Type: Task10m}
			}
		}
	}
}

// 检查正在运行的proxy的forwarder的健康状态，如果有问题则立刻剔除掉问题节点，并发起一次ProxiesUpdated
func doTask10s() {
	if len(activeProxies) < maxSelectedProxies {
		taskChan <- ProxyTask{Type: ProxiesUpdated}
		return
	}
	//只测通不通，不测速
	nodes := testOnDutyByChatgpt(false)
	if len(nodes) < len(activeProxies) {
		//说明有问题节点，马上剔除
		validNodes := make(map[uint32]bool)
		for _, node := range nodes {
			validNodes[node.Id] = true
		}
		tmpActives := []ProxyInfo{}
		for _, p := range activeProxies {
			_, ok := validNodes[p.Id]
			if !ok {
				fmt.Println("Proxy ", p, " failed.")
				activeXrayCmdsRWLock.Lock()
				activeXrayCmds[p.Id].Process.Kill()
				delete(activeXrayCmds, p.Id)
				activeXrayCmdsRWLock.Unlock()
			} else {
				tmpActives = append(tmpActives, p)
			}
		}
		activeRWLock.Lock()
		activeProxies = tmpActives
		activeRWLock.Unlock()
		taskChan <- ProxyTask{Type: ProxiesUpdated}
	}
}

// 对正在运行的proxy进行一次测速，如果发生节点失效，则按Task10s处理，else如果发生forward优先级变化，则重新启动red/green glider进行相应调整
func doTask10m() {
	//测通，测速
	nodes := testOnDutyByChatgpt(true)
	idsLen := len(nodes)
	if idsLen < len(activeProxies) {
		//说明有问题节点，马上剔除
		validNodes := make(map[uint32]bool)
		for _, node := range nodes {
			validNodes[node.Id] = true
		}
		tmpActives := []ProxyInfo{}
		for _, p := range activeProxies {
			_, ok := validNodes[p.Id]
			if !ok {
				fmt.Println("Proxy ", p, " failed.")
				activeXrayCmdsRWLock.Lock()
				activeXrayCmds[p.Id].Process.Kill()
				delete(activeXrayCmds, p.Id)
				activeXrayCmdsRWLock.Unlock()
			} else {
				tmpActives = append(tmpActives, p)
			}
		}
		activeProxies = tmpActives
		taskChan <- ProxyTask{Type: ProxiesUpdated}
	} else if idsLen > 0 {
		//只比较在-1位置的就可以了，因为这个位置的是实际onDuty的
		if nodes[idsLen-1].Id != activeProxies[idsLen-1].Id {
			fmt.Println("The order of On duty proxies has changed.")
			//更新actives
			activeRWLock.Lock()
			activeProxies = []ProxyInfo{}
			for _, proxyNode := range nodes {
				activeProxies = append(activeProxies, ProxyInfo{
					Id:       proxyNode.Id,
					Port:     proxyNode.Port,
					IsOnDuty: true,
				})
			}
			activeRWLock.Unlock()
			//重启red and green glider
			startRedGreenGliderNXrays(nodes)
		}
	}
}

// 重新从./chatgpt目录装入配置，然后发起一次ProxiesUpdated
func doTask30m() {
	loadProxiesFromConfigDir("./chatgpt/")
	taskChan <- ProxyTask{Type: ProxiesUpdated}
}

func doTask2h() {
	w()
	finalizeOutdatedCfgs()
	taskChan <- ProxyTask{Type: ProxiesUpdated}
}

func finalizeOutdatedCfgs() {
	// 指定要删除文件的目录
	directory := "./cfgs/"

	// 获取当前时间
	now := time.Now()

	// 遍历目录中的文件
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// 只处理JSON文件
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			// 计算文件的更新时间
			updateTime := info.ModTime()

			// 计算与当前时间的差值
			duration := now.Sub(updateTime)

			// 如果文件超过12小时没有更新，则删除文件
			if duration > 12*time.Hour {
				err := os.Remove(path)
				if err != nil {
					fmt.Println("Error deleting file:", err)
				} else {
					fmt.Println("Deleted file:", path)
				}
			}
		}

		return nil
	})

	if err != nil {
		fmt.Println("Error:", err)
	}
}
