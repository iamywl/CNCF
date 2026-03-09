package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka Admin API 시뮬레이션
//
// 이 PoC는 Kafka Admin API의 핵심 개념을 시뮬레이션한다:
//   1. 토픽 생성/삭제 (ReplicationControlManager)
//   2. 파티션 추가 (CreatePartitions)
//   3. 파티션 재배치 (AlterPartitionReassignments)
//   4. ACL 관리 (AclControlManager)
//   5. ControllerMutationQuota (TokenBucket 기반 쿼터)
//   6. 인가 검사 (AuthHelper)
//   7. 메타데이터 레코드 생성 (__cluster_metadata)
//
// 참조 소스:
//   core/src/main/scala/kafka/server/ControllerApis.scala
//   metadata/src/main/java/org/apache/kafka/controller/ReplicationControlManager.java
//   metadata/src/main/java/org/apache/kafka/controller/AclControlManager.java
//   server/src/main/java/org/apache/kafka/server/quota/ControllerMutationQuotaManager.java
// =============================================================================

// --- 기본 타입 ---

// UUID는 토픽/ACL의 고유 식별자이다.
type UUID string

func newUUID() UUID {
	return UUID(fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rand.Int31(), rand.Int31n(0xFFFF), rand.Int31n(0xFFFF),
		rand.Int31n(0xFFFF), rand.Int63n(0xFFFFFFFFFFFF)))
}

// ApiError는 API 에러를 나타낸다.
type ApiError struct {
	Code    int
	Message string
}

func (e ApiError) String() string {
	if e.Code == 0 {
		return "OK"
	}
	return fmt.Sprintf("Error(%d): %s", e.Code, e.Message)
}

var (
	ErrNone                       = ApiError{0, ""}
	ErrTopicAlreadyExists         = ApiError{36, "Topic already exists"}
	ErrUnknownTopicOrPartition    = ApiError{3, "Unknown topic or partition"}
	ErrInvalidPartitions          = ApiError{37, "Invalid partitions"}
	ErrTopicAuthorizationFailed   = ApiError{29, "Topic authorization failed"}
	ErrClusterAuthorizationFailed = ApiError{31, "Cluster authorization failed"}
	ErrInvalidRequest             = ApiError{42, "Invalid request"}
	ErrThrottlingQuotaExceeded    = ApiError{89, "Throttling quota exceeded"}
	ErrTopicDeletionDisabled      = ApiError{73, "Topic deletion is disabled"}
)

// --- 메타데이터 레코드 ---

// MetadataRecord는 __cluster_metadata 토픽에 기록되는 레코드이다.
type MetadataRecord struct {
	Type    string
	Payload interface{}
}

func (r MetadataRecord) String() string {
	return fmt.Sprintf("[%s] %v", r.Type, r.Payload)
}

// --- 파티션 ---

// PartitionRegistration은 파티션의 메타데이터이다.
// Kafka의 PartitionRegistration에 대응한다.
type PartitionRegistration struct {
	PartitionID int
	Replicas    []int
	ISR         []int
	Leader      int
	LeaderEpoch int
}

// --- 토픽 ---

// TopicControlInfo는 토픽의 메타데이터이다.
// Kafka의 ReplicationControlManager.TopicControlInfo에 대응한다.
type TopicControlInfo struct {
	Name       string
	ID         UUID
	Partitions map[int]*PartitionRegistration
}

// --- ACL ---

// AclBinding은 ACL 바인딩을 나타낸다.
type AclBinding struct {
	ResourceType string // TOPIC, CLUSTER, GROUP
	ResourceName string
	PatternType  string // LITERAL, PREFIXED
	Principal    string // "User:alice"
	Host         string
	Operation    string // READ, WRITE, CREATE, DELETE, etc.
	Permission   string // ALLOW, DENY
}

func (a AclBinding) String() string {
	return fmt.Sprintf("%s %s:%s %s %s %s",
		a.Permission, a.ResourceType, a.ResourceName, a.PatternType, a.Principal, a.Operation)
}

// --- TokenBucket (ControllerMutationQuota) ---

// TokenBucket은 컨트롤러 변경 쿼터에 사용되는 토큰 버킷이다.
type TokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	rate       float64
	maxTokens  float64
	lastTimeMs int64
}

