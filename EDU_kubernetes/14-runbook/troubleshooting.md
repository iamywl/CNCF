# 트러블슈팅 가이드

> 마지막 업데이트: 2024-03-01
>
> 이 문서는 실제 장애 경험을 바탕으로 작성되었습니다.
> 장애 복기 후 반드시 이 문서를 업데이트하세요.

## 트러블슈팅 원칙

1. **증상을 정확히 파악**하라 (어떤 API? 에러 코드? 빈도?)
2. **최근 변경 사항을 확인**하라 (배포? 설정 변경? 트래픽 증가?)
3. **로그를 확인**하라 (애플리케이션 로그 → 시스템 로그)
4. **한 번에 한 가지만 변경**하고 결과를 확인하라

---

## 증상별 대응

### 1. Pod이 CrashLoopBackOff 상태

**증상**: `kubectl get pods`에서 Status가 `CrashLoopBackOff`

**진단 순서**:
```bash
# 1. Pod 상태 상세 확인
kubectl describe pod <pod-name> -n ecommerce

# 2. 최근 로그 확인 (이전 실행의 로그)
kubectl logs <pod-name> -n ecommerce --previous

# 3. 이벤트 확인
kubectl get events -n ecommerce --sort-by='.lastTimestamp' | tail -20
```

**자주 발생하는 원인과 해결**:

| 원인 | 로그 키워드 | 해결 |
|------|------------|------|
| DB 연결 실패 | `ECONNREFUSED`, `connection timeout` | DB 상태 확인, 시크릿 확인 |
| 환경 변수 누락 | `undefined`, `missing required` | ConfigMap/Secret 확인 |
| 메모리 부족 | `OOMKilled` | 리소스 limits 증가 |
| 포트 충돌 | `EADDRINUSE` | 컨테이너 포트 설정 확인 |

---

### 2. API 응답 시간 느림 (Latency 증가)

**증상**: 평소 100ms 이하이던 API가 1초 이상 걸림

**진단 순서**:
```bash
# 1. Pod 리소스 사용량 확인
kubectl top pods -n ecommerce

# 2. Node 리소스 확인
kubectl top nodes

# 3. DB 슬로우 쿼리 확인 (PostgreSQL)
kubectl exec -it <db-pod> -- psql -c "
SELECT query, calls, mean_exec_time
FROM pg_stat_statements
ORDER BY mean_exec_time DESC
LIMIT 10;"

# 4. 커넥션 풀 상태 확인
kubectl exec -it <db-pod> -- psql -c "
SELECT count(*) as active FROM pg_stat_activity
WHERE state = 'active';"
```

**자주 발생하는 원인과 해결**:

| 원인 | 해결 |
|------|------|
| DB 커넥션 풀 포화 | `DB_POOL_SIZE` 증가 또는 쿼리 최적화 |
| N+1 쿼리 | ORM 쿼리 확인, JOIN으로 변경 |
| 인덱스 누락 | `EXPLAIN ANALYZE`로 확인 후 인덱스 추가 |
| CPU 부족 | HPA(Horizontal Pod Autoscaler) 확인, Pod 수 증가 |
| 외부 API 지연 | 타임아웃 설정 확인, 서킷 브레이커 동작 확인 |

---

### 3. 5xx 에러 급증

**증상**: Grafana/Sentry에서 500 에러 급증 알림

**진단 순서**:
```bash
# 1. 어떤 엔드포인트에서 발생하는지 확인
kubectl logs -n ecommerce -l app=ecommerce-api --tail=100 | grep "500\|Error\|error"

# 2. 최근 배포 확인
kubectl rollout history deployment/ecommerce-api -n ecommerce

# 3. Sentry에서 에러 스택트레이스 확인
# https://sentry.example.com/...
```

**긴급 조치**:
```bash
# 최근 배포가 원인이라면 즉시 롤백
kubectl rollout undo deployment/ecommerce-api -n ecommerce

# 롤백 확인
kubectl rollout status deployment/ecommerce-api -n ecommerce
```

---

### 4. DB 커넥션 에러

**증상**: `Error: Connection terminated unexpectedly` 또는 `too many connections`

```bash
# 1. 현재 커넥션 수 확인
kubectl exec -it <db-pod> -- psql -c "
SELECT count(*) FROM pg_stat_activity;"

# 2. 커넥션 소유자 확인
kubectl exec -it <db-pod> -- psql -c "
SELECT application_name, state, count(*)
FROM pg_stat_activity
GROUP BY application_name, state
ORDER BY count DESC;"

# 3. idle 커넥션 정리 (긴급 시)
kubectl exec -it <db-pod> -- psql -c "
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE state = 'idle'
AND state_change < now() - interval '10 minutes';"
```

---

### 5. 디스크 용량 부족

**증상**: `No space left on device` 에러

```bash
# 1. 노드 디스크 사용량 확인
kubectl get nodes -o custom-columns=NAME:.metadata.name,DISK:.status.allocatable.ephemeral-storage

# 2. PV 사용량 확인
kubectl get pvc -n ecommerce

# 3. 큰 파일/로그 찾기 (노드 접속 후)
du -sh /var/log/containers/* | sort -rh | head -10

# 4. 오래된 이미지 정리
docker system prune -a --filter "until=168h"
```

---

## 장애 복기 (Post-mortem) 템플릿

장애 해결 후 반드시 아래 형식으로 기록하고, 이 문서를 업데이트하세요.

```markdown
## [날짜] 장애 제목

### 타임라인
- HH:MM - 증상 감지
- HH:MM - 원인 파악
- HH:MM - 조치 완료
- HH:MM - 정상화 확인

### 영향 범위
- 영향받은 서비스:
- 영향 시간:
- 영향받은 사용자 수:

### 근본 원인 (Root Cause)
(상세 설명)

### 해결 방법
(어떻게 해결했는지)

### 재발 방지 대책
- [ ] 대책 1
- [ ] 대책 2

### 교훈
(이번 장애에서 배운 점)
```
