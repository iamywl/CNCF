// SPDX-License-Identifier: Apache-2.0
// Cilium Cloud Provider Integration PoC
//
// 이 프로그램은 Cilium의 클라우드 프로바이더 통합 메커니즘을 시뮬레이션한다.
// AWS ENI, Azure NIC, Alibaba Cloud ENI의 IPAM 동작을 순수 Go 표준 라이브러리만으로 구현한다.
//
// 주요 시뮬레이션 항목:
// 1. AWS ENI 라이프사이클: ENI 생성 -> 인스턴스 부착 -> 보조 IP 할당 -> Pod 할당
// 2. Azure NIC: NIC에 IP Configuration 추가 -> 서브넷 관리
// 3. 클라우드 메타데이터 서비스: instance-id, region, vpc-id, subnet-id 조회
// 4. Rate limiting 및 API throttling 처리
// 5. 프로바이더별 할당 패턴 비교

package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// 공통 타입 정의 (pkg/ipam/types 참조)
// =============================================================================

// AllocationMap은 IP -> 할당 정보 매핑
type AllocationMap map[string]AllocationIP

// AllocationIP는 IP 할당 정보
type AllocationIP struct {
	Resource string // ENI/NIC ID
}

// Subnet은 서브넷 정보
type Subnet struct {
	ID                 string
	Name               string
	CIDR               string
	AvailabilityZone   string
	VirtualNetworkID   string
	AvailableAddresses int
	Tags               map[string]string
}

// SecurityGroup은 보안 그룹 정보
type SecurityGroup struct {
	ID    string
	VpcID string
	Tags  map[string]string
}

// Limits는 인스턴스 타입별 제한
type Limits struct {
	Adapters       int // 최대 ENI/NIC 수
	IPv4           int // ENI/NIC당 최대 IPv4 수
	HypervisorType string
}

// InstanceTypeLimits는 인스턴스 타입별 제한 매핑
var InstanceTypeLimits = map[string]Limits{
	// AWS
	"t3.micro":   {Adapters: 2, IPv4: 2, HypervisorType: "nitro"},
	"t3.small":   {Adapters: 3, IPv4: 4, HypervisorType: "nitro"},
	"m5.large":   {Adapters: 3, IPv4: 10, HypervisorType: "nitro"},
	"m5.xlarge":  {Adapters: 4, IPv4: 15, HypervisorType: "nitro"},
	"c5.2xlarge": {Adapters: 4, IPv4: 15, HypervisorType: "nitro"},
	// Alibaba Cloud
	"ecs.g6.large":   {Adapters: 3, IPv4: 6},
	"ecs.g6.xlarge":  {Adapters: 4, IPv4: 10},
	"ecs.g6.2xlarge": {Adapters: 4, IPv4: 10},
}

// =============================================================================
// Rate Limiter (pkg/api/helpers/rate_limiter.go 참조)
// =============================================================================

// APILimiter는 클라우드 API 호출 속도 제한기
type APILimiter struct {
	mu          sync.Mutex
	tokens      float64
	maxTokens   float64
	refillRate  float64
	lastRefill  time.Time
	totalCalls  int64
	throttled   int64
	metrics     map[string]*APIMetric
}

// APIMetric은 API 호출 메트릭
type APIMetric struct {
	CallCount   int64
	TotalTime   time.Duration
	ErrorCount  int64
}

// NewAPILimiter는 새 rate limiter를 생성한다
func NewAPILimiter(rateLimit float64, burst int) *APILimiter {
	return &APILimiter{
		tokens:     float64(burst),
		maxTokens:  float64(burst),
		refillRate: rateLimit,
		lastRefill: time.Now(),
		metrics:    make(map[string]*APIMetric),
	}
}

// Limit는 API 호출 전에 속도 제한을 적용한다
func (l *APILimiter) Limit(ctx context.Context, operation string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 토큰 리필
	now := time.Now()
	elapsed := now.Sub(l.lastRefill).Seconds()
	l.tokens += elapsed * l.refillRate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
	l.lastRefill = now

	// 토큰 소비
	if l.tokens < 1 {
		atomic.AddInt64(&l.throttled, 1)
		waitTime := time.Duration((1 - l.tokens) / l.refillRate * float64(time.Second))
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			l.mu.Lock()
			return ctx.Err()
		case <-time.After(waitTime):
		}

		l.mu.Lock()
		l.tokens = 0
	} else {
		l.tokens--
	}

	atomic.AddInt64(&l.totalCalls, 1)

	// 메트릭 업데이트
	if _, ok := l.metrics[operation]; !ok {
		l.metrics[operation] = &APIMetric{}
	}
	l.metrics[operation].CallCount++

	return nil
}

// ObserveAPICall은 API 호출 결과를 기록한다
func (l *APILimiter) ObserveAPICall(operation, status string, duration time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.metrics[operation]; !ok {
		l.metrics[operation] = &APIMetric{}
	}
	l.metrics[operation].TotalTime += duration
	if status != "OK" {
		l.metrics[operation].ErrorCount++
	}
}

// PrintMetrics는 수집된 메트릭을 출력한다
func (l *APILimiter) PrintMetrics() {
	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Printf("    Rate Limiter 통계: 총 호출=%d, 스로틀링=%d\n",
		atomic.LoadInt64(&l.totalCalls), atomic.LoadInt64(&l.throttled))
	for op, m := range l.metrics {
		avgTime := time.Duration(0)
		if m.CallCount > 0 {
			avgTime = m.TotalTime / time.Duration(m.CallCount)
		}
		fmt.Printf("    - %-35s: 호출=%d, 평균지연=%v, 에러=%d\n",
			op, m.CallCount, avgTime, m.ErrorCount)
	}
}

// =============================================================================
// 메타데이터 서비스 시뮬레이션 (pkg/aws/metadata, pkg/azure/api, pkg/alibabacloud/metadata 참조)
// =============================================================================

// CloudMetadata는 클라우드 메타데이터 정보
type CloudMetadata struct {
	InstanceID       string
	InstanceType     string
	Region           string
	AvailabilityZone string
	VPCID            string
	SubnetID         string
	// Azure 전용
	SubscriptionID    string
	ResourceGroupName string
	CloudEnvironment  string
}

// MetadataService는 클라우드 메타데이터 서비스를 시뮬레이션한다
type MetadataService struct {
	provider string
	endpoint string
	data     CloudMetadata
	latency  time.Duration
}

// NewAWSMetadataService는 AWS IMDS를 시뮬레이션한다
// 참조: pkg/aws/metadata/metadata.go
func NewAWSMetadataService() *MetadataService {
	return &MetadataService{
		provider: "AWS",
		endpoint: "http://169.254.169.254/latest/meta-data",
		data: CloudMetadata{
			InstanceID:       "i-0abcdef1234567890",
			InstanceType:     "m5.large",
			Region:           "us-west-2",
			AvailabilityZone: "us-west-2a",
			VPCID:            "vpc-0abc1234def567890",
			SubnetID:         "subnet-0abc1234",
		},
		latency: 2 * time.Millisecond,
	}
}

