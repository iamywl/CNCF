# etcd 운영 가이드

## 1. 개요

etcd 클러스터의 배포, 설정, 모니터링, 트러블슈팅을 다룬다. etcd는 Kubernetes의 핵심 데이터 저장소이므로 안정적인 운영이 극히 중요하다.

## 2. 배포

### 2.1 하드웨어 요구사항

| 항목 | 소규모 (< 100 노드) | 대규모 (> 1000 노드) |
|------|---------------------|---------------------|
| CPU | 2~4 코어 | 8~16 코어 |
| 메모리 | 8GB | 16~64GB |
| 디스크 | 50 IOPS SSD | 500+ IOPS SSD |
| 네트워크 | 1Gbps | 10Gbps |

**디스크가 가장 중요하다.** etcd는 WAL과 스냅샷을 디스크에 fsync하므로, 디스크 지연이 전체 성능을 좌우한다.

### 2.2 클러스터 크기

| 노드 수 | 장애 허용 | 권장 용도 |
|--------|---------|----------|
| 1 | 0 | 개발/테스트 |
| 3 | 1 | 프로덕션 (가장 일반적) |
| 5 | 2 | 대규모 프로덕션 |
| 7 | 3 | 극도로 높은 가용성 요구 |

**홀수 노드**를 사용한다. 짝수 노드는 과반수 요구에 이점이 없다 (4노드도 3노드와 동일한 1대 장애 허용).

### 2.3 정적 클러스터 시작

```bash
# 노드 1
etcd --name infra0 \
  --initial-advertise-peer-urls http://10.0.1.10:2380 \
  --listen-peer-urls http://10.0.1.10:2380 \
  --listen-client-urls http://10.0.1.10:2379,http://127.0.0.1:2379 \
  --advertise-client-urls http://10.0.1.10:2379 \
  --initial-cluster-token etcd-cluster-1 \
  --initial-cluster infra0=http://10.0.1.10:2380,infra1=http://10.0.1.11:2380,infra2=http://10.0.1.12:2380 \
  --initial-cluster-state new

# 노드 2, 3도 동일하게 (name, URL만 변경)
```

### 2.4 동적 멤버 추가

```bash
# 기존 클러스터에 Learner로 추가
etcdctl member add infra3 \
  --peer-urls="http://10.0.1.13:2380" \
  --learner

# 새 노드 시작
etcd --name infra3 \
  --initial-cluster "infra0=...,infra1=...,infra2=...,infra3=http://10.0.1.13:2380" \
  --initial-cluster-state existing

# Learner → Voter 승격
etcdctl member promote <member-id>
```

## 3. 핵심 설정

### 3.1 서버 설정 (`server/config/config.go`)

```yaml
# etcd 설정 예시 (YAML 또는 플래그)

# 기본 정보
name: "node1"
data-dir: "/var/lib/etcd"
wal-dir: ""                    # 별도 WAL 디스크 (권장)

# 네트워크
listen-client-urls: "http://0.0.0.0:2379"
advertise-client-urls: "http://10.0.1.10:2379"
listen-peer-urls: "http://0.0.0.0:2380"
initial-advertise-peer-urls: "http://10.0.1.10:2380"

# Raft 타이밍
heartbeat-interval: 100        # ms (기본)
election-timeout: 1000         # ms (기본, heartbeat × 10)

# 스냅샷
snapshot-count: 10000          # 스냅샷 트리거 엔트리 수

# 자동 컴팩션
auto-compaction-mode: "periodic"    # 또는 "revision"
auto-compaction-retention: "1h"     # 1시간 이전 리비전 정리

# 리소스 제한
quota-backend-bytes: 8589934592     # 8GB (기본 2GB)
max-txn-ops: 128                    # 트랜잭션당 최대 연산
max-request-bytes: 1572864          # 1.5MB

# 보안
client-cert-auth: true
cert-file: "/etc/etcd/server.crt"
key-file: "/etc/etcd/server.key"
trusted-ca-file: "/etc/etcd/ca.crt"
peer-cert-file: "/etc/etcd/peer.crt"
peer-key-file: "/etc/etcd/peer.key"
peer-trusted-ca-file: "/etc/etcd/ca.crt"

# 인증
auth-token: "jwt"                   # "simple" 또는 "jwt"
```

