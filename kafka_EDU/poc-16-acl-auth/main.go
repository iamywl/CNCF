package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Kafka ACL 인가(Authorization) 시스템 시뮬레이션
//
// 참조: clients/src/main/java/org/apache/kafka/server/authorizer/Authorizer.java
//       clients/src/main/java/org/apache/kafka/common/acl/AclBinding.java
//       clients/src/main/java/org/apache/kafka/common/resource/ResourceType.java
//       clients/src/main/java/org/apache/kafka/common/acl/AclOperation.java
//       metadata/src/main/java/org/apache/kafka/metadata/authorizer/StandardAuthorizerData.java
//
// Kafka는 DENY-first 평가 방식으로 ACL을 검사한다.
// DENY 규칙이 하나라도 매칭되면 즉시 거부하고, 그 후 ALLOW 규칙을 검사한다.
// =============================================================================

// ResourceType은 Kafka에서 ACL이 적용되는 리소스 유형이다.
// clients/src/main/java/org/apache/kafka/common/resource/ResourceType.java에 해당한다.
type ResourceType int

const (
	ResourceUnknown ResourceType = iota
	ResourceTopic
	ResourceGroup
	ResourceCluster
	ResourceTransactionalID
	ResourceDelegationToken
)

func (r ResourceType) String() string {
	switch r {
	case ResourceTopic:
		return "TOPIC"
	case ResourceGroup:
		return "GROUP"
	case ResourceCluster:
		return "CLUSTER"
	case ResourceTransactionalID:
		return "TRANSACTIONAL_ID"
	case ResourceDelegationToken:
		return "DELEGATION_TOKEN"
	default:
		return "UNKNOWN"
	}
}

// AclOperation은 리소스에 대해 수행할 수 있는 작업이다.
// clients/src/main/java/org/apache/kafka/common/acl/AclOperation.java에 해당한다.
type AclOperation int

const (
	OpUnknown AclOperation = iota
	OpAll                  // 모든 작업 (와일드카드)
	OpRead
	OpWrite
	OpCreate
	OpDelete
	OpAlter
	OpDescribe
	OpAlterConfigs
	OpDescribeConfigs
)

func (o AclOperation) String() string {
	switch o {
	case OpAll:
		return "ALL"
	case OpRead:
		return "READ"
	case OpWrite:
		return "WRITE"
	case OpCreate:
		return "CREATE"
	case OpDelete:
		return "DELETE"
	case OpAlter:
		return "ALTER"
	case OpDescribe:
		return "DESCRIBE"
	case OpAlterConfigs:
		return "ALTER_CONFIGS"
	case OpDescribeConfigs:
		return "DESCRIBE_CONFIGS"
	default:
		return "UNKNOWN"
	}
}

// AclPermissionType은 허용/거부 유형이다.
// clients/src/main/java/org/apache/kafka/common/acl/AclPermissionType.java에 해당한다.
type AclPermissionType int

const (
	PermUnknown AclPermissionType = iota
	PermAllow
	PermDeny
)

func (p AclPermissionType) String() string {
	switch p {
	case PermAllow:
		return "ALLOW"
	case PermDeny:
		return "DENY"
	default:
		return "UNKNOWN"
	}
}

// PatternType은 리소스 패턴 매칭 유형이다.
// clients/src/main/java/org/apache/kafka/common/resource/PatternType.java에 해당한다.
type PatternType int

const (
	PatternLiteral  PatternType = iota // 정확한 이름 매칭
	PatternPrefixed                    // 접두사 매칭
	PatternWildcard                    // 와일드카드 (*) 매칭 - 모든 리소스
)

func (p PatternType) String() string {
	switch p {
	case PatternLiteral:
		return "LITERAL"
	case PatternPrefixed:
		return "PREFIXED"
	case PatternWildcard:
		return "WILDCARD"
	default:
		return "UNKNOWN"
	}
}

// ResourcePattern은 ACL이 적용되는 리소스 패턴이다.
// clients/src/main/java/org/apache/kafka/common/resource/ResourcePattern.java에 해당한다.
type ResourcePattern struct {
	ResourceType ResourceType
	Name         string      // 리소스 이름 (또는 접두사, "*")
	PatternType  PatternType
}