func NewTokenBucket(rate float64) *TokenBucket {
	return &TokenBucket{
		tokens:     rate, // 초기 토큰 = 1초분
		rate:       rate,
		maxTokens:  rate * 10,
		lastTimeMs: time.Now().UnixMilli(),
	}
}

func (tb *TokenBucket) TryConsume(permits float64) (int64, bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now().UnixMilli()
	elapsed := float64(now-tb.lastTimeMs) / 1000.0
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastTimeMs = now

	if tb.tokens < 0 {
		throttleMs := int64(-tb.tokens / tb.rate * 1000)
		return throttleMs, false // 쿼터 초과
	}

	tb.tokens -= permits
	return 0, true
}

// --- AuthHelper ---

// AuthHelper는 인가 검사를 수행한다.
// Kafka의 AuthHelper에 대응한다.
type AuthHelper struct {
	acls []AclBinding
}

func NewAuthHelper() *AuthHelper {
	return &AuthHelper{
		acls: []AclBinding{},
	}
}

// AddAcl은 ACL을 추가한다.
func (a *AuthHelper) AddAcl(binding AclBinding) {
	a.acls = append(a.acls, binding)
}

// Authorize는 특정 작업에 대한 인가를 검사한다.
func (a *AuthHelper) Authorize(principal, operation, resourceType, resourceName string) bool {
	for _, acl := range a.acls {
		if acl.Principal == principal &&
			acl.Operation == operation &&
			acl.ResourceType == resourceType &&
			(acl.ResourceName == resourceName || acl.ResourceName == "*") &&
			acl.Permission == "ALLOW" {
			return true
		}
	}
	return false
}

// FilterAuthorized는 인가된 리소스만 필터링한다.
func (a *AuthHelper) FilterAuthorized(principal, operation, resourceType string, names []string) []string {
	var result []string
	for _, name := range names {
		if a.Authorize(principal, operation, resourceType, name) {
			result = append(result, name)
		}
	}
	return result
}

// --- AdminController ---

// AdminController는 Kafka의 QuorumController가 관리하는 Admin API 기능을 시뮬레이션한다.
type AdminController struct {
	mu sync.Mutex

	// 토픽 관리 (ReplicationControlManager)
	topicsByName map[string]*TopicControlInfo
	topicsByID   map[UUID]*TopicControlInfo

	// ACL 관리 (AclControlManager)
	aclsByID map[UUID]AclBinding
	aclSet   map[string]bool // 중복 검사용

	// 쿼터 (ControllerMutationQuota)
	quotaBucket *TokenBucket
	quotaStrict bool // Strict 모드 여부

	// 메타데이터 레코드 (__cluster_metadata)
	metadataLog []MetadataRecord

	// 클러스터 상태
	brokers              []int
	deleteTopicEnabled   bool
	defaultReplication   int
	defaultNumPartitions int

	// 인가
	authHelper *AuthHelper
}

func NewAdminController(brokers []int) *AdminController {
	return &AdminController{
		topicsByName:         make(map[string]*TopicControlInfo),
		topicsByID:           make(map[UUID]*TopicControlInfo),
		aclsByID:             make(map[UUID]AclBinding),
		aclSet:               make(map[string]bool),
		quotaBucket:          NewTokenBucket(10), // 초당 10 mutations
		quotaStrict:          true,
		metadataLog:          make([]MetadataRecord, 0),
		brokers:              brokers,
		deleteTopicEnabled:   true,
		defaultReplication:   3,
		defaultNumPartitions: 1,
		authHelper:           NewAuthHelper(),
	}
}

// appendRecord는 메타데이터 레코드를 추가한다.
func (ac *AdminController) appendRecord(record MetadataRecord) {
	ac.metadataLog = append(ac.metadataLog, record)
}

// --- 토픽 생성 ---

// CreateTopicResult는 토픽 생성 결과이다.
type CreateTopicResult struct {
	Name           string
	TopicID        UUID
	NumPartitions  int
	ReplicationFac int
	Error          ApiError
}

