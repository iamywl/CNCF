# PoC-16: Kafka ACL 인가(Authorization) 시스템

## 개요

Kafka의 ACL 기반 인가 시스템을 시뮬레이션한다. ResourceType, AclOperation, AclBinding, PatternType 등 핵심 타입을 구현하고, StandardAuthorizer의 DENY-first 평가 알고리즘을 재현한다.

## 참조 소스코드

| 파일 | 핵심 로직 |
|------|----------|
| `clients/src/main/java/org/apache/kafka/server/authorizer/Authorizer.java` | `authorize(ctx, actions)` - 인가 인터페이스 |
| `clients/src/main/java/org/apache/kafka/common/acl/AclBinding.java` | ResourcePattern + AccessControlEntry 결합 |
| `clients/src/main/java/org/apache/kafka/common/resource/ResourceType.java` | TOPIC, GROUP, CLUSTER 등 리소스 유형 |
| `clients/src/main/java/org/apache/kafka/common/acl/AclOperation.java` | READ, WRITE, CREATE, DELETE 등 작업 유형 |
| `metadata/src/main/java/org/apache/kafka/metadata/authorizer/StandardAuthorizerData.java` | DENY-first 평가, WILDCARD 매칭, 슈퍼유저 |

## 핵심 알고리즘

```
authorize(principal, host, action):
  1. if principal in superUsers: return ALLOWED
  2. for each ACL matching resource+principal+host+operation:
       if permissionType == DENY: return DENIED    // DENY-first
  3. for each ACL matching resource+principal+host+operation:
       if permissionType == ALLOW: return ALLOWED
  4. if no matching ACL and allowEveryoneIfNoAcl: return ALLOWED
  5. return DENIED
```

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|---------|------|
| 1. 기본 ALLOW/DENY | 사용자별 토픽 READ/WRITE 권한 부여 |
| 2. DENY-first | ALLOW와 DENY 동시 존재 시 DENY가 우선 |
| 3. 슈퍼 유저 | DENY-all 규칙도 우회하는 슈퍼 유저 |
| 4. 패턴 매칭 | LITERAL, PREFIXED, WILDCARD 패턴 비교 |
| 5. 호스트 기반 제한 | 특정 IP에서만 접근 허용 |
| 6. 일괄 인가 | 다중 리소스/작업 동시 인가 검사 |

## 실행

```bash
go run main.go
```

## 핵심 개념

- **ResourceType**: TOPIC, GROUP, CLUSTER, TRANSACTIONAL_ID 등 ACL 적용 대상
- **AclOperation**: READ, WRITE, CREATE, DELETE, DESCRIBE, ALL(와일드카드) 등
- **PatternType**: LITERAL(정확 매칭), PREFIXED(접두사), WILDCARD(전체)
- **DENY-first**: DENY 규칙이 ALLOW보다 항상 우선하는 평가 방식
- **Super Users**: 모든 ACL 검사를 우회하는 관리자 계정