func (rp ResourcePattern) String() string {
	return fmt.Sprintf("%s:%s(%s)", rp.ResourceType, rp.Name, rp.PatternType)
}

// Matches는 주어진 리소스 유형과 이름이 이 패턴에 매칭되는지 검사한다.
func (rp ResourcePattern) Matches(resourceType ResourceType, resourceName string) bool {
	if rp.ResourceType != resourceType {
		return false
	}

	switch rp.PatternType {
	case PatternLiteral:
		return rp.Name == resourceName
	case PatternPrefixed:
		return strings.HasPrefix(resourceName, rp.Name)
	case PatternWildcard:
		return true // 모든 리소스에 매칭
	}
	return false
}

// AccessControlEntry는 ACL의 접근 제어 항목이다.
// clients/src/main/java/org/apache/kafka/common/acl/AccessControlEntry.java에 해당한다.
type AccessControlEntry struct {
	Principal      string            // "User:alice", "User:*" (와일드카드)
	Host           string            // "192.168.1.1", "*" (와일드카드)
	Operation      AclOperation
	PermissionType AclPermissionType
}

func (ace AccessControlEntry) String() string {
	return fmt.Sprintf("(%s, host=%s, op=%s, perm=%s)",
		ace.Principal, ace.Host, ace.Operation, ace.PermissionType)
}

// MatchesPrincipalAndHost는 주어진 principal과 호스트가 이 ACE에 매칭되는지 검사한다.
// StandardAuthorizerData.java의 WILDCARD_PRINCIPAL ("User:*") 패턴에 기반한다.
func (ace AccessControlEntry) MatchesPrincipalAndHost(principal, host string) bool {
	principalMatch := ace.Principal == principal || ace.Principal == "User:*"
	hostMatch := ace.Host == host || ace.Host == "*"
	return principalMatch && hostMatch
}

// MatchesOperation은 작업이 매칭되는지 검사한다.
// OpAll은 모든 작업에 매칭된다.
func (ace AccessControlEntry) MatchesOperation(op AclOperation) bool {
	return ace.Operation == OpAll || ace.Operation == op
}

// AclBinding은 ResourcePattern과 AccessControlEntry의 결합이다.
// clients/src/main/java/org/apache/kafka/common/acl/AclBinding.java에 해당한다.
type AclBinding struct {
	Pattern ResourcePattern
	Entry   AccessControlEntry
}

func (ab AclBinding) String() string {
	return fmt.Sprintf("(pattern=%s, entry=%s)", ab.Pattern, ab.Entry)
}

// AuthorizationResult는 인가 결과이다.
type AuthorizationResult int

const (
	ResultAllowed AuthorizationResult = iota
	ResultDenied
)

func (r AuthorizationResult) String() string {
	if r == ResultAllowed {
		return "ALLOWED"
	}
	return "DENIED"
}

// Action은 인가 요청의 개별 작업이다.
// clients/src/main/java/org/apache/kafka/server/authorizer/Action.java에 해당한다.
type Action struct {
	ResourceType ResourceType
	ResourceName string
	Operation    AclOperation
}

func (a Action) String() string {
	return fmt.Sprintf("%s:%s/%s", a.ResourceType, a.ResourceName, a.Operation)
}

// RequestContext는 인가 요청의 컨텍스트이다.
type RequestContext struct {
	Principal string
	Host      string
}

// StandardAuthorizer는 Kafka의 내장 인가기(Authorizer)를 시뮬레이션한다.
// metadata/src/main/java/org/apache/kafka/metadata/authorizer/StandardAuthorizer.java에 해당한다.
type StandardAuthorizer struct {
	acls       []AclBinding
	superUsers map[string]bool
	// allowEveryoneIfNoAcl: ACL이 없을 때 모든 접근을 허용할지 여부
	// StandardAuthorizerData.java의 noAclRule에 해당
	allowEveryoneIfNoAcl bool
}

func NewStandardAuthorizer(allowIfNoAcl bool) *StandardAuthorizer {
	return &StandardAuthorizer{
		acls:                 make([]AclBinding, 0),
		superUsers:           make(map[string]bool),
		allowEveryoneIfNoAcl: allowIfNoAcl,
	}
}

