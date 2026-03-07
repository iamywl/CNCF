# Alertmanager 운영 가이드

## 1. 설치 및 실행

### 1.1 바이너리 실행

```bash
# 바이너리 설치
go install github.com/prometheus/alertmanager/cmd/...@latest

# 실행
alertmanager --config.file=alertmanager.yml

# amtool 설치
go install github.com/prometheus/alertmanager/cmd/amtool@latest
```

### 1.2 Docker 실행

```bash
docker run --name alertmanager \
  -d -p 127.0.0.1:9093:9093 \
  -v /path/to/alertmanager.yml:/etc/alertmanager/alertmanager.yml \
  quay.io/prometheus/alertmanager
```

### 1.3 주요 CLI 플래그

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--config.file` | `alertmanager.yml` | 설정 파일 경로 |
| `--storage.path` | `data/` | 스냅샷 저장 경로 |
| `--web.listen-address` | `:9093` | HTTP 바인드 주소 |
| `--web.external-url` | | 외부 접근 URL |
| `--web.route-prefix` | | URL 경로 프리픽스 |
| `--log.level` | `info` | 로그 레벨 (debug, info, warn, error) |
| `--log.format` | `logfmt` | 로그 포맷 (logfmt, json) |
| `--data.retention` | `120h` | 데이터 보존 기간 |
| `--alerts.gc-interval` | `30m` | Alert GC 간격 |

### 1.4 클러스터 플래그

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--cluster.listen-address` | `0.0.0.0:9094` | 클러스터 바인드 주소 |
| `--cluster.advertise-address` | | 클러스터 공개 주소 |
| `--cluster.peer` | | 피어 주소 (반복 가능) |
| `--cluster.peer-timeout` | `15s` | 피어 타임아웃 |
| `--cluster.gossip-interval` | `200ms` | Gossip 전파 간격 |
| `--cluster.pushpull-interval` | `1m0s` | Push-Pull 교환 간격 |
| `--cluster.settle-timeout` | | 안정화 대기 시간 |
| `--cluster.probe-interval` | `1s` | 피어 프로빙 간격 |
| `--cluster.reconnect-interval` | `10s` | 피어 재연결 간격 |
| `--cluster.label` | | 클러스터 라벨 (격리용) |

## 2. 설정 (Configuration)

### 2.1 기본 설정 구조

```yaml
global:
  resolve_timeout: 5m          # Alert 자동 해제 시간
  smtp_smarthost: 'localhost:25'
  smtp_from: 'alertmanager@example.org'

templates:
  - '/etc/alertmanager/templates/*.tmpl'

route:
  receiver: 'default'
  group_by: ['alertname', 'cluster']
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - matchers:
        - severity="critical"
      receiver: 'pager'
    - matchers:
        - severity="warning"
      receiver: 'slack'

inhibit_rules:
  - source_matchers:
      - severity="critical"
    target_matchers:
      - severity="warning"
    equal: ['alertname']

receivers:
  - name: 'default'
    email_configs:
      - to: 'team@example.org'
  - name: 'pager'
    pagerduty_configs:
      - routing_key: '<key>'
  - name: 'slack'
    slack_configs:
      - api_url: 'https://hooks.slack.com/...'
        channel: '#alerts'

time_intervals:
  - name: 'business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - start_time: '09:00'
            end_time: '18:00'
        location: 'Asia/Seoul'
```

### 2.2 라우팅 설정 가이드

#### 그룹핑 전략

```yaml
# 방법 1: 특정 레이블로 그룹핑
group_by: ['alertname', 'cluster']

# 방법 2: 모든 레이블로 그룹핑 (그룹핑 비활성화)
group_by: ['...']
```

#### 시간 기반 뮤팅

```yaml
route:
  receiver: 'default'
  routes:
    - matchers:
        - severity="info"
      receiver: 'slack'
      mute_time_intervals:
        - 'nights-and-weekends'    # 이 시간대에는 알림 억제

    - matchers:
        - severity="critical"
      receiver: 'pager'
      active_time_intervals:
        - 'business-hours'         # 이 시간대에만 알림 전송
```

#### Continue 플래그

```yaml
route:
  receiver: 'default'
  routes:
    - matchers:
        - team="backend"
      receiver: 'backend-slack'
      continue: true               # 다음 Route도 매칭 시도

    - matchers:
        - severity="critical"
      receiver: 'pager'            # 위와 동시에 매칭 가능
```

