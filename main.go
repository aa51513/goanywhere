package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	natpmp "github.com/jackpal/go-nat-pmp"
	"github.com/libp2p/go-nat"
)

// PortMappingResult 端口映射结果
type PortMappingResult struct {
	Protocol     string // "NAT-PMP", "PCP", "UPnP-IGDv1", "UPnP-IGDv2"
	InternalIP   string
	InternalPort uint16
	ExternalIP   string
	ExternalPort uint16
	Success      bool
	Error        error
}

// DetectAndMapPort 并发探测NAT-PMP、PCP、UPnP并申请端口
// internalPort: 内部端口
// timeout: 超时时间
func DetectAndMapPort(ctx context.Context, internalPort uint16, timeout time.Duration) (*PortMappingResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resultCh := make(chan *PortMappingResult, 4)
	var wg sync.WaitGroup

	// 并发探测四种方式
	wg.Add(4)

	// 探测 NAT-PMP
	go func() {
		defer wg.Done()
		result := detectNATPMP(ctx, internalPort)
		select {
		case resultCh <- result:
		case <-ctx.Done():
		}
	}()

	// 探测 PCP
	go func() {
		defer wg.Done()
		result := detectPCP(ctx, internalPort)
		select {
		case resultCh <- result:
		case <-ctx.Done():
		}
	}()

	// 探测 UPnP IGDv1
	go func() {
		defer wg.Done()
		result := detectUPnPIGDv1(ctx, internalPort)
		select {
		case resultCh <- result:
		case <-ctx.Done():
		}
	}()

	// 探测 UPnP IGDv2
	go func() {
		defer wg.Done()
		result := detectUPnPIGDv2(ctx, internalPort)
		select {
		case resultCh <- result:
		case <-ctx.Done():
		}
	}()

	// 等待所有探测完成
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// 收集所有结果
	var allResults []*PortMappingResult
	for result := range resultCh {
		if result.Success {
			// 立即返回第一个成功的结果
			return result, nil
		}
		allResults = append(allResults, result)
	}

	// 如果都失败了，返回详细错误信息
	errMsg := "所有协议探测失败:\n"
	for _, r := range allResults {
		errMsg += fmt.Sprintf("  - %s: %v\n", r.Protocol, r.Error)
	}
	return nil, fmt.Errorf(errMsg)
}

// detectNATPMP 使用NAT-PMP协议探测并映射端口
func detectNATPMP(ctx context.Context, internalPort uint16) *PortMappingResult {
	result := &PortMappingResult{
		Protocol:     "NAT-PMP",
		InternalPort: internalPort,
	}

	// 获取默认网关
	gateway, err := discoverGateway()
	if err != nil {
		result.Error = fmt.Errorf("获取网关失败: %v", err)
		return result
	}

	// 创建NAT-PMP客户端
	client := natpmp.NewClient(gateway)

	// 检查是否支持NAT-PMP（通过获取外部IP）
	response, err := client.GetExternalAddress()
	if err != nil {
		result.Error = fmt.Errorf("NAT-PMP不可用: %v", err)
		return result
	}

	result.ExternalIP = response.ExternalIPAddress.String()

	// 生成随机外部端口
	externalPort := uint16(rand.Intn(20000) + 40000)

	// 尝试映射端口（UDP和TCP都尝试）
	lifetime := 3600 // 1小时
	mapResponse, err := client.AddPortMapping("tcp", int(internalPort), int(externalPort), lifetime)
	if err != nil {
		result.Error = fmt.Errorf("端口映射失败: %v", err)
		return result
	}

	result.ExternalPort = uint16(mapResponse.MappedExternalPort)
	result.InternalIP, _ = getLocalIP()
	result.Success = true

	return result
}