// CreateTopics는 토픽을 생성한다.
// Kafka의 ControllerApis.createTopics() + ReplicationControlManager 로직에 대응한다.
func (ac *AdminController) CreateTopics(principal string, topics []CreateTopicRequest) []CreateTopicResult {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	results := make([]CreateTopicResult, len(topics))

	for i, req := range topics {
		results[i].Name = req.Name

		// 1. 인가 검사 (Kafka 실제 흐름: ControllerApis에서 인가를 먼저 수행한 뒤
		//    QuorumController.createTopics()로 전달한다)
		if !ac.authHelper.Authorize(principal, "CREATE", "TOPIC", req.Name) &&
			!ac.authHelper.Authorize(principal, "CREATE", "CLUSTER", "*") {
			results[i].Error = ErrTopicAuthorizationFailed
			continue
		}

		// 2. 쿼터 검사 (인가 통과 후 쿼터 소비)
		permits := float64(req.NumPartitions)
		if permits == 0 {
			permits = float64(ac.defaultNumPartitions)
		}
		if throttleMs, ok := ac.quotaBucket.TryConsume(permits); !ok && ac.quotaStrict {
			results[i].Error = ErrThrottlingQuotaExceeded
			results[i].Error.Message = fmt.Sprintf("Throttling quota exceeded (throttle %dms)", throttleMs)
			continue
		}

		// 3. 중복 검사
		if _, exists := ac.topicsByName[req.Name]; exists {
			results[i].Error = ErrTopicAlreadyExists
			continue
		}

		// 4. 파라미터 기본값 적용
		numPartitions := req.NumPartitions
		if numPartitions == 0 {
			numPartitions = ac.defaultNumPartitions
		}
		replicationFactor := req.ReplicationFactor
		if replicationFactor == 0 {
			replicationFactor = ac.defaultReplication
		}

		// 5. 파티션 배치
		topicID := newUUID()
		topicInfo := &TopicControlInfo{
			Name:       req.Name,
			ID:         topicID,
			Partitions: make(map[int]*PartitionRegistration),
		}

		// 메타데이터 레코드: TopicRecord
		ac.appendRecord(MetadataRecord{
			Type:    "TopicRecord",
			Payload: map[string]interface{}{"name": req.Name, "topicId": string(topicID)},
		})

		for p := 0; p < numPartitions; p++ {
			replicas := ac.assignReplicas(replicationFactor, p)
			partition := &PartitionRegistration{
				PartitionID: p,
				Replicas:    replicas,
				ISR:         replicas,
				Leader:      replicas[0],
				LeaderEpoch: 0,
			}
			topicInfo.Partitions[p] = partition

			// 메타데이터 레코드: PartitionRecord
			ac.appendRecord(MetadataRecord{
				Type: "PartitionRecord",
				Payload: map[string]interface{}{
					"topicId":     string(topicID),
					"partitionId": p,
					"replicas":    replicas,
					"isr":         replicas,
					"leader":      replicas[0],
				},
			})
		}

		ac.topicsByName[req.Name] = topicInfo
		ac.topicsByID[topicID] = topicInfo

		results[i].TopicID = topicID
		results[i].NumPartitions = numPartitions
		results[i].ReplicationFac = replicationFactor
		results[i].Error = ErrNone
	}

	return results
}

type CreateTopicRequest struct {
	Name              string
	NumPartitions     int
	ReplicationFactor int
}

// assignReplicas는 간단한 라운드 로빈 배치를 수행한다.
func (ac *AdminController) assignReplicas(replicationFactor, partitionIdx int) []int {
	n := len(ac.brokers)
	if replicationFactor > n {
		replicationFactor = n
	}
	replicas := make([]int, replicationFactor)
	start := partitionIdx % n
	for i := 0; i < replicationFactor; i++ {
		replicas[i] = ac.brokers[(start+i)%n]
	}
	return replicas
}

// --- 토픽 삭제 ---