### 2.3 설정 검증

```bash
# amtool로 설정 유효성 검사
amtool check-config alertmanager.yml

# 라우팅 테스트
amtool config routes test \
  --config.file=alertmanager.yml \
  severity=critical team=infra

# 라우팅 트리 시각화
amtool config routes --config.file=alertmanager.yml
```

### 2.4 설정 리로드

```bash
# 방법 1: SIGHUP 시그널
kill -HUP $(pidof alertmanager)

# 방법 2: HTTP 엔드포인트
curl -X POST http://localhost:9093/-/reload
```

## 3. 고가용성 운영

### 3.1 3-노드 클러스터 구성

```bash
# 노드 1
alertmanager \
  --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager2:9094 \
  --cluster.peer=alertmanager3:9094 \
  --web.listen-address=:9093

# 노드 2
alertmanager \
  --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager1:9094 \
  --cluster.peer=alertmanager3:9094 \
  --web.listen-address=:9093

# 노드 3
alertmanager \
  --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager1:9094 \
  --cluster.peer=alertmanager2:9094 \
  --web.listen-address=:9093
```

### 3.2 Prometheus 설정

```yaml
# prometheus.yml
alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - alertmanager1:9093
            - alertmanager2:9093
            - alertmanager3:9093
```

**중요**: 로드밸런서를 사용하지 말 것. Prometheus는 모든 Alertmanager 인스턴스에 알림을 직접 전송해야 한다.

### 3.3 클러스터 비활성화

```bash
alertmanager --cluster.listen-address=""
```

### 3.4 네트워크 요구사항

| 프로토콜 | 포트 | 용도 |
|----------|------|------|
| TCP | 9093 | HTTP API/UI |
| TCP | 9094 | 클러스터 (Push-Pull, 상태 교환) |
| UDP | 9094 | 클러스터 (Gossip 메시지) |

## 4. 모니터링

### 4.1 핵심 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_alerts` | Gauge | 현재 활성 Alert 수 |
| `alertmanager_silences` | Gauge | 현재 Silence 수 (state 레이블) |
| `alertmanager_notifications_total` | Counter | 알림 전송 총 수 |
| `alertmanager_notifications_failed_total` | Counter | 알림 전송 실패 수 |
| `alertmanager_notification_latency_seconds` | Histogram | 알림 전송 지연시간 |
| `alertmanager_dispatcher_aggregation_groups` | Gauge | 활성 Aggregation Group 수 |
| `alertmanager_dispatcher_alert_processing_duration_seconds` | Summary | Alert 처리 소요시간 |

### 4.2 설정 관련 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_config_hash` | Gauge | 현재 설정 해시값 |
| `alertmanager_config_last_reload_successful` | Gauge | 마지막 리로드 성공 여부 (1/0) |
| `alertmanager_config_last_reload_success_timestamp_seconds` | Gauge | 마지막 성공 리로드 시간 |

### 4.3 클러스터 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_cluster_enabled` | Gauge | 클러스터 활성화 여부 |
| `alertmanager_cluster_members` | Gauge | 클러스터 멤버 수 |
| `alertmanager_cluster_messages_received_total` | Counter | 수신 메시지 수 |
| `alertmanager_cluster_messages_sent_total` | Counter | 송신 메시지 수 |
| `alertmanager_cluster_health_score` | Gauge | 클러스터 건강 점수 |
| `alertmanager_cluster_peer_info` | Gauge | 피어 정보 |

### 4.4 HTTP 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_http_request_duration_seconds` | Histogram | HTTP 요청 소요시간 |
| `alertmanager_http_requests_in_flight` | Gauge | 현재 처리 중인 요청 수 |

### 4.5 Alertmanager 자체 모니터링 규칙

```yaml
# Prometheus alerting rules
groups:
  - name: alertmanager
    rules:
      - alert: AlertmanagerConfigReloadFailed
        expr: alertmanager_config_last_reload_successful == 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Alertmanager 설정 리로드 실패"

      - alert: AlertmanagerNotificationsFailing
        expr: rate(alertmanager_notifications_failed_total[5m]) > 0
        for: 10m
        labels:
          severity: critical
        annotations:
          summary: "알림 전송 실패 발생"

      - alert: AlertmanagerClusterDown
        expr: alertmanager_cluster_members < 3
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "클러스터 멤버 부족"
```