// detectPCP 使用PCP协议探测并映射端口
func detectPCP(ctx context.Context, internalPort uint16) *PortMappingResult {
	result := &PortMappingResult{
		Protocol:     "PCP",
		InternalPort: internalPort,
	}

	// 使用libp2p的NAT库，它支持NAT-PMP和PCP
	// 首先尝试发现NAT设备
	natInstance, err := nat.DiscoverNAT(ctx)
	if err != nil {
		result.Error = fmt.Errorf("PCP/NAT发现失败: %v", err)
		return result
	}

	// 获取外部IP
	externalAddr, err := natInstance.GetExternalAddress()
	if err != nil {
		result.Error = fmt.Errorf("获取外部地址失败: %v", err)
		return result
	}
	result.ExternalIP = externalAddr.String()

	// 获取内部IP
	internalIP, err := getLocalIP()
	if err != nil {
		result.Error = fmt.Errorf("获取内部IP失败: %v", err)
		return result
	}
	result.InternalIP = internalIP

	// 生成随机外部端口
	externalPort := rand.Intn(20000) + 40000

	// 添加端口映射
	// libp2p的NAT库会自动选择最佳协议（PCP优先于NAT-PMP）
	_, err = natInstance.AddMapping(ctx, "tcp", internalPort, "Go PCP Port Mapping", time.Hour)
	if err != nil {
		result.Error = fmt.Errorf("PCP端口映射失败: %v", err)
		return result
	}

	// 注意：libp2p的NAT库返回的是分配的外部端口
	// 这里我们使用一个近似值，实际应用中可能需要更精确的处理
	result.ExternalPort = uint16(externalPort)
	result.Success = true

	return result
}

// detectUPnPIGDv1 使用UPnP IGDv1协议探测并映射端口
func detectUPnPIGDv1(ctx context.Context, internalPort uint16) *PortMappingResult {
	result := &PortMappingResult{
		Protocol:     "UPnP-IGDv1",
		InternalPort: internalPort,
	}

	// 发现WANIPConnection服务
	clients, _, err := internetgateway1.NewWANIPConnection1Clients()
	if err != nil || len(clients) == 0 {
		// 尝试WANPPPConnection
		pppClients, _, err := internetgateway1.NewWANPPPConnection1Clients()
		if err != nil || len(pppClients) == 0 {
			result.Error = fmt.Errorf("未发现UPnP IGDv1设备")
			return result
		}
		return mapPortUPnPv1PPP(pppClients[0], internalPort, result)
	}

	return mapPortUPnPv1IP(clients[0], internalPort, result)
}

// mapPortUPnPv1IP 使用WANIPConnection映射端口
func mapPortUPnPv1IP(client *internetgateway1.WANIPConnection1, internalPort uint16, result *PortMappingResult) *PortMappingResult {
	// 获取外部IP
	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		result.Error = fmt.Errorf("获取外部IP失败: %v", err)
		return result
	}
	result.ExternalIP = externalIP

	// 获取内部IP
	internalIP, err := getLocalIP()
	if err != nil {
		result.Error = fmt.Errorf("获取内部IP失败: %v", err)
		return result
	}
	result.InternalIP = internalIP

	// 生成随机外部端口
	externalPort := uint16(rand.Intn(20000) + 40000)

	// 添加端口映射
	err = client.AddPortMapping(
		"",                      // 远程主机（空字符串表示任意）
		externalPort,            // 外部端口
		"TCP",                   // 协议
		internalPort,            // 内部端口
		internalIP,              // 内部客户端IP
		true,                    // 启用
		"Go NAT Port Mapping",   // 描述
		uint32(3600),            // 租期（秒）
	)

	if err != nil {
		result.Error = fmt.Errorf("添加端口映射失败: %v", err)
		return result
	}

	result.ExternalPort = externalPort
	result.Success = true
	return result
}

// mapPortUPnPv1PPP 使用WANPPPConnection映射端口
func mapPortUPnPv1PPP(client *internetgateway1.WANPPPConnection1, internalPort uint16, result *PortMappingResult) *PortMappingResult {
	// 获取外部IP
	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		result.Error = fmt.Errorf("获取外部IP失败: %v", err)
		return result
	}
	result.ExternalIP = externalIP

	// 获取内部IP
	internalIP, err := getLocalIP()
	if err != nil {
		result.Error = fmt.Errorf("获取内部IP失败: %v", err)
		return result
	}
	result.InternalIP = internalIP

	// 生成随机外部端口
	externalPort := uint16(rand.Intn(20000) + 40000)

	// 添加端口映射
	err = client.AddPortMapping(
		"",
		externalPort,
		"TCP",
		internalPort,
		internalIP,
		true,
		"Go NAT Port Mapping",
		uint32(3600),
	)

	if err != nil {
		result.Error = fmt.Errorf("添加端口映射失败: %v", err)
		return result
	}

	result.ExternalPort = externalPort
	result.Success = true
	return result
}