// AddSuperUser는 슈퍼 유저를 등록한다.
// StandardAuthorizer.java의 SUPER_USERS_CONFIG에 해당한다.
func (a *StandardAuthorizer) AddSuperUser(principal string) {
	a.superUsers[principal] = true
}

// AddAcl은 ACL 바인딩을 추가한다.
func (a *StandardAuthorizer) AddAcl(binding AclBinding) {
	a.acls = append(a.acls, binding)
}

// Authorize는 주어진 요청 컨텍스트와 액션 목록에 대해 인가를 수행한다.
// Authorizer.java의 authorize(requestContext, actions) 메서드에 해당한다.
//
// 인가 알고리즘 (StandardAuthorizerData.java 기반):
// 1. 슈퍼 유저이면 즉시 ALLOWED
// 2. DENY 규칙 먼저 검사 (DENY-first)
//    - 매칭되는 DENY가 있으면 즉시 DENIED
// 3. ALLOW 규칙 검사
//    - 매칭되는 ALLOW가 있으면 ALLOWED
// 4. ACL이 없고 allowEveryoneIfNoAcl이면 ALLOWED
// 5. 그 외 DENIED
func (a *StandardAuthorizer) Authorize(ctx RequestContext, actions []Action) []AuthorizationResult {
	results := make([]AuthorizationResult, len(actions))

	for i, action := range actions {
		results[i] = a.authorizeAction(ctx, action)
	}
	return results
}