// DeleteTopics는 토픽을 삭제한다.
func (ac *AdminController) DeleteTopics(principal string, topicNames []string) map[string]ApiError {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	results := make(map[string]ApiError)

	// delete.topic.enable 검사
	if !ac.deleteTopicEnabled {
		for _, name := range topicNames {
			results[name] = ErrTopicDeletionDisabled
		}
		return results
	}

	for _, name := range topicNames {
		// 인가 검사
		if !ac.authHelper.Authorize(principal, "DELETE", "TOPIC", name) &&
			!ac.authHelper.Authorize(principal, "DELETE", "CLUSTER", "*") {
			results[name] = ErrTopicAuthorizationFailed
			continue
		}

		topic, exists := ac.topicsByName[name]
		if !exists {
			results[name] = ErrUnknownTopicOrPartition
			continue
		}

		// 쿼터 소비
		ac.quotaBucket.TryConsume(1)

		// 삭제 수행
		delete(ac.topicsByName, name)
		delete(ac.topicsByID, topic.ID)

		// 메타데이터 레코드: RemoveTopicRecord
		ac.appendRecord(MetadataRecord{
			Type:    "RemoveTopicRecord",
			Payload: map[string]interface{}{"topicId": string(topic.ID)},
		})

		results[name] = ErrNone
	}

	return results
}

// --- 파티션 추가 ---

// CreatePartitions는 기존 토픽에 파티션을 추가한다.
func (ac *AdminController) CreatePartitions(principal string, topicName string, newTotalCount int) ApiError {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	// 인가 검사 (TOPIC ALTER 또는 CLUSTER ALTER 권한 필요)
	// Kafka 실제 흐름: ControllerApis.handleCreatePartitions()에서
	// authHelper.authorize(TOPIC, ALTER)를 검사하며, super user(CLUSTER ALTER)도 허용된다.
	if !ac.authHelper.Authorize(principal, "ALTER", "TOPIC", topicName) &&
		!ac.authHelper.Authorize(principal, "ALTER", "CLUSTER", "*") {
		return ErrTopicAuthorizationFailed
	}

	topic, exists := ac.topicsByName[topicName]
	if !exists {
		return ErrUnknownTopicOrPartition
	}

	currentCount := len(topic.Partitions)
	if newTotalCount <= currentCount {
		return ApiError{37, fmt.Sprintf("파티션 수는 현재(%d)보다 커야 합니다", currentCount)}
	}

	// 쿼터 소비
	addCount := newTotalCount - currentCount
	ac.quotaBucket.TryConsume(float64(addCount))

	// 새 파티션 추가
	existingReplicas := topic.Partitions[0].Replicas
	replicationFactor := len(existingReplicas)

	for p := currentCount; p < newTotalCount; p++ {
		replicas := ac.assignReplicas(replicationFactor, p)
		partition := &PartitionRegistration{
			PartitionID: p,
			Replicas:    replicas,
			ISR:         replicas,
			Leader:      replicas[0],
			LeaderEpoch: 0,
		}
		topic.Partitions[p] = partition

		ac.appendRecord(MetadataRecord{
			Type: "PartitionRecord",
			Payload: map[string]interface{}{
				"topicId":     string(topic.ID),
				"partitionId": p,
				"replicas":    replicas,
			},
		})
	}

	return ErrNone
}

// --- ACL 관리 ---

// CreateAcls는 ACL을 생성한다.
// Kafka의 AclControlManager.createAcls()에 대응한다.
func (ac *AdminController) CreateAcls(principal string, bindings []AclBinding) []ApiError {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	// 클러스터 ALTER 권한 필요
	if !ac.authHelper.Authorize(principal, "ALTER", "CLUSTER", "*") {
		results := make([]ApiError, len(bindings))
		for i := range results {
			results[i] = ErrClusterAuthorizationFailed
		}
		return results
	}

	results := make([]ApiError, len(bindings))

	for i, binding := range bindings {
		// 검증
		if err := validateAclBinding(binding); err != ErrNone {
			results[i] = err
			continue
		}

		// 중복 검사
		key := binding.String()
		if ac.aclSet[key] {
			// 이미 존재하면 성공으로 처리 (멱등성)
			results[i] = ErrNone
			continue
		}

		// ACL 생성
		id := newUUID()
		ac.aclsByID[id] = binding
		ac.aclSet[key] = true

		// 인가 헬퍼에도 추가
		ac.authHelper.AddAcl(binding)

		// 메타데이터 레코드
		ac.appendRecord(MetadataRecord{
			Type: "AccessControlEntryRecord",
			Payload: map[string]interface{}{
				"id":           string(id),
				"resourceType": binding.ResourceType,
				"resourceName": binding.ResourceName,
				"principal":    binding.Principal,
				"operation":    binding.Operation,
				"permission":   binding.Permission,
			},
		})

		results[i] = ErrNone
	}

	return results
}