// NewAzureMetadataService는 Azure IMDS를 시뮬레이션한다
// 참조: pkg/azure/api/metadata.go
func NewAzureMetadataService() *MetadataService {
	return &MetadataService{
		provider: "Azure",
		endpoint: "http://169.254.169.254/metadata",
		data: CloudMetadata{
			InstanceID:        "vm-azure-node-001",
			InstanceType:      "Standard_D4s_v3",
			Region:            "westus2",
			AvailabilityZone:  "westus2-1",
			VPCID:             "vnet-cilium-prod",
			SubnetID:          "subnet-pods",
			SubscriptionID:    "12345678-1234-1234-1234-123456789012",
			ResourceGroupName: "rg-cilium-prod",
			CloudEnvironment:  "AzurePublicCloud",
		},
		latency: 3 * time.Millisecond,
	}
}

// NewAlibabaMetadataService는 Alibaba Cloud 메타데이터 서비스를 시뮬레이션한다
// 참조: pkg/alibabacloud/metadata/metadata.go
func NewAlibabaMetadataService() *MetadataService {
	return &MetadataService{
		provider: "AlibabaCloud",
		endpoint: "http://100.100.100.200/latest/meta-data",
		data: CloudMetadata{
			InstanceID:       "i-bp1abc1234567890xyz",
			InstanceType:     "ecs.g6.2xlarge",
			Region:           "cn-shanghai",
			AvailabilityZone: "cn-shanghai-b",
			VPCID:            "vpc-bp1abc1234567890",
			SubnetID:         "vsw-bp1abc1234567890",
		},
		latency: 2 * time.Millisecond,
	}
}

// GetMetadata는 메타데이터를 경로별로 조회한다
func (m *MetadataService) GetMetadata(path string) (string, error) {
	// 실제 IMDS 네트워크 지연 시뮬레이션
	time.Sleep(m.latency)

	switch path {
	case "instance-id":
		return m.data.InstanceID, nil
	case "instance-type", "instance/instance-type":
		return m.data.InstanceType, nil
	case "placement/availability-zone", "zone-id":
		return m.data.AvailabilityZone, nil
	case "region-id":
		return m.data.Region, nil
	case "vpc-id":
		return m.data.VPCID, nil
	case "vpc-cidr-block":
		return "10.0.0.0/16", nil
	// Azure 전용
	case "instance/compute/subscriptionId":
		return m.data.SubscriptionID, nil
	case "instance/compute/resourceGroupName":
		return m.data.ResourceGroupName, nil
	case "instance/compute/azEnvironment":
		return m.data.CloudEnvironment, nil
	default:
		// MAC 주소 기반 경로 (AWS)
		if strings.Contains(path, "mac") {
			return "0a:1b:2c:3d:4e:5f", nil
		}
		if strings.Contains(path, "vpc-id") {
			return m.data.VPCID, nil
		}
		if strings.Contains(path, "subnet-id") {
			return m.data.SubnetID, nil
		}
		return "", fmt.Errorf("metadata path not found: %s", path)
	}
}

// =============================================================================
// AWS ENI 시뮬레이션 (pkg/aws/ec2, pkg/aws/eni 참조)
// =============================================================================

// AWSENI는 AWS Elastic Network Interface를 나타낸다
// 참조: pkg/aws/eni/types/types.go
type AWSENI struct {
	ID               string
	PrimaryIP        string
	MAC              string
	SubnetID         string
	VPCID            string
	SecurityGroups   []string
	Addresses        []string // 보조 IP 목록
	Prefixes         []string // /28 프리픽스 목록
	Number           int      // 디바이스 인덱스
	Description      string
	AttachmentID     string
	Tags             map[string]string
}

// AWSENIManager는 AWS ENI IPAM을 시뮬레이션한다
// 참조: pkg/aws/eni/instances.go, pkg/aws/ec2/ec2.go
type AWSENIManager struct {
	mu                sync.RWMutex
	instanceID        string
	instanceType      string
	vpcID             string
	availabilityZone  string
	enis              map[string]*AWSENI
	subnets           map[string]*Subnet
	securityGroups    map[string]*SecurityGroup
	limiter           *APILimiter
	nextENIID         int
	nextIPSuffix      int
	prefixDelegation  bool
}

// NewAWSENIManager는 새 AWS ENI 매니저를 생성한다
func NewAWSENIManager(meta *MetadataService, prefixDelegation bool) *AWSENIManager {
	instanceID, _ := meta.GetMetadata("instance-id")
	instanceType, _ := meta.GetMetadata("instance-type")
	vpcID, _ := meta.GetMetadata("vpc-id")
	az, _ := meta.GetMetadata("placement/availability-zone")

	m := &AWSENIManager{
		instanceID:       instanceID,
		instanceType:     instanceType,
		vpcID:            vpcID,
		availabilityZone: az,
		enis:             make(map[string]*AWSENI),
		subnets:          make(map[string]*Subnet),
		securityGroups:   make(map[string]*SecurityGroup),
		limiter:          NewAPILimiter(10.0, 5), // 초당 10회, 버스트 5
		nextIPSuffix:     10,
		prefixDelegation: prefixDelegation,
	}

	// 기본 서브넷 설정
	m.subnets["subnet-0abc1234"] = &Subnet{
		ID: "subnet-0abc1234", Name: "pod-subnet-a",
		CIDR: "10.0.1.0/24", AvailabilityZone: az,
		VirtualNetworkID: vpcID, AvailableAddresses: 250,
	}
	m.subnets["subnet-0def5678"] = &Subnet{
		ID: "subnet-0def5678", Name: "pod-subnet-b",
		CIDR: "10.0.2.0/24", AvailabilityZone: az,
		VirtualNetworkID: vpcID, AvailableAddresses: 200,
	}

	// 기본 보안 그룹 설정
	m.securityGroups["sg-0abc1234"] = &SecurityGroup{
		ID: "sg-0abc1234", VpcID: vpcID,
		Tags: map[string]string{"kubernetes.io/cluster/my-cluster": "owned"},
	}

	// eth0 (기본 ENI) 추가
	m.enis["eni-0primary"] = &AWSENI{
		ID: "eni-0primary", PrimaryIP: "10.0.1.5",
		MAC: "0a:1b:2c:3d:4e:5f", SubnetID: "subnet-0abc1234",
		VPCID: vpcID, SecurityGroups: []string{"sg-0abc1234"},
		Number: 0, Description: "Primary ENI",
	}

	return m
}