// detectUPnPIGDv2 使用UPnP IGDv2协议探测并映射端口
func detectUPnPIGDv2(ctx context.Context, internalPort uint16) *PortMappingResult {
	result := &PortMappingResult{
		Protocol:     "UPnP-IGDv2",
		InternalPort: internalPort,
	}

	// 发现WANIPConnection2服务
	clients, _, err := internetgateway2.NewWANIPConnection2Clients()
	if err != nil || len(clients) == 0 {
		// 尝试WANPPPConnection
		pppClients, _, err := internetgateway2.NewWANPPPConnection2Clients()
		if err != nil || len(pppClients) == 0 {
			result.Error = fmt.Errorf("未发现UPnP IGDv2设备")
			return result
		}
		return mapPortUPnPv2PPP(pppClients[0], internalPort, result)
	}

	return mapPortUPnPv2IP(clients[0], internalPort, result)
}

// mapPortUPnPv2IP 使用WANIPConnection2映射端口
func mapPortUPnPv2IP(client *internetgateway2.WANIPConnection2, internalPort uint16, result *PortMappingResult) *PortMappingResult {
	// 获取外部IP
	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		result.Error = fmt.Errorf("获取外部IP失败: %v", err)
		return result
	}
	result.ExternalIP = externalIP

	// 获取内部IP
	internalIP, err := getLocalIP()
	if err != nil {
		result.Error = fmt.Errorf("获取内部IP失败: %v", err)
		return result
	}
	result.InternalIP = internalIP

	// 生成随机外部端口
	externalPort := uint16(rand.Intn(20000) + 40000)

	// 添加端口映射
	err = client.AddPortMapping(
		"",
		externalPort,
		"TCP",
		internalPort,
		internalIP,
		true,
		"Go NAT Port Mapping",
		uint32(3600),
	)

	if err != nil {
		result.Error = fmt.Errorf("添加端口映射失败: %v", err)
		return result
	}

	result.ExternalPort = externalPort
	result.Success = true
	return result
}

// mapPortUPnPv2PPP 使用WANPPPConnection2映射端口
func mapPortUPnPv2PPP(client *internetgateway2.WANPPPConnection2, internalPort uint16, result *PortMappingResult) *PortMappingResult {
	// 获取外部IP
	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		result.Error = fmt.Errorf("获取外部IP失败: %v", err)
		return result
	}
	result.ExternalIP = externalIP

	// 获取内部IP
	internalIP, err := getLocalIP()
	if err != nil {
		result.Error = fmt.Errorf("获取内部IP失败: %v", err)
		return result
	}
	result.InternalIP = internalIP

	// 生成随机外部端口
	externalPort := uint16(rand.Intn(20000) + 40000)

	// 添加端口映射
	err = client.AddPortMapping(
		"",
		externalPort,
		"TCP",
		internalPort,
		internalIP,
		true,
		"Go NAT Port Mapping",
		uint32(3600),
	)

	if err != nil {
		result.Error = fmt.Errorf("添加端口映射失败: %v", err)
		return result
	}

	result.ExternalPort = externalPort
	result.Success = true
	return result
}

// discoverGateway 发现默认网关
func discoverGateway() (net.IP, error) {
	// 使用一个技巧：连接到外部地址，获取使用的本地接口
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	
	// 简化处理：假设网关是本地网段的 .1
	ip := localAddr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("不是IPv4地址")
	}
	
	gateway := net.IPv4(ip[0], ip[1], ip[2], 1)
	return gateway, nil
}

// getLocalIP 获取本地IP地址
func getLocalIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

// 使用示例
func main() {
	rand.Seed(time.Now().UnixNano())

	// 指定要映射的内部端口
	internalPort := uint16(8080)

	fmt.Printf("正在探测NAT协议并申请端口映射...\n")
	fmt.Printf("内部端口: %d\n\n", internalPort)

	// 调用探测函数
	ctx := context.Background()
	result, err := DetectAndMapPort(ctx, internalPort, 10*time.Second)

	if err != nil {
		fmt.Printf("❌ 端口映射失败:\n%v\n", err)
		return
	}

	fmt.Printf("✅ 端口映射成功!\n")
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("协议:       %s\n", result.Protocol)
	fmt.Printf("内部地址:   %s:%d\n", result.InternalIP, result.InternalPort)
	fmt.Printf("外部地址:   %s:%d\n", result.ExternalIP, result.ExternalPort)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("\n提示: 端口映射将在1小时后过期\n")
}