// DeleteAcls는 필터와 매칭되는 ACL을 삭제한다.
func (ac *AdminController) DeleteAcls(principal string, filter AclBinding) ([]AclBinding, ApiError) {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	// 클러스터 ALTER 권한 필요
	if !ac.authHelper.Authorize(principal, "ALTER", "CLUSTER", "*") {
		return nil, ErrClusterAuthorizationFailed
	}

	var deleted []AclBinding
	var toDelete []UUID

	for id, acl := range ac.aclsByID {
		if matchesFilter(acl, filter) {
			toDelete = append(toDelete, id)
			deleted = append(deleted, acl)
		}
	}

	for _, id := range toDelete {
		binding := ac.aclsByID[id]
		delete(ac.aclsByID, id)
		delete(ac.aclSet, binding.String())

		ac.appendRecord(MetadataRecord{
			Type:    "RemoveAccessControlEntryRecord",
			Payload: map[string]interface{}{"id": string(id)},
		})
	}

	return deleted, ErrNone
}

func validateAclBinding(binding AclBinding) ApiError {
	if binding.ResourceType == "" || binding.ResourceType == "UNKNOWN" || binding.ResourceType == "ANY" {
		return ApiError{42, "Invalid resourceType: " + binding.ResourceType}
	}
	if binding.PatternType != "LITERAL" && binding.PatternType != "PREFIXED" {
		return ApiError{42, "Invalid patternType: " + binding.PatternType}
	}
	if binding.Operation == "" || binding.Operation == "UNKNOWN" || binding.Operation == "ANY" {
		return ApiError{42, "Invalid operation: " + binding.Operation}
	}
	if binding.Permission != "ALLOW" && binding.Permission != "DENY" {
		return ApiError{42, "Invalid permissionType: " + binding.Permission}
	}
	if binding.ResourceName == "" {
		return ApiError{42, "Resource name should not be empty"}
	}
	if !strings.Contains(binding.Principal, ":") {
		return ApiError{42, "Could not parse principal: " + binding.Principal}
	}
	return ErrNone
}

func matchesFilter(acl, filter AclBinding) bool {
	if filter.ResourceType != "" && filter.ResourceType != acl.ResourceType {
		return false
	}
	if filter.ResourceName != "" && filter.ResourceName != acl.ResourceName {
		return false
	}
	if filter.Principal != "" && filter.Principal != acl.Principal {
		return false
	}
	if filter.Operation != "" && filter.Operation != acl.Operation {
		return false
	}
	return true
}

// --- 상태 출력 ---

func (ac *AdminController) PrintTopics() {
	fmt.Println("  현재 토픽 목록:")
	names := make([]string, 0, len(ac.topicsByName))
	for name := range ac.topicsByName {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		topic := ac.topicsByName[name]
		fmt.Printf("    %s (ID: %s, 파티션: %d)\n", name, topic.ID[:8], len(topic.Partitions))
		for p := 0; p < len(topic.Partitions); p++ {
			part := topic.Partitions[p]
			fmt.Printf("      P%d: replicas=%v, isr=%v, leader=%d\n",
				p, part.Replicas, part.ISR, part.Leader)
		}
	}
}

func (ac *AdminController) PrintACLs() {
	fmt.Println("  현재 ACL 목록:")
	if len(ac.aclsByID) == 0 {
		fmt.Println("    (없음)")
		return
	}
	for _, acl := range ac.aclsByID {
		fmt.Printf("    %s\n", acl)
	}
}

func (ac *AdminController) PrintMetadataLog() {
	fmt.Println("  __cluster_metadata 레코드:")
	for i, record := range ac.metadataLog {
		fmt.Printf("    offset=%d: %s\n", i, record)
	}
}