// CreateNetworkInterface는 새 ENI를 생성한다
// 참조: pkg/aws/ec2/ec2.go - CreateNetworkInterface
func (m *AWSENIManager) CreateNetworkInterface(ctx context.Context, toAllocate int, subnetID string,
	securityGroups []string, allocatePrefixes bool) (string, *AWSENI, error) {

	if err := m.limiter.Limit(ctx, "CreateNetworkInterface"); err != nil {
		return "", nil, err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("CreateNetworkInterface", "OK", time.Since(start))
	}()

	// API 호출 지연 시뮬레이션
	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	// 인스턴스 타입 제한 확인
	limits, ok := InstanceTypeLimits[m.instanceType]
	if !ok {
		return "", nil, fmt.Errorf("unknown instance type: %s", m.instanceType)
	}
	if len(m.enis) >= limits.Adapters {
		return "", nil, fmt.Errorf("ENI limit reached: %d/%d", len(m.enis), limits.Adapters)
	}

	m.nextENIID++
	eniID := fmt.Sprintf("eni-%012d", m.nextENIID)

	subnet := m.subnets[subnetID]
	if subnet == nil {
		return "", nil, fmt.Errorf("subnet not found: %s", subnetID)
	}

	eni := &AWSENI{
		ID:             eniID,
		PrimaryIP:      fmt.Sprintf("10.0.1.%d", m.nextIPSuffix),
		MAC:            generateMAC(),
		SubnetID:       subnetID,
		VPCID:          m.vpcID,
		SecurityGroups: securityGroups,
		Description:    fmt.Sprintf("Cilium-CNI (%s)", m.instanceID),
		Tags:           map[string]string{"cilium-managed": "true"},
	}
	m.nextIPSuffix++

	// 보조 IP 또는 프리픽스 할당
	if allocatePrefixes && limits.HypervisorType == "nitro" {
		numPrefixes := (toAllocate + 15) / 16 // /28 프리픽스 = 16 IPs
		for i := 0; i < numPrefixes; i++ {
			prefix := fmt.Sprintf("10.0.1.%d/28", m.nextIPSuffix)
			eni.Prefixes = append(eni.Prefixes, prefix)
			// 프리픽스 내 개별 IP 추가
			for j := 0; j < 16 && len(eni.Addresses) < toAllocate; j++ {
				eni.Addresses = append(eni.Addresses, fmt.Sprintf("10.0.1.%d", m.nextIPSuffix+j))
			}
			m.nextIPSuffix += 16
		}
	} else {
		for i := 0; i < toAllocate; i++ {
			eni.Addresses = append(eni.Addresses, fmt.Sprintf("10.0.1.%d", m.nextIPSuffix))
			m.nextIPSuffix++
		}
	}

	subnet.AvailableAddresses -= len(eni.Addresses) + 1 // +1 for primary
	return eniID, eni, nil
}