## 5. amtool 운영 가이드

### 5.1 Alert 관리

```bash
# 현재 발생 중인 모든 Alert 조회
amtool alert --alertmanager.url=http://localhost:9093

# 레이블로 Alert 필터링
amtool alert query alertname="HighCPU"
amtool alert query instance=~".+node1"

# 확장 출력
amtool -o extended alert

# JSON 출력
amtool -o json alert
```

### 5.2 Silence 관리

```bash
# Silence 생성
amtool silence add alertname=Test_Alert --comment="유지보수 중"

# 정규식 Silence
amtool silence add alertname=~"Test.*" instance=~".+node0"

# 시간 지정 Silence
amtool silence add alertname=Test_Alert \
  --expires=2h \
  --comment="2시간 동안 억제"

# Silence 조회
amtool silence query

# Silence 만료
amtool silence expire <silenceID>

# 모든 Silence 만료
amtool silence expire $(amtool silence query -q)
```

### 5.3 설정 관리

```bash
# 설정 유효성 검사
amtool check-config alertmanager.yml

# 라우팅 트리 표시
amtool config routes --config.file=alertmanager.yml

# 라우팅 테스트
amtool config routes test \
  --config.file=alertmanager.yml \
  --verify.receivers=team-pager \
  severity=critical team=backend
```

### 5.4 템플릿 테스트

```bash
amtool template render \
  --template.glob='/path/to/templates/*.tmpl' \
  --template.text='{{ template "slack.default.markdown.v1" . }}'
```

### 5.5 amtool 설정 파일

`~/.config/amtool/config.yml`:

```yaml
alertmanager.url: "http://localhost:9093"
author: admin@example.com
comment_required: true
output: extended
```

## 6. 트러블슈팅

### 6.1 일반적인 문제

| 문제 | 원인 | 해결 |
|------|------|------|
| 알림이 전송되지 않음 | Receiver 설정 오류, Silence 활성 | `amtool alert` 확인, Silence 상태 확인 |
| 중복 알림 수신 | 클러스터 nflog 동기화 실패 | 클러스터 메트릭 확인, 네트워크 확인 |
| 설정 리로드 실패 | YAML 문법 오류, 잘못된 참조 | `amtool check-config` 실행 |
| 클러스터 분할 | 네트워크 문제, 방화벽 | UDP/TCP 9094 포트 확인 |
| 메모리 증가 | 대량 Alert, GC 부족 | `alerts.gc-interval` 조정 |

### 6.2 디버깅 방법

```bash
# 디버그 로그 활성화
alertmanager --log.level=debug

# HTTP API로 상태 확인
curl http://localhost:9093/api/v2/status | jq .

# 클러스터 상태 확인
curl http://localhost:9093/api/v2/status | jq .cluster

# 현재 Alert 확인
curl http://localhost:9093/api/v2/alerts | jq .

# 활성 Silence 확인
curl http://localhost:9093/api/v2/silences | jq '.[] | select(.status.state=="active")'
```

### 6.3 성능 튜닝

| 파라미터 | 영향 | 권장 |
|----------|------|------|
| `group_wait` | 첫 알림 지연 | 30s~1m (긴급 알림은 짧게) |
| `group_interval` | 후속 알림 빈도 | 5m~15m |
| `repeat_interval` | 반복 알림 빈도 | 1h~4h |
| `--alerts.gc-interval` | GC 빈도 | 30m (기본값) |
| `--cluster.gossip-interval` | Gossip 빈도 | 200ms (기본값) |
| `--cluster.pushpull-interval` | Push-Pull 빈도 | 1m (기본값) |

## 7. 보안

### 7.1 TLS 설정

```bash
# HTTP TLS
alertmanager \
  --web.config.file=web-config.yml

# web-config.yml
tls_server_config:
  cert_file: /path/to/cert.pem
  key_file: /path/to/key.pem
```

### 7.2 클러스터 TLS

```bash
alertmanager \
  --cluster.tls-config=cluster-tls.yml
```

### 7.3 인증

`--web.config.file`로 기본 인증을 설정할 수 있다:

```yaml
# web-config.yml
basic_auth_users:
  admin: $2y$10$...  # bcrypt 해시
```