// =============================================================================
// 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Kafka Admin API 시뮬레이션 ===")
	fmt.Println()

	// 컨트롤러 생성 (5개 브로커)
	controller := NewAdminController([]int{1, 2, 3, 4, 5})

	// 관리자 ACL 설정
	controller.authHelper.AddAcl(AclBinding{
		ResourceType: "CLUSTER", ResourceName: "*", PatternType: "LITERAL",
		Principal: "User:admin", Operation: "CREATE", Permission: "ALLOW",
	})
	controller.authHelper.AddAcl(AclBinding{
		ResourceType: "CLUSTER", ResourceName: "*", PatternType: "LITERAL",
		Principal: "User:admin", Operation: "DELETE", Permission: "ALLOW",
	})
	controller.authHelper.AddAcl(AclBinding{
		ResourceType: "CLUSTER", ResourceName: "*", PatternType: "LITERAL",
		Principal: "User:admin", Operation: "ALTER", Permission: "ALLOW",
	})

	demo1CreateTopics(controller)
	demo2DeleteTopics(controller)
	demo3CreatePartitions(controller)
	demo4AclManagement(controller)
	demo5QuotaEnforcement(controller)
	demo6MetadataLog(controller)

	fmt.Println("=== 시뮬레이션 완료 ===")
}

func demo1CreateTopics(controller *AdminController) {
	fmt.Println("--- 데모 1: 토픽 생성 ---")
	fmt.Println()

	topics := []CreateTopicRequest{
		{Name: "orders", NumPartitions: 3, ReplicationFactor: 3},
		{Name: "events", NumPartitions: 5, ReplicationFactor: 2},
		{Name: "logs", NumPartitions: 1, ReplicationFactor: 1},
	}

	results := controller.CreateTopics("User:admin", topics)

	for _, r := range results {
		if r.Error == ErrNone {
			fmt.Printf("  토픽 생성 성공: %s (ID: %s, 파티션: %d, 복제: %d)\n",
				r.Name, r.TopicID[:8], r.NumPartitions, r.ReplicationFac)
		} else {
			fmt.Printf("  토픽 생성 실패: %s → %s\n", r.Name, r.Error)
		}
	}

	// 중복 생성 시도
	fmt.Println()
	fmt.Println("  중복 토픽 생성 시도:")
	dupResults := controller.CreateTopics("User:admin", []CreateTopicRequest{
		{Name: "orders", NumPartitions: 3, ReplicationFactor: 3},
	})
	for _, r := range dupResults {
		fmt.Printf("    %s → %s\n", r.Name, r.Error)
	}

	// 인가 실패 시도
	fmt.Println()
	fmt.Println("  권한 없는 사용자의 토픽 생성 시도:")
	unauthResults := controller.CreateTopics("User:bob", []CreateTopicRequest{
		{Name: "secret-topic", NumPartitions: 1, ReplicationFactor: 1},
	})
	for _, r := range unauthResults {
		fmt.Printf("    %s → %s\n", r.Name, r.Error)
	}

	fmt.Println()
	controller.PrintTopics()
	fmt.Println()
}

func demo2DeleteTopics(controller *AdminController) {
	fmt.Println("--- 데모 2: 토픽 삭제 ---")
	fmt.Println()

	results := controller.DeleteTopics("User:admin", []string{"logs"})
	for name, err := range results {
		if err == ErrNone {
			fmt.Printf("  토픽 삭제 성공: %s\n", name)
		} else {
			fmt.Printf("  토픽 삭제 실패: %s → %s\n", name, err)
		}
	}

	// 존재하지 않는 토픽 삭제
	fmt.Println()
	fmt.Println("  존재하지 않는 토픽 삭제 시도:")
	results2 := controller.DeleteTopics("User:admin", []string{"nonexistent"})
	for name, err := range results2 {
		fmt.Printf("    %s → %s\n", name, err)
	}

	fmt.Println()
}

func demo3CreatePartitions(controller *AdminController) {
	fmt.Println("--- 데모 3: 파티션 추가 ---")
	fmt.Println()

	// orders 토픽의 파티션을 3 → 6으로 증가
	err := controller.CreatePartitions("User:admin", "orders", 6)
	if err == ErrNone {
		fmt.Printf("  파티션 추가 성공: orders → 6개 파티션\n")
	} else {
		fmt.Printf("  파티션 추가 실패: %s\n", err)
	}

	// 파티션 감소 시도 (불가능)
	fmt.Println()
	fmt.Println("  파티션 감소 시도 (현재 6개 → 3개):")
	err2 := controller.CreatePartitions("User:admin", "orders", 3)
	fmt.Printf("    결과: %s\n", err2)

	fmt.Println()
	controller.PrintTopics()
	fmt.Println()
}