func (a *StandardAuthorizer) authorizeAction(ctx RequestContext, action Action) AuthorizationResult {
	// 1) 슈퍼 유저 체크
	if a.superUsers[ctx.Principal] {
		return ResultAllowed
	}

	// 매칭되는 ACL이 있는지 추적
	hasMatchingAcl := false

	// 2) DENY-first 평가: DENY 규칙부터 검사
	for _, binding := range a.acls {
		if !binding.Pattern.Matches(action.ResourceType, action.ResourceName) {
			continue
		}
		if !binding.Entry.MatchesPrincipalAndHost(ctx.Principal, ctx.Host) {
			continue
		}
		if !binding.Entry.MatchesOperation(action.Operation) {
			continue
		}

		hasMatchingAcl = true

		if binding.Entry.PermissionType == PermDeny {
			return ResultDenied // DENY가 하나라도 매칭되면 즉시 거부
		}
	}

	// 3) ALLOW 규칙 검사
	for _, binding := range a.acls {
		if !binding.Pattern.Matches(action.ResourceType, action.ResourceName) {
			continue
		}
		if !binding.Entry.MatchesPrincipalAndHost(ctx.Principal, ctx.Host) {
			continue
		}
		if !binding.Entry.MatchesOperation(action.Operation) {
			continue
		}

		if binding.Entry.PermissionType == PermAllow {
			return ResultAllowed
		}
	}

	// 4) 매칭되는 ACL이 없고 allowEveryoneIfNoAcl이면 허용
	if !hasMatchingAcl && a.allowEveryoneIfNoAcl {
		return ResultAllowed
	}

	// 5) 기본: 거부
	return ResultDenied
}

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printAuthResult(ctx RequestContext, action Action, result AuthorizationResult) {
	symbol := "+"
	if result == ResultDenied {
		symbol = "X"
	}
	fmt.Printf("  [%s] %s@%s -> %s : %s\n",
		symbol, ctx.Principal, ctx.Host, action, result)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Kafka ACL 인가(Authorization) 시스템 시뮬레이션            ║")
	fmt.Println("║  참조: Authorizer.java, AclBinding.java, StandardAuthorizerData.java║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 시나리오 1: 기본 ALLOW/DENY 규칙
	// =========================================================================
	printSeparator("시나리오 1: 기본 ALLOW/DENY 규칙")
	fmt.Println("alice는 'orders' 토픽에 READ/WRITE 허용, bob은 READ만 허용.")
	fmt.Println()

	auth := NewStandardAuthorizer(false)

	// alice에게 orders 토픽 READ/WRITE 허용
	auth.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "orders", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:alice", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})
	auth.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "orders", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:alice", Host: "*", Operation: OpWrite, PermissionType: PermAllow,
		},
	})

	// bob에게 orders 토픽 READ만 허용
	auth.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "orders", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:bob", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Println()

	// alice 테스트
	aliceCtx := RequestContext{Principal: "User:alice", Host: "192.168.1.10"}
	aliceActions := []Action{
		{ResourceTopic, "orders", OpRead},
		{ResourceTopic, "orders", OpWrite},
		{ResourceTopic, "orders", OpDelete},
		{ResourceTopic, "payments", OpRead},
	}
	results := auth.Authorize(aliceCtx, aliceActions)
	fmt.Println("alice의 인가 결과:")
	for i, action := range aliceActions {
		printAuthResult(aliceCtx, action, results[i])
	}

	// bob 테스트
	fmt.Println()
	bobCtx := RequestContext{Principal: "User:bob", Host: "192.168.1.20"}
	bobActions := []Action{
		{ResourceTopic, "orders", OpRead},
		{ResourceTopic, "orders", OpWrite},
	}
	results = auth.Authorize(bobCtx, bobActions)
	fmt.Println("bob의 인가 결과:")
	for i, action := range bobActions {
		printAuthResult(bobCtx, action, results[i])
	}

	// =========================================================================
	// 시나리오 2: DENY-first 평가
	// =========================================================================
	printSeparator("시나리오 2: DENY-first 평가")
	fmt.Println("DENY 규칙이 ALLOW보다 우선한다. eve는 ALLOW와 DENY 둘 다 있을 때 거부된다.")
	fmt.Println()

	auth2 := NewStandardAuthorizer(false)

	// eve에게 모든 토픽 READ 허용 (와일드카드)
	auth2.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "*", PatternWildcard},
		Entry: AccessControlEntry{
			Principal: "User:eve", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})

	// eve의 sensitive-* 토픽 READ 거부
	auth2.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "sensitive-", PatternPrefixed},
		Entry: AccessControlEntry{
			Principal: "User:eve", Host: "*", Operation: OpRead, PermissionType: PermDeny,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth2.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Println()

	eveCtx := RequestContext{Principal: "User:eve", Host: "10.0.0.5"}
	eveActions := []Action{
		{ResourceTopic, "public-data", OpRead},
		{ResourceTopic, "user-events", OpRead},
		{ResourceTopic, "sensitive-pii", OpRead},
		{ResourceTopic, "sensitive-financial", OpRead},
	}
	results = auth2.Authorize(eveCtx, eveActions)
	fmt.Println("eve의 인가 결과 (DENY-first):")
	for i, action := range eveActions {
		printAuthResult(eveCtx, action, results[i])
	}
	fmt.Println()
	fmt.Println("  --> sensitive-* 접두사 매칭으로 DENY가 적용되어 ALLOW보다 우선")

	// =========================================================================
	// 시나리오 3: 슈퍼 유저
	// =========================================================================
	printSeparator("시나리오 3: 슈퍼 유저")
	fmt.Println("슈퍼 유저는 모든 ACL 검사를 건너뛰고 항상 허용된다.")
	fmt.Println()

	auth3 := NewStandardAuthorizer(false)
	auth3.AddSuperUser("User:admin")

	// DENY-all 규칙 추가 (슈퍼 유저는 무시)
	auth3.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "*", PatternWildcard},
		Entry: AccessControlEntry{
			Principal: "User:*", Host: "*", Operation: OpAll, PermissionType: PermDeny,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth3.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Printf("슈퍼 유저: %v\n\n", []string{"User:admin"})

	adminCtx := RequestContext{Principal: "User:admin", Host: "10.0.0.1"}
	normalCtx := RequestContext{Principal: "User:normal", Host: "10.0.0.2"}

	adminActions := []Action{
		{ResourceTopic, "any-topic", OpRead},
		{ResourceTopic, "any-topic", OpWrite},
		{ResourceTopic, "any-topic", OpDelete},
		{ResourceCluster, "kafka-cluster", OpAlter},
	}

	results = auth3.Authorize(adminCtx, adminActions)
	fmt.Println("admin (슈퍼유저) 인가 결과:")
	for i, action := range adminActions {
		printAuthResult(adminCtx, action, results[i])
	}

	results = auth3.Authorize(normalCtx, adminActions[:2])
	fmt.Println("\nnormal (일반유저) 인가 결과:")
	for i, action := range adminActions[:2] {
		printAuthResult(normalCtx, action, results[i])
	}

	// =========================================================================
	// 시나리오 4: 와일드카드 패턴 매칭
	// =========================================================================
	printSeparator("시나리오 4: 패턴 매칭 (Literal, Prefixed, Wildcard)")
	fmt.Println("다양한 패턴 유형으로 ACL을 정의하고 매칭 결과를 확인한다.")
	fmt.Println()

	auth4 := NewStandardAuthorizer(false)

	// LITERAL: 정확한 이름 매칭
	auth4.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "exact-topic", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:dev", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})

	// PREFIXED: 접두사 매칭
	auth4.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "team-a.", PatternPrefixed},
		Entry: AccessControlEntry{
			Principal: "User:dev", Host: "*", Operation: OpAll, PermissionType: PermAllow,
		},
	})

	// WILDCARD: 컨슈머 그룹 전체
	auth4.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceGroup, "*", PatternWildcard},
		Entry: AccessControlEntry{
			Principal: "User:dev", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth4.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Println()

	devCtx := RequestContext{Principal: "User:dev", Host: "172.16.0.5"}

	type testCase struct {
		action  Action
		comment string
	}

	tests := []testCase{
		{Action{ResourceTopic, "exact-topic", OpRead}, "LITERAL 정확 매칭"},
		{Action{ResourceTopic, "exact-topic-2", OpRead}, "LITERAL 불일치"},
		{Action{ResourceTopic, "team-a.orders", OpRead}, "PREFIXED 매칭"},
		{Action{ResourceTopic, "team-a.payments", OpWrite}, "PREFIXED + OpAll 매칭"},
		{Action{ResourceTopic, "team-b.orders", OpRead}, "PREFIXED 불일치"},
		{Action{ResourceGroup, "my-consumer-group", OpRead}, "WILDCARD 매칭"},
		{Action{ResourceGroup, "any-group", OpRead}, "WILDCARD 매칭"},
		{Action{ResourceGroup, "my-group", OpWrite}, "WILDCARD + Op 불일치"},
	}

	fmt.Println("dev의 인가 결과:")
	for _, tc := range tests {
		result := auth4.Authorize(devCtx, []Action{tc.action})
		printAuthResult(devCtx, tc.action, result[0])
		fmt.Printf("         (%s)\n", tc.comment)
	}

	// =========================================================================
	// 시나리오 5: 호스트 기반 제한
	// =========================================================================
	printSeparator("시나리오 5: 호스트 기반 접근 제한")
	fmt.Println("특정 IP에서만 접근을 허용하고, 다른 IP에서는 거부한다.")
	fmt.Println()

	auth5 := NewStandardAuthorizer(false)

	// service 계정: 10.0.1.100에서만 허용
	auth5.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "production-", PatternPrefixed},
		Entry: AccessControlEntry{
			Principal: "User:service", Host: "10.0.1.100", Operation: OpAll, PermissionType: PermAllow,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth5.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Println()

	allowedHost := RequestContext{Principal: "User:service", Host: "10.0.1.100"}
	blockedHost := RequestContext{Principal: "User:service", Host: "10.0.1.200"}

	action := Action{ResourceTopic, "production-orders", OpWrite}

	result := auth5.Authorize(allowedHost, []Action{action})
	fmt.Println("허용된 호스트에서 접근:")
	printAuthResult(allowedHost, action, result[0])

	result = auth5.Authorize(blockedHost, []Action{action})
	fmt.Println("\n차단된 호스트에서 접근:")
	printAuthResult(blockedHost, action, result[0])

	// =========================================================================
	// 시나리오 6: 다중 리소스/작업 일괄 인가
	// =========================================================================
	printSeparator("시나리오 6: 다중 리소스 일괄 인가")
	fmt.Println("하나의 요청에서 여러 리소스/작업을 동시에 인가 검사한다.")
	fmt.Println("(Authorizer.authorize(ctx, List<Action>) 패턴)")
	fmt.Println()

	auth6 := NewStandardAuthorizer(false)

	// 컨슈머 앱: 특정 토픽 READ + 컨슈머 그룹 READ 필요
	auth6.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceTopic, "events", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:consumer-app", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})
	auth6.AddAcl(AclBinding{
		Pattern: ResourcePattern{ResourceGroup, "consumer-group-1", PatternLiteral},
		Entry: AccessControlEntry{
			Principal: "User:consumer-app", Host: "*", Operation: OpRead, PermissionType: PermAllow,
		},
	})

	fmt.Println("등록된 ACL:")
	for _, acl := range auth6.acls {
		fmt.Printf("  %s\n", acl)
	}
	fmt.Println()

	consumerCtx := RequestContext{Principal: "User:consumer-app", Host: "10.0.0.50"}

	// Kafka 컨슈머가 실제로 필요한 권한들
	consumerActions := []Action{
		{ResourceTopic, "events", OpRead},             // 토픽 읽기
		{ResourceTopic, "events", OpDescribe},         // 토픽 메타데이터
		{ResourceGroup, "consumer-group-1", OpRead},   // 컨슈머 그룹
		{ResourceGroup, "consumer-group-2", OpRead},   // 다른 그룹 (미허용)
		{ResourceTopic, "other-topic", OpRead},        // 다른 토픽 (미허용)
	}

	results = auth6.Authorize(consumerCtx, consumerActions)
	fmt.Println("consumer-app 일괄 인가 결과:")
	for i, action := range consumerActions {
		printAuthResult(consumerCtx, action, results[i])
	}

	allAllowed := true
	for _, r := range results {
		if r == ResultDenied {
			allAllowed = false
			break
		}
	}
	fmt.Printf("\n  일괄 결과: %v (컨슈머 시작 %s)\n",
		func() string {
			if allAllowed {
				return "모두 허용"
			}
			return "일부 거부"
		}(),
		func() string {
			if allAllowed {
				return "가능"
			}
			return "불가"
		}())

	// =========================================================================
	// 인가 흐름 시각화
	// =========================================================================
	printSeparator("ACL 인가 평가 흐름")
	fmt.Println(`
  요청: (principal=User:X, host=1.2.3.4, action=READ TOPIC:orders)
                     │
                     v
          ┌──────────────────┐
          │ 슈퍼 유저인가?    │──── Yes ──── ALLOWED
          └────────┬─────────┘
                   │ No
                   v
          ┌──────────────────┐
          │ DENY ACL 매칭?   │──── Yes ──── DENIED
          │ (DENY-first)     │              (DENY가 ALLOW보다 우선)
          └────────┬─────────┘
                   │ No
                   v
          ┌──────────────────┐
          │ ALLOW ACL 매칭?  │──── Yes ──── ALLOWED
          └────────┬─────────┘
                   │ No
                   v
          ┌──────────────────┐
          │ allowIfNoAcl?    │──── Yes ──── ALLOWED
          └────────┬─────────┘
                   │ No
                   v
                DENIED

  패턴 매칭 우선순위:
  1. LITERAL  - 정확한 이름 매칭 (topic="orders")
  2. PREFIXED - 접두사 매칭 (topic="order*")
  3. WILDCARD - 모든 리소스 매칭 (topic="*")`)

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("핵심 요약")
	fmt.Println(`
  1. AclBinding = ResourcePattern + AccessControlEntry
     - ResourcePattern: (ResourceType, Name, PatternType)
     - AccessControlEntry: (Principal, Host, Operation, PermissionType)

  2. DENY-first 평가 방식 (StandardAuthorizerData.java)
     - DENY 규칙이 하나라도 매칭되면 즉시 DENIED
     - 그 후 ALLOW 규칙 검사

  3. 슈퍼 유저는 모든 ACL 검사 우회
     - super.users 설정으로 지정
     - StandardAuthorizer.java의 SUPER_USERS_CONFIG

  4. 패턴 매칭 유형: LITERAL, PREFIXED, WILDCARD
     - WILDCARD_PRINCIPAL = "User:*" (모든 사용자)
     - WILDCARD 호스트 = "*" (모든 호스트)

  5. Authorizer.authorize(ctx, List<Action>)
     - 하나의 요청에 대해 여러 액션을 일괄 검사
     - 각 액션에 대해 독립적으로 ALLOW/DENY 결과 반환`)
}
