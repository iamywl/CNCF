# PoC-08: Kafka 컨슈머 그룹 코디네이션

## 개요

Kafka의 클래식 컨슈머 그룹 프로토콜을 시뮬레이션한다. 그룹 상태 머신, JoinGroup/SyncGroup 프로토콜, Range 파티션 할당 전략, 하트비트 기반 장애 감지를 포함한다.

## 실행 방법

```bash
go run main.go
```

## Kafka 소스코드 참조

| 컴포넌트 | 원본 파일 | 설명 |
|----------|----------|------|
| ClassicGroup | `group-coordinator/src/main/java/.../classic/ClassicGroup.java` | 그룹 상태 및 멤버 관리 |
| ClassicGroupState | `group-coordinator/src/main/java/.../classic/ClassicGroupState.java` | 상태 머신 정의 |
| ClassicGroupMember | `group-coordinator/src/main/java/.../classic/ClassicGroupMember.java` | 멤버 메타데이터 |
| GroupMetadataManager | `group-coordinator/src/main/java/.../group/GroupMetadataManager.java` | 그룹 메타데이터 관리 |

## 시뮬레이션하는 핵심 개념

### 1. 그룹 상태 머신 (ClassicGroupState.java)

```
EMPTY ──────────────→ PREPARING_REBALANCE ──→ COMPLETING_REBALANCE ──→ STABLE
  ↑                          ↑                                          |
  └──모든 멤버 탈퇴──────────└─── 멤버 장애/탈퇴/새 참여 ───────────────┘

모든 상태 ──→ DEAD (그룹 만료 시)
```

상태 전이 규칙:
```java
EMPTY.addValidPreviousStates(PREPARING_REBALANCE);
PREPARING_REBALANCE.addValidPreviousStates(STABLE, COMPLETING_REBALANCE, EMPTY);
COMPLETING_REBALANCE.addValidPreviousStates(PREPARING_REBALANCE);
STABLE.addValidPreviousStates(COMPLETING_REBALANCE);
DEAD.addValidPreviousStates(STABLE, PREPARING_REBALANCE, COMPLETING_REBALANCE, EMPTY, DEAD);
```

### 2. JoinGroup/SyncGroup 프로토콜

```
Consumer-A ──JoinGroup──→ Coordinator ←──JoinGroup── Consumer-B
                              |
                    (모든 멤버 참여 완료)
                              |
Consumer-A ←─JoinResp─── Coordinator ───JoinResp──→ Consumer-B
 (리더: 멤버 목록 수신)                     (팔로워)

Consumer-A ──SyncGroup──→ Coordinator ←──SyncGroup── Consumer-B
 (할당 제출)                    |           (할당 없이 대기)
                              |
Consumer-A ←─SyncResp─── Coordinator ───SyncResp──→ Consumer-B
 (자신의 할당)                                (자신의 할당)
```

### 3. Range 파티션 할당 전략

```
예: 토픽 "orders" 파티션 3개, 컨슈머 2명

  파티션: [orders-0, orders-1, orders-2]
  멤버: [consumer-A, consumer-B] (사전순)

  partitionsPerMember = 3 / 2 = 1
  remainder = 3 % 2 = 1

  consumer-A → [orders-0, orders-1]  (1 + 나머지 1)
  consumer-B → [orders-2]           (1)
```

### 4. 하트비트 타임아웃 감지

```
각 멤버는 session.timeout.ms 내에 하트비트를 전송해야 한다.
타임아웃되면:
  1. 해당 멤버를 그룹에서 제거
  2. 리더가 제거되면 새 리더 선출
  3. STABLE → PREPARING_REBALANCE로 전이 (리밸런스 트리거)
```

### 5. 멤버 탈퇴 (LeaveGroup)

```
멤버가 자발적으로 탈퇴하면 즉시 리밸런스가 트리거된다.
하트비트 타임아웃을 기다리지 않으므로 더 빠르게 리밸런스가 시작된다.
```

## 리밸런스 시나리오

이 PoC에서 시뮬레이션하는 시나리오:

1. **초기 참여**: 3명의 컨슈머가 JoinGroup → 6개 파티션 Range 할당
2. **하트비트 타임아웃**: 1명의 컨슈머 장애 → 2명으로 재할당
3. **자발적 탈퇴**: 1명 LeaveGroup → 1명이 모든 파티션 담당

## 상태별 동작

| 상태 | Heartbeat 응답 | JoinGroup | SyncGroup |
|------|---------------|-----------|-----------|
| EMPTY | UNKNOWN_MEMBER_ID | 수락 | UNKNOWN_MEMBER_ID |
| PREPARING_REBALANCE | REBALANCE_IN_PROGRESS | 수락 | REBALANCE_IN_PROGRESS |
| COMPLETING_REBALANCE | REBALANCE_IN_PROGRESS | 리밸런스 트리거 | 수락 |
| STABLE | 정상 | 리밸런스 트리거 | 현재 할당 반환 |
