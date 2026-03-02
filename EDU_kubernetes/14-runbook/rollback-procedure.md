# 롤백 절차 (Rollback Procedure)

> 배포 후 문제가 발생했을 때 이전 버전으로 복구하는 절차

## 롤백 판단 기준

아래 중 **하나라도 해당**되면 즉시 롤백합니다:

- [ ] 5xx 에러 비율이 평소 대비 3배 이상 증가
- [ ] 핵심 API (로그인, 결제, 주문) 응답 실패
- [ ] 평균 응답 시간이 평소 대비 5배 이상 증가
- [ ] Pod이 반복적으로 CrashLoopBackOff

## 긴급 롤백 (5분 이내)

### 1단계: Kubernetes Rollback

```bash
# 현재 상태 확인
kubectl rollout history deployment/ecommerce-api -n ecommerce

# 직전 버전으로 롤백
kubectl rollout undo deployment/ecommerce-api -n ecommerce

# 롤백 상태 확인 (완료될 때까지 대기)
kubectl rollout status deployment/ecommerce-api -n ecommerce --timeout=300s
```

### 2단계: 롤백 검증

```bash
# 현재 실행 중인 이미지 버전 확인
kubectl get deployment/ecommerce-api -n ecommerce \
  -o jsonpath='{.spec.template.spec.containers[0].image}'

# 헬스 체크
curl https://api.example.com/health

# Pod 상태 확인
kubectl get pods -n ecommerce -l app=ecommerce-api

# 로그 확인 (에러 없는지)
kubectl logs -n ecommerce -l app=ecommerce-api --tail=20 --since=2m
```

### 3단계: 알림

```
Slack #deploy-alerts:
"🔴 [프로덕션] ecommerce-api 롤백 완료
  - 원인: (간단히 기술)
  - 롤백 버전: vX.Y.Z → vX.Y.W
  - 담당자: @이름"
```

---

## 특정 리비전으로 롤백

```bash
# 배포 이력 확인
kubectl rollout history deployment/ecommerce-api -n ecommerce

# 출력 예시:
# REVISION  CHANGE-CAUSE
# 3         v1.0.0
# 4         v1.1.0
# 5         v1.2.0  ← 문제 버전
# 6         v1.2.1  ← 현재 (롤백했지만 여전히 문제)

# 특정 리비전으로 롤백 (v1.1.0으로)
kubectl rollout undo deployment/ecommerce-api -n ecommerce --to-revision=4
```

---

## DB 마이그레이션 롤백

> **주의**: DB 마이그레이션이 포함된 배포는 단순 이미지 롤백만으로는 불충분합니다.

```bash
# 1. 마이그레이션 이력 확인
kubectl exec -it <api-pod> -- npx knex migrate:list

# 2. 마지막 마이그레이션 롤백
kubectl exec -it <api-pod> -- npx knex migrate:rollback

# 3. 이미지 롤백
kubectl rollout undo deployment/ecommerce-api -n ecommerce

# 4. 검증
kubectl exec -it <api-pod> -- npx knex migrate:list
```

**주의사항**:
- 컬럼 삭제/이름 변경이 포함된 마이그레이션은 **데이터 손실 위험**
- 롤백 전 반드시 **DB 스냅샷** 생성
  ```bash
  # RDS 스냅샷 (AWS)
  aws rds create-db-snapshot \
    --db-instance-identifier prod-db \
    --db-snapshot-identifier pre-rollback-$(date +%Y%m%d-%H%M)
  ```

---

## 롤백 후 체크리스트

- [ ] 서비스 정상화 확인 (헬스 체크, 핵심 API)
- [ ] Grafana 대시보드 에러율 정상화 확인
- [ ] Sentry 신규 에러 없음 확인
- [ ] Slack `#deploy-alerts`에 롤백 완료 알림
- [ ] 장애 원인 분석 시작 (Post-mortem)
- [ ] 이 문서에 장애 사례 추가 (필요시)