### 3.2 핵심 설정 설명

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `heartbeat-interval` | 100ms | Raft 하트비트 주기. 네트워크 RTT의 0.5~1.5배 |
| `election-timeout` | 1000ms | 선거 타임아웃. heartbeat의 5~50배 |
| `snapshot-count` | 10000 | 이 수만큼 엔트리가 쌓이면 스냅샷 |
| `quota-backend-bytes` | 2GB | DB 크기 제한. 초과 시 ALARM NOSPACE |
| `auto-compaction-retention` | - | 자동 컴팩션 보존 기간/리비전 |
| `max-request-bytes` | 1.5MB | 최대 요청 크기 |
| `wal-dir` | data-dir/member/wal | WAL 전용 디스크 경로 (I/O 분리 권장) |

### 3.3 WAL 디스크 분리

WAL과 데이터를 별도 디스크에 두면 성능이 크게 향상된다:

```bash
etcd --data-dir /ssd1/etcd-data \
     --wal-dir /ssd2/etcd-wal
```

```
SSD 1 (데이터):
  /ssd1/etcd-data/member/snap/     # 스냅샷 + BoltDB

SSD 2 (WAL):
  /ssd2/etcd-wal/                  # WAL 세그먼트 (순차 쓰기)
```

## 4. TLS 보안

### 4.1 통신 채널

| 채널 | 기본 포트 | TLS 설정 |
|------|---------|----------|
| Client → Server | 2379 | `cert-file`, `key-file`, `trusted-ca-file` |
| Server → Server (Peer) | 2380 | `peer-cert-file`, `peer-key-file`, `peer-trusted-ca-file` |

### 4.2 인증 활성화

```bash
# 1. root 사용자 생성
etcdctl user add root

# 2. 인증 활성화
etcdctl auth enable

# 3. 역할 생성 및 권한 부여
etcdctl role add reader
etcdctl role grant-permission reader read /app/ /app0

# 4. 사용자에 역할 부여
etcdctl user add app-user
etcdctl user grant-role app-user reader

# 5. 인증된 접근
etcdctl --user app-user:password get /app/config
```

## 5. 모니터링

### 5.1 핵심 메트릭

etcd는 Prometheus 형식의 메트릭을 `/metrics` 엔드포인트에서 제공한다.

#### 서버 상태

| 메트릭 | 설명 | 경고 기준 |
|--------|------|----------|
| `etcd_server_has_leader` | 리더 존재 여부 | 0이면 즉시 조치 |
| `etcd_server_leader_changes_seen_total` | 리더 변경 횟수 | 잦은 변경은 네트워크 문제 |
| `etcd_server_proposals_committed_total` | 커밋된 제안 수 | 증가 멈추면 문제 |
| `etcd_server_proposals_failed_total` | 실패한 제안 수 | 0이 아니면 조사 |
| `etcd_server_proposals_pending` | 대기 중 제안 수 | 증가하면 과부하 |

#### 디스크 성능

| 메트릭 | 설명 | 경고 기준 |
|--------|------|----------|
| `etcd_disk_wal_fsync_duration_seconds` | WAL fsync 지연 | p99 > 10ms 경고 |
| `etcd_disk_backend_commit_duration_seconds` | DB 커밋 지연 | p99 > 25ms 경고 |

#### 네트워크

| 메트릭 | 설명 | 경고 기준 |
|--------|------|----------|
| `etcd_network_peer_round_trip_time_seconds` | 피어 RTT | p99 > 50ms 경고 |
| `etcd_network_peer_sent_failures_total` | 피어 전송 실패 | 증가하면 네트워크 문제 |

#### 저장소

| 메트릭 | 설명 | 경고 기준 |
|--------|------|----------|
| `etcd_mvcc_db_total_size_in_bytes` | DB 전체 크기 | quota의 80% 경고 |
| `etcd_mvcc_db_total_size_in_use_in_bytes` | DB 사용 크기 | total과 차이 크면 defrag 필요 |
| `etcd_debugging_mvcc_keys_total` | 총 키 수 | - |
| `etcd_debugging_mvcc_watch_stream_total` | Watch 스트림 수 | - |

### 5.2 Prometheus 알림 규칙 예시

