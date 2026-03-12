# PoC-05: Raft 합의 알고리즘 시뮬레이션

## 개요

etcd의 핵심인 Raft 합의 알고리즘을 Go 채널 기반으로 시뮬레이션한다.
리더 선거, 로그 복제, 하트비트, 리더 장애 시 재선거를 재현한다.

## 핵심 개념

| 개념 | 설명 | etcd 소스 |
|------|------|-----------|
| 노드 상태 | Follower, Candidate, Leader | `raft.StateType` |
| 리더 선거 | 선거 타임아웃 → RequestVote → 과반수 → 리더 | `raft.campaign()` |
| 로그 복제 | Leader → AppendEntries → ACK → 커밋 | `raft.bcastAppend()` |
| 하트비트 | 리더가 주기적으로 팔로워에 전송 | `raft.tickHeartbeat()` |
| 텀(Term) | 리더 선출 세대, 높은 텀이 항상 우선 | `raft.Term` |

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **리더 선출**: 3노드 클러스터 시작 → 선거 타임아웃 → 투표 → 리더 당선
2. **로그 복제**: 리더에 PUT 명령 제안 → 팔로워에 복제 → 과반수 ACK → 커밋
3. **리더 장애**: 리더 Kill → 팔로워가 타임아웃 감지 → 새 선거 시작
4. **재선거**: 살아남은 노드 중 새 리더 선출 → 계속 명령 처리
5. **노드 복구**: 장애 노드 복구 → 팔로워로 합류 → 로그 동기화

## 참조 소스

- `go.etcd.io/raft/v3/raft.go` - Raft 상태머신 핵심
- `server/etcdserver/raft.go` - etcd 서버의 Raft 통합
- `go.etcd.io/raft/v3/raftpb/raft.pb.go` - Raft 메시지 정의
