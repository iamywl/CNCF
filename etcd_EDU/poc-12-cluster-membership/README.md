# PoC-12: 클러스터 멤버십 (Cluster Membership)

## 개요

etcd Raft 클러스터의 동적 멤버 관리를 시뮬레이션한다. 멤버 추가, 제거, Learner에서 Voter로의 승격, 멤버 ID 생성 알고리즘을 재현한다.

## 핵심 개념

| 개념 | 설명 |
|------|------|
| Member | ID + name + peerURLs + clientURLs + isLearner |
| MemberID | SHA1(peerURLs + clusterName + timestamp)의 상위 8바이트 |
| Voter | 투표권이 있는 정상 멤버. Quorum 계산에 포함 |
| Learner | 투표권 없이 로그만 복제받는 멤버. 승격 전 안전한 동기화 보장 |
| removed set | 제거된 멤버 ID 기록. 동일 ID 재사용 방지 |
| ConfChange | Raft 설정 변경: AddNode, RemoveNode, AddLearner |
| Quorum | VoterCount/2 + 1 (Learner 제외) |

## etcd 소스코드 참조

- `server/etcdserver/api/membership/member.go` — `Member`, `computeMemberID()`, `NewMemberAsLearner()`
- `server/etcdserver/api/membership/cluster.go` — `RaftCluster`, `AddMember()`, `RemoveMember()`, `PromoteMember()`
- `server/etcdserver/api/membership/cluster.go` — `ConfigChangeContext`, `IsReadyToPromoteMember()`

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 초기 3노드 Voter 클러스터 구성
2. 멤버 ID 생성 원리 (SHA1 해시, 결정적/비결정적)
3. 4번째 노드를 Learner로 추가 (Quorum 변화 없음)
4. Learner → Voter 승격 (Quorum 증가)
5. 멤버 제거 + removed set
6. 에러 케이스 (ID 재사용, Learner 수 제한 등)
7. ConfChange 히스토리 확인
8. 멤버십 변경 흐름 요약