```yaml
groups:
  - name: etcd
    rules:
      - alert: EtcdNoLeader
        expr: etcd_server_has_leader == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "etcd 클러스터에 리더가 없음"

      - alert: EtcdHighFsyncDuration
        expr: histogram_quantile(0.99, etcd_disk_wal_fsync_duration_seconds_bucket) > 0.5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "WAL fsync 지연 높음"

      - alert: EtcdDatabaseSpaceExceeded
        expr: etcd_mvcc_db_total_size_in_bytes / etcd_server_quota_backend_bytes > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "DB 용량 80% 초과"

      - alert: EtcdHighNumberOfLeaderChanges
        expr: increase(etcd_server_leader_changes_seen_total[1h]) > 3
        labels:
          severity: warning
        annotations:
          summary: "1시간 내 리더 변경 3회 초과"
```

### 5.3 Grafana 대시보드 핵심 패널

```
Row 1: 클러스터 상태
  - 리더 여부 (has_leader)
  - 리더 변경 횟수
  - 대기 제안 수

Row 2: 성능
  - WAL fsync 지연 (히스토그램)
  - DB 커밋 지연 (히스토그램)
  - 피어 RTT

Row 3: 저장소
  - DB 크기 (total vs in-use)
  - 키 수
  - 컴팩션 리비전

Row 4: gRPC
  - 요청 비율 (per RPC)
  - 요청 지연 (히스토그램)
  - 실패 비율
```

## 6. 일상 운영

### 6.1 상태 확인

```bash
# 클러스터 상태
etcdctl endpoint status --cluster -w table

# 멤버 목록
etcdctl member list -w table

# 헬스 체크
etcdctl endpoint health --cluster
```

### 6.2 컴팩션

```bash
# 현재 리비전 확인
rev=$(etcdctl endpoint status --write-out="json" | jq '.[0].Status.header.revision')

# 수동 컴팩션
etcdctl compact $rev

# 자동 컴팩션 설정 (서버 시작 시)
etcd --auto-compaction-mode=periodic --auto-compaction-retention=1h
```

### 6.3 조각 모음 (Defragment)

```bash
# 단일 노드 defrag
etcdctl defrag --endpoints=http://10.0.1.10:2379

# 전체 클러스터 defrag (한 노드씩 순차적으로)
etcdctl defrag --cluster

# 주의: defrag 중 해당 노드가 일시 중단됨
# 프로덕션에서는 한 노드씩 순차 실행 권장
```

### 6.4 백업과 복구

```bash
# 스냅샷 백업
etcdctl snapshot save /backup/etcd-$(date +%Y%m%d).db

# 스냅샷 상태 확인
etcdctl snapshot status /backup/etcd-20240101.db -w table

# 스냅샷 복구
etcdutl snapshot restore /backup/etcd-20240101.db \
  --name infra0 \
  --initial-cluster "infra0=http://10.0.1.10:2380,..." \
  --initial-cluster-token etcd-cluster-1 \
  --initial-advertise-peer-urls http://10.0.1.10:2380 \
  --data-dir /var/lib/etcd-restored
```

### 6.5 알람 관리

```bash
# 알람 조회
etcdctl alarm list

# NOSPACE 알람 해제 (defrag 후)
etcdctl alarm disarm

# quota 초과 시 복구 절차
# 1. 컴팩션 실행
etcdctl compact <revision>
# 2. 조각 모음
etcdctl defrag --cluster
# 3. 알람 해제
etcdctl alarm disarm
```

## 7. 트러블슈팅

### 7.1 일반적인 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| "etcdserver: no leader" | 과반수 노드 다운 | 노드 복구, 스냅샷 복원 |
| "etcdserver: request timed out" | 디스크 느림, 네트워크 지연 | SSD 교체, 네트워크 확인 |
| "mvcc: database space exceeded" | DB quota 초과 | 컴팩션 → defrag → alarm disarm |
| "rafthttp: failed to dial" | 피어 통신 불가 | 방화벽, DNS, TLS 확인 |
| 잦은 리더 변경 | 불안정한 네트워크/디스크 | 타이밍 조정, 하드웨어 개선 |
| Watch 지연 | 이벤트 과다, 워처 수 | Watch 최적화, 필터 사용 |

### 7.2 디스크 성능 확인

```bash
# fio로 fsync 성능 측정
fio --rw=write --ioengine=sync --fdatasync=1 \
    --directory=/var/lib/etcd --size=22m \
    --bs=2300 --name=etcd-test

# 결과: fsync/s 수치 확인
# 500+ : 좋음
# 100-500 : 보통
# < 100 : etcd에 부적합
```

### 7.3 로그 분석