func demo4AclManagement(controller *AdminController) {
	fmt.Println("--- 데모 4: ACL 관리 ---")
	fmt.Println()

	// ACL 생성
	acls := []AclBinding{
		{
			ResourceType: "TOPIC", ResourceName: "orders", PatternType: "LITERAL",
			Principal: "User:alice", Host: "*", Operation: "READ", Permission: "ALLOW",
		},
		{
			ResourceType: "TOPIC", ResourceName: "orders", PatternType: "LITERAL",
			Principal: "User:alice", Host: "*", Operation: "WRITE", Permission: "ALLOW",
		},
		{
			ResourceType: "TOPIC", ResourceName: "events", PatternType: "LITERAL",
			Principal: "User:bob", Host: "*", Operation: "READ", Permission: "ALLOW",
		},
	}

	results := controller.CreateAcls("User:admin", acls)
	for i, err := range results {
		if err == ErrNone {
			fmt.Printf("  ACL 생성 성공: %s\n", acls[i])
		} else {
			fmt.Printf("  ACL 생성 실패: %s → %s\n", acls[i], err)
		}
	}

	fmt.Println()
	controller.PrintACLs()

	// ACL 삭제 (필터 기반)
	fmt.Println()
	fmt.Println("  ACL 삭제 (User:alice의 orders 토픽 ACL):")
	deleted, err := controller.DeleteAcls("User:admin", AclBinding{
		ResourceType: "TOPIC", ResourceName: "orders", Principal: "User:alice",
	})
	if err == ErrNone {
		fmt.Printf("    삭제된 ACL 수: %d\n", len(deleted))
		for _, d := range deleted {
			fmt.Printf("      - %s\n", d)
		}
	}

	fmt.Println()
	controller.PrintACLs()

	// 잘못된 ACL 생성 시도
	fmt.Println()
	fmt.Println("  잘못된 ACL 생성 시도:")
	invalidAcls := []AclBinding{
		{ResourceType: "UNKNOWN", ResourceName: "test", PatternType: "LITERAL",
			Principal: "User:test", Operation: "READ", Permission: "ALLOW"},
		{ResourceType: "TOPIC", ResourceName: "", PatternType: "LITERAL",
			Principal: "User:test", Operation: "READ", Permission: "ALLOW"},
		{ResourceType: "TOPIC", ResourceName: "test", PatternType: "LITERAL",
			Principal: "invalid-principal", Operation: "READ", Permission: "ALLOW"},
	}
	invalidResults := controller.CreateAcls("User:admin", invalidAcls)
	for i, err := range invalidResults {
		fmt.Printf("    ACL %d → %s\n", i+1, err)
	}
	fmt.Println()
}

func demo5QuotaEnforcement(controller *AdminController) {
	fmt.Println("--- 데모 5: ControllerMutationQuota ---")
	fmt.Println()

	// 쿼터를 낮게 설정하여 초과 유발
	controller.quotaBucket = NewTokenBucket(3) // 초당 3 mutations만 허용
	fmt.Println("  쿼터 설정: 초당 3 mutations")
	fmt.Println()

	// 빠른 연속 토픽 생성 시도
	for i := 0; i < 5; i++ {
		topicName := fmt.Sprintf("burst-topic-%d", i)
		results := controller.CreateTopics("User:admin", []CreateTopicRequest{
			{Name: topicName, NumPartitions: 3, ReplicationFactor: 1},
		})
		for _, r := range results {
			if r.Error.Code == 0 {
				fmt.Printf("  토픽 생성: %s → 성공\n", r.Name)
			} else {
				fmt.Printf("  토픽 생성: %s → %s (Strict 모드)\n", r.Name, r.Error)
			}
		}
	}
	fmt.Println()
}

func demo6MetadataLog(controller *AdminController) {
	fmt.Println("--- 데모 6: __cluster_metadata 레코드 ---")
	fmt.Println()
	controller.PrintMetadataLog()
	fmt.Println()
	fmt.Printf("  총 레코드 수: %d\n", len(controller.metadataLog))
	fmt.Println()
}