// AttachNetworkInterface는 ENI를 인스턴스에 부착한다
// 참조: pkg/aws/ec2/ec2.go - AttachNetworkInterface
func (m *AWSENIManager) AttachNetworkInterface(ctx context.Context, index int, eniID string) (string, error) {
	if err := m.limiter.Limit(ctx, "AttachNetworkInterface"); err != nil {
		return "", err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("AttachNetworkInterface", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(30+rand.Intn(70)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	eni, ok := m.enis[eniID]
	if !ok {
		// 새로 생성된 ENI를 등록
		return "", fmt.Errorf("ENI not found: %s (will be added after create)", eniID)
	}

	// 인덱스 충돌 확인
	for _, existingENI := range m.enis {
		if existingENI.Number == index {
			return "", fmt.Errorf("device index %d already in use (attachment conflict)", index)
		}
	}

	eni.Number = index
	attachmentID := fmt.Sprintf("eni-attach-%s-%d", eniID[4:], index)
	eni.AttachmentID = attachmentID

	return attachmentID, nil
}

// AssignPrivateIpAddresses는 기존 ENI에 보조 IP를 할당한다
// 참조: pkg/aws/ec2/ec2.go - AssignPrivateIpAddresses
func (m *AWSENIManager) AssignPrivateIpAddresses(ctx context.Context, eniID string, count int) ([]string, error) {
	if err := m.limiter.Limit(ctx, "AssignPrivateIpAddresses"); err != nil {
		return nil, err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("AssignPrivateIpAddresses", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(20+rand.Intn(50)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	eni, ok := m.enis[eniID]
	if !ok {
		return nil, fmt.Errorf("ENI not found: %s", eniID)
	}

	limits := InstanceTypeLimits[m.instanceType]
	available := limits.IPv4 - len(eni.Addresses) - 1 // -1 for primary
	if count > available {
		return nil, fmt.Errorf("requested %d IPs but only %d available on ENI", count, available)
	}

	assigned := make([]string, count)
	for i := 0; i < count; i++ {
		ip := fmt.Sprintf("10.0.1.%d", m.nextIPSuffix)
		eni.Addresses = append(eni.Addresses, ip)
		assigned[i] = ip
		m.nextIPSuffix++
	}

	return assigned, nil
}

// UnassignPrivateIpAddresses는 보조 IP를 해제한다
// 참조: pkg/aws/ec2/ec2.go - UnassignPrivateIpAddresses
func (m *AWSENIManager) UnassignPrivateIpAddresses(ctx context.Context, eniID string, addresses []string) error {
	if err := m.limiter.Limit(ctx, "UnassignPrivateIpAddresses"); err != nil {
		return err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("UnassignPrivateIpAddresses", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(20+rand.Intn(30)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	eni, ok := m.enis[eniID]
	if !ok {
		return fmt.Errorf("ENI not found: %s", eniID)
	}

	toRemove := make(map[string]bool)
	for _, addr := range addresses {
		toRemove[addr] = true
	}

	newAddresses := make([]string, 0, len(eni.Addresses))
	for _, addr := range eni.Addresses {
		if !toRemove[addr] {
			newAddresses = append(newAddresses, addr)
		}
	}
	eni.Addresses = newAddresses

	return nil
}

// RunENILifecycle는 AWS ENI 전체 라이프사이클을 시뮬레이션한다
func (m *AWSENIManager) RunENILifecycle(ctx context.Context) {
	fmt.Println("\n========================================")
	fmt.Println("  AWS ENI 라이프사이클 시뮬레이션")
	fmt.Println("========================================")
	fmt.Printf("  인스턴스: %s (%s)\n", m.instanceID, m.instanceType)
	fmt.Printf("  VPC: %s, AZ: %s\n", m.vpcID, m.availabilityZone)

	limits := InstanceTypeLimits[m.instanceType]
	fmt.Printf("  제한: ENI %d개, IP/ENI %d개\n", limits.Adapters, limits.IPv4)
	if m.prefixDelegation {
		fmt.Println("  프리픽스 위임: 활성화 (/28 = 16 IPs)")
	}
	fmt.Println()

	// Step 1: 보안 그룹 결정 (3단계 폴백)
	fmt.Println("  [Step 1] 보안 그룹 결정 (3단계 폴백)")
	sgIDs := m.getSecurityGroupIDs()
	fmt.Printf("    -> 보안 그룹: %v (eth0에서 상속)\n\n", sgIDs)

	// Step 2: 서브넷 선택
	fmt.Println("  [Step 2] 적합한 서브넷 선택")
	bestSubnet := m.findBestSubnet()
	fmt.Printf("    -> 서브넷: %s (%s), 가용 IP: %d\n\n",
		bestSubnet.ID, bestSubnet.CIDR, bestSubnet.AvailableAddresses)

	// Step 3: ENI 생성
	fmt.Println("  [Step 3] ENI 생성")
	toAllocate := min(limits.IPv4-1, 5) // 초기 할당 수
	eniID, eni, err := m.CreateNetworkInterface(ctx, toAllocate, bestSubnet.ID, sgIDs, m.prefixDelegation)
	if err != nil {
		fmt.Printf("    [ERROR] ENI 생성 실패: %v\n", err)
		return
	}

	// 생성된 ENI를 매니저에 등록
	m.mu.Lock()
	m.enis[eniID] = eni
	m.mu.Unlock()

	fmt.Printf("    -> ENI 생성: %s (Primary IP: %s)\n", eniID, eni.PrimaryIP)
	if len(eni.Prefixes) > 0 {
		fmt.Printf("    -> 할당된 프리픽스: %v\n", eni.Prefixes)
	}
	fmt.Printf("    -> 초기 보조 IP: %v\n\n", eni.Addresses)

	// Step 4: ENI 부착 (재시도 로직 포함)
	fmt.Println("  [Step 4] ENI를 인스턴스에 부착 (재시도 로직)")
	maxRetries := 5
	var attachmentID string
	index := 1 // FirstInterfaceIndex
	for attempt := 0; attempt < maxRetries; attempt++ {
		attachmentID, err = m.AttachNetworkInterface(ctx, index, eniID)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "attachment conflict") {
			fmt.Printf("    인덱스 %d 충돌, 다음 인덱스 시도...\n", index)
			index++
			continue
		}
		// 새 ENI이므로 직접 설정
		m.mu.Lock()
		eni.Number = index
		eni.AttachmentID = fmt.Sprintf("eni-attach-%d", index)
		attachmentID = eni.AttachmentID
		m.mu.Unlock()
		break
	}
	fmt.Printf("    -> 부착 성공: %s (인덱스: %d)\n\n", attachmentID, index)

	// Step 5: 추가 보조 IP 할당
	fmt.Println("  [Step 5] 추가 보조 IP 할당")
	additionalIPs, err := m.AssignPrivateIpAddresses(ctx, eniID, 3)
	if err != nil {
		fmt.Printf("    [ERROR] IP 할당 실패: %v\n", err)
	} else {
		fmt.Printf("    -> 추가 할당된 IP: %v\n\n", additionalIPs)
	}

	// Step 6: Pod에 IP 할당 시뮬레이션
	fmt.Println("  [Step 6] Pod에 IP 할당")
	m.mu.RLock()
	allIPs := make([]string, len(eni.Addresses))
	copy(allIPs, eni.Addresses)
	m.mu.RUnlock()

	podAllocations := make(map[string]string)
	for i, ip := range allIPs {
		if i >= 4 {
			break
		}
		podName := fmt.Sprintf("nginx-deployment-%d", i)
		podAllocations[podName] = ip
		fmt.Printf("    Pod %-25s -> IP: %s (ENI: %s)\n", podName, ip, eniID)
	}
	fmt.Println()

	// Step 7: IP 해제 (Pod 종료 시)
	fmt.Println("  [Step 7] Pod 종료 시 IP 해제")
	if len(additionalIPs) > 0 {
		releaseIPs := additionalIPs[:1]
		err = m.UnassignPrivateIpAddresses(ctx, eniID, releaseIPs)
		if err != nil {
			fmt.Printf("    [ERROR] IP 해제 실패: %v\n", err)
		} else {
			fmt.Printf("    -> 해제된 IP: %v\n\n", releaseIPs)
		}
	}

	// Step 8: 최종 상태
	fmt.Println("  [Step 8] 최종 ENI 상태")
	m.mu.RLock()
	for id, e := range m.enis {
		fmt.Printf("    ENI: %s (인덱스: %d)\n", id, e.Number)
		fmt.Printf("      Primary IP: %s\n", e.PrimaryIP)
		fmt.Printf("      보조 IP 수: %d\n", len(e.Addresses))
		if len(e.SecurityGroups) > 0 {
			fmt.Printf("      보안 그룹: %v\n", e.SecurityGroups)
		}
	}
	m.mu.RUnlock()

	fmt.Println("\n  API 메트릭:")
	m.limiter.PrintMetrics()
}

func (m *AWSENIManager) getSecurityGroupIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// eth0의 보안 그룹 상속 (3순위 폴백)
	for _, eni := range m.enis {
		if eni.Number == 0 {
			return eni.SecurityGroups
		}
	}
	return nil
}

func (m *AWSENIManager) findBestSubnet() *Subnet {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var best *Subnet
	for _, s := range m.subnets {
		if s.AvailabilityZone == m.availabilityZone && s.VirtualNetworkID == m.vpcID {
			if best == nil || s.AvailableAddresses > best.AvailableAddresses {
				best = s
			}
		}
	}
	return best
}

// =============================================================================
// Azure NIC 시뮬레이션 (pkg/azure/ipam, pkg/azure/api 참조)
// =============================================================================

// AzureNIC는 Azure Network Interface를 나타낸다
// 참조: pkg/azure/types/types.go
type AzureNIC struct {
	ID            string
	Name          string
	MAC           string
	Addresses     []AzureAddress
	SecurityGroup string
	Gateway       string
	CIDR          string
	VMSSName      string // VMSS 이름 (있으면 VMSS VM)
	VMID          string
	ResourceGroup string
}

// AzureAddress는 Azure IP 구성을 나타낸다
type AzureAddress struct {
	IP     string
	Subnet string
	State  string // "succeeded" or "provisioning"
}

// AzureNICManager는 Azure NIC IPAM을 시뮬레이션한다
// 참조: pkg/azure/ipam/instances.go, pkg/azure/ipam/node.go
type AzureNICManager struct {
	mu             sync.RWMutex
	instanceID     string
	subscriptionID string
	resourceGroup  string
	nics           map[string]*AzureNIC
	subnets        map[string]*Subnet
	limiter        *APILimiter
	nextIPSuffix   int
	isVMSS         bool
	vmssName       string
}

// NewAzureNICManager는 새 Azure NIC 매니저를 생성한다
func NewAzureNICManager(meta *MetadataService, isVMSS bool) *AzureNICManager {
	instanceID, _ := meta.GetMetadata("instance-id")
	subscriptionID, _ := meta.GetMetadata("instance/compute/subscriptionId")
	resourceGroup, _ := meta.GetMetadata("instance/compute/resourceGroupName")

	m := &AzureNICManager{
		instanceID:     instanceID,
		subscriptionID: subscriptionID,
		resourceGroup:  resourceGroup,
		nics:           make(map[string]*AzureNIC),
		subnets:        make(map[string]*Subnet),
		limiter:        NewAPILimiter(8.0, 4),
		nextIPSuffix:   20,
		isVMSS:         isVMSS,
		vmssName:       "vmss-cilium-nodes",
	}

	// 기본 서브넷
	m.subnets["subnet-pods"] = &Subnet{
		ID: "subnet-pods", Name: "pod-subnet",
		CIDR: "10.1.0.0/16", VirtualNetworkID: "vnet-cilium-prod",
		AvailableAddresses: 65000,
	}

	// 기본 NIC 추가
	nicID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/nic-primary",
		subscriptionID, resourceGroup)
	m.nics["nic-primary"] = &AzureNIC{
		ID:            nicID,
		Name:          "nic-primary",
		MAC:           generateMAC(),
		SecurityGroup: "nsg-cilium-nodes",
		Gateway:       "10.1.0.1",
		CIDR:          "10.1.0.0/16",
		ResourceGroup: resourceGroup,
		Addresses: []AzureAddress{
			{IP: "10.1.0.10", Subnet: "subnet-pods", State: "succeeded"},
		},
	}
	if isVMSS {
		m.nics["nic-primary"].VMSSName = m.vmssName
		m.nics["nic-primary"].VMID = "0"
	}

	return m
}

// AssignPrivateIpAddresses는 기존 NIC에 IP Configuration을 추가한다
// Azure에서는 CreateInterface가 미구현이므로 기존 NIC에만 IP를 추가
// 참조: pkg/azure/ipam/node.go - AllocateIPs
func (m *AzureNICManager) AssignPrivateIpAddresses(ctx context.Context, subnetID, nicName string, count int) error {
	opName := "Interfaces.CreateOrUpdate"
	if m.isVMSS {
		opName = "VirtualMachineScaleSetVMs.Update"
	}

	if err := m.limiter.Limit(ctx, opName); err != nil {
		return err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall(opName, "OK", time.Since(start))
	}()

	// Azure API 호출 지연은 보통 더 김
	time.Sleep(time.Duration(100+rand.Intn(200)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	nic, ok := m.nics[nicName]
	if !ok {
		return fmt.Errorf("NIC not found: %s", nicName)
	}

	// InterfaceAddressLimit = 256
	const maxAddresses = 256
	if len(nic.Addresses)+count > maxAddresses {
		return fmt.Errorf("address limit exceeded: %d + %d > %d",
			len(nic.Addresses), count, maxAddresses)
	}

	for i := 0; i < count; i++ {
		addr := AzureAddress{
			IP:     fmt.Sprintf("10.1.0.%d", m.nextIPSuffix),
			Subnet: subnetID,
			State:  "succeeded",
		}
		nic.Addresses = append(nic.Addresses, addr)
		m.nextIPSuffix++
	}

	return nil
}

// RunAzureNICLifecycle는 Azure NIC IPAM 라이프사이클을 시뮬레이션한다
func (m *AzureNICManager) RunAzureNICLifecycle(ctx context.Context) {
	fmt.Println("\n========================================")
	fmt.Println("  Azure NIC IPAM 시뮬레이션")
	fmt.Println("========================================")
	fmt.Printf("  인스턴스: %s\n", m.instanceID)
	fmt.Printf("  구독 ID: %s\n", m.subscriptionID)
	fmt.Printf("  리소스 그룹: %s\n", m.resourceGroup)
	if m.isVMSS {
		fmt.Printf("  VMSS: %s\n", m.vmssName)
	}
	fmt.Printf("  최대 IP/NIC: 256\n\n")

	// Step 1: 3단계 리싱크 (네트워크 인터페이스 조회 최적화)
	fmt.Println("  [Step 1] 3단계 리싱크 전략")
	fmt.Println("    Phase 1: 네트워크 인터페이스 목록 조회 (Azure API 1회)")
	m.limiter.Limit(ctx, "Interfaces.List")
	m.limiter.ObserveAPICall("Interfaces.List", "OK", 80*time.Millisecond)
	time.Sleep(80 * time.Millisecond)

	fmt.Println("    Phase 2: 빈 서브넷으로 파싱 (메모리 내, API 없음)")
	fmt.Println("    Phase 3: 사용 중인 서브넷만 선별적 조회")
	m.limiter.Limit(ctx, "Subnets.Get")
	m.limiter.ObserveAPICall("Subnets.Get", "OK", 40*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	fmt.Println("    Phase 4: 동일한 데이터를 서브넷 정보와 재파싱 (API 없음)")
	fmt.Println()

	// Step 2: 가용 NIC 확인
	fmt.Println("  [Step 2] 가용 NIC 및 IP 확인")
	m.mu.RLock()
	for name, nic := range m.nics {
		available := 256 - len(nic.Addresses)
		fmt.Printf("    NIC: %s, 현재 IP: %d, 가용: %d, NSG: %s\n",
			name, len(nic.Addresses), available, nic.SecurityGroup)
	}
	m.mu.RUnlock()
	fmt.Println()

	// Step 3: NIC 생성 시도 -> 미지원
	fmt.Println("  [Step 3] 새 NIC 생성 시도")
	fmt.Println("    -> Azure에서는 CreateInterface가 미구현")
	fmt.Println("    -> 기존 NIC에 IP Configuration을 추가하는 방식 사용")
	fmt.Println()

	// Step 4: IP 할당 (VM vs VMSS 분기)
	fmt.Println("  [Step 4] IP 할당")
	if m.isVMSS {
		fmt.Println("    모드: VMSS (VirtualMachineScaleSetVMs.Update API 사용)")
	} else {
		fmt.Println("    모드: VM (Interfaces.CreateOrUpdate API 사용)")
	}

	batchSize := 8
	err := m.AssignPrivateIpAddresses(ctx, "subnet-pods", "nic-primary", batchSize)
	if err != nil {
		fmt.Printf("    [ERROR] IP 할당 실패: %v\n", err)
		return
	}

	m.mu.RLock()
	nic := m.nics["nic-primary"]
	fmt.Printf("    -> %d개 IP 할당 완료. 총 IP 수: %d\n", batchSize, len(nic.Addresses))
	fmt.Println("    할당된 IP 목록:")
	for i, addr := range nic.Addresses {
		if i == 0 {
			fmt.Printf("      %s (Primary, subnet: %s, state: %s)\n",
				addr.IP, addr.Subnet, addr.State)
		} else {
			fmt.Printf("      %s (Secondary, subnet: %s, state: %s)\n",
				addr.IP, addr.Subnet, addr.State)
		}
	}
	m.mu.RUnlock()
	fmt.Println()

	// Step 5: Pod에 IP 할당
	fmt.Println("  [Step 5] Pod에 IP 할당")
	m.mu.RLock()
	for i, addr := range nic.Addresses {
		if i == 0 {
			continue // Primary IP 스킵
		}
		if addr.State != "succeeded" {
			fmt.Printf("    [SKIP] %s: state=%s (아직 프로비저닝 중)\n", addr.IP, addr.State)
			continue
		}
		if i > 5 {
			fmt.Printf("    ... 이하 %d개 더\n", len(nic.Addresses)-i)
			break
		}
		fmt.Printf("    Pod azure-app-%-4d -> IP: %s (NIC: %s)\n", i, addr.IP, nic.Name)
	}
	m.mu.RUnlock()
	fmt.Println()

	// Step 6: IP 해제 -> 미구현
	fmt.Println("  [Step 6] IP 해제")
	fmt.Println("    -> Azure IPAM에서는 ReleaseIPs가 미구현")
	fmt.Println("    -> IP는 NIC에 유지되며, NIC 전체가 해제될 때만 회수")
	fmt.Println()

	fmt.Println("  API 메트릭:")
	m.limiter.PrintMetrics()
}

// =============================================================================
// Alibaba Cloud ENI 시뮬레이션 (pkg/alibabacloud 참조)
// =============================================================================

// AlibabaENI는 Alibaba Cloud ENI를 나타낸다
// 참조: pkg/alibabacloud/eni/types/types.go
type AlibabaENI struct {
	NetworkInterfaceID string
	MACAddress         string
	Type               string // "Primary" or "Secondary"
	InstanceID         string
	SecurityGroupIDs   []string
	VPCID              string
	ZoneID             string
	VSwitchID          string
	PrimaryIPAddress   string
	PrivateIPSets      []AlibabaPrivateIPSet
	Tags               map[string]string
	Status             string // "Available", "InUse"
}

// AlibabaPrivateIPSet은 IP 세트를 나타낸다
type AlibabaPrivateIPSet struct {
	PrivateIpAddress string
	Primary          bool
}

// AlibabaENIManager는 Alibaba Cloud ENI IPAM을 시뮬레이션한다
type AlibabaENIManager struct {
	mu               sync.RWMutex
	instanceID       string
	instanceType     string
	vpcID            string
	zoneID           string
	enis             map[string]*AlibabaENI
	vSwitches        map[string]*Subnet
	securityGroups   map[string]*SecurityGroup
	limiter          *APILimiter
	nextENIID        int
	nextIPSuffix     int
}

// NewAlibabaENIManager는 새 Alibaba Cloud ENI 매니저를 생성한다
func NewAlibabaENIManager(meta *MetadataService) *AlibabaENIManager {
	instanceID, _ := meta.GetMetadata("instance-id")
	instanceType, _ := meta.GetMetadata("instance/instance-type")
	vpcID, _ := meta.GetMetadata("vpc-id")
	zoneID, _ := meta.GetMetadata("zone-id")

	m := &AlibabaENIManager{
		instanceID:     instanceID,
		instanceType:   instanceType,
		vpcID:          vpcID,
		zoneID:         zoneID,
		enis:           make(map[string]*AlibabaENI),
		vSwitches:      make(map[string]*Subnet),
		securityGroups: make(map[string]*SecurityGroup),
		limiter:        NewAPILimiter(10.0, 5),
		nextIPSuffix:   10,
	}

	// 기본 VSwitch
	m.vSwitches["vsw-bp1abc1234567890"] = &Subnet{
		ID: "vsw-bp1abc1234567890", Name: "pod-vswitch",
		CIDR: "172.16.0.0/20", AvailabilityZone: zoneID,
		VirtualNetworkID: vpcID, AvailableAddresses: 4000,
	}

	// 보안 그룹
	m.securityGroups[vpcID] = &SecurityGroup{
		ID: "sg-bp1abc1234", VpcID: vpcID,
		Tags: map[string]string{"app": "cilium"},
	}

	// Primary ENI
	m.enis["eni-bp1primary"] = &AlibabaENI{
		NetworkInterfaceID: "eni-bp1primary",
		MACAddress:         generateMAC(),
		Type:               "Primary",
		InstanceID:         instanceID,
		SecurityGroupIDs:   []string{"sg-bp1abc1234"},
		VPCID:              vpcID,
		ZoneID:             zoneID,
		VSwitchID:          "vsw-bp1abc1234567890",
		PrimaryIPAddress:   "172.16.0.5",
		PrivateIPSets:      []AlibabaPrivateIPSet{{PrivateIpAddress: "172.16.0.5", Primary: true}},
		Status:             "InUse",
		Tags:               map[string]string{},
	}

	return m
}

// CreateNetworkInterface는 Alibaba Cloud ENI를 생성한다
// 참조: pkg/alibabacloud/api/api.go - CreateNetworkInterface
func (m *AlibabaENIManager) CreateNetworkInterface(ctx context.Context, secondaryIPCount int,
	vSwitchID string, groups []string, tags map[string]string) (string, *AlibabaENI, error) {

	if err := m.limiter.Limit(ctx, "CreateNetworkInterface"); err != nil {
		return "", nil, err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("CreateNetworkInterface", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	limits, ok := InstanceTypeLimits[m.instanceType]
	if !ok {
		return "", nil, fmt.Errorf("unknown instance type: %s", m.instanceType)
	}
	if len(m.enis) >= limits.Adapters {
		return "", nil, fmt.Errorf("ENI limit reached: %d/%d", len(m.enis), limits.Adapters)
	}

	// Alibaba Cloud에서는 초기 생성 시 최대 10개
	const maxENIIPCreate = 10
	if secondaryIPCount > maxENIIPCreate {
		secondaryIPCount = maxENIIPCreate
	}

	m.nextENIID++
	eniID := fmt.Sprintf("eni-bp1%010d", m.nextENIID)
	primaryIP := fmt.Sprintf("172.16.0.%d", m.nextIPSuffix)
	m.nextIPSuffix++

	eni := &AlibabaENI{
		NetworkInterfaceID: eniID,
		MACAddress:         generateMAC(),
		Type:               "Secondary",
		SecurityGroupIDs:   groups,
		VPCID:              m.vpcID,
		ZoneID:             m.zoneID,
		VSwitchID:          vSwitchID,
		PrimaryIPAddress:   primaryIP,
		Status:             "Available",
		Tags:               tags,
		PrivateIPSets: []AlibabaPrivateIPSet{
			{PrivateIpAddress: primaryIP, Primary: true},
		},
	}

	// 보조 IP 할당
	for i := 0; i < secondaryIPCount; i++ {
		ip := fmt.Sprintf("172.16.0.%d", m.nextIPSuffix)
		eni.PrivateIPSets = append(eni.PrivateIPSets, AlibabaPrivateIPSet{
			PrivateIpAddress: ip, Primary: false,
		})
		m.nextIPSuffix++
	}

	return eniID, eni, nil
}

// AttachNetworkInterface는 ENI를 인스턴스에 부착한다
// 참조: pkg/alibabacloud/api/api.go
func (m *AlibabaENIManager) AttachNetworkInterface(ctx context.Context, eniID string) error {
	if err := m.limiter.Limit(ctx, "AttachNetworkInterface"); err != nil {
		return err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("AttachNetworkInterface", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)
	return nil
}

// WaitENIAttached는 ENI 부착 완료를 폴링으로 대기한다
// 참조: pkg/alibabacloud/api/api.go - WaitENIAttached
func (m *AlibabaENIManager) WaitENIAttached(ctx context.Context, eniID string) (string, error) {
	fmt.Printf("    ENI 부착 대기 (폴링)...")

	// 지수 백오프 시뮬레이션 (maxAttachRetries)
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		m.limiter.Limit(ctx, "DescribeNetworkInterfaces")
		m.limiter.ObserveAPICall("DescribeNetworkInterfaces", "OK", 30*time.Millisecond)
		time.Sleep(backoff)

		// 3번째 시도에서 성공 시뮬레이션
		if attempt >= 2 {
			m.mu.Lock()
			if eni, ok := m.enis[eniID]; ok {
				eni.Status = "InUse"
				eni.InstanceID = m.instanceID
			}
			m.mu.Unlock()
			fmt.Printf(" 완료 (시도: %d)\n", attempt+1)
			return m.instanceID, nil
		}
		fmt.Printf(".")
		backoff = time.Duration(float64(backoff) * 1.1) // 약간의 지터
	}

	return "", fmt.Errorf("timeout waiting for ENI attachment")
}

// AssignPrivateIPAddresses는 보조 IP를 할당한다
func (m *AlibabaENIManager) AssignPrivateIPAddresses(ctx context.Context, eniID string, count int) ([]string, error) {
	if err := m.limiter.Limit(ctx, "AssignPrivateIpAddresses"); err != nil {
		return nil, err
	}
	start := time.Now()
	defer func() {
		m.limiter.ObserveAPICall("AssignPrivateIpAddresses", "OK", time.Since(start))
	}()

	time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	eni, ok := m.enis[eniID]
	if !ok {
		return nil, fmt.Errorf("ENI not found: %s", eniID)
	}

	assigned := make([]string, count)
	for i := 0; i < count; i++ {
		ip := fmt.Sprintf("172.16.0.%d", m.nextIPSuffix)
		eni.PrivateIPSets = append(eni.PrivateIPSets, AlibabaPrivateIPSet{
			PrivateIpAddress: ip, Primary: false,
		})
		assigned[i] = ip
		m.nextIPSuffix++
	}

	return assigned, nil
}

// RunAlibabaENILifecycle는 Alibaba Cloud ENI 라이프사이클을 시뮬레이션한다
func (m *AlibabaENIManager) RunAlibabaENILifecycle(ctx context.Context) {
	fmt.Println("\n========================================")
	fmt.Println("  Alibaba Cloud ENI 라이프사이클 시뮬레이션")
	fmt.Println("========================================")
	fmt.Printf("  인스턴스: %s (%s)\n", m.instanceID, m.instanceType)
	fmt.Printf("  VPC: %s, Zone: %s\n", m.vpcID, m.zoneID)

	limits, ok := InstanceTypeLimits[m.instanceType]
	if !ok {
		fmt.Println("  [ERROR] 인스턴스 타입 제한을 찾을 수 없음")
		return
	}
	fmt.Printf("  제한: ENI %d개, IP/ENI %d개\n", limits.Adapters, limits.IPv4)
	fmt.Println("  프리픽스 위임: 미지원")
	fmt.Println()

	// Step 1: VSwitch 선택
	fmt.Println("  [Step 1] VSwitch 선택")
	var bestVSwitch *Subnet
	for _, vs := range m.vSwitches {
		if vs.VirtualNetworkID == m.vpcID && vs.AvailabilityZone == m.zoneID {
			if bestVSwitch == nil || vs.AvailableAddresses > bestVSwitch.AvailableAddresses {
				bestVSwitch = vs
			}
		}
	}
	if bestVSwitch != nil {
		fmt.Printf("    -> VSwitch: %s (%s), 가용 IP: %d\n\n",
			bestVSwitch.ID, bestVSwitch.CIDR, bestVSwitch.AvailableAddresses)
	}

	// Step 2: 보안 그룹 결정 (Primary ENI에서 상속)
	fmt.Println("  [Step 2] 보안 그룹 결정 (Primary ENI에서 상속)")
	var sgIDs []string
	for _, eni := range m.enis {
		if eni.Type == "Primary" {
			sgIDs = eni.SecurityGroupIDs
			break
		}
	}
	fmt.Printf("    -> 보안 그룹: %v\n\n", sgIDs)

	// Step 3: ENI 인덱스 할당 (태그 기반)
	fmt.Println("  [Step 3] ENI 인덱스 할당 (태그 기반)")
	eniIndex := 1 // 0은 Primary ENI
	fmt.Printf("    -> 할당된 인덱스: %d\n\n", eniIndex)

	// Step 4: ENI 생성 (초기 최대 10개 IP)
	fmt.Println("  [Step 4] ENI 생성 (초기 maxENIIPCreate=10)")
	initialIPs := min(limits.IPv4, 10) - 1 // Primary IP 제외
	tags := map[string]string{"cilium-eni-index": fmt.Sprintf("%d", eniIndex)}
	eniID, eni, err := m.CreateNetworkInterface(ctx, initialIPs, bestVSwitch.ID, sgIDs, tags)
	if err != nil {
		fmt.Printf("    [ERROR] ENI 생성 실패: %v\n", err)
		return
	}

	m.mu.Lock()
	m.enis[eniID] = eni
	m.mu.Unlock()

	fmt.Printf("    -> ENI 생성: %s (Type: %s)\n", eniID, eni.Type)
	fmt.Printf("    -> Primary IP: %s\n", eni.PrimaryIPAddress)
	fmt.Printf("    -> 보조 IP 수: %d\n\n", len(eni.PrivateIPSets)-1)

	// Step 5: ENI 부착 + WaitENIAttached (폴링)
	fmt.Println("  [Step 5] ENI 부착 (비동기 + 폴링 대기)")
	err = m.AttachNetworkInterface(ctx, eniID)
	if err != nil {
		fmt.Printf("    [ERROR] ENI 부착 실패: %v\n", err)
		return
	}
	_, err = m.WaitENIAttached(ctx, eniID)
	if err != nil {
		fmt.Printf("    [ERROR] ENI 부착 대기 실패: %v\n", err)
		return
	}
	fmt.Println()

	// Step 6: 추가 IP 할당
	fmt.Println("  [Step 6] 추가 보조 IP 할당")
	additionalIPs, err := m.AssignPrivateIPAddresses(ctx, eniID, 3)
	if err != nil {
		fmt.Printf("    [ERROR] IP 할당 실패: %v\n", err)
	} else {
		fmt.Printf("    -> 추가 할당: %v\n\n", additionalIPs)
	}

	// Step 7: Pod에 IP 할당
	fmt.Println("  [Step 7] Pod에 IP 할당")
	m.mu.RLock()
	for _, ipSet := range eni.PrivateIPSets {
		if ipSet.Primary {
			continue // Primary IP는 Pod에 할당하지 않음
		}
		fmt.Printf("    Pod alibaba-app -> IP: %s (ENI: %s)\n", ipSet.PrivateIpAddress, eniID)
	}
	m.mu.RUnlock()
	fmt.Println()

	// Step 8: 최종 상태
	fmt.Println("  [Step 8] 최종 ENI 상태")
	m.mu.RLock()
	for id, e := range m.enis {
		fmt.Printf("    ENI: %s (Type: %s, Status: %s)\n", id, e.Type, e.Status)
		fmt.Printf("      Primary IP: %s\n", e.PrimaryIPAddress)
		totalIPs := 0
		for _, ip := range e.PrivateIPSets {
			if !ip.Primary {
				totalIPs++
			}
		}
		fmt.Printf("      보조 IP 수: %d\n", totalIPs)
		if len(e.Tags) > 0 {
			fmt.Printf("      태그: %v\n", e.Tags)
		}
	}
	m.mu.RUnlock()

	fmt.Println("\n  API 메트릭:")
	m.limiter.PrintMetrics()
}

// =============================================================================
// 프로바이더간 비교 시뮬레이션
// =============================================================================

func runProviderComparison() {
	fmt.Println("\n╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║          클라우드 프로바이더 IPAM 비교 분석                  ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	fmt.Println("\n┌─────────────────────┬───────────────┬───────────────┬───────────────┐")
	fmt.Println("│ 기능                │ AWS ENI       │ Azure NIC     │ Alibaba ENI   │")
	fmt.Println("├─────────────────────┼───────────────┼───────────────┼───────────────┤")
	fmt.Println("│ 인터페이스 동적생성 │ O             │ X             │ O             │")
	fmt.Println("│ 프리픽스 위임       │ O (/28)       │ X             │ X             │")
	fmt.Println("│ IP 해제             │ O             │ X             │ O             │")
	fmt.Println("│ ENI GC              │ O             │ X             │ X             │")
	fmt.Println("│ VMSS 지원           │ -             │ O             │ -             │")
	fmt.Println("│ 부착 확인 방식      │ 동기          │ 동기          │ 폴링(비동기)  │")
	fmt.Println("│ 메타데이터 URL      │ 169.254.169.254│ 169.254.169.254│ 100.100.100.200│")
	fmt.Println("│ SDK                 │ aws-sdk-go-v2 │ azure-sdk-for │ alibaba-cloud │")
	fmt.Println("│                     │               │ -go           │ -sdk-go       │")
	fmt.Println("│ 최대 IP/인터페이스  │ 타입별 상이   │ 256 고정      │ 타입별 상이   │")
	fmt.Println("└─────────────────────┴───────────────┴───────────────┴───────────────┘")

	// Rate limiting 비교 시뮬레이션
	fmt.Println("\n--- Rate Limiting 비교 시뮬레이션 ---")
	fmt.Println("  각 프로바이더에 대해 20회 API 호출을 발생시켜 스로틀링 동작 확인")

	ctx := context.Background()
	providers := []struct {
		name    string
		limiter *APILimiter
	}{
		{"AWS", NewAPILimiter(10.0, 5)},     // 초당 10회, 버스트 5
		{"Azure", NewAPILimiter(8.0, 4)},    // 초당 8회, 버스트 4
		{"Alibaba", NewAPILimiter(10.0, 5)}, // 초당 10회, 버스트 5
	}

	for _, p := range providers {
		start := time.Now()
		for i := 0; i < 20; i++ {
			p.limiter.Limit(ctx, "TestAPICall")
			p.limiter.ObserveAPICall("TestAPICall", "OK", time.Millisecond)
		}
		elapsed := time.Since(start)
		fmt.Printf("  %-10s: 20회 호출 완료 시간: %v, 스로틀링 횟수: %d\n",
			p.name, elapsed.Round(time.Millisecond), atomic.LoadInt64(&p.limiter.throttled))
	}

	// 메타데이터 서비스 비교
	fmt.Println("\n--- 메타데이터 서비스 비교 ---")
	metaServices := []struct {
		name string
		svc  *MetadataService
	}{
		{"AWS IMDS", NewAWSMetadataService()},
		{"Azure IMDS", NewAzureMetadataService()},
		{"Alibaba Meta", NewAlibabaMetadataService()},
	}

	for _, ms := range metaServices {
		fmt.Printf("\n  [%s] (엔드포인트: %s)\n", ms.name, ms.svc.endpoint)
		id, _ := ms.svc.GetMetadata("instance-id")
		itype, _ := ms.svc.GetMetadata("instance-type")
		vpc, _ := ms.svc.GetMetadata("vpc-id")
		fmt.Printf("    instance-id: %s\n", id)
		fmt.Printf("    instance-type: %s\n", itype)
		fmt.Printf("    vpc-id: %s\n", vpc)

		if ms.name == "Azure IMDS" {
			sub, _ := ms.svc.GetMetadata("instance/compute/subscriptionId")
			rg, _ := ms.svc.GetMetadata("instance/compute/resourceGroupName")
			cloud, _ := ms.svc.GetMetadata("instance/compute/azEnvironment")
			fmt.Printf("    subscriptionId: %s\n", sub)
			fmt.Printf("    resourceGroupName: %s\n", rg)
			fmt.Printf("    azEnvironment: %s\n", cloud)
		}
	}

	// IP 할당 용량 비교
	fmt.Println("\n--- 인스턴스 타입별 최대 Pod IP 용량 ---")
	fmt.Printf("  %-20s %-8s %-12s %-15s\n", "인스턴스 타입", "ENI수", "IP/ENI", "최대 Pod IP")
	fmt.Println("  " + strings.Repeat("-", 58))
	for _, entry := range []struct {
		name string
		typ  string
	}{
		{"AWS", "t3.micro"},
		{"AWS", "m5.large"},
		{"AWS", "m5.xlarge"},
		{"Alibaba", "ecs.g6.large"},
		{"Alibaba", "ecs.g6.2xlarge"},
	} {
		limits := InstanceTypeLimits[entry.typ]
		// 첫 번째 인터페이스는 보통 제외 (firstInterfaceIndex=1)
		maxPodIPs := (limits.Adapters - 1) * (limits.IPv4 - 1)
		fmt.Printf("  %-20s %-8d %-12d %-15d\n", entry.typ, limits.Adapters, limits.IPv4, maxPodIPs)
	}
	fmt.Println("  (Azure는 NIC당 최대 256 IP, 인스턴스 타입에 무관)")
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func generateMAC() string {
	mac := make(net.HardwareAddr, 6)
	mac[0] = 0x0a // locally administered
	for i := 1; i < 6; i++ {
		mac[i] = byte(rand.Intn(256))
	}
	return mac.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║   Cilium 클라우드 프로바이더 통합 PoC                       ║")
	fmt.Println("║                                                              ║")
	fmt.Println("║   AWS ENI / Azure NIC / Alibaba Cloud ENI IPAM 시뮬레이션   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	ctx := context.Background()

	// ===== 1. AWS ENI 라이프사이클 =====
	awsMeta := NewAWSMetadataService()
	awsManager := NewAWSENIManager(awsMeta, false) // 프리픽스 위임 비활성화
	awsManager.RunENILifecycle(ctx)

	// ===== 2. AWS ENI (프리픽스 위임) =====
	fmt.Println("\n========================================")
	fmt.Println("  AWS ENI 프리픽스 위임 시뮬레이션")
	fmt.Println("========================================")
	awsManagerPD := NewAWSENIManager(awsMeta, true)
	fmt.Printf("  인스턴스: %s (%s, Nitro)\n", awsManagerPD.instanceID, awsManagerPD.instanceType)
	fmt.Println("  프리픽스 위임: 활성화 (/28 = 16 IPs per prefix)")

	eniID, eni, err := awsManagerPD.CreateNetworkInterface(ctx, 20, "subnet-0abc1234",
		[]string{"sg-0abc1234"}, true)
	if err != nil {
		fmt.Printf("  [ERROR]: %v\n", err)
	} else {
		fmt.Printf("  -> ENI: %s\n", eniID)
		fmt.Printf("  -> 할당된 프리픽스: %v\n", eni.Prefixes)
		fmt.Printf("  -> 총 IP 수: %d (프리픽스 %d개 * 16 IPs)\n", len(eni.Addresses), len(eni.Prefixes))
	}

	// ===== 3. Azure NIC 라이프사이클 =====
	azureMeta := NewAzureMetadataService()
	azureManager := NewAzureNICManager(azureMeta, false) // 일반 VM
	azureManager.RunAzureNICLifecycle(ctx)

	// ===== 4. Azure NIC (VMSS) =====
	azureManagerVMSS := NewAzureNICManager(azureMeta, true) // VMSS VM
	azureManagerVMSS.RunAzureNICLifecycle(ctx)

	// ===== 5. Alibaba Cloud ENI 라이프사이클 =====
	alibabaMeta := NewAlibabaMetadataService()
	alibabaManager := NewAlibabaENIManager(alibabaMeta)
	alibabaManager.RunAlibabaENILifecycle(ctx)

	// ===== 6. 프로바이더간 비교 =====
	runProviderComparison()

	fmt.Println("\n========================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("참조 코드:")
	fmt.Println("  - AWS ENI:    pkg/aws/eni/node.go, pkg/aws/ec2/ec2.go")
	fmt.Println("  - Azure NIC:  pkg/azure/ipam/node.go, pkg/azure/api/api.go")
	fmt.Println("  - Alibaba:    pkg/alibabacloud/eni/node.go, pkg/alibabacloud/api/api.go")
	fmt.Println("  - Metadata:   pkg/aws/metadata/, pkg/azure/api/metadata.go, pkg/alibabacloud/metadata/")
	fmt.Println("  - Operator:   operator/pkg/ipam/aws.go, azure.go, alibabacloud.go")
	fmt.Println("  - Allocator:  pkg/ipam/allocator/aws/, azure/, alibabacloud/")
}