```bash
# 느린 요청 감지
grep "apply request took too long" /var/log/etcd.log

# 리더 변경 추적
grep "became leader" /var/log/etcd.log
grep "lost leader" /var/log/etcd.log

# Raft 문제 진단
grep "failed to send" /var/log/etcd.log
grep "slow fdatasync" /var/log/etcd.log
```

### 7.4 DB 덤프 도구

```bash
# DB 내용 덤프
etcd-dump-db iterate-db /var/lib/etcd/member/snap/db

# WAL 로그 덤프
etcd-dump-logs /var/lib/etcd/member/wal
```

## 8. 성능 튜닝

### 8.1 Raft 타이밍 조정

```
원칙: network RTT < heartbeat-interval < election-timeout

LAN (RTT < 1ms):
  --heartbeat-interval=100 --election-timeout=1000

WAN (RTT 50-100ms):
  --heartbeat-interval=500 --election-timeout=5000
```

### 8.2 Quota 설정

```bash
# 기본 2GB → 8GB로 증가
etcd --quota-backend-bytes=$((8*1024*1024*1024))

# Kubernetes 대규모 클러스터: 8GB 권장
```

### 8.3 동시 연결 관리

```bash
# gRPC 최대 동시 스트림
--max-concurrent-streams=1000

# Watch 진행 알림 주기 (긴 유휴 Watch 방지)
--experimental-watch-progress-notify-interval=600s
```

## 9. Kubernetes에서의 etcd

### 9.1 매니페스트 예시 (Static Pod)

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: etcd
  namespace: kube-system
spec:
  containers:
  - name: etcd
    image: registry.k8s.io/etcd:3.5.12-0
    command:
    - etcd
    - --data-dir=/var/lib/etcd
    - --listen-client-urls=https://0.0.0.0:2379
    - --advertise-client-urls=https://10.0.1.10:2379
    - --listen-peer-urls=https://0.0.0.0:2380
    - --initial-advertise-peer-urls=https://10.0.1.10:2380
    - --cert-file=/etc/kubernetes/pki/etcd/server.crt
    - --key-file=/etc/kubernetes/pki/etcd/server.key
    - --trusted-ca-file=/etc/kubernetes/pki/etcd/ca.crt
    - --peer-cert-file=/etc/kubernetes/pki/etcd/peer.crt
    - --peer-key-file=/etc/kubernetes/pki/etcd/peer.key
    - --peer-trusted-ca-file=/etc/kubernetes/pki/etcd/ca.crt
    - --snapshot-count=10000
    - --quota-backend-bytes=8589934592
    volumeMounts:
    - mountPath: /var/lib/etcd
      name: etcd-data
    - mountPath: /etc/kubernetes/pki/etcd
      name: etcd-certs
  volumes:
  - hostPath:
      path: /var/lib/etcd
    name: etcd-data
  - hostPath:
      path: /etc/kubernetes/pki/etcd
    name: etcd-certs
```

### 9.2 Kubernetes 관련 주의사항

| 항목 | 권장 사항 |
|------|----------|
| 배포 방식 | Static Pod 또는 외부 클러스터 |
| 백업 주기 | 최소 매시간, 가능하면 매 30분 |
| quota | 대규모 클러스터는 8GB |
| 컴팩션 | kube-apiserver가 자동 수행 |
| 모니터링 | Prometheus + Alertmanager 필수 |
| 디스크 | 전용 SSD, WAL 분리 권장 |

## 10. 재해 복구

### 10.1 단일 노드 장애

```
1. 장애 노드 제거: etcdctl member remove <id>
2. 새 노드 준비
3. 새 노드 추가: etcdctl member add <name> --peer-urls=<url>
4. 새 노드 시작 (--initial-cluster-state=existing)
```

### 10.2 과반수 손실 (재해)

```
1. 최신 스냅샷 확보
2. etcdutl snapshot restore로 각 노드 복원
3. 모든 노드 동시 시작 (--force-new-cluster 또는 새 클러스터)
4. 데이터 무결성 확인
5. Kubernetes API 서버 재시작
```

## 11. 운영 체크리스트

```
일간:
  □ etcd_server_has_leader == 1 확인
  □ WAL fsync 지연 모니터링
  □ DB 크기 모니터링

주간:
  □ 스냅샷 백업 검증
  □ 리더 변경 이력 검토
  □ 디스크 사용량 추세 확인

월간:
  □ 스냅샷 복구 테스트
  □ Defrag 실행 (필요 시)
  □ 성능 벤치마크
  □ TLS 인증서 만료 확인
```
